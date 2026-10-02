package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/accountlink"
	"github.com/stripe/stripe-go/v82/webhook"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

type StripeHandler struct {
	DB    *pgxpool.Pool
	Cfg   *config.Config
	Email *services.EmailService
	IVS   *services.IVSService

	// process handles one verified event; nil means processStripeEvent.
	// Tests replace it to simulate failures and retries.
	process func(ctx context.Context, event stripe.Event) (webhookOutcome, error)
}

func (h *StripeHandler) ConnectOnboard(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe payment processing is not configured"})
	}

	stripe.Key = h.Cfg.StripeSecretKey

	var existingAccountID string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT stripe_account_id FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&existingAccountID)

	var stripeAccountID string
	if existingAccountID != "" {
		stripeAccountID = existingAccountID
	} else {
		var hostEmail string
		_ = h.DB.QueryRow(context.Background(),
			`SELECT email FROM users WHERE id = $1`, hostID,
		).Scan(&hostEmail)

		acctID, err := services.CreateExpressAccountV2(h.Cfg.StripeSecretKey, hostEmail)
		if err != nil {
			return stripeConnectErrorMsg(c, "create account", err)
		}

		stripeAccountID = acctID
		_, err = h.DB.Exec(context.Background(),
			`INSERT INTO connected_accounts (user_id, stripe_account_id, payout_gateway)
			 VALUES ($1, $2, 'stripe')
			 ON CONFLICT (user_id) DO UPDATE SET stripe_account_id = EXCLUDED.stripe_account_id, payout_gateway = 'stripe'`,
			hostID, stripeAccountID,
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save account"})
		}
	}

	link, err := accountlink.New(&stripe.AccountLinkParams{
		Account:    stripe.String(stripeAccountID),
		RefreshURL: stripe.String(h.Cfg.FrontendURL + "/connect/refresh"),
		ReturnURL:  stripe.String(h.Cfg.FrontendURL + "/connect/complete"),
		Type:       stripe.String("account_onboarding"),
	})
	if err != nil {
		return stripeConnectError(c, "create onboarding link", err)
	}

	auditPayoutEvent(h.DB, c, hostID, "stripe_onboarding_started")
	return c.JSON(fiber.Map{"url": link.URL})
}

func stripeConnectError(c *fiber.Ctx, operation string, err error) error {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		fmt.Printf("stripe connect %s error: request_id=%s code=%s param=%s message=%s\n", operation, stripeErr.RequestID, stripeErr.Code, stripeErr.Param, stripeErr.Msg)
		if stripeErr.Msg != "" {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe Connect error: " + stripeErr.Msg})
		}
	}
	fmt.Printf("stripe connect %s error: %v\n", operation, err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe Connect could not " + operation + ". Check backend logs."})
}

// stripeConnectErrorMsg mirrors stripeConnectError for calls that hit
// Stripe's v2 API directly (services.CreateExpressAccountV2) instead of
// through stripe-go, so the error is a plain error rather than *stripe.Error.
func stripeConnectErrorMsg(c *fiber.Ctx, operation string, err error) error {
	fmt.Printf("stripe connect %s error: %v\n", operation, err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe Connect error: " + err.Error()})
}

// Stripe gives each webhook destination its own signing secret. Test and live
// destinations can share this public URL but have different secrets, so accept
// a comma-separated list in STRIPE_WEBHOOK_SECRET rather than forcing one
// environment to break when the other is configured.
func constructStripeWebhookEvent(payload []byte, signature, configuredSecrets string) (stripe.Event, error) {
	var lastErr error
	for _, secret := range strings.Split(configuredSecrets, ",") {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		event, err := webhook.ConstructEvent(payload, signature, secret)
		if err == nil {
			return event, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Stripe webhook signing secret configured")
	}
	return stripe.Event{}, lastErr
}

func stripeTicketSessionPaid(sess *stripe.CheckoutSession) bool {
	return sess != nil &&
		sess.Metadata["type"] == "ticket" &&
		sess.Metadata["event_id"] != "" &&
		sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid &&
		strings.EqualFold(string(sess.Currency), "usd") &&
		sess.AmountTotal > 0
}
