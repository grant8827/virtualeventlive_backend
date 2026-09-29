package handlers

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"
)

// Forgot-password flow. Tokens reuse the invitation helpers: a random
// 32-byte token in the link, only its SHA-256 in the database.

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// ForgotPassword emails a reset link. It answers the same way whether or not
// the address has an account, so it can't be used to discover who's
// registered, and sends at most one link per account per minute.
func (h *AuthHandler) ForgotPassword(c *fiber.Ctx) error {
	var req forgotPasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	email := strings.TrimSpace(req.Email)
	// Same answer on every path, whether or not the account exists.
	respond := func() error {
		return c.JSON(fiber.Map{"message": "If an account exists for that email, we've sent a link to reset the password."})
	}
	if email == "" {
		return respond()
	}

	ctx := context.Background()
	var userID, accountEmail string
	if err := h.DB.QueryRow(ctx,
		`SELECT id, email FROM users WHERE lower(email) = lower($1)`, email,
	).Scan(&userID, &accountEmail); err != nil {
		return respond()
	}

	var recent bool
	if err := h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM password_resets
		 WHERE user_id = $1 AND created_at > NOW() - INTERVAL '1 minute')`, userID,
	).Scan(&recent); err != nil || recent {
		return respond()
	}

	token, hash, err := newInvitationToken()
	if err != nil {
		return respond()
	}
	// A new link replaces any older unused ones.
	if _, err := h.DB.Exec(ctx,
		`DELETE FROM password_resets WHERE user_id = $1 AND used_at IS NULL`, userID,
	); err != nil {
		return respond()
	}
	if _, err := h.DB.Exec(ctx,
		`INSERT INTO password_resets (user_id, token_hash, expires_at)
		 VALUES ($1, $2, NOW() + INTERVAL '1 hour')`, userID, hash,
	); err != nil {
		return respond()
	}

	// Sent in the background so the response time doesn't reveal whether
	// the account exists.
	resetURL := h.Cfg.FrontendURL + "/reset-password?token=" + token
	go func() {
		if h.Email == nil {
			return
		}
		if err := h.Email.SendPasswordReset(accountEmail, resetURL); err != nil {
			log.Printf("password reset email to %s failed: %v", accountEmail, err)
		}
	}()
	return respond()
}

// LookupPasswordReset lets the reset page say up front when a link is no
// longer valid, instead of after the person types a new password.
func (h *AuthHandler) LookupPasswordReset(c *fiber.Ctx) error {
	var (
		usedAt    *time.Time
		expiresAt time.Time
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT used_at, expires_at FROM password_resets WHERE token_hash = $1`,
		hashInvitationToken(c.Params("token")),
	).Scan(&usedAt, &expiresAt)
	if err != nil || usedAt != nil || time.Now().After(expiresAt) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "This reset link is invalid or has expired. Request a new one."})
	}
	return c.JSON(fiber.Map{"ok": true})
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// ResetPassword sets a new password from a valid link and uses the link up.
func (h *AuthHandler) ResetPassword(c *fiber.Ctx) error {
	var req resetPasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if len(req.Password) < 8 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "password must be at least 8 characters"})
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reset password"})
	}

	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reset password"})
	}
	defer tx.Rollback(ctx)

	// Claim the link; FOR UPDATE stops it being used twice at once.
	var resetID, userID string
	if err := tx.QueryRow(ctx,
		`SELECT id, user_id FROM password_resets
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		 FOR UPDATE`, hashInvitationToken(req.Token),
	).Scan(&resetID, &userID); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "This reset link is invalid or has expired. Request a new one."})
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, string(hash), userID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reset password"})
	}
	if _, err := tx.Exec(ctx, `UPDATE password_resets SET used_at = NOW() WHERE id = $1`, resetID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reset password"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reset password"})
	}
	return c.JSON(fiber.Map{"message": "Your password has been reset. You can sign in now."})
}
