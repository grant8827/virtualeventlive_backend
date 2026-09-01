package handlers

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
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
		stripeAccountID          *string
		wipayAccountID           *string
		paypalAccountID          *string
		payoutGateway            *string
		stripePayoutOK           bool
		paypalPaymentsReceivable bool
		paypalEmailConfirmed     bool
		paypalOnboardingComplete bool
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT stripe_account_id, wipay_account_id, paypal_merchant_id, payout_gateway, payout_enabled,
		        paypal_payments_receivable, paypal_email_confirmed, paypal_onboarding_complete
		 FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&stripeAccountID, &wipayAccountID, &paypalAccountID, &payoutGateway, &stripePayoutOK,
		&paypalPaymentsReceivable, &paypalEmailConfirmed, &paypalOnboardingComplete)
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
		ready := paypalPaymentsReceivable && paypalEmailConfirmed && paypalOnboardingComplete
		paypal = gatewayStatus{Connected: ready, AccountID: *paypalAccountID, PayoutEnabled: ready}
	}

	return c.JSON(fiber.Map{
		"active_gateway": activeGateway,
		"stripe":         stripe,
		"wipay":          wipay,
		"paypal":         paypal,
	})
}

type connectAccountRequest struct {
	AccountID string `json:"account_id"`
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
	if req.Gateway != "stripe" && req.Gateway != "wipay" && req.Gateway != "paypal" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "gateway must be stripe, wipay, or paypal"})
	}

	result, err := h.DB.Exec(context.Background(),
		`UPDATE connected_accounts SET payout_gateway = $1
		 WHERE user_id = $2 AND CASE $1
		   WHEN 'stripe' THEN stripe_account_id IS NOT NULL AND payout_enabled = true
		   WHEN 'wipay' THEN wipay_account_id IS NOT NULL
		   WHEN 'paypal' THEN paypal_merchant_id IS NOT NULL
		     AND paypal_payments_receivable = true
		     AND paypal_email_confirmed = true
		     AND paypal_onboarding_complete = true
		   ELSE false END`, req.Gateway, hostID,
	)
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
	auditPayoutEvent(h.DB, c, hostID, "wipay_account_connected")

	return c.JSON(fiber.Map{"connected": true, "active_gateway": "wipay"})
}

// ConnectPayPal starts PayPal Commerce Platform hosted seller onboarding.
// The platform never asks for or stores the host's PayPal password or bank data.
func (h *PayoutHandler) ConnectPayPal(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	if h.PayPal == nil || !h.PayPal.MarketplaceEnabled() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PayPal marketplace onboarding is not configured yet"})
	}
	trackingID := uuid.NewString()
	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, paypal_tracking_id)
		 VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET paypal_tracking_id = EXCLUDED.paypal_tracking_id`,
		hostID, trackingID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start PayPal onboarding"})
	}
	returnURL := fmt.Sprintf("%s/api/v1/connect/paypal/complete?state=%s",
		strings.TrimRight(h.Cfg.PublicAPIURL, "/"), url.QueryEscape(trackingID))
	actionURL, err := h.PayPal.CreateSellerReferral(trackingID, returnURL)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}
	auditPayoutEvent(h.DB, c, hostID, "paypal_onboarding_started")
	return c.JSON(fiber.Map{"url": actionURL})
}

func (h *PayoutHandler) PayPalComplete(c *fiber.Ctx) error {
	trackingID := strings.TrimSpace(c.Query("state"))
	merchantID := strings.TrimSpace(c.Query("merchantIdInPayPal"))
	if trackingID == "" || merchantID == "" {
		return c.Redirect(h.Cfg.FrontendURL+"/dashboard/payouts?paypal=error", fiber.StatusSeeOther)
	}
	var hostID string
	if err := h.DB.QueryRow(context.Background(),
		`SELECT user_id FROM connected_accounts WHERE paypal_tracking_id = $1`, trackingID,
	).Scan(&hostID); err != nil {
		return c.Redirect(h.Cfg.FrontendURL+"/dashboard/payouts?paypal=error", fiber.StatusSeeOther)
	}
	status, err := h.PayPal.GetSellerStatus(merchantID)
	if err != nil {
		return c.Redirect(h.Cfg.FrontendURL+"/dashboard/payouts?paypal=verification_failed", fiber.StatusSeeOther)
	}
	ready := status.PaymentsReceivable && status.EmailConfirmed
	_, err = h.DB.Exec(context.Background(),
		`UPDATE connected_accounts
		 SET paypal_merchant_id = $1, paypal_account_id = NULL,
		     paypal_payments_receivable = $2, paypal_email_confirmed = $3,
		     paypal_onboarding_complete = $4,
		     payout_gateway = CASE WHEN $4 THEN 'paypal' ELSE payout_gateway END
		 WHERE user_id = $5 AND paypal_tracking_id = $6`,
		status.MerchantID, status.PaymentsReceivable, status.EmailConfirmed, ready, hostID, trackingID,
	)
	if err != nil {
		return c.Redirect(h.Cfg.FrontendURL+"/dashboard/payouts?paypal=error", fiber.StatusSeeOther)
	}
	result := "pending"
	if ready {
		result = "connected"
	}
	return c.Redirect(h.Cfg.FrontendURL+"/dashboard/payouts?paypal="+result, fiber.StatusSeeOther)
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
	if gateway == "paypal" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "PayPal ticket proceeds settle automatically — nothing to trigger"})
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
	auditPayoutEvent(h.DB, c, hostID, "payout_sent_"+gateway)

	return c.JSON(fiber.Map{"paid": true, "amount": pending, "gateway": gateway, "reference": transactionRef})
}
