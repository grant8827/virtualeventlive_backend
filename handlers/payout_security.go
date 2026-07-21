package handlers

import (
	"context"
	"regexp"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
)

const payoutUnlockDuration = 5 * time.Minute

var sixDigitPasscode = regexp.MustCompile(`^[0-9]{6}$`)

type PayoutSecurityHandler struct {
	DB  *pgxpool.Pool
	Cfg *config.Config
}

type createPayoutPasscodeRequest struct {
	Passcode        string `json:"passcode"`
	ConfirmPasscode string `json:"confirm_passcode"`
	Password        string `json:"password"`
}

type unlockPayoutRequest struct {
	Passcode string `json:"passcode"`
}

func (h *PayoutSecurityHandler) Status(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)
	var passcodeSet bool
	err := h.DB.QueryRow(context.Background(),
		`SELECT payout_passcode_hash IS NOT NULL FROM users WHERE id = $1`, userID,
	).Scan(&passcodeSet)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	return c.JSON(fiber.Map{"passcode_set": passcodeSet})
}

func (h *PayoutSecurityHandler) Create(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)
	var req createPayoutPasscodeRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if !sixDigitPasscode.MatchString(req.Passcode) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "passcode must be exactly 6 digits"})
	}
	if req.Passcode != req.ConfirmPasscode {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "passcodes do not match"})
	}
	if req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "account password is required"})
	}

	var passwordHash string
	var alreadySet bool
	err := h.DB.QueryRow(context.Background(),
		`SELECT password_hash, payout_passcode_hash IS NOT NULL FROM users WHERE id = $1`, userID,
	).Scan(&passwordHash, &alreadySet)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	if alreadySet {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "payout passcode is already set"})
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		auditPayoutEvent(h.DB, c, userID, "passcode_create_denied")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account password is incorrect"})
	}

	// Pepper the low-entropy numeric passcode with the server secret before
	// hashing so a database-only leak cannot be brute-forced over just 1M values.
	passcodeHash, err := bcrypt.GenerateFromPassword([]byte(req.Passcode+":"+h.Cfg.JWTSecret), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to secure passcode"})
	}
	result, err := h.DB.Exec(context.Background(),
		`UPDATE users SET payout_passcode_hash = $1, payout_passcode_failed_attempts = 0,
		 payout_passcode_locked_until = NULL, payout_passcode_created_at = NOW(), updated_at = NOW()
		 WHERE id = $2 AND payout_passcode_hash IS NULL`, string(passcodeHash), userID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save payout passcode"})
	}
	if result.RowsAffected() != 1 {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "payout passcode is already set"})
	}
	auditPayoutEvent(h.DB, c, userID, "passcode_created")
	return h.issueToken(c, userID)
}

func (h *PayoutSecurityHandler) Unlock(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)
	var req unlockPayoutRequest
	if err := c.BodyParser(&req); err != nil || !sixDigitPasscode.MatchString(req.Passcode) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter your 6-digit passcode"})
	}

	var passcodeHash *string
	var lockedUntil *time.Time
	err := h.DB.QueryRow(context.Background(),
		`SELECT payout_passcode_hash, payout_passcode_locked_until FROM users WHERE id = $1`, userID,
	).Scan(&passcodeHash, &lockedUntil)
	if err != nil || passcodeHash == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "payout passcode has not been created"})
	}
	if lockedUntil != nil && lockedUntil.After(time.Now()) {
		auditPayoutEvent(h.DB, c, userID, "unlock_blocked")
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error":        "too many attempts; try again later",
			"locked_until": lockedUntil,
		})
	}

	if bcrypt.CompareHashAndPassword([]byte(*passcodeHash), []byte(req.Passcode+":"+h.Cfg.JWTSecret)) != nil {
		var nextLock *time.Time
		_ = h.DB.QueryRow(context.Background(),
			`UPDATE users SET
			 payout_passcode_failed_attempts = CASE
			   WHEN payout_passcode_locked_until IS NOT NULL AND payout_passcode_locked_until <= NOW() THEN 1
			   ELSE payout_passcode_failed_attempts + 1 END,
			 payout_passcode_locked_until = CASE
			   WHEN (CASE WHEN payout_passcode_locked_until IS NOT NULL AND payout_passcode_locked_until <= NOW() THEN 1 ELSE payout_passcode_failed_attempts + 1 END) >= 5
			   THEN NOW() + INTERVAL '15 minutes' ELSE NULL END
			 WHERE id = $1 RETURNING payout_passcode_locked_until`, userID,
		).Scan(&nextLock)
		if nextLock != nil {
			auditPayoutEvent(h.DB, c, userID, "passcode_locked")
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many attempts; try again in 15 minutes"})
		}
		auditPayoutEvent(h.DB, c, userID, "unlock_failed")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "incorrect payout passcode"})
	}

	_, _ = h.DB.Exec(context.Background(),
		`UPDATE users SET payout_passcode_failed_attempts = 0, payout_passcode_locked_until = NULL WHERE id = $1`, userID,
	)
	auditPayoutEvent(h.DB, c, userID, "unlocked")
	return h.issueToken(c, userID)
}

func auditPayoutEvent(db *pgxpool.Pool, c *fiber.Ctx, userID, eventType string) {
	_, _ = db.Exec(context.Background(),
		`INSERT INTO payout_security_events (user_id, event_type, ip_address, user_agent)
		 VALUES ($1, $2, $3, $4)`, userID, eventType, c.IP(), c.Get("User-Agent"),
	)
}

func (h *PayoutSecurityHandler) issueToken(c *fiber.Ctx, userID string) error {
	now := time.Now()
	claims := &middleware.PayoutClaims{
		UserID: userID,
		Scope:  "payout",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(payoutUnlockDuration)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(h.Cfg.JWTSecret))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to unlock payouts"})
	}
	return c.JSON(fiber.Map{"payout_token": tokenString, "expires_in": int(payoutUnlockDuration.Seconds())})
}
