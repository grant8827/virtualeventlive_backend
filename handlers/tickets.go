package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	DB     *pgxpool.Pool
	Cfg    *config.Config
	Email  *services.EmailService
	PayPal *services.PayPalService
}

func (h *TicketHandler) ListMine(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT t.id, t.access_token, t.serial_no, t.purchased_at,
		        t.checked_in_at, t.checked_in_channel,
		        e.id AS event_id, e.title, e.start_time, e.ticket_type, e.ticket_price,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image, e.logo_image, e.venue_address
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

	tickets := []ticketRow{}
	for rows.Next() {
		var t ticketRow
		if err := rows.Scan(&t.ID, &t.AccessToken, &t.SerialNo, &t.PurchasedAt,
			&t.CheckedInAt, &t.CheckedInChannel,
			&t.EventID, &t.EventTitle, &t.EventStartsAt, &t.TicketType, &t.TicketPrice,
			&t.CardBgFrom, &t.CardBgTo, &t.CardBgImage, &t.LogoImage, &t.VenueAddress); err != nil {
			continue
		}
		tickets = append(tickets, t)
	}

	return c.JSON(fiber.Map{"tickets": tickets})
}

// ticketRow is the shape returned to buyers by both ListMine and Lookup —
// everything the ticket UI needs to render the full visual ticket
// (TicketCard on the frontend), not just the bare access code.
type ticketRow struct {
	ID               string     `json:"id"`
	AccessToken      string     `json:"access_token"`
	SerialNo         int64      `json:"serial_no"`
	PurchasedAt      time.Time  `json:"purchased_at"`
	CheckedInAt      *time.Time `json:"checked_in_at"`
	CheckedInChannel *string    `json:"checked_in_channel"`
	EventID          string     `json:"event_id"`
	EventTitle       string     `json:"event_title"`
	EventStartsAt    time.Time  `json:"event_starts_at"`
	TicketType       string     `json:"ticket_type"`
	TicketPrice      float64    `json:"ticket_price"`
	CardBgFrom       string     `json:"card_bg_from"`
	CardBgTo         string     `json:"card_bg_to"`
	CardBgImage      *string    `json:"card_bg_image"`
	LogoImage        *string    `json:"logo_image"`
	VenueAddress     *string    `json:"venue_address"`
	EventIsActive    bool       `json:"event_is_active"`
	EventExpired     bool       `json:"event_expired"`
}

