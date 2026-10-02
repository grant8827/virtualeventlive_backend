package handlers

import (
	"context"
	"errors"
	"log"
	"net/mail"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
	"vertualeventlive/backend/services"
)

// Verified email (AUTH-05). Links are single-use, expire after a day and are
// stored as SHA-256 hashes (same construction as invitations).

const emailVerificationTTL = 24 * time.Hour

type emailVerifier struct {
	DB    *pgxpool.Pool
	Email *services.EmailService
	Cfg   *config.Config
}

// start issues a verification link for userID and emails it to toEmail.
// purpose "verify" confirms the current address; "change" confirms a new one.
func (v emailVerifier) start(ctx context.Context, userID, toEmail, purpose string) error {
	token, hash, err := newInvitationToken()
	if err != nil {
		return err
	}
	// Only the newest link for this user and purpose works.
	if _, err := v.DB.Exec(ctx,
		`DELETE FROM email_verifications WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose,
	); err != nil {
		return err
	}
	if _, err := v.DB.Exec(ctx,
		`INSERT INTO email_verifications (user_id, email, purpose, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, NOW() + make_interval(secs => $5))`,
		userID, toEmail, purpose, hash, emailVerificationTTL.Seconds(),
	); err != nil {
		return err
	}
	link := v.Cfg.FrontendURL + "/verify-email?token=" + token
	if v.Email == nil || !v.Email.Enabled() {
		// Local development without email: print the link so it can be used.
		if v.Cfg.IsDevelopment() {
			log.Printf("email verification (dev, email not configured) for %s: %s", toEmail, link)
		}
		return nil
	}
	go func() {
		if err := v.Email.SendEmailVerification(toEmail, link, purpose == "change"); err != nil {
			log.Printf("verification email to %s failed: %v", toEmail, err)
		}
	}()
	return nil
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

var errVerificationInvalid = errors.New("This verification link is invalid or has expired. Request a new one.")

// VerifyEmail applies a verification link: confirms the account's email, or
// switches it to the new address that was requested.
func (h *AuthHandler) VerifyEmail(c *fiber.Ctx) error {
	var req verifyEmailRequest
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": errVerificationInvalid.Error()})
	}
	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to verify email"})
	}
	defer tx.Rollback(ctx)

	var id, userID, email, purpose string
	err = tx.QueryRow(ctx,
		`SELECT id, user_id, email, purpose FROM email_verifications
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		 FOR UPDATE`, hashInvitationToken(req.Token),
	).Scan(&id, &userID, &email, &purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": errVerificationInvalid.Error()})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to verify email"})
	}

	var tag pgconn.CommandTag
	if purpose == "change" {
		// The new address becomes the sign-in email only now that it's proven.
		tag, err = tx.Exec(ctx,
			`UPDATE users SET email = $2, pending_email = NULL, email_verified_at = NOW(), updated_at = NOW()
			 WHERE id = $1 AND lower(pending_email) = lower($2)`, userID, email)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "That email address is already used by another account."})
		}
	} else {
		tag, err = tx.Exec(ctx,
			`UPDATE users SET email_verified_at = COALESCE(email_verified_at, NOW()), updated_at = NOW()
			 WHERE id = $1 AND lower(email) = lower($2)`, userID, email)
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to verify email"})
	}
	// The address changed again since the link was sent: the link is stale.
	if tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": errVerificationInvalid.Error()})
	}
	if _, err := tx.Exec(ctx, `UPDATE email_verifications SET used_at = NOW() WHERE id = $1`, id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to verify email"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to verify email"})
	}
	recordSecurityEvent(ctx, h.DB, c, "email_verified", userID, userID, map[string]any{"purpose": purpose})
	msg := "Your email is verified. You can sign in now."
	if purpose == "change" {
		msg = "Your new email address is confirmed and is now your sign-in email."
	}
	return c.JSON(fiber.Map{"message": msg})
}

type resendVerificationRequest struct {
	Email string `json:"email"`
}

const resendGenericMessage = "If that account needs verifying, we've sent a new link. Check your inbox."

// ResendVerification sends a fresh link to an unverified account. Same
// response whether or not the account exists.
func (h *AuthHandler) ResendVerification(c *fiber.Ctx) error {
	var req resendVerificationRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	email := strings.TrimSpace(req.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a valid email address"})
	}
	ctx := context.Background()
	if n, _ := h.Limiter.Hit(ctx, "resend-verify:"+middleware.HashKey(email), 5*time.Minute); n > 1 {
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"message": resendGenericMessage})
	}
	var userID string
	if err := h.DB.QueryRow(ctx,
		`SELECT id FROM users WHERE lower(email) = lower($1) AND email_verified_at IS NULL`, email,
	).Scan(&userID); err == nil {
		_ = h.verifier().start(ctx, userID, email, "verify")
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"message": resendGenericMessage})
}

func (h *AuthHandler) verifier() emailVerifier {
	return emailVerifier{DB: h.DB, Email: h.Email, Cfg: h.Cfg}
}

func isValidEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}

var errEmailTaken = errors.New("That email address is already used by another account.")

// requestEmailChange records newEmail as pending and sends it a confirmation
// link. The account's sign-in email is unchanged until the link is used.
func requestEmailChange(ctx context.Context, v emailVerifier, userID, newEmail string) (string, error) {
	var taken bool
	if err := v.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE lower(email) = lower($1) AND id <> $2)`, newEmail, userID,
	).Scan(&taken); err != nil {
		return "", errors.New("failed to update email")
	}
	if taken {
		return "", errEmailTaken
	}
	if _, err := v.DB.Exec(ctx, `UPDATE users SET pending_email = $2, updated_at = NOW() WHERE id = $1`, userID, newEmail); err != nil {
		return "", errors.New("failed to update email")
	}
	if err := v.start(ctx, userID, newEmail, "change"); err != nil {
		return "", errors.New("failed to send the confirmation email")
	}
	return newEmail, nil
}
