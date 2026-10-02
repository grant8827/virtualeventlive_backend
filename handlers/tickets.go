package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
	"vertualeventlive/backend/services"
)

type TicketHandler struct {
	DB       *pgxpool.Pool
	Cfg      *config.Config
	Email    *services.EmailService
	PayPal   *services.PayPalService
	Playback *PlaybackAccess
	// Limiter: shared (Redis) rate limits; nil uses an in-process fallback.
	Limiter *middleware.RateLimiter
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
		 WHERE t.buyer_id = $1 AND t.voided_at IS NULL
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
		channelARN       *string
		venuePaid        bool
		cancelled        bool
		ticketType       string
		checkedInChannel *string
		voided           bool
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT t.id, e.id, e.title, e.is_active, e.ends_at, e.aws_playback_url, e.aws_channel_arn,
		        e.venue_paid, e.cancelled_at IS NOT NULL, e.ticket_type, t.checked_in_channel,
		        t.voided_at IS NOT NULL
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.access_token = $1`,
		code,
	).Scan(&ticketID, &eventID, &eventTitle, &isActive, &endsAt, &playbackURL, &channelARN,
		&venuePaid, &cancelled, &ticketType, &checkedInChannel, &voided)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ticket not found"})
	}
	if voided {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket was refunded or cancelled and is no longer valid."})
	}

	if time.Now().After(endsAt) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event has ended."})
	}
	if cancelled {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event was cancelled."})
	}
	if !isActive || !venuePaid {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event is not active yet."})
	}

	// "Virtual + Location" tickets are single-use across channels: whichever
	// of "watch online" or "scan in at the door" happens first claims the
	// ticket, and the other channel is then locked out. Re-entering here on
	// the SAME channel (e.g. reloading the Watch page) stays unaffected —
	// it's only a first-claim marker, not a one-time-only gate for virtual.
	if ticketType == "Virtual + Location" {
		if checkedInChannel == nil {
			// Claim the ticket for the stream only if nobody has claimed it
			// yet (PAY-10): a door scan racing this request can't both win.
			tag, err := h.DB.Exec(context.Background(),
				`UPDATE tickets SET checked_in_at = NOW(), checked_in_channel = 'virtual'
				 WHERE id = $1 AND checked_in_channel IS NULL AND voided_at IS NULL`,
				ticketID,
			)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record entry"})
			}
			if tag.RowsAffected() == 0 {
				// Someone else claimed it first; read who (a new statement,
				// so it sees their committed write).
				if err := h.DB.QueryRow(context.Background(),
					`SELECT checked_in_channel FROM tickets WHERE id = $1`, ticketID,
				).Scan(&checkedInChannel); err != nil || checkedInChannel == nil {
					return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record entry"})
				}
			}
		}
		if checkedInChannel != nil && *checkedInChannel == "physical" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket was already used to check in at the venue."})
		}
	}

	// The ticket is valid for an active, unexpired event: only now is a
	// playback URL issued — signed and short-lived once IVS signing is set up.
	playURL, err := h.Playback.URL(playbackURL, channelARN)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "The stream can't be opened right now. Please try again shortly."})
	}
	resp := fiber.Map{
		"event_id":    eventID,
		"event_title": eventTitle,
		"is_active":   isActive,
	}
	if playURL != "" {
		resp["playback_url"] = playURL
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
		eventAssignedTo  *string
		ticketType       string
		checkedInAt      *time.Time
		checkedInChannel *string
		voided           bool
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT t.id, t.serial_no, e.title, e.host_id, e.assigned_to, e.ticket_type, t.checked_in_at, t.checked_in_channel,
		        t.voided_at IS NOT NULL
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.serial_no::text = $1 OR t.access_token = $1`,
		code,
	).Scan(&ticketID, &serialNo, &eventTitle, &eventHostID, &eventAssignedTo, &ticketType, &checkedInAt, &checkedInChannel, &voided)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Ticket not found."})
	}

	if eventHostID != hostID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket belongs to a different host's event."})
	}
	if staffID, isStaff := middleware.AssignedStaff(c); isStaff && (eventAssignedTo == nil || *eventAssignedTo != staffID) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This ticket is for an event that isn't assigned to you."})
	}
	if ticketType != "Virtual + Location" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "This is a virtual-only ticket — it doesn't need a door check-in."})
	}
	if voided {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "This ticket was refunded or cancelled — do not admit.", "used": true, "voided": true,
			"event_title": eventTitle, "serial_no": serialNo,
		})
	}

	// Claim the ticket only if it's still unclaimed (PAY-10). Two scanners,
	// or a scan racing the stream, can't both admit it: exactly one UPDATE
	// changes the row.
	if checkedInChannel == nil {
		tag, err := h.DB.Exec(context.Background(),
			`UPDATE tickets SET checked_in_at = NOW(), checked_in_channel = 'physical'
			 WHERE id = $1 AND checked_in_channel IS NULL AND voided_at IS NULL`,
			ticketID,
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record check-in"})
		}
		if tag.RowsAffected() == 1 {
			return c.JSON(fiber.Map{
				"ok":          true,
				"used":        false,
				"event_title": eventTitle,
				"serial_no":   serialNo,
			})
		}
		// Lost the race: report who won.
		if err := h.DB.QueryRow(context.Background(),
			`SELECT checked_in_at, checked_in_channel FROM tickets WHERE id = $1`, ticketID,
		).Scan(&checkedInAt, &checkedInChannel); err != nil || checkedInChannel == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record check-in"})
		}
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

	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to record check-in"})
}