// Lookup is a public endpoint — buyers retrieve their tickets by email address.
// Covers both authenticated-user purchases and guest purchases.
func (h *TicketHandler) Lookup(c *fiber.Ctx) error {
	email := c.Query("email")
	if email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email query param is required"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT t.id, t.access_token, t.serial_no, t.purchased_at,
		        t.checked_in_at, t.checked_in_channel,
		        e.id AS event_id, e.title, e.start_time, e.ticket_type, e.ticket_price,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image, e.logo_image, e.venue_address,
		        e.is_active, e.ends_at
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

	tickets := []ticketRow{}
	for rows.Next() {
		var t ticketRow
		var endsAt time.Time
		if err := rows.Scan(&t.ID, &t.AccessToken, &t.SerialNo, &t.PurchasedAt,
			&t.CheckedInAt, &t.CheckedInChannel,
			&t.EventID, &t.EventTitle, &t.EventStartsAt, &t.TicketType, &t.TicketPrice,
			&t.CardBgFrom, &t.CardBgTo, &t.CardBgImage, &t.LogoImage, &t.VenueAddress,
			&t.EventIsActive, &endsAt); err != nil {
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
		ticketID         string
		eventID          string
		eventTitle       string
		isActive         bool
		endsAt           time.Time
		playbackURL      *string
		ticketType       string
		checkedInChannel *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT t.id, e.id, e.title, e.is_active, e.ends_at, e.aws_playback_url, e.ticket_type, t.checked_in_channel
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.access_token = $1`,
		code,
	).Scan(&ticketID, &eventID, &eventTitle, &isActive, &endsAt, &playbackURL, &ticketType, &checkedInChannel)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ticket not found"})
	}

	if time.Now().After(endsAt) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event has ended."})
	}
	if !isActive {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event is not active yet."})
	}

	// "Virtual + Location" tickets are single-use across channels: whichever
	// of "watch online" or "scan in at the door" happens first claims the
	// ticket, and the other channel is then locked out. Re-entering here on
	// the SAME channel (e.g. reloading the Watch page) stays unaffected —
	// it's only a first-claim marker, not a one-time-only gate for virtual.
	if ticketType == "Virtual + Location" {
		if checkedInChannel != nil && *checkedInChannel == "physical" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket was already used to check in at the venue."})
		}
		if checkedInChannel == nil {
			if _, err := h.DB.Exec(context.Background(),
				`UPDATE tickets SET checked_in_at = NOW(), checked_in_channel = 'virtual' WHERE id = $1`,
				ticketID,
			); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record entry"})
			}
		}
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

type checkinRequest struct {
	Code string `json:"code"`
}

// CheckIn is the dashboard door-scanner's endpoint: a host scans a ticket's
// QR code (or types its serial number in as a manual fallback) to admit a
// "Virtual + Location" ticket holder in person. Tickets are single-use
// across channels — see Enter — so a ticket already claimed by the virtual
// stream is rejected here too, and vice versa.
func (h *TicketHandler) CheckIn(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req checkinRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "code is required"})
	}

	// The QR payload and the printed serial fallback are the same value, so
	// staff can scan or type either one. Accept a raw access_token too, in
	// case someone reads it off the viewing-code half of the stub instead.
	var (
		ticketID         string
		serialNo         int64
		eventTitle       string
		eventHostID      string
		ticketType       string
		checkedInAt      *time.Time
		checkedInChannel *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT t.id, t.serial_no, e.title, e.host_id, e.ticket_type, t.checked_in_at, t.checked_in_channel
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.serial_no::text = $1 OR t.access_token = $1`,
		code,
	).Scan(&ticketID, &serialNo, &eventTitle, &eventHostID, &ticketType, &checkedInAt, &checkedInChannel)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Ticket not found."})
	}

	if eventHostID != hostID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket belongs to a different host's event."})
	}
	if ticketType != "Virtual + Location" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "This is a virtual-only ticket — it doesn't need a door check-in."})
	}

	if checkedInChannel != nil {
		usedWhere := "at the door"
		if *checkedInChannel == "virtual" {
			usedWhere = "on the live stream"
		}
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":         "Ticket already used — " + usedWhere + ".",
			"used":          true,
			"channel":       *checkedInChannel,
			"checked_in_at": checkedInAt,
			"event_title":   eventTitle,
			"serial_no":     serialNo,
		})
	}

	if _, err := h.DB.Exec(context.Background(),
		`UPDATE tickets SET checked_in_at = NOW(), checked_in_channel = 'physical' WHERE id = $1`,
		ticketID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record check-in"})
	}

	return c.JSON(fiber.Map{
		"ok":          true,
		"used":        false,
		"event_title": eventTitle,
		"serial_no":   serialNo,
	})
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
		eventTitle    string
		ticketPrice   float64
		startsAt      time.Time
		payoutGateway string
		stripeAccount *string
		paypalAccount *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, e.ticket_price, e.start_time, COALESCE(ca.payout_gateway, ''),
		        ca.stripe_account_id, ca.paypal_account_id
		 FROM events e
		 LEFT JOIN connected_accounts ca ON ca.user_id = e.host_id
		 WHERE e.id = $1 AND e.venue_paid = true AND e.ends_at > NOW()`,
		req.EventID,
	).Scan(&eventTitle, &ticketPrice, &startsAt, &payoutGateway, &stripeAccount, &paypalAccount)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or not available"})
	}

	// Free tickets do not need a configured payment processor.
	if ticketPrice == 0 {
		accessToken := services.GenerateTicketCode()

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

	if payoutGateway == "paypal" {
		if paypalAccount == nil || h.PayPal == nil || !h.PayPal.PartnerEnabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "host PayPal checkout is not available"})
		}
		return h.createPayPalTicketCheckout(c, req.EventID, eventTitle, req.Email, nil, *paypalAccount, ticketPrice)
	}
	if payoutGateway != "stripe" {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not selected a payout account"})
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe payment processing is not configured"})
	}

	// Stripe checkout
	if stripeAccount == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}
	var stripeReady bool
	_ = h.DB.QueryRow(context.Background(), `SELECT payout_enabled FROM connected_accounts ca JOIN events e ON e.host_id = ca.user_id WHERE e.id = $1`, req.EventID).Scan(&stripeReady)
	if !stripeReady {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}

	split := services.CalculateSplit(ticketPrice)
	stripe.Key = h.Cfg.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
		PaymentMethodTypes: []*string{stripe.String("card")},
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
	params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
		ApplicationFeeAmount: stripe.Int64(int64(split.PlatformFee * 100)),
		TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
			Destination: stripe.String(*stripeAccount),
		},
	}

	s, err := session.New(params)
	if err != nil {
		return stripeCheckoutError(c, err)
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
		paypalAccountID *string
		payoutGateway   string
		stripeReady     bool
		buyerEmail      string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, e.ticket_price, ca.stripe_account_id, ca.paypal_account_id,
		        COALESCE(ca.payout_gateway, ''), ca.payout_enabled, u.email
		 FROM events e
		 JOIN connected_accounts ca ON ca.user_id = e.host_id
		 JOIN users u ON u.id = $2
		 WHERE e.id = $1 AND e.is_active = true AND e.venue_paid = true AND e.ends_at > NOW()
		   AND ca.payout_gateway IN ('stripe', 'paypal')`,
		req.EventID, buyerID,
	).Scan(&eventTitle, &ticketPrice, &stripeAccountID, &paypalAccountID, &payoutGateway, &stripeReady, &buyerEmail)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "event not found, not yet published, or host has not connected a payout account",
		})
	}
	if ticketPrice == 0 {
		accessToken := services.GenerateTicketCode()
		if _, err := h.DB.Exec(context.Background(), `INSERT INTO tickets (event_id, buyer_id, access_token) VALUES ($1,$2,$3)`, req.EventID, buyerID, accessToken); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create ticket"})
		}
		return c.JSON(fiber.Map{"access_token": accessToken, "event_id": req.EventID})
	}
	if payoutGateway == "paypal" {
		if paypalAccountID == nil || h.PayPal == nil || !h.PayPal.PartnerEnabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "host PayPal checkout is not available"})
		}
		return h.createPayPalTicketCheckout(c, req.EventID, eventTitle, buyerEmail, &buyerID, *paypalAccountID, ticketPrice)
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe payment processing is not configured"})
	}
	if stripeAccountID == nil || !stripeReady {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}

	split := services.CalculateSplit(ticketPrice)
	stripe.Key = h.Cfg.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
		PaymentMethodTypes: []*string{stripe.String("card")},
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
	params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
		ApplicationFeeAmount: stripe.Int64(int64(split.PlatformFee * 100)),
		TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
			Destination: stripe.String(*stripeAccountID),
		},
	}

	s, err := session.New(params)
	if err != nil {
		return stripeCheckoutError(c, err)
	}

	return c.JSON(fiber.Map{"checkout_url": s.URL})
}

