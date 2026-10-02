package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
	"vertualeventlive/backend/services"
)

// TeamHandler manages the staff and admin logins attached to a host account.
// Every route runs behind middleware.HostAccount, so Locals("user_id") is the
// owner's ID and Locals("actor_id") is whoever is signed in.
type TeamHandler struct {
	DB    *pgxpool.Pool
	Email *services.EmailService
	Cfg   *config.Config
}

func (h *TeamHandler) verifier() emailVerifier {
	return emailVerifier{DB: h.DB, Email: h.Email, Cfg: h.Cfg}
}

type teamMember struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	FullName   *string   `json:"full_name"`
	StreamName *string   `json:"stream_name"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	// The member signs in only once they've confirmed their email.
	EmailVerified bool    `json:"email_verified"`
	PendingEmail  *string `json:"pending_email"`
}

func validTeamRole(role string) bool {
	return role == middleware.TeamStaff || role == middleware.TeamAdmin
}

func (h *TeamHandler) List(c *fiber.Ctx) error {
	ownerID := c.Locals("user_id").(string)

	rows, err := h.DB.Query(context.Background(),
		`SELECT id, email, full_name, stream_name, role, status, created_at, email_verified_at IS NOT NULL, pending_email
		 FROM users WHERE account_owner_id = $1 ORDER BY created_at`, ownerID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
	}
	defer rows.Close()

	members := []teamMember{}
	for rows.Next() {
		var m teamMember
		if err := rows.Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt, &m.EmailVerified, &m.PendingEmail); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
		}
		members = append(members, m)
	}
	return c.JSON(fiber.Map{"users": members})
}

type createTeamMemberRequest struct {
	FullName   string `json:"full_name"`
	StreamName string `json:"stream_name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	Role       string `json:"role"`
}

func (h *TeamHandler) Create(c *fiber.Ctx) error {
	ownerID := c.Locals("user_id").(string)

	var req createTeamMemberRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.FullName = strings.TrimSpace(req.FullName)
	req.Email = strings.TrimSpace(req.Email)
	req.StreamName = strings.TrimSpace(req.StreamName)
	if req.FullName == "" || req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name and email are required"})
	}
	if len(req.Password) < 8 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "password must be at least 8 characters"})
	}
	if !validTeamRole(req.Role) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "role must be staff or admin"})
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
	}

	var m teamMember
	err = h.DB.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role, full_name, stream_name, account_owner_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, email, full_name, stream_name, role, status, created_at, email_verified_at IS NOT NULL, pending_email`,
		req.Email, string(hash), req.Role, req.FullName, nullIfEmpty(req.StreamName), ownerID,
	).Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt, &m.EmailVerified, &m.PendingEmail)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
	}
	// The host chose this address, so the member must prove they own it.
	_ = h.verifier().start(context.Background(), m.ID, m.Email, "verify")
	recordSecurityEvent(context.Background(), h.DB, c, "team_member_created", actorOf(c), m.ID, map[string]any{"role": m.Role})
	return c.Status(fiber.StatusCreated).JSON(m)
}

type updateTeamMemberRequest struct {
	FullName   *string `json:"full_name"`
	StreamName *string `json:"stream_name"`
	Email      *string `json:"email"`
	Password   *string `json:"password"`
	Role       *string `json:"role"`
	Status     *string `json:"status"`
}

// Update edits a member. Every field is optional: only the ones sent change.
// An empty stream_name clears it; an empty password leaves it unchanged.
func (h *TeamHandler) Update(c *fiber.Ctx) error {
	ownerID := c.Locals("user_id").(string)
	actorID := c.Locals("actor_id").(string)
	memberID := c.Params("id")

	if memberID == actorID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "you cannot edit yourself here; use your Profile page"})
	}

	var req updateTeamMemberRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.FullName != nil {
		if *req.FullName = strings.TrimSpace(*req.FullName); *req.FullName == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name cannot be empty"})
		}
	}
	if req.Email != nil {
		if *req.Email = strings.TrimSpace(*req.Email); *req.Email == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email cannot be empty"})
		}
	}
	if req.Role != nil && !validTeamRole(*req.Role) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "role must be staff or admin"})
	}
	if req.Status != nil && *req.Status != "active" && *req.Status != "suspended" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "status must be active or suspended"})
	}

	var passwordHash *string
	if req.Password != nil && *req.Password != "" {
		if len(*req.Password) < 8 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "password must be at least 8 characters"})
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
		}
		hashed := string(hash)
		passwordHash = &hashed
	}

	var streamName *string
	if req.StreamName != nil {
		streamName = nullIfEmpty(strings.TrimSpace(*req.StreamName))
	}

	var m teamMember
	err := h.DB.QueryRow(context.Background(),
		`UPDATE users SET
			full_name     = COALESCE($1, full_name),
			stream_name   = CASE WHEN $2 THEN $3 ELSE stream_name END,
			password_hash = COALESCE($4, password_hash),
			role          = COALESCE($5, role),
			status        = COALESCE($6, status),
			updated_at    = NOW()
		 WHERE id = $7 AND account_owner_id = $8
		 RETURNING id, email, full_name, stream_name, role, status, created_at, email_verified_at IS NOT NULL, pending_email`,
		req.FullName, req.StreamName != nil, streamName, passwordHash,
		req.Role, req.Status, memberID, ownerID,
	).Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt, &m.EmailVerified, &m.PendingEmail)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	// A new email only replaces the old one once the member confirms it.
	if req.Email != nil && !strings.EqualFold(*req.Email, m.Email) {
		pending, err := requestEmailChange(context.Background(), h.verifier(), m.ID, *req.Email)
		if err != nil {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
		}
		m.PendingEmail = &pending
	}
	// A role, status or password change takes effect everywhere at once:
	// the member's existing sessions end and they sign in again.
	if req.Role != nil || req.Status != nil || passwordHash != nil {
		_, _ = middleware.RevokeUserSessions(context.Background(), h.DB, m.ID, "account_changed_by_host", "")
	}
	changed := map[string]any{}
	if req.Role != nil {
		changed["role"] = *req.Role
	}
	if req.Status != nil {
		changed["status"] = *req.Status
	}
	if passwordHash != nil {
		changed["password_reset"] = true
	}
	if req.Email != nil {
		changed["email_change_requested"] = true
	}
	recordSecurityEvent(context.Background(), h.DB, c, "team_member_updated", actorOf(c), m.ID, changed)
	return c.JSON(m)
}

func (h *TeamHandler) Delete(c *fiber.Ctx) error {
	ownerID := c.Locals("user_id").(string)
	actorID := c.Locals("actor_id").(string)
	memberID := c.Params("id")

	if memberID == actorID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "you cannot delete yourself"})
	}

	tag, err := h.DB.Exec(context.Background(),
		`DELETE FROM users WHERE id = $1 AND account_owner_id = $2`, memberID, ownerID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete user"})
	}
	if tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	recordSecurityEvent(context.Background(), h.DB, c, "team_member_deleted", actorOf(c), "", map[string]any{"member_id": memberID})
	return c.JSON(fiber.Map{"message": "user deleted"})
}