type guestPurchaseRequest struct {
	EventID string `json:"event_id"`
	Email   string `json:"email"`
}

// feeRule is the commission new ticket orders are sold under (PAY-07).
func feeRule(cfg *config.Config) services.FeeRule {
	if cfg == nil {
		return services.FeeRule{BasisPoints: services.DefaultPlatformFeeBPS}
	}
	return services.FeeRule{BasisPoints: cfg.PlatformFeeBPS}
}

// GuestPurchase allows anyone to buy a ticket with just an email — no account required.
// Free tickets are issued at once; paid tickets go through the host's
// checkout provider.
func (h *TicketHandler) GuestPurchase(c *fiber.Ctx) error {
	var req guestPurchaseRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email is required"})
	}
	if addr, err := mail.ParseAddress(req.Email); err != nil || addr.Address != req.Email {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a valid email address"})
	}
	if req.EventID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event_id is required"})
	}

	var (
		eventTitle    string
		priceCents    int64
		startsAt      time.Time
		payoutGateway string
		stripeAccount *string
		paypalAccount *string
		stripeReady   bool
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, (e.ticket_price * 100)::bigint, e.start_time, COALESCE(ca.payout_gateway, ''),
		        ca.stripe_account_id, ca.paypal_account_id, COALESCE(ca.payout_enabled, false)
		 FROM events e
		 LEFT JOIN connected_accounts ca ON ca.user_id = e.host_id
		 WHERE e.id = $1 AND e.venue_paid = true AND e.ends_at > NOW()`,
		req.EventID,
	).Scan(&eventTitle, &priceCents, &startsAt, &payoutGateway, &stripeAccount, &paypalAccount, &stripeReady)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or not available"})
	}
	if err := canSellTickets(context.Background(), h.DB, req.EventID, false); err != nil {
		return saleRuleResponse(c, err)
	}

	// Free tickets do not need a configured payment processor.
	if priceCents == 0 {
		accessToken, claim, err := h.issueFreeTicket(context.Background(), req.EventID, nil, req.Email)
		if err != nil {
			return saleRuleResponse(c, err)
		}
		sendTicketEmail(h.DB, h.Email, req.Email, accessToken)

		// access_token goes only to the purchaser in this response; claim
		// opens /ticket-success without putting an email in the URL.
		return c.JSON(fiber.Map{
			"access_token": accessToken,
			"event_id":     req.EventID,
			"event_title":  eventTitle,
			"claim":        claim,
		})
	}

	sale := paidSale{EventID: req.EventID, Title: eventTitle, PriceCents: priceCents, BuyerEmail: req.Email}
	if payoutGateway == "paypal" {
		if paypalAccount == nil || h.PayPal == nil || !h.PayPal.PartnerEnabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "host PayPal checkout is not available"})
		}
		return h.createPayPalTicketCheckout(c, sale, *paypalAccount)
	}
	if payoutGateway != "stripe" {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not selected a payout account"})
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe payment processing is not configured"})
	}
	if stripeAccount == nil || !stripeReady {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}
	return h.createStripeTicketCheckout(c, sale, *stripeAccount)
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
		priceCents      int64
		stripeAccountID *string
		paypalAccountID *string
		payoutGateway   string
		stripeReady     bool
		buyerEmail      string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, (e.ticket_price * 100)::bigint, ca.stripe_account_id, ca.paypal_account_id,
		        COALESCE(ca.payout_gateway, ''), ca.payout_enabled, u.email
		 FROM events e
		 JOIN connected_accounts ca ON ca.user_id = e.host_id
		 JOIN users u ON u.id = $2
		 WHERE e.id = $1 AND e.is_active = true AND e.venue_paid = true AND e.ends_at > NOW()
		   AND ca.payout_gateway IN ('stripe', 'paypal')`,
		req.EventID, buyerID,
	).Scan(&eventTitle, &priceCents, &stripeAccountID, &paypalAccountID, &payoutGateway, &stripeReady, &buyerEmail)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "event not found, not yet published, or host has not connected a payout account",
		})
	}
	if err := canSellTickets(context.Background(), h.DB, req.EventID, false); err != nil {
		return saleRuleResponse(c, err)
	}
	if priceCents == 0 {
		accessToken, claim, err := h.issueFreeTicket(context.Background(), req.EventID, &buyerID, buyerEmail)
		if err != nil {
			return saleRuleResponse(c, err)
		}
		sendTicketEmail(h.DB, h.Email, buyerEmail, accessToken)
		return c.JSON(fiber.Map{"access_token": accessToken, "event_id": req.EventID, "claim": claim})
	}
	sale := paidSale{EventID: req.EventID, Title: eventTitle, PriceCents: priceCents, BuyerID: &buyerID, BuyerEmail: buyerEmail}
	if payoutGateway == "paypal" {
		if paypalAccountID == nil || h.PayPal == nil || !h.PayPal.PartnerEnabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "host PayPal checkout is not available"})
		}
		return h.createPayPalTicketCheckout(c, sale, *paypalAccountID)
	}
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe payment processing is not configured"})
	}
	if stripeAccountID == nil || !stripeReady {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "host has not completed Stripe onboarding"})
	}
	return h.createStripeTicketCheckout(c, sale, *stripeAccountID)
}

