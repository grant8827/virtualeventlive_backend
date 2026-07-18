package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/account"
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
}

func (h *StripeHandler) ConnectOnboard(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	stripe.Key = h.Cfg.StripeSecretKey

	var existingAccountID string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT stripe_account_id FROM connected_accounts WHERE user_id = $1`, hostID,
	).Scan(&existingAccountID)

	var stripeAccountID string
	if existingAccountID != "" {
		stripeAccountID = existingAccountID
		// Re-activate Stripe as the payout gateway in case the host had
		// switched to WiPay/PayPal since first onboarding here.
		_, _ = h.DB.Exec(context.Background(),
			`UPDATE connected_accounts SET payout_gateway = 'stripe' WHERE user_id = $1`, hostID,
		)
	} else {
		acc, err := account.New(&stripe.AccountParams{
			Type: stripe.String(string(stripe.AccountTypeExpress)),
			Capabilities: &stripe.AccountCapabilitiesParams{
				CardPayments: &stripe.AccountCapabilitiesCardPaymentsParams{Requested: stripe.Bool(true)},
				Transfers:    &stripe.AccountCapabilitiesTransfersParams{Requested: stripe.Bool(true)},
			},
		})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create Stripe account"})
		}

		stripeAccountID = acc.ID
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
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create onboarding link"})
	}

	return c.JSON(fiber.Map{"url": link.URL})
}

func (h *StripeHandler) Webhook(c *fiber.Ctx) error {
	payload := c.Body()
	sigHeader := c.Get("Stripe-Signature")

	event, err := webhook.ConstructEvent(payload, sigHeader, h.Cfg.StripeWebhookSecret)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid webhook signature"})
	}

	switch event.Type {
	case "checkout.session.completed":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "failed to parse event data"})
		}
		switch sess.Metadata["type"] {
		case "venue_fee":
			if err := h.handleVenueFeePaid(&sess); err != nil {
				fmt.Printf("webhook venue_fee error: %v\n", err)
			}
		case "ticket":
			if err := h.handleTicketPurchase(&sess); err != nil {
				fmt.Printf("webhook ticket purchase error: %v\n", err)
			}
		}

	case "account.updated":
		var acc stripe.Account
		if err := json.Unmarshal(event.Data.Raw, &acc); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "failed to parse event data"})
		}
		_, _ = h.DB.Exec(context.Background(),
			`UPDATE connected_accounts SET payout_enabled = $1 WHERE stripe_account_id = $2`,
			acc.PayoutsEnabled, acc.ID,
		)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"received": true})
}

func (h *StripeHandler) handleVenueFeePaid(sess *stripe.CheckoutSession) error {
	eventID := sess.Metadata["event_id"]
	if eventID == "" {
		return nil
	}

	// Mark event as paid and active
	if _, err := h.DB.Exec(context.Background(),
		`UPDATE events SET venue_paid = true, is_active = true WHERE id = $1`,
		eventID,
	); err != nil {
		return err
	}

	// Provision IVS channel if AWS is configured
	if h.IVS.Enabled {
		var title string
		_ = h.DB.QueryRow(context.Background(),
			`SELECT title FROM events WHERE id = $1`, eventID,
		).Scan(&title)

		creds, err := h.IVS.ProvisionChannel(context.Background(), title)
		if err != nil {
			fmt.Printf("IVS provision failed (non-fatal): %v\n", err)
			return nil
		}

		_, _ = h.DB.Exec(context.Background(),
			`UPDATE events SET
				aws_channel_arn   = $1,
				stream_ingest_url = $2,
				stream_key_value  = $3,
				aws_playback_url  = $4
			 WHERE id = $5`,
			creds.ChannelARN, creds.IngestURL, creds.StreamKey, creds.PlaybackURL, eventID,
		)
	}

	return nil
}

func (h *StripeHandler) handleTicketPurchase(sess *stripe.CheckoutSession) error {
	eventID := sess.Metadata["event_id"]
	if eventID == "" {
		return nil
	}

	buyerID := sess.Metadata["buyer_id"]
	guestEmail := sess.Metadata["guest_email"]
	if buyerID == "" && guestEmail == "" {
		return nil
	}

	var ticketPrice float64
	var eventTitle string
	var startsAt time.Time

	if err := h.DB.QueryRow(context.Background(),
		`SELECT ticket_price, title, start_time FROM events WHERE id = $1`,
		eventID,
	).Scan(&ticketPrice, &eventTitle, &startsAt); err != nil {
		return fmt.Errorf("fetch event: %w", err)
	}

	// Determine buyer email
	buyerEmail := guestEmail
	if buyerID != "" {
		var userEmail string
		_ = h.DB.QueryRow(context.Background(),
			`SELECT email FROM users WHERE id = $1`, buyerID,
		).Scan(&userEmail)
		if userEmail != "" {
			buyerEmail = userEmail
		}
	}

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	accessToken := "TCK-" + hex.EncodeToString(tokenBytes)

	var ticketID string
	var insertErr error
	if buyerID != "" {
		insertErr = h.DB.QueryRow(context.Background(),
			`INSERT INTO tickets (event_id, buyer_id, access_token) VALUES ($1, $2, $3) RETURNING id`,
			eventID, buyerID, accessToken,
		).Scan(&ticketID)
	} else {
		insertErr = h.DB.QueryRow(context.Background(),
			`INSERT INTO tickets (event_id, buyer_id, guest_email, access_token) VALUES ($1, NULL, $2, $3) RETURNING id`,
			eventID, guestEmail, accessToken,
		).Scan(&ticketID)
	}
	if insertErr != nil {
		return fmt.Errorf("insert ticket: %w", insertErr)
	}

	var payoutGateway string
	_ = h.DB.QueryRow(context.Background(),
		`SELECT ca.payout_gateway FROM connected_accounts ca
		 JOIN events e ON e.host_id = ca.user_id WHERE e.id = $1`,
		eventID,
	).Scan(&payoutGateway)
	if payoutGateway == "" {
		payoutGateway = "stripe"
	}
	// Stripe hosts are paid via destination charge at checkout, so their
	// entries settle immediately. WiPay/PayPal hosts wait for a manual payout.
	payoutStatus := "paid"
	if payoutGateway != "stripe" {
		payoutStatus = "pending"
	}

	split := services.CalculateSplit(ticketPrice)
	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO ledger_entries (ticket_id, event_id, gross_amount, stripe_fee, platform_fee, host_payout, payout_gateway, payout_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ticketID, eventID, split.GrossAmount, split.StripeFee, split.PlatformFee, split.HostPayout, payoutGateway, payoutStatus,
	); err != nil {
		return fmt.Errorf("insert ledger: %w", err)
	}

	if buyerEmail != "" {
		if err := h.Email.SendTicketConfirmation(buyerEmail, eventTitle, accessToken, startsAt); err != nil {
			fmt.Printf("email send failed (non-fatal): %v\n", err)
		}
	}

	return nil
}
