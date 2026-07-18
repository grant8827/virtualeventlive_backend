package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

type TicketHandler struct {
	DB    *pgxpool.Pool
	Cfg   *config.Config
	Email *services.EmailService
}

func (h *TicketHandler) ListMine(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT t.id, t.access_token, t.purchased_at,
		        e.id AS event_id, e.title, e.start_time
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.buyer_id = $1
		 ORDER BY t.purchased_at DESC`,
		buyerID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch tickets"})
	}
	defer rows.Close()

	type ticketRow struct {
		ID            string    `json:"id"`
		AccessToken   string    `json:"access_token"`
		PurchasedAt   time.Time `json:"purchased_at"`
		EventID       string    `json:"event_id"`
		EventTitle    string    `json:"event_title"`
		EventStartsAt time.Time `json:"event_starts_at"`
	}

	tickets := []ticketRow{}
	for rows.Next() {
		var t ticketRow
		if err := rows.Scan(&t.ID, &t.AccessToken, &t.PurchasedAt,
			&t.EventID, &t.EventTitle, &t.EventStartsAt); err != nil {
			continue
		}
		tickets = append(tickets, t)
	}

	return c.JSON(fiber.Map{"tickets": tickets})
}

// Lookup is a public endpoint — buyers retrieve their tickets by email address.
// Covers both authenticated-user purchases and guest purchases.
func (h *TicketHandler) Lookup(c *fiber.Ctx) error {
	email := c.Query("email")
	if email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email query param is required"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT t.id, t.access_token, t.purchased_at,
		        e.id AS event_id, e.title, e.start_time, e.is_active, e.ends_at
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 LEFT JOIN users u ON u.id = t.buyer_id
		 WHERE u.email = $1 OR t.guest_email = $1
		 ORDER BY t.purchased_at DESC`,
		email,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup failed"})
	}
	defer rows.Close()

	type ticketRow struct {
		ID            string    `json:"id"`
		AccessToken   string    `json:"access_token"`
		PurchasedAt   time.Time `json:"purchased_at"`
		EventID       string    `json:"event_id"`
		EventTitle    string    `json:"event_title"`
		EventStartsAt time.Time `json:"event_starts_at"`
		EventIsActive bool      `json:"event_is_active"`
		EventExpired  bool      `json:"event_expired"`
	}

	tickets := []ticketRow{}
	for rows.Next() {
		var t ticketRow
		var endsAt time.Time
		if err := rows.Scan(&t.ID, &t.AccessToken, &t.PurchasedAt,
			&t.EventID, &t.EventTitle, &t.EventStartsAt, &t.EventIsActive, &endsAt); err != nil {
			continue
		}
		t.EventExpired = time.Now().After(endsAt)
		tickets = append(tickets, t)
	}

	return c.JSON(fiber.Map{"tickets": tickets})
}

