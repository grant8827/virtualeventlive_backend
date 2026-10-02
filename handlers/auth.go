package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
	"vertualeventlive/backend/services"
)

type AuthHandler struct {
	DB      *pgxpool.Pool
	Cfg     *config.Config
	Email   *services.EmailService
	Session middleware.SessionConfig
}

type registerRequest struct {
	// Registration is invite-only: the token from the invite link decides
	// the account's email address.
	InviteToken      string `json:"invite_token"`
	Email            string `json:"email"`
	Password         string `json:"password"`
	FullName         string `json:"full_name"`
	Phone            string `json:"phone"`
	AddressLine1     string `json:"address_line1"`
	AddressLine2     string `json:"address_line2"`
	City             string `json:"city"`
	State            string `json:"state"`
	PostalCode       string `json:"postal_code"`
	Country          string `json:"country"`
	OrganizationName string `json:"organization_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) Register(c *fiber.Ctx) error {
	var req registerRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.InviteToken == "" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "registration is by invitation only"})
	}
	if len(req.Password) < 8 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "password must be at least 8 characters"})
	}
	if req.FullName == "" || req.Phone == "" || req.AddressLine1 == "" || req.City == "" ||
		req.State == "" || req.PostalCode == "" || req.Country == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "full name, phone, and address are required"})
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
	}

	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create account"})
	}
	defer tx.Rollback(ctx)

	// Claim the invitation. FOR UPDATE stops the same link registering twice.
	var inviteID, email string
	err = tx.QueryRow(ctx,
		`SELECT id, email FROM invitations
		 WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > NOW()
		 FOR UPDATE`, hashInvitationToken(req.InviteToken),
	).Scan(&inviteID, &email)
	if err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "this invitation link is invalid, used or expired"})
	}

	// New hosts wait for a superuser to approve them before they can sign in.
	var userID string
	err = tx.QueryRow(ctx,
		`INSERT INTO users (
			email, password_hash, role, status, full_name, phone,
			address_line1, address_line2, city, state, postal_code, country, organization_name
		) VALUES ($1, $2, 'host', 'pending', $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		email, string(hash), req.FullName, req.Phone,
		req.AddressLine1, req.AddressLine2, req.City, req.State, req.PostalCode, req.Country, req.OrganizationName,
	).Scan(&userID)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
	}
	if _, err := tx.Exec(ctx,
		`UPDATE invitations SET accepted_at = NOW(), user_id = $1 WHERE id = $2`, userID, inviteID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create account"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create account"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":     userID,
		"email":  email,
		"role":   "host",
		"status": "pending",
	})
}

// dummyPasswordHash is compared against when the email is unknown, so a
// failed sign-in takes the same time whether or not the account exists.
var dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	ctx := context.Background()

	var userID, email, passwordHash, role, status string
	err := h.DB.QueryRow(ctx,
		`SELECT id, email, password_hash, role, status FROM users WHERE email = $1`,
		req.Email,
	).Scan(&userID, &email, &passwordHash, &role, &status)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}
	if status == "pending" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "your account is waiting for approval; you'll get an email once it's approved"})
	}
	if status != "active" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "this account has been suspended"})
	}

	// A new server-side session; the browser gets its ID only as an HttpOnly
	// cookie, never in the response body or in storage scripts can read.
	token, expires, err := middleware.CreateSession(ctx, h.DB, userID, c.IP(), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to sign in"})
	}
	middleware.SetSessionCookie(c, h.Session, token, expires)

	return c.JSON(fiber.Map{
		"id":    userID,
		"email": email,
		"role":  role,
	})
}

// Logout revokes the current session on the server and clears the cookie.
func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	if token := c.Cookies(h.Session.CookieName()); token != "" {
		if s, err := middleware.LookupSession(context.Background(), h.DB, token); err == nil {
			_ = middleware.RevokeSession(context.Background(), h.DB, s.SessionID, "logout")
		}
	}
	middleware.ClearSessionCookie(c, h.Session)
	return c.JSON(fiber.Map{"message": "logged out"})
}

// LogoutEverywhere revokes every session of the signed-in user, including
// this one.
func (h *AuthHandler) LogoutEverywhere(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)
	if _, err := middleware.RevokeUserSessions(context.Background(), h.DB, userID, "logout_everywhere", ""); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to sign out"})
	}
	middleware.ClearSessionCookie(c, h.Session)
	return c.JSON(fiber.Map{"message": "signed out on all devices"})
}

func (h *AuthHandler) Me(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)

	// Role comes from the database rather than the token so the dashboard
	// picks up a staff/admin change without the member signing in again.
	var (
		email, role, status                                                  string
		fullName, phone, addressLine1, addressLine2, city, state, postalCode *string
		country, organizationName                                            *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT email, role, status, full_name, phone, address_line1, address_line2, city, state, postal_code, country, organization_name
		 FROM users WHERE id = $1`, userID,
	).Scan(&email, &role, &status, &fullName, &phone, &addressLine1, &addressLine2, &city, &state, &postalCode, &country, &organizationName)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	if status != "active" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "account is not active"})
	}

	return c.JSON(fiber.Map{
		"id":                userID,
		"email":             email,
		"role":              role,
		"full_name":         fullName,
		"phone":             phone,
		"address_line1":     addressLine1,
		"address_line2":     addressLine2,
		"city":              city,
		"state":             state,
		"postal_code":       postalCode,
		"country":           country,
		"organization_name": organizationName,
	})
}

type updateProfileRequest struct {
	Email            string `json:"email"`
	FullName         string `json:"full_name"`
	Phone            string `json:"phone"`
	AddressLine1     string `json:"address_line1"`
	AddressLine2     string `json:"address_line2"`
	City             string `json:"city"`
	State            string `json:"state"`
	PostalCode       string `json:"postal_code"`
	Country          string `json:"country"`
	OrganizationName string `json:"organization_name"`
}

func (h *AuthHandler) UpdateProfile(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)

	var req updateProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.Email == "" || req.FullName == "" || req.Phone == "" || req.AddressLine1 == "" ||
		req.City == "" || req.State == "" || req.PostalCode == "" || req.Country == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email, full name, phone, and address are required"})
	}

	_, err := h.DB.Exec(context.Background(),
		`UPDATE users SET email = $1, full_name = $2, phone = $3, address_line1 = $4, address_line2 = $5,
		 city = $6, state = $7, postal_code = $8, country = $9, organization_name = $10, updated_at = NOW()
		 WHERE id = $11`,
		req.Email, req.FullName, req.Phone, req.AddressLine1, req.AddressLine2,
		req.City, req.State, req.PostalCode, req.Country, req.OrganizationName, userID,
	)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
	}

	return c.JSON(fiber.Map{"message": "profile updated"})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

func (h *AuthHandler) ChangePassword(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)

	var req changePasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if len(req.NewPassword) < 8 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "new password must be at least 8 characters"})
	}
	if req.NewPassword != req.ConfirmPassword {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "new passwords do not match"})
	}

	var currentHash string
	if err := h.DB.QueryRow(context.Background(),
		`SELECT password_hash FROM users WHERE id = $1`, userID,
	).Scan(&currentHash); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(req.CurrentPassword)) != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "current password is incorrect"})
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
	}
	if _, err := h.DB.Exec(context.Background(),
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, string(newHash), userID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update password"})
	}
	// Sign out every other device; this one stays signed in.
	currentSession, _ := c.Locals("session_id").(string)
	_, _ = middleware.RevokeUserSessions(context.Background(), h.DB, userID, "password_changed", currentSession)

	return c.JSON(fiber.Map{"message": "password updated"})
}
