package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/mail"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Invitations are how new hosts join: registration is invite-only. The raw
// token only ever lives in the invite link; the database keeps its hash.

func newInvitationToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashInvitationToken(token), nil
}

func hashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type invitation struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	InvitedBy  *string    `json:"invited_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
	// pending (link not used yet), accepted (registered) or expired.
	Status string `json:"status"`
	// Only returned when the invite is created or resent, so it can be
	// copied and shared when email isn't configured.
	Token string `json:"token,omitempty"`
}

const invitationSelect = `
	SELECT i.id, i.email, u.email, i.created_at, i.expires_at, i.accepted_at,
	       CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted'
	            WHEN i.expires_at < NOW() THEN 'expired'
	            ELSE 'pending' END
	FROM invitations i LEFT JOIN users u ON u.id = i.invited_by`

func scanInvitation(row interface{ Scan(...any) error }) (invitation, error) {
	var inv invitation
	err := row.Scan(&inv.ID, &inv.Email, &inv.InvitedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.Status)
	return inv, err
}

func (h *SuperuserHandler) ListInvitations(c *fiber.Ctx) error {
	rows, err := h.DB.Query(context.Background(), invitationSelect+` ORDER BY i.created_at DESC LIMIT 200`)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load invitations"})
	}
	defer rows.Close()

	invites := []invitation{}
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load invitations"})
		}
		invites = append(invites, inv)
	}
	return c.JSON(fiber.Map{"invitations": invites, "email_enabled": h.Email != nil && h.Email.Enabled()})
}

type createInvitationRequest struct {
	Email string `json:"email"`
}

// CreateInvitation invites an email address. Inviting an address that
// already has an unused invite replaces it with a fresh link.
func (h *SuperuserHandler) CreateInvitation(c *fiber.Ctx) error {
	ctx := context.Background()
	actorID, _ := c.Locals("user_id").(string)

	var req createInvitationRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	email := strings.TrimSpace(req.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a valid email address"})
	}

	var exists bool
	if err := h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE lower(email) = lower($1))`, email,
	).Scan(&exists); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create invitation"})
	}
	if exists {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "that email already has an account"})
	}

	if _, err := h.DB.Exec(ctx,
		`DELETE FROM invitations WHERE lower(email) = lower($1) AND accepted_at IS NULL`, email,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create invitation"})
	}
	return h.issueInvitation(c, email, actorID, fiber.StatusCreated)
}

// ResendInvitation issues a new link (and a new 7-day window) for an unused
// invite. The old link stops working.
func (h *SuperuserHandler) ResendInvitation(c *fiber.Ctx) error {
	ctx := context.Background()
	actorID, _ := c.Locals("user_id").(string)

	var email string
	err := h.DB.QueryRow(ctx,
		`DELETE FROM invitations WHERE id = $1 AND accepted_at IS NULL RETURNING email`, c.Params("id"),
	).Scan(&email)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "invitation not found or already used"})
	}
	return h.issueInvitation(c, email, actorID, fiber.StatusOK)
}

func (h *SuperuserHandler) issueInvitation(c *fiber.Ctx, email, actorID string, status int) error {
	ctx := context.Background()
	token, hash, err := newInvitationToken()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create invitation"})
	}

	var id string
	if err := h.DB.QueryRow(ctx,
		`INSERT INTO invitations (email, token_hash, invited_by, expires_at)
		 VALUES ($1, $2, $3, NOW() + INTERVAL '7 days') RETURNING id`,
		email, hash, actorID,
	).Scan(&id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create invitation"})
	}

	inv, err := scanInvitation(h.DB.QueryRow(ctx, invitationSelect+` WHERE i.id = $1`, id))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create invitation"})
	}
	inv.Token = token

	resp := fiber.Map{"invitation": inv, "email_sent": false}
	if h.Email != nil && h.Email.Enabled() {
		if err := h.Email.SendInvitation(email, h.Email.SiteURL+"/register?token="+token, inv.ExpiresAt); err != nil {
			resp["email_error"] = "the invitation was created but the email could not be sent; copy the link instead"
		} else {
			resp["email_sent"] = true
		}
	}
	return c.Status(status).JSON(resp)
}

func (h *SuperuserHandler) DeleteInvitation(c *fiber.Ctx) error {
	tag, err := h.DB.Exec(context.Background(),
		`DELETE FROM invitations WHERE id = $1 AND accepted_at IS NULL`, c.Params("id"),
	)
	if err != nil || tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "invitation not found or already used"})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// LookupInvitation is public: the register page calls it to show which
// email the invite is for, or that the link is no longer valid.
func (h *AuthHandler) LookupInvitation(c *fiber.Ctx) error {
	var (
		email      string
		acceptedAt *time.Time
		expiresAt  time.Time
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT email, accepted_at, expires_at FROM invitations WHERE token_hash = $1`,
		hashInvitationToken(c.Params("token")),
	).Scan(&email, &acceptedAt, &expiresAt)
	switch {
	case err != nil:
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "This invitation link isn't valid. Ask for a new one."})
	case acceptedAt != nil:
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "This invitation has already been used. Sign in instead."})
	case time.Now().After(expiresAt):
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "This invitation has expired. Ask for a new one."})
	}
	return c.JSON(fiber.Map{"email": email, "expires_at": expiresAt})
}
