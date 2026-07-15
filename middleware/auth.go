package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

func Protected(jwtSecret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		if authHeader == "" {
			token := c.Cookies("auth_token")
			if token == "" {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"error": "missing authentication token",
				})
			}
			authHeader = "Bearer " + token
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "invalid or expired token",
			})
		}

		c.Locals("user_id", claims.UserID)
		c.Locals("role", claims.Role)
		return c.Next()
	}
}

func RequireRole(role string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userRole, ok := c.Locals("role").(string)
		if !ok || userRole != role {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error": "insufficient permissions",
			})
		}
		return c.Next()
	}
}