// paidSale is one paid ticket about to go to checkout.
type paidSale struct {
	EventID    string
	Title      string
	PriceCents int64
	BuyerID    *string
	BuyerEmail string
}

func (h *TicketHandler) pendingTicketOrder(ctx context.Context, s paidSale, provider string) (*order, error) {
	return createPendingOrder(ctx, h.DB, newOrder{
		Kind: "ticket", EventID: s.EventID, BuyerID: s.BuyerID, BuyerEmail: s.BuyerEmail,
		Quantity: 1, UnitCents: s.PriceCents, Fee: feeRule(h.Cfg), Provider: provider,
		Reserve: true, PerBuyerLimit: h.Cfg.TicketsPerBuyer(),
	})
}

// createStripeTicketCheckout reserves the ticket in a pending order, then
// opens a Stripe Checkout for exactly that order: the charge, the platform
// fee and the host's destination transfer all come from the order's cents.
func (h *TicketHandler) createStripeTicketCheckout(c *fiber.Ctx, s paidSale, destination string) error {
	ctx := context.Background()
	o, err := h.pendingTicketOrder(ctx, s, "stripe")
	if err != nil {
		return saleRuleResponse(c, err)
	}
	stripe.Key = h.Cfg.StripeSecretKey
	claimToken, claimHash, err := newClaimToken()
	if err != nil {
		_ = endPendingOrder(ctx, h.DB, o.ID, "failed", "checkout could not be created")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start checkout"})
	}

	metadata := map[string]string{"type": "ticket", "event_id": s.EventID, "order_id": o.ID}
	if s.BuyerID != nil {
		metadata["buyer_id"] = *s.BuyerID
	} else {
		metadata["guest_email"] = s.BuyerEmail
	}
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(s.Title + " — Ticket"),
					},
					UnitAmount: stripe.Int64(o.UnitAmountCents),
				},
				Quantity: stripe.Int64(int64(o.Quantity)),
			},
		},
		ClientReferenceID: stripe.String(o.ID),
		CustomerEmail:     stripe.String(s.BuyerEmail),
		// Shortest Stripe allows: narrows the gap between "on sale" at
		// checkout and payment completing. The order's reservation outlasts it.
		ExpiresAt:  stripe.Int64(time.Now().Add(30 * time.Minute).Unix()),
		SuccessURL: stripe.String(h.Cfg.FrontendURL + "/ticket-success?claim=" + url.QueryEscape(claimToken)),
		CancelURL:  stripe.String(h.Cfg.FrontendURL + "/events/" + s.EventID),
		Metadata:   metadata,
	}
	params.AddExtra("managed_payments[enabled]", "false")
	params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
		// Refunds and disputes carry the PaymentIntent's metadata, which
		// is how they're matched to the order.
		Metadata: map[string]string{"order_id": o.ID, "event_id": s.EventID},
		TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
			Destination: stripe.String(destination),
		},
	}
	if o.PlatformFeeCents > 0 {
		params.PaymentIntentData.ApplicationFeeAmount = stripe.Int64(o.PlatformFeeCents)
	}
	params.SetIdempotencyKey("checkout-" + o.ID)

	sess, err := session.New(params)
	if err != nil {
		_ = endPendingOrder(ctx, h.DB, o.ID, "failed", "checkout could not be created")
		return stripeCheckoutError(c, err)
	}
	if err := setOrderCheckout(ctx, h.DB, o.ID, sess.ID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start checkout"})
	}
	// The webhook attaches the ticket to this claim once payment completes.
	if err := insertTicketClaim(ctx, h.DB, claimHash, s.EventID, claimTarget{StripeSessionID: &sess.ID}); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start checkout"})
	}

	return c.JSON(fiber.Map{"checkout_url": sess.URL})
}

