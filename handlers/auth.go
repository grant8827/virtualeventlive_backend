package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
)

type AuthHandler struct {
	DB  *pgxpool.Pool
	Cfg *config.Config
}

type registerRequest struct {
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
	if req.Email == "" || req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email and password are required"})
	}
	if req.FullName == "" || req.Phone == "" || req.AddressLine1 == "" || req.City == "" ||
		req.State == "" || req.PostalCode == "" || req.Country == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "full name, phone, and address are required"})
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
	}

	var userID string
	err = h.DB.QueryRow(context.Background(),
		`INSERT INTO users (
			email, password_hash, role, full_name, phone,
			address_line1, address_line2, city, state, postal_code, country, organization_name
		) VALUES ($1, $2, 'host', $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		req.Email, string(hash), req.FullName, req.Phone,
		req.AddressLine1, req.AddressLine2, req.City, req.State, req.PostalCode, req.Country, req.OrganizationName,
	).Scan(&userID)
	if err != nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "email already in use"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":    userID,
		"email": req.Email,
		"role":  "host",
	})
}

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	var userID, passwordHash, role string
	err := h.DB.QueryRow(context.Background(),
		`SELECT id, password_hash, role FROM users WHERE email = $1`,
		req.Email,
	).Scan(&userID, &passwordHash, &role)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid credentials"})
	}

	claims := &middleware.Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(h.Cfg.JWTSecret))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to generate token"})
	}

	c.Cookie(&fiber.Cookie{
		Name:     "auth_token",
		Value:    tokenStr,
		HTTPOnly: true,
		SameSite: "Lax",
		MaxAge:   int((24 * time.Hour).Seconds()),
	})

	return c.JSON(fiber.Map{
		"id":    userID,
		"email": req.Email,
		"role":  role,
		"token": tokenStr,
	})
}

func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	c.Cookie(&fiber.Cookie{
		Name:    "auth_token",
		Value:   "",
		MaxAge:  -1,
		Expires: time.Now().Add(-time.Hour),
	})
	return c.JSON(fiber.Map{"message": "logged out"})
}

func (h *AuthHandler) Me(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(string)
	role := c.Locals("role").(string)

	var (
		email                                                                string
		fullName, phone, addressLine1, addressLine2, city, state, postalCode *string
		country, organizationName                                            *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT email, full_name, phone, address_line1, address_line2, city, state, postal_code, country, organization_name
		 FROM users WHERE id = $1`, userID,
	).Scan(&email, &fullName, &phone, &addressLine1, &addressLine2, &city, &state, &postalCode, &country, &organizationName)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
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

	return c.JSON(fiber.Map{"message": "password updated"})
}