// Enter looks up a ticket by its access_token (the code printed on the ticket),
// and is the single gate that authorizes watching a stream. Public — no auth
// required, since guest-purchased tickets have no user account. Used both by
// the "enter your ticket code" flow and by the Watch page itself on every load.
func (h *TicketHandler) Enter(c *fiber.Ctx) error {
	code := c.Query("code")
	if code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "code is required"})
	}

	var (
		eventID     string
		eventTitle  string
		isActive    bool
		endsAt      time.Time
		playbackURL *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.id, e.title, e.is_active, e.ends_at, e.aws_playback_url
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.access_token = $1`,
		code,
	).Scan(&eventID, &eventTitle, &isActive, &endsAt, &playbackURL)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ticket not found"})
	}

	if time.Now().After(endsAt) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event has ended."})
	}
	if !isActive {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event is not active yet."})
	}

	resp := fiber.Map{
		"event_id":    eventID,
		"event_title": eventTitle,
		"is_active":   isActive,
	}
	if playbackURL != nil {
		resp["playback_url"] = *playbackURL
	}
	return c.JSON(resp)
}

type guestPurchaseRequest struct {
	EventID string `json:"event_id"`
	Email   string `json:"email"`
}

// GuestPurchase allows anyone to buy a ticket with just an email — no account required.
// If Stripe is configured, returns a checkout URL. If not (dev/bypass), creates the
// ticket directly and returns the access_token immediately.
func (h *TicketHandler) GuestPurchase(c *fiber.Ctx) error {
	var req guestPurchaseRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email is required"})
	}
	if req.EventID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event_id is required"})
	}

	var (
		eventTitle  string
		ticketPrice float64
		startsAt    time.Time
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT title, ticket_price, start_time FROM events WHERE id = $1 AND venue_paid = true AND ends_at > NOW()`,
		req.EventID,
	).Scan(&eventTitle, &ticketPrice, &startsAt)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or not available"})
	}

	// Dev bypass — Stripe not configured
	if h.Cfg.StripeSecretKey == "" || ticketPrice == 0 {
		tokenBytes := make([]byte, 16)
		rand.Read(tokenBytes)
		accessToken := "TCK-" + hex.EncodeToString(tokenBytes)

		if _, err := h.DB.Exec(context.Background(),
			`INSERT INTO tickets (event_id, buyer_id, guest_email, access_token) VALUES ($1, NULL, $2, $3)`,
			req.EventID, req.Email, accessToken,
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create ticket"})
		}

		if h.Email != nil {
			_ = h.Email.SendTicketConfirmation(req.Email, eventTitle, accessToken, startsAt)
		}

		return c.JSON(fiber.Map{
			"access_token": accessToken,
			"event_id":     req.EventID,
			"event_title":  eventTitle,
		})
	}

	// Stripe checkout
	var stripeAccountID *string
	var payoutGateway string
	err = h.DB.QueryRow(context.Background(),
		`SELECT ca.stripe_account_id, ca.payout_gateway FROM connected_accounts ca
		 JOIN events e ON e.host_id = ca.user_id
		 WHERE e.id = $1`,
		req.EventID,
	).Scan(&stripeAccountID, &payoutGateway)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not connected a payout account"})
	}
	if payoutGateway == "stripe" && stripeAccountID == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}

	split := services.CalculateSplit(ticketPrice)
	stripe.Key = h.Cfg.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(eventTitle + " — Ticket"),
					},
					UnitAmount: stripe.Int64(int64(ticketPrice * 100)),
				},
				Quantity: stripe.Int64(1),
			},
		},
		CustomerEmail: stripe.String(req.Email),
		SuccessURL:    stripe.String(h.Cfg.FrontendURL + "/ticket-success?email=" + req.Email),
		CancelURL:     stripe.String(h.Cfg.FrontendURL + "/events/" + req.EventID),
		Metadata: map[string]string{
			"type":        "ticket",
			"event_id":    req.EventID,
			"guest_email": req.Email,
		},
	}
	// Stripe-connected hosts get paid via a destination charge at checkout time.
	// WiPay/PayPal hosts are charged into the platform's balance and paid out
	// separately (see PayoutHandler.Payout), so no transfer here.
	if payoutGateway == "stripe" {
		params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
			ApplicationFeeAmount: stripe.Int64(int64(split.PlatformFee * 100)),
			TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
				Destination: stripe.String(*stripeAccountID),
			},
		}
	}

	s, err := session.New(params)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}

	return c.JSON(fiber.Map{"checkout_url": s.URL})
}

type purchaseRequest struct {
	EventID string `json:"event_id"`
}

func (h *TicketHandler) Purchase(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "payment processing not configured yet"})
	}

	var req purchaseRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.EventID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event_id is required"})
	}

	var (
		eventTitle      string
		ticketPrice     float64
		stripeAccountID *string
		payoutGateway   string
		buyerEmail      string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, e.ticket_price, ca.stripe_account_id, ca.payout_gateway, u.email
		 FROM events e
		 JOIN connected_accounts ca ON ca.user_id = e.host_id
		 JOIN users u ON u.id = $2
		 WHERE e.id = $1 AND e.is_active = true AND e.venue_paid = true AND e.ends_at > NOW()`,
		req.EventID, buyerID,
	).Scan(&eventTitle, &ticketPrice, &stripeAccountID, &payoutGateway, &buyerEmail)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "event not found, not yet published, or host has not connected a payout account",
		})
	}
	if payoutGateway == "stripe" && stripeAccountID == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}

	split := services.CalculateSplit(ticketPrice)
	stripe.Key = h.Cfg.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(eventTitle + " — Ticket"),
					},
					UnitAmount: stripe.Int64(int64(ticketPrice * 100)),
				},
				Quantity: stripe.Int64(1),
			},
		},
		CustomerEmail: stripe.String(buyerEmail),
		SuccessURL:    stripe.String(h.Cfg.FrontendURL + "/ticket-success?email=" + buyerEmail),
		CancelURL:     stripe.String(h.Cfg.FrontendURL + "/events/" + req.EventID),
		Metadata: map[string]string{
			"type":     "ticket",
			"event_id": req.EventID,
			"buyer_id": buyerID,
		},
	}
	if payoutGateway == "stripe" {
		params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
			ApplicationFeeAmount: stripe.Int64(int64(split.PlatformFee * 100)),
			TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
				Destination: stripe.String(*stripeAccountID),
			},
		}
	}

	s, err := session.New(params)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}

	return c.JSON(fiber.Map{"checkout_url": s.URL})
}