func (h *TicketHandler) createPayPalTicketCheckout(c *fiber.Ctx, s paidSale, merchantID string) error {
	ctx := context.Background()
	o, err := h.pendingTicketOrder(ctx, s, "paypal")
	if err != nil {
		return saleRuleResponse(c, err)
	}
	claimToken, claimHash, err := newClaimToken()
	if err != nil {
		_ = endPendingOrder(ctx, h.DB, o.ID, "failed", "checkout could not be created")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to start checkout"})
	}
	// PayPal appends &token=<order id>; the claim rides along so the return
	// page can show this purchase's ticket without any email in the URL.
	returnURL := strings.TrimRight(c.BaseURL(), "/") + "/api/v1/tickets/paypal/complete?claim=" + url.QueryEscape(claimToken)
	ppOrder, err := h.PayPal.CreateCheckoutOrder(services.CheckoutOrderRequest{
		AmountCents: o.AmountCents, Description: s.Title + " — Ticket", Reference: "ticket-" + s.EventID,
		ReturnURL: returnURL, CancelURL: h.Cfg.FrontendURL + "/events/" + s.EventID,
		PayeeMerchantID: merchantID, PlatformFeeCents: o.PlatformFeeCents, CustomID: o.ID,
	})
	if err != nil {
		fmt.Printf("paypal ticket checkout error: %v\n", err)
		_ = endPendingOrder(ctx, h.DB, o.ID, "failed", "checkout could not be created")
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "PayPal could not create checkout"})
	}
	if _, err := h.DB.Exec(ctx,
		`INSERT INTO paypal_ticket_orders (order_id, event_id, buyer_id, buyer_email, amount, merchant_id, app_order_id)
		 VALUES ($1, $2, $3, $4, ($5::bigint)::numeric / 100, $6, $7)`,
		ppOrder.ID, s.EventID, s.BuyerID, s.BuyerEmail, o.AmountCents, merchantID, o.ID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save PayPal checkout"})
	}
	if err := setOrderCheckout(ctx, h.DB, o.ID, ppOrder.ID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save PayPal checkout"})
	}
	if err := insertTicketClaim(ctx, h.DB, claimHash, s.EventID, claimTarget{PayPalOrderID: &ppOrder.ID}); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save PayPal checkout"})
	}
	return c.JSON(fiber.Map{"checkout_provider": "paypal", "checkout_url": ppOrder.ApprovalURL})
}

// PayPalComplete captures an approved ticket order and creates exactly one
// ticket for it. Order details come only from the server-side row.
func (h *TicketHandler) PayPalComplete(c *fiber.Ctx) error {
	orderID := strings.TrimSpace(c.Query("token"))
	redirect := h.Cfg.FrontendURL + "/ticket-success"
	if orderID == "" || h.PayPal == nil {
		return c.Redirect(redirect + "?payment=failed")
	}
	_, _, err := h.completePayPalTicketOrder(context.Background(), orderID)
	if claim := strings.TrimSpace(c.Query("claim")); claim != "" {
		redirect += "?claim=" + url.QueryEscape(claim)
	}
	if err != nil {
		fmt.Printf("paypal ticket completion error: %v\n", err)
		separator := "?"
		if strings.Contains(redirect, "?") {
			separator = "&"
		}
		return c.Redirect(redirect + separator + "payment=failed")
	}
	return c.Redirect(redirect)
}

// paypalCaptureMatches checks PayPal took exactly this order's money, in
// USD, for this event, into the host's account (PAY-04).
func paypalCaptureMatches(captured *services.CapturedOrder, o *order, merchantID string) error {
	switch {
	case captured == nil:
		return errors.New("no capture details")
	case captured.Currency != "USD":
		return fmt.Errorf("captured in %q, not USD", captured.Currency)
	case captured.AmountCents != o.AmountCents:
		return fmt.Errorf("captured %d cents for an order of %d cents", captured.AmountCents, o.AmountCents)
	case captured.Reference != "ticket-"+o.EventID:
		return fmt.Errorf("capture reference %q is not this event", captured.Reference)
	case captured.PayeeMerchantID != merchantID:
		return errors.New("captured into a different PayPal account")
	case captured.CaptureID == "":
		return errors.New("capture has no ID")
	}
	return nil
}

