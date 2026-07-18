package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

// PayoutHandler covers the gateway-agnostic parts of host payouts: which
// gateway a host has chosen (Stripe, WiPay, or PayPal), their pending balance,
// and triggering a manual payout for gateways that aren't paid automatically
// at checkout time the way Stripe destination charges are.
type PayoutHandler struct {
	DB     *pgxpool.Pool
	Cfg    *config.Config
	WiPay  *services.WiPayService
	PayPal *services.PayPalService
}

type gatewayStatus struct {
	Connected     bool   `json:"connected"`
	AccountID     string `json:"account_id,omitempty"`
	PayoutEnabled bool   `json:"payout_enabled"`
}

func (h *PayoutHandler) Status(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var (
		stripeAccountID *string
		wipayAccountID  *string
		paypalAccountID *string
		payoutGateway   string
		stripePayoutOK  bool
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT stripe_account_id, wipay_account_id, paypal_account_id, payout_gateway, payout_enabled
		 FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&stripeAccountID, &wipayAccountID, &paypalAccountID, &payoutGateway, &stripePayoutOK)
	if err != nil {
		return c.JSON(fiber.Map{
			"active_gateway": "",
			"stripe":         gatewayStatus{},
			"wipay":          gatewayStatus{},
			"paypal":         gatewayStatus{},
		})
	}

	stripe := gatewayStatus{PayoutEnabled: payoutGateway == "stripe" && stripePayoutOK}
	if stripeAccountID != nil {
		stripe.Connected = true
		stripe.AccountID = *stripeAccountID
	}
	wipay := gatewayStatus{}
	if wipayAccountID != nil {
		wipay = gatewayStatus{Connected: true, AccountID: *wipayAccountID, PayoutEnabled: true}
	}
	paypal := gatewayStatus{}
	if paypalAccountID != nil {
		paypal = gatewayStatus{Connected: true, AccountID: *paypalAccountID, PayoutEnabled: true}
	}

	return c.JSON(fiber.Map{
		"active_gateway": payoutGateway,
		"stripe":         stripe,
		"wipay":          wipay,
		"paypal":         paypal,
	})
}

type connectAccountRequest struct {
	AccountID string `json:"account_id"`
}

// ConnectWiPay saves the host's WiPay account number and makes WiPay the
// active payout gateway. WiPay has no OAuth-style onboarding like Stripe
// Connect — the host just tells us where to send their cut.
func (h *PayoutHandler) ConnectWiPay(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req connectAccountRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "account_id is required"})
	}

	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, wipay_account_id, payout_gateway)
		 VALUES ($1, $2, 'wipay')
		 ON CONFLICT (user_id) DO UPDATE SET wipay_account_id = EXCLUDED.wipay_account_id, payout_gateway = 'wipay'`,
		hostID, req.AccountID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save WiPay account"})
	}

	return c.JSON(fiber.Map{"connected": true, "active_gateway": "wipay"})
}

// ConnectPayPal saves the host's PayPal email and makes PayPal the active
// payout gateway. PayPal Payouts sends to a receiver email directly, so no
// separate onboarding link is needed either.
func (h *PayoutHandler) ConnectPayPal(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req connectAccountRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.AccountID = strings.TrimSpace(strings.ToLower(req.AccountID))
	if req.AccountID == "" || !strings.Contains(req.AccountID, "@") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "a valid PayPal email is required"})
	}

	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, paypal_account_id, payout_gateway)
		 VALUES ($1, $2, 'paypal')
		 ON CONFLICT (user_id) DO UPDATE SET paypal_account_id = EXCLUDED.paypal_account_id, payout_gateway = 'paypal'`,
		hostID, req.AccountID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save PayPal account"})
	}

	return c.JSON(fiber.Map{"connected": true, "active_gateway": "paypal"})
}

// Balance sums ledger entries not yet paid out to the host.
func (h *PayoutHandler) Balance(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var pending float64
	var gateway string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT COALESCE(payout_gateway, 'stripe') FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&gateway)

	_ = h.DB.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(le.host_payout), 0)
		 FROM ledger_entries le
		 JOIN events e ON e.id = le.event_id
		 WHERE e.host_id = $1 AND le.payout_status = 'pending'`,
		hostID,
	).Scan(&pending)

	return c.JSON(fiber.Map{
		"pending_amount": pending,
		"gateway":        gateway,
		"currency":       "USD",
	})
}

// Payout sends the host's pending balance through their active non-Stripe
// gateway. Stripe hosts are paid automatically at checkout via destination
// charges, so there's nothing to trigger here.
func (h *PayoutHandler) Payout(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var gateway string
	var wipayAccountID, paypalAccountID *string
	err := h.DB.QueryRow(context.Background(),
		`SELECT payout_gateway, wipay_account_id, paypal_account_id FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&gateway, &wipayAccountID, &paypalAccountID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no payout account connected"})
	}
	if gateway == "stripe" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Stripe payouts happen automatically — nothing to trigger"})
	}

	var pending float64
	if err := h.DB.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(le.host_payout), 0)
		 FROM ledger_entries le
		 JOIN events e ON e.id = le.event_id
		 WHERE e.host_id = $1 AND le.payout_status = 'pending'`,
		hostID,
	).Scan(&pending); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to calculate balance"})
	}
	if pending <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no pending balance to pay out"})
	}

	var transactionRef string
	switch gateway {
	case "wipay":
		if wipayAccountID == nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no WiPay account connected"})
		}
		ref, err := h.WiPay.SendPayout(*wipayAccountID, pending, "")
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
		}
		transactionRef = ref
	case "paypal":
		if paypalAccountID == nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no PayPal account connected"})
		}
		ref, err := h.PayPal.SendPayout(*paypalAccountID, pending, "VirtualEventLive ticket revenue")
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
		}
		transactionRef = ref
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown payout gateway"})
	}

	if _, err := h.DB.Exec(context.Background(),
		`UPDATE ledger_entries le SET payout_status = 'paid', payout_gateway = $2, paid_out_at = NOW()
		 FROM events e
		 WHERE le.event_id = e.id AND e.host_id = $1 AND le.payout_status = 'pending'`,
		hostID, gateway,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "payout sent but failed to update ledger — contact support"})
	}

	return c.JSON(fiber.Map{"paid": true, "amount": pending, "gateway": gateway, "reference": transactionRef})
}
