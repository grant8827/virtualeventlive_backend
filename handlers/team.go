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

	"vertualeventlive/backend/middleware"
)

// TeamHandler manages the staff and admin logins attached to a host account.
// Every route runs behind middleware.HostAccount, so Locals("user_id") is the
// owner's ID and Locals("actor_id") is whoever is signed in.
type TeamHandler struct {
	DB *pgxpool.Pool
}

type teamMember struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	FullName   *string   `json:"full_name"`
	StreamName *string   `json:"stream_name"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

func validTeamRole(role string) bool {
	return role == middleware.TeamStaff || role == middleware.TeamAdmin
}

func (h *TeamHandler) List(c *fiber.Ctx) error {
	ownerID := c.Locals("user_id").(string)

	rows, err := h.DB.Query(context.Background(),
		`SELECT id, email, full_name, stream_name, role, status, created_at
		 FROM users WHERE account_owner_id = $1 ORDER BY created_at`, ownerID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
	}
	defer rows.Close()

	members := []teamMember{}
	for rows.Next() {
		var m teamMember
		if err := rows.Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt); err != nil {
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
		 RETURNING id, email, full_name, stream_name, role, status, created_at`,
		req.Email, string(hash), req.Role, req.FullName, nullIfEmpty(req.StreamName), ownerID,
	).Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
	}
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
			email         = COALESCE($4, email),
			password_hash = COALESCE($5, password_hash),
			role          = COALESCE($6, role),
			status        = COALESCE($7, status),
			updated_at    = NOW()
		 WHERE id = $8 AND account_owner_id = $9
		 RETURNING id, email, full_name, stream_name, role, status, created_at`,
		req.FullName, req.StreamName != nil, streamName, req.Email, passwordHash,
		req.Role, req.Status, memberID, ownerID,
	).Scan(&m.ID, &m.Email, &m.FullName, &m.StreamName, &m.Role, &m.Status, &m.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
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
	return c.JSON(fiber.Map{"message": "user deleted"})
}
