package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

type PayoutClaims struct {
	UserID string `json:"user_id"`
	Scope  string `json:"scope"`
	jwt.RegisteredClaims
}

// RequirePayoutUnlock requires a short-lived, payout-scoped token in addition
// to the user's normal authenticated session.
func RequirePayoutUnlock(jwtSecret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		tokenStr := strings.TrimSpace(c.Get("X-Payout-Token"))
		if tokenStr == "" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "payout passcode required"})
		}

		claims := &PayoutClaims{}
		err := ParseToken(jwtSecret, tokenStr, AudiencePayout, claims)
		userID, _ := c.Locals("user_id").(string)
		if err != nil || claims.Scope != "payout" || claims.UserID == "" || claims.UserID != userID {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "payout access expired; enter your passcode again"})
		}

		return c.Next()
	}
}