func (h *TicketHandler) createPayPalTicketCheckout(c *fiber.Ctx, eventID, eventTitle, buyerEmail string, buyerID *string, merchantID string, amount float64) error {
	returnURL := strings.TrimRight(c.BaseURL(), "/") + "/api/v1/tickets/paypal/complete"
	split := services.CalculateSplit(amount)
	order, err := h.PayPal.CreateCheckoutOrder(services.CheckoutOrderRequest{
		Amount: amount, Description: eventTitle + " — Ticket", Reference: "ticket-" + eventID,
		ReturnURL: returnURL, CancelURL: h.Cfg.FrontendURL + "/events/" + eventID,
		PayeeMerchantID: merchantID, PlatformFee: split.PlatformFee,
	})
	if err != nil {
		fmt.Printf("paypal ticket checkout error: %v\n", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "PayPal could not create checkout"})
	}
	if _, err := h.DB.Exec(context.Background(),
		`INSERT INTO paypal_ticket_orders (order_id, event_id, buyer_id, buyer_email, amount, merchant_id)
		 VALUES ($1,$2,$3,$4,$5,$6)`, order.ID, eventID, buyerID, buyerEmail, amount, merchantID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save PayPal checkout"})
	}
	return c.JSON(fiber.Map{"checkout_provider": "paypal", "checkout_url": order.ApprovalURL})
}

