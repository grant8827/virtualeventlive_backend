package handlers

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PayoutAccountHandler struct {
	DB *pgxpool.Pool
}

// payoutProviderFields defines the required details fields per provider —
// these are simple payout destinations (no OAuth), since the platform holds
// its own PayPal/WiPay merchant accounts and pays hosts out separately.
var payoutProviderFields = map[string][]string{
	"paypal": {"email"},
	"wipay":  {"account_name", "bank_name", "account_number"},
}

type payoutAccountStatus struct {
	Provider  string            `json:"provider"`
	Connected bool              `json:"connected"`
	Details   map[string]string `json:"details,omitempty"`
}

func (h *PayoutAccountHandler) List(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT provider, details FROM payout_accounts WHERE user_id = $1`, hostID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch payout accounts"})
	}
	defer rows.Close()

	statuses := map[string]payoutAccountStatus{
		"paypal": {Provider: "paypal"},
		"wipay":  {Provider: "wipay"},
	}
	for rows.Next() {
		var provider string
		var raw []byte
		if err := rows.Scan(&provider, &raw); err != nil {
			continue
		}
		var details map[string]string
		_ = json.Unmarshal(raw, &details)
		statuses[provider] = payoutAccountStatus{Provider: provider, Connected: true, Details: details}
	}

	return c.JSON(fiber.Map{
		"paypal": statuses["paypal"],
		"wipay":  statuses["wipay"],
	})
}

func (h *PayoutAccountHandler) Upsert(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	provider := c.Params("provider")
	requiredFields, ok := payoutProviderFields[provider]
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported provider"})
	}

	var body map[string]string
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	details := make(map[string]string, len(requiredFields))
	for _, field := range requiredFields {
		val := strings.TrimSpace(body[field])
		if val == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": field + " is required"})
		}
		details[field] = val
	}

	raw, err := json.Marshal(details)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to encode details"})
	}

	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO payout_accounts (user_id, provider, details, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, provider) DO UPDATE SET details = EXCLUDED.details, updated_at = NOW()`,
		hostID, provider, raw,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save payout account"})
	}

	return c.JSON(fiber.Map{"provider": provider, "connected": true, "details": details})
}