// completePayPalTicketOrder captures and fulfils one locally-created PayPal
// order. The row locks make browser returns and webhook retries converge on
// exactly one capture, one ticket and one set of journals.
func (h *TicketHandler) completePayPalTicketOrder(ctx context.Context, paypalOrderID string) (buyerEmail string, alreadyCompleted bool, resultErr error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)
	var (
		eventID, status, merchantID string
		buyerID, orderID            *string
		amount                      float64
	)
	if err := tx.QueryRow(ctx,
		`SELECT event_id, buyer_id, buyer_email, amount::float8, status, merchant_id, app_order_id::text
		 FROM paypal_ticket_orders WHERE order_id = $1 FOR UPDATE`, paypalOrderID,
	).Scan(&eventID, &buyerID, &buyerEmail, &amount, &status, &merchantID, &orderID); err != nil {
		return "", false, err
	}
	if status == "completed" {
		return buyerEmail, true, nil
	}

	var o *order
	if orderID != nil {
		if o, err = lockOrder(ctx, tx, `id = $1`, *orderID); err != nil {
			return buyerEmail, false, err
		}
	}
	if o == nil {
		// A checkout started by the previous release: record it as an order.
		cents, cerr := services.DollarsToCents(amount)
		if cerr != nil {
			return buyerEmail, false, cerr
		}
		if o, err = insertOrder(ctx, tx, newOrder{
			Kind: "ticket", EventID: eventID, BuyerID: buyerID, BuyerEmail: buyerEmail, Quantity: 1,
			UnitCents: cents, Fee: feeRule(h.Cfg), Provider: "paypal", Adopt: true, CheckoutID: paypalOrderID,
		}, "pending"); err != nil {
			return buyerEmail, false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE paypal_ticket_orders SET app_order_id = $2 WHERE order_id = $1`, paypalOrderID, o.ID); err != nil {
			return buyerEmail, false, err
		}
	}
	if o.Status == "paid" || o.Status == "needs_attention" {
		return buyerEmail, true, nil
	}

	// Re-check the sales rule before taking the money: if the event was
	// cancelled, disabled, ended or sold out since checkout started, the
	// order is never captured and the buyer isn't charged.
	if err := checkSale(ctx, tx, eventID, saleCheck{Lock: true, Quantity: o.Quantity, ExcludeOrder: &o.ID}); err != nil {
		if isSaleRuleError(err) {
			if eerr := endPendingOrder(ctx, tx, o.ID, "failed", err.Error()); eerr == nil {
				_ = tx.Commit(ctx)
			}
		}
		return buyerEmail, false, err
	}
	captured, err := h.PayPal.CaptureOrder(paypalOrderID, merchantID)
	if err != nil {
		return buyerEmail, false, err
	}
	if mismatch := paypalCaptureMatches(captured, o, merchantID); mismatch != nil {
		reason := "PayPal capture doesn't match the order (" + mismatch.Error() + "); not booked"
		if err := markNeedsAttention(ctx, tx, o, reason); err != nil {
			return buyerEmail, false, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE orders SET provider_payment_id = NULLIF($2, '') WHERE id = $1`, o.ID, captured.CaptureID); err != nil {
			return buyerEmail, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return buyerEmail, false, err
		}
		return buyerEmail, false, needsAttention("PayPal order %s: %s", paypalOrderID, reason)
	}

	codes, err := markOrderPaid(ctx, tx, o, fulfillment{CheckoutID: paypalOrderID, PaymentID: captured.CaptureID}, nil)
	if err != nil {
		return buyerEmail, false, err
	}
	ticketID, err := firstTicketID(ctx, tx, o.ID)
	if err != nil {
		return buyerEmail, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ticket_claims SET ticket_id = $1 WHERE paypal_order_id = $2`, ticketID, paypalOrderID); err != nil {
		return buyerEmail, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE paypal_ticket_orders SET status='completed', ticket_id=$2, completed_at=NOW() WHERE order_id=$1 AND status='created'`, paypalOrderID, ticketID); err != nil {
		return buyerEmail, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return buyerEmail, false, err
	}
	for _, code := range codes {
		sendTicketEmail(h.DB, h.Email, buyerEmail, code)
	}
	return buyerEmail, false, nil
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