// PayPalComplete captures an approved ticket order and creates exactly one
// ticket/ledger pair for it. Order details come only from the server-side row.
func (h *TicketHandler) PayPalComplete(c *fiber.Ctx) error {
	orderID := strings.TrimSpace(c.Query("token"))
	redirect := h.Cfg.FrontendURL + "/ticket-success"
	if orderID == "" || h.PayPal == nil {
		return c.Redirect(redirect + "?payment=failed")
	}
	tx, err := h.DB.Begin(context.Background())
	if err != nil {
		return c.Redirect(redirect + "?payment=failed")
	}
	defer tx.Rollback(context.Background())
	var eventID, buyerEmail, status, merchantID string
	var buyerID *string
	var amount float64
	if err := tx.QueryRow(context.Background(),
		`SELECT event_id, buyer_id, buyer_email, amount, status, merchant_id
		 FROM paypal_ticket_orders WHERE order_id = $1 FOR UPDATE`, orderID,
	).Scan(&eventID, &buyerID, &buyerEmail, &amount, &status, &merchantID); err != nil {
		return c.Redirect(redirect + "?payment=failed")
	}
	redirect += "?email=" + url.QueryEscape(buyerEmail)
	if status == "completed" {
		return c.Redirect(redirect)
	}
	if err := h.PayPal.CaptureCheckoutOrder(orderID, merchantID); err != nil {
		fmt.Printf("paypal ticket capture error: %v\n", err)
		return c.Redirect(redirect + "&payment=failed")
	}

	var ticketID, accessToken string
	accessToken = services.GenerateTicketCode()
	if buyerID != nil {
		err = tx.QueryRow(context.Background(), `INSERT INTO tickets (event_id,buyer_id,access_token) VALUES ($1,$2,$3) RETURNING id`, eventID, *buyerID, accessToken).Scan(&ticketID)
	} else {
		err = tx.QueryRow(context.Background(), `INSERT INTO tickets (event_id,buyer_id,guest_email,access_token) VALUES ($1,NULL,$2,$3) RETURNING id`, eventID, buyerEmail, accessToken).Scan(&ticketID)
	}
	if err != nil {
		return c.Redirect(redirect + "&payment=failed")
	}
	split := services.CalculateSplit(amount)
	if _, err = tx.Exec(context.Background(),
		`INSERT INTO ledger_entries (ticket_id,event_id,gross_amount,stripe_fee,platform_fee,host_payout,payout_gateway,payout_status)
		 VALUES ($1,$2,$3,0,$4,$5,'paypal','paid')`, ticketID, eventID, amount, split.PlatformFee, split.HostShare()); err != nil {
		return c.Redirect(redirect + "&payment=failed")
	}
	if _, err = tx.Exec(context.Background(), `UPDATE paypal_ticket_orders SET status='completed', ticket_id=$2, completed_at=NOW() WHERE order_id=$1 AND status='created'`, orderID, ticketID); err != nil {
		return c.Redirect(redirect + "&payment=failed")
	}
	if err = tx.Commit(context.Background()); err != nil {
		return c.Redirect(redirect + "&payment=failed")
	}
	var eventTitle string
	var startsAt time.Time
	_ = h.DB.QueryRow(context.Background(), `SELECT title,start_time FROM events WHERE id=$1`, eventID).Scan(&eventTitle, &startsAt)
	if h.Email != nil {
		_ = h.Email.SendTicketConfirmation(buyerEmail, eventTitle, accessToken, startsAt)
	}
	return c.Redirect(redirect)
}

// stripeCheckoutError preserves Stripe's actionable, non-sensitive error
// message. The previous generic 500 made deployment/configuration failures
// impossible to diagnose from the checkout screen or Railway logs.
func stripeCheckoutError(c *fiber.Ctx, err error) error {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		fmt.Printf("stripe checkout error: request_id=%s code=%s param=%s message=%s\n", stripeErr.RequestID, stripeErr.Code, stripeErr.Param, stripeErr.Msg)
		if stripeErr.Msg != "" {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe checkout error: " + stripeErr.Msg})
		}
	}
	fmt.Printf("stripe checkout error: %v\n", err)
	return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe could not create checkout. Check the backend logs."})
}
