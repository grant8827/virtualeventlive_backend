package handlers

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

// PayoutHandler manages host payout accounts.
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
		payoutGateway   *string
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

	activeGateway := ""
	if payoutGateway != nil {
		activeGateway = *payoutGateway
	}
	stripe := gatewayStatus{PayoutEnabled: activeGateway == "stripe" && stripePayoutOK}
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
		paypal = gatewayStatus{Connected: true, AccountID: *paypalAccountID, PayoutEnabled: activeGateway == "paypal" && h.PayPal != nil && h.PayPal.PartnerEnabled()}
	}

	return c.JSON(fiber.Map{
		"active_gateway": activeGateway,
		"stripe":         stripe,
		"wipay":          wipay,
		"paypal":         paypal,
	})
}

type activateGatewayRequest struct {
	Gateway string `json:"gateway"`
}

// Activate switches to an account that the host has already connected. Stripe
// can only be selected after Stripe reports that payouts are enabled.
func (h *PayoutHandler) Activate(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req activateGatewayRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.Gateway = strings.ToLower(strings.TrimSpace(req.Gateway))
	if req.Gateway != "stripe" && req.Gateway != "paypal" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "available payout providers are Stripe and PayPal"})
	}

	if req.Gateway == "paypal" && (h.PayPal == nil || !h.PayPal.PartnerEnabled()) {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PayPal is not configured on the platform"})
	}

	query := `UPDATE connected_accounts SET payout_gateway = 'paypal'
		 WHERE user_id = $1 AND paypal_account_id IS NOT NULL`
	args := []any{hostID}
	if req.Gateway == "stripe" {
		query = `UPDATE connected_accounts SET payout_gateway = 'stripe'
			 WHERE user_id = $1 AND stripe_account_id IS NOT NULL AND payout_enabled = true`
	}
	result, err := h.DB.Exec(context.Background(), query, args...)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to activate payout account"})
	}
	if result.RowsAffected() == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "that payout account is not connected or ready"})
	}
	auditPayoutEvent(h.DB, c, hostID, "payout_gateway_activated_"+req.Gateway)
	return c.JSON(fiber.Map{"active_gateway": req.Gateway})
}

// Deactivate pauses new ticket sales without deleting any connected account.
func (h *PayoutHandler) Deactivate(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if _, err := h.DB.Exec(context.Background(),
		`UPDATE connected_accounts SET payout_gateway = NULL WHERE user_id = $1`, hostID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to deactivate payout accounts"})
	}
	auditPayoutEvent(h.DB, c, hostID, "payout_gateway_deactivated")
	return c.JSON(fiber.Map{"active_gateway": ""})
}

// ConnectWiPay is reserved for the future WiPay integration.
func (h *PayoutHandler) ConnectWiPay(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "WiPay payouts are coming soon"})
}

// ConnectPayPal creates a one-time PayPal Partner Referrals signup URL.
func (h *PayoutHandler) ConnectPayPal(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if h.PayPal == nil || !h.PayPal.PartnerEnabled() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PayPal Commerce Platform is not configured"})
	}
	returnURL := strings.TrimRight(c.BaseURL(), "/") + "/api/v1/connect/paypal/complete"
	connectURL, err := h.PayPal.CreateSellerReferral(hostID, returnURL)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}
	auditPayoutEvent(h.DB, c, hostID, "paypal_connected")
	return c.JSON(fiber.Map{"url": connectURL})
}

// CompletePayPal verifies PayPal's returned seller merchant ID and only
// activates accounts that can receive payments and have a confirmed email.
func (h *PayoutHandler) CompletePayPal(c *fiber.Ctx) error {
	trackingID := strings.TrimSpace(c.Query("merchantId"))
	merchantID := strings.TrimSpace(c.Query("merchantIdInPayPal"))
	failURL := h.Cfg.FrontendURL + "/dashboard/payouts?paypal_connected=0"
	if trackingID == "" || merchantID == "" || h.PayPal == nil {
		return c.Redirect(failURL)
	}
	status, err := h.PayPal.GetSellerStatus(merchantID)
	if err != nil || status.TrackingID != trackingID || !status.PaymentsReceivable || !status.PrimaryEmailConfirmed {
		return c.Redirect(failURL)
	}
	result, err := h.DB.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, paypal_account_id, payout_gateway)
		 VALUES ($1,$2,'paypal') ON CONFLICT (user_id) DO UPDATE
		 SET paypal_account_id=EXCLUDED.paypal_account_id, payout_gateway='paypal'`, trackingID, merchantID)
	if err != nil || result.RowsAffected() == 0 {
		return c.Redirect(failURL)
	}
	return c.Redirect(h.Cfg.FrontendURL + "/dashboard/payouts?paypal_connected=1")
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
		 WHERE e.host_id = $1 AND le.payout_status = 'pending' AND le.payout_gateway = $2`,
		hostID, gateway,
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

	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start payout"})
	}
	defer tx.Rollback(ctx)
	// Serialize payouts per host. Without this lock two quick requests could
	// submit the same pending balance twice to an external payout provider.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, hostID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to lock payout"})
	}

	var gateway string
	var wipayAccountID, paypalAccountID *string
	err = tx.QueryRow(ctx,
		`SELECT payout_gateway, wipay_account_id, paypal_account_id FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&gateway, &wipayAccountID, &paypalAccountID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no payout account connected"})
	}
	if gateway == "stripe" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Stripe payouts happen automatically — nothing to trigger"})
	}

	var pending float64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(le.host_payout), 0)
		 FROM ledger_entries le
		 JOIN events e ON e.id = le.event_id
		 WHERE e.host_id = $1 AND le.payout_status = 'pending' AND le.payout_gateway = $2`,
		hostID, gateway,
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
		if paypalAccountID == nil || h.PayPal == nil || !h.PayPal.Enabled() {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no PayPal account connected"})
		}
		ref, err := h.PayPal.SendPayout(*paypalAccountID, pending, "Virtual Event Plus ticket revenue")
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
		}
		transactionRef = ref
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown payout gateway"})
	}

	if _, err := tx.Exec(ctx,
		`UPDATE ledger_entries le SET payout_status = 'paid', payout_gateway = $2, paid_out_at = NOW()
		 FROM events e
		 WHERE le.event_id = e.id AND e.host_id = $1 AND le.payout_status = 'pending'
		   AND le.payout_gateway = $2`,
		hostID, gateway,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "payout sent but failed to update ledger — contact support"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "payout sent but failed to finalize ledger — contact support"})
	}
	auditPayoutEvent(h.DB, c, hostID, "payout_sent_"+gateway)

	return c.JSON(fiber.Map{"paid": true, "amount": pending, "gateway": gateway, "reference": transactionRef})
}
