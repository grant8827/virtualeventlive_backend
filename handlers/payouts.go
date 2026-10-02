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

// Balance is what the platform still owes the host: their share of sales
// that were not paid to them at checkout, net of refunds, from the ledger.
// Stripe and PayPal hosts are paid at checkout, so theirs is normally zero.
func (h *PayoutHandler) Balance(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var gateway string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT COALESCE(payout_gateway, 'stripe') FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&gateway)

	var owedCents int64
	if err := h.DB.QueryRow(context.Background(),
		`SELECT COALESCE(-SUM(p.amount_cents), 0)
		 FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		 WHERE j.host_id = $1 AND p.account = 'host_payable'`, hostID,
	).Scan(&owedCents); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load balance"})
	}

	return c.JSON(fiber.Map{
		"pending_amount": services.CentsToDollars(owedCents),
		"pending_cents":  owedCents,
		"gateway":        gateway,
		"currency":       "USD",
	})
}

// Payout used to send a pending balance through WiPay. WiPay's payout API
// was never verified end to end, so it is disabled (PAY-08): nothing is sent
// and nothing is marked paid. Stripe and PayPal hosts are paid at checkout.
// Any balance still owed is paid by the platform manually and recorded then.
func (h *PayoutHandler) Payout(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var gateway *string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT payout_gateway FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&gateway)
	switch {
	case gateway != nil && *gateway == "stripe":
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Stripe payouts happen automatically — nothing to trigger"})
	case gateway != nil && *gateway == "paypal":
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "PayPal splits and disburses ticket revenue automatically — nothing to trigger"})
	}
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"error": "Manual payouts aren't available yet. Contact support to be paid any balance owed.",
	})
}
