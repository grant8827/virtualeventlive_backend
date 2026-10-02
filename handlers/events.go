package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"strconv"
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

type EventHandler struct {
	DB     *pgxpool.Pool
	Cfg    *config.Config
	IVS    *services.IVSService
	WiPay  *services.WiPayService
	PayPal *services.PayPalService
}

// nullIfEmpty maps "" to a real SQL NULL instead of storing an empty string,
// so an unset optional text column reads back as nil rather than "".
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type createEventRequest struct {
	Title       string    `json:"title"`
	EventType   string    `json:"event_type"`
	Description string    `json:"description"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	// Optional team member responsible for the event. Staff only see
	// events assigned to them.
	AssignedTo string `json:"assigned_to"`
}

func (h *EventHandler) Create(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req createEventRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.Title == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "title is required"})
	}
	if req.EndsAt.IsZero() || !req.EndsAt.After(req.StartsAt) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ends_at must be after starts_at"})
	}

	duration := req.EndsAt.Sub(req.StartsAt)
	if duration < time.Hour {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "minimum booking is 1 hour"})
	}

	if req.EventType == "" {
		req.EventType = "other"
	}

	assignedTo := nullIfEmpty(req.AssignedTo)
	if assignedTo != nil {
		var onTeam bool
		if err := h.DB.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND account_owner_id = $2 AND status = 'active')`,
			*assignedTo, hostID,
		).Scan(&onTeam); err != nil || !onTeam {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "assigned user is not an active member of this account"})
		}
	}

	hours := int(math.Ceil(duration.Hours()))
	venueFee := float64(hours) * h.Cfg.HourlyRate

	var eventID string
	err := h.DB.QueryRow(context.Background(),
		`INSERT INTO events
			(host_id, title, event_type, description, start_time, ends_at, ticket_price, venue_fee, is_active, package_type, assigned_to)
		 VALUES ($1,$2,$3,$4,$5,$6,0,$7,false,'revenue_share',$8)
		 RETURNING id`,
		hostID, req.Title, req.EventType, req.Description, req.StartsAt, req.EndsAt, venueFee, assignedTo,
	).Scan(&eventID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create event"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":          eventID,
		"title":       req.Title,
		"event_type":  req.EventType,
		"description": req.Description,
		"starts_at":   req.StartsAt,
		"ends_at":     req.EndsAt,
		"venue_fee":   venueFee,
		"hours":       hours,
		"venue_paid":  false,
		"assigned_to": assignedTo,
	})
}

// Pricing is public: the site shows the venue rate from here rather than
// hard-coding it, so changing HOURLY_RATE updates every page.
func (h *EventHandler) Pricing(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"hourly_rate": h.Cfg.HourlyRate,
		// Shown on the ticket setup page before anything is sold (PAY-07).
		"platform_fee_bps": h.Cfg.PlatformFeeBPS,
	})
}

func (h *EventHandler) Checkout(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID := c.Params("id")
	var (
		title     string
		feeCents  int64
		venuePaid bool
		hostEmail string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.title, (e.venue_fee * 100)::bigint, e.venue_paid, u.email
		 FROM events e JOIN users u ON u.id = e.host_id WHERE e.id = $1 AND e.host_id = $2`,
		eventID, hostID,
	).Scan(&title, &feeCents, &venuePaid, &hostEmail)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if venuePaid {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "venue fee already paid"})
	}

	// Event booking is a separate platform purchase and always belongs to the
	// platform Stripe account. The host's selected ticket payout gateway is
	// intentionally not consulted here.
	if h.Cfg.StripeSecretKey == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe event booking checkout is not configured"})
	}

	ctx := context.Background()
	o, err := createPendingOrder(ctx, h.DB, newOrder{
		Kind: "booking_fee", EventID: eventID, BuyerID: &hostID, BuyerEmail: hostEmail,
		Quantity: 1, UnitCents: feeCents, Provider: "stripe",
	})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}

	stripe.Key = h.Cfg.StripeSecretKey

	// The host comes back through the backend, which confirms the payment
	// with Stripe before activating — so it works even where Stripe's webhook
	// can't reach us (local development). The webhook does the same check.
	returnURL := fmt.Sprintf("%s/api/v1/events/%s/stripe/complete?session_id={CHECKOUT_SESSION_ID}",
		strings.TrimRight(c.BaseURL(), "/"), url.PathEscape(eventID))

	metadata := map[string]string{
		"type":     "venue_fee",
		"event_id": eventID,
		"host_id":  hostID,
		"order_id": o.ID,
	}
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name:        stripe.String("Event booking platform fee — " + title),
						Description: stripe.String("Virtual Event Plus event booking and activation fee"),
					},
					UnitAmount: stripe.Int64(o.UnitAmountCents),
				},
				Quantity: stripe.Int64(1),
			},
		},
		ClientReferenceID: stripe.String(o.ID),
		SuccessURL:        stripe.String(returnURL),
		CancelURL:         stripe.String(h.Cfg.FrontendURL + "/dashboard?venue_paid=0"),
		Metadata:          metadata,
		PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{Metadata: metadata},
	}
	// The platform uses standard Checkout. Stripe enables Managed Payments by
	// default on this account, but that product requires tax codes for every
	// inline product. stripe-go v82 predates this typed field, so pass Stripe's
	// documented opt-out as an extra parameter.
	params.AddExtra("managed_payments[enabled]", "false")
	params.SetIdempotencyKey("checkout-" + o.ID)

	s, err := session.New(params)
	if err != nil {
		_ = endPendingOrder(ctx, h.DB, o.ID, "failed", "checkout session could not be created")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}
	if err := setOrderCheckout(ctx, h.DB, o.ID, s.ID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}

	return c.JSON(fiber.Map{"checkout_provider": "stripe", "checkout_url": s.URL})
}

func (h *EventHandler) WiPayLaunch(c *fiber.Ctx) error {
	if !h.Cfg.WipayVenueFeeEnabled || h.WiPay == nil {
		return c.Status(fiber.StatusServiceUnavailable).SendString("WiPay checkout is disabled")
	}
	eventID := c.Params("id")
	state := c.Query("state")
	if state == "" {
		state = c.FormValue("state")
	}
	if state == "" {
		return c.Status(fiber.StatusBadRequest).SendString("missing WiPay state")
	}

	var (
		hostID   string
		title    string
		venueFee float64
	)
	if err := h.DB.QueryRow(context.Background(),
		`SELECT host_id, title, venue_fee FROM events WHERE id = $1`,
		eventID,
	).Scan(&hostID, &title, &venueFee); err != nil {
		return c.Status(fiber.StatusNotFound).SendString("event not found")
	}

	if err := h.verifyVenueFeeState(state, eventID, hostID, venueFee); err != nil {
		return c.Status(fiber.StatusUnauthorized).SendString("invalid WiPay state")
	}

	successURL := fmt.Sprintf("%s/api/v1/events/%s/wipay/complete?state=%s",
		strings.TrimRight(c.BaseURL(), "/"),
		url.PathEscape(eventID),
		url.QueryEscape(state),
	)
	cancelURL := h.Cfg.FrontendURL + "/dashboard"

	checkout, err := h.WiPay.BuildHostedCheckout(services.HostedCheckoutRequest{
		Amount:      venueFee,
		Description: "Virtual Event Plus venue fee - " + title,
		// WiPay order IDs only allow alphanumeric characters, hyphens, and
		// underscores. Event UUIDs satisfy that constraint with this prefix.
		Reference:  "venue-fee-" + eventID,
		SuccessURL: successURL,
		CancelURL:  cancelURL,
	})
	if err != nil {
		return c.Status(fiber.StatusBadGateway).SendString(err.Error())
	}

	return c.Type("html").SendString(renderHostedCheckoutHTML(checkout))
}

// WiPayComplete activates an event from WiPay's browser return. The status
// it reads comes from the URL and is not verified with WiPay, so the route
// is disabled unless WIPAY_VENUE_FEE_ENABLED is set (refused in production).
func (h *EventHandler) WiPayComplete(c *fiber.Ctx) error {
	if !h.Cfg.WipayVenueFeeEnabled {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}
	eventID := c.Params("id")
	state := c.Query("state")
	if state == "" {
		// WiPay posts the hosted-checkout return values as form fields, whereas
		// a browser redirect supplies them in the query string.
		state = c.FormValue("state")
	}
	if state == "" {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}

	var (
		hostID   string
		venueFee float64
	)
	if err := h.DB.QueryRow(context.Background(),
		`SELECT host_id, venue_fee FROM events WHERE id = $1`,
		eventID,
	).Scan(&hostID, &venueFee); err != nil {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}

	if err := h.verifyVenueFeeState(state, eventID, hostID, venueFee); err != nil {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}

	status := c.Query("status")
	if status == "" {
		status = c.FormValue("status")
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if strings.Contains(status, "cancel") || strings.Contains(status, "fail") {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}

	if err := activateVenuePaidEvent(context.Background(), h.DB, h.IVS, eventID); err != nil {
		return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
	}

	return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=1")
}

// PayPalComplete was the return route for PayPal booking-fee payments.
// Booking fees are Stripe-only and nothing creates PayPal booking-fee orders
// any more, so the route is retired (PAY-08): it never captures a payment
// and never activates an event.
func (h *EventHandler) PayPalComplete(c *fiber.Ctx) error {
	return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0")
}

// StripeComplete is where Stripe sends the host after paying the booking
// fee. The session ID comes from the URL, so the payment is re-read from
// Stripe and checked before the event is activated and the fee is booked —
// the same steps, and the same idempotency, as the webhook.
func (h *EventHandler) StripeComplete(c *fiber.Ctx) error {
	eventID := c.Params("id")
	fail := func() error { return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=0") }

	sessionID := c.Query("session_id")
	if sessionID == "" || h.Cfg.StripeSecretKey == "" {
		return fail()
	}
	stripe.Key = h.Cfg.StripeSecretKey
	sess, err := session.Get(sessionID, nil)
	if err != nil {
		log.Printf("venue fee: Stripe session %s lookup failed: %v", sessionID, err)
		return fail()
	}
	if sess.Metadata["event_id"] != eventID {
		log.Printf("venue fee: Stripe session %s does not match event %s", sessionID, eventID)
		return fail()
	}
	if err := recordBookingFee(context.Background(), h.DB, h.IVS, sess); err != nil {
		log.Printf("venue fee: session %s for event %s not recorded: %v", sessionID, eventID, err)
		return fail()
	}
	return c.Redirect(h.Cfg.FrontendURL + "/dashboard?venue_paid=1")
}

// venueFeeStripeSessionMatches checks a Stripe Checkout session is a paid
// booking fee for this event, for the full amount in USD.
func venueFeeStripeSessionMatches(sess *stripe.CheckoutSession, eventID string, feeCents int64) bool {
	return sess != nil &&
		sess.Metadata["type"] == "venue_fee" &&
		sess.Metadata["event_id"] == eventID &&
		sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid &&
		strings.EqualFold(string(sess.Currency), "usd") &&
		sess.AmountTotal == feeCents
}

func (h *EventHandler) signVenueFeeState(eventID, hostID string, venueFee float64) (string, error) {
	// WiPay limits response_url to 255 characters. The event ID is already in
	// the route, so a compact expiry plus HMAC can bind the state to the event,
	// host, and amount without embedding a much longer JWT in the URL.
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	payload := venueFeeStatePayload(eventID, hostID, venueFee, expiresAt)
	mac := hmac.New(sha256.New, []byte(h.Cfg.JWTSecret))
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return strconv.FormatInt(expiresAt, 10) + "." + signature, nil
}

func (h *EventHandler) verifyVenueFeeState(tokenString, eventID, hostID string, venueFee float64) error {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 2 {
		return fmt.Errorf("invalid state token")
	}
	expiresAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expiresAt {
		return fmt.Errorf("expired or invalid state token")
	}

	provided, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("invalid state signature")
	}
	mac := hmac.New(sha256.New, []byte(h.Cfg.JWTSecret))
	_, _ = mac.Write([]byte(venueFeeStatePayload(eventID, hostID, venueFee, expiresAt)))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("state signature mismatch")
	}

	return nil
}

func venueFeeStatePayload(eventID, hostID string, venueFee float64, expiresAt int64) string {
	return eventID + ":" + hostID + ":" + strconv.FormatInt(int64(math.Round(venueFee*100)), 10) + ":" + strconv.FormatInt(expiresAt, 10)
}

func renderHostedCheckoutHTML(checkout *services.HostedCheckout) string {
	var body strings.Builder
	body.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Redirecting to WiPay</title></head><body>")
	body.WriteString("<form id=\"wipay-checkout\" method=\"")
	body.WriteString(checkout.Method)
	body.WriteString("\" action=\"")
	body.WriteString(templateEscape(checkout.URL))
	body.WriteString("\">")
	for key, value := range checkout.Fields {
		body.WriteString("<input type=\"hidden\" name=\"")
		body.WriteString(templateEscape(key))
		body.WriteString("\" value=\"")
		body.WriteString(templateEscape(value))
		body.WriteString("\">")
	}
	body.WriteString("</form><p>Redirecting to WiPay...</p><script>document.getElementById('wipay-checkout').submit()</script></body></html>")
	return body.String()
}

func templateEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(value)
}

type ticketSetupRequest struct {
	TicketName  string  `json:"ticket_name"`
	TicketPrice float64 `json:"ticket_price"`
	// MaxTickets caps sales (PAY-09). Omitted keeps the current cap; 0 or
	// null removes it.
	MaxTickets   *int   `json:"max_tickets"`
	TicketType   string `json:"ticket_type"`
	CardBgFrom   string `json:"card_bg_from"`
	CardBgTo     string `json:"card_bg_to"`
	CardBgImage  string `json:"card_bg_image"`
	VenueAddress string `json:"venue_address"`
}

func (h *EventHandler) TicketSetup(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID := c.Params("id")

	var req ticketSetupRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.TicketName == "" {
		req.TicketName = "General Admission"
	}
	if req.TicketPrice < 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ticket price cannot be negative"})
	}
	// Exact cents: no fractions of a cent, no float rounding (PAY-06).
	priceCents, err := services.DollarsToCents(req.TicketPrice)
	if err != nil || priceCents > services.MaxTicketPriceCents {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a ticket price in dollars and cents, up to $999,999.99"})
	}
	// Paid tickets must cover Stripe's minimum charge.
	if priceCents > 0 && priceCents < services.MinTicketPriceCents {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "paid tickets must be at least $0.50"})
	}
	// Only a request that sends max_tickets changes the cap, so older
	// clients that don't know about it never clear one by accident.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(c.Body(), &fields)
	_, setCap := fields["max_tickets"]
	var capValue *int
	if setCap {
		if req.MaxTickets != nil && *req.MaxTickets != 0 {
			if *req.MaxTickets < 0 || *req.MaxTickets > 1_000_000 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "capacity must be between 1 and 1,000,000, or empty for no limit"})
			}
			capValue = req.MaxTickets
		}
	}
	if req.CardBgFrom == "" {
		req.CardBgFrom = "#7c3aed"
	}
	if req.CardBgTo == "" {
		req.CardBgTo = "#1e1b4b"
	}

	if req.TicketType == "" {
		req.TicketType = "Virtual Only"
	}
	// Address only makes sense once there's a physical door to check in at.
	if req.TicketType != "Virtual + Location" {
		req.VenueAddress = ""
	}

	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save ticket"})
	}
	defer tx.Rollback(ctx)
	// Lock the event so the capacity check can't race a purchase (lock,
	// then count in a new statement: see checkSale).
	var one, held int
	if err := tx.QueryRow(ctx,
		`SELECT 1 FROM events WHERE id = $1 AND host_id = $2 AND ends_at > NOW() FOR UPDATE`, eventID, hostID,
	).Scan(&one); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or has already ended"})
	}
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM tickets t WHERE t.event_id = $1 AND t.voided_at IS NULL)
		      + (SELECT COALESCE(SUM(o.quantity), 0) FROM orders o WHERE o.event_id = $1 AND o.kind = 'ticket'
		           AND o.status = 'pending' AND o.reserved_until > NOW())`, eventID,
	).Scan(&held); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save ticket"})
	}
	if setCap && capValue != nil && *capValue < held {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("capacity can't be below the %d tickets already sold or being bought", held)})
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events
		 SET ticket_name = $1, ticket_price = ($2::bigint)::numeric / 100, ticket_type = $3,
		     card_bg_from = $4, card_bg_to = $5, card_bg_image = $6, venue_address = $7,
		     max_tickets = CASE WHEN $10 THEN $11::int ELSE max_tickets END
		 WHERE id = $8 AND host_id = $9`,
		req.TicketName, priceCents, req.TicketType,
		req.CardBgFrom, req.CardBgTo, req.CardBgImage, nullIfEmpty(req.VenueAddress),
		eventID, hostID, setCap, capValue,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save ticket"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save ticket"})
	}

	return c.JSON(fiber.Map{"ok": true})
}

// BypassActivate marks an event as paid and active without going through
// Stripe. Dev/testing use only: it is refused unless ALLOW_PAYMENT_BYPASS is
// on. Superusers activate events from the platform dashboard instead.
func (h *EventHandler) BypassActivate(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if !h.Cfg.AllowPaymentBypass {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "payment bypass is disabled"})
	}
	eventID := c.Params("id")

	result, err := h.DB.Exec(context.Background(),
		`UPDATE events SET venue_paid = true, is_active = true, venue_bypassed = true
		 WHERE id = $1 AND host_id = $2`,
		eventID, hostID,
	)
	if err != nil || result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	return c.JSON(fiber.Map{"ok": true})
}

// Delete removes a host-owned event from active/public listings. Issued
// tickets and financial records are preserved; the event is archived by
// deactivating it and moving its end time into the past.
func (h *EventHandler) Delete(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	tx, err := h.DB.Begin(context.Background())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event"})
	}
	defer tx.Rollback(context.Background())

	eventID := c.Params("id")
	if _, err := tx.Exec(context.Background(),
		`DELETE FROM advertisements
		 WHERE event_id = $1 AND host_id = $2
		   AND EXISTS (
		       SELECT 1 FROM events
		       WHERE id = $1 AND host_id = $2
		   )`,
		eventID, hostID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event advertisements"})
	}

	result, err := tx.Exec(context.Background(),
		`UPDATE events
		 SET is_active = false,
		     ends_at = LEAST(ends_at, NOW())
		 WHERE id = $1 AND host_id = $2`,
		eventID, hostID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event"})
	}
	if result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	if err := tx.Commit(context.Background()); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event"})
	}

	return c.JSON(fiber.Map{"ok": true})
}

func (h *EventHandler) ListByHost(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	dateFilter := "e.ends_at > NOW()"
	orderBy := "e.start_time ASC"
	if c.QueryBool("archive") {
		dateFilter = "e.ends_at <= NOW()"
		orderBy = "e.ends_at DESC"
	}

	// Staff only list events assigned to them; nil shows every event.
	var staffID *string
	if id, ok := middleware.AssignedStaff(c); ok {
		staffID = &id
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT e.id, e.title, e.event_type, e.start_time, e.ends_at, e.ticket_name,
		        e.ticket_price, e.ticket_type, e.max_tickets,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image, e.logo_image, e.venue_address,
		        e.venue_fee, e.venue_paid, e.is_active, (e.ends_at < NOW()) AS expired,
		        e.created_at,
		        (SELECT count(*) FROM tickets t WHERE t.event_id = e.id AND t.voided_at IS NULL) AS ticket_count,
		        e.assigned_to, COALESCE(u.full_name, u.email) AS assigned_to_name,
		        COALESCE(f.gross, 0), COALESCE(f.platform_fee, 0), COALESCE(f.refunded, 0)
		 FROM events e
		 LEFT JOIN users u ON u.id = e.assigned_to
		 LEFT JOIN LATERAL (
		     SELECT SUM(o.amount_cents) AS gross,
		            SUM(o.platform_fee_cents - o.platform_fee_refunded_cents) AS platform_fee,
		            SUM(o.refunded_cents) AS refunded
		     FROM orders o
		     WHERE o.event_id = e.id AND o.kind = 'ticket' AND o.status IN ('paid', 'needs_attention')
		 ) f ON true
		 WHERE e.host_id = $1 AND ($2::uuid IS NULL OR e.assigned_to = $2) AND `+dateFilter+`
		 ORDER BY `+orderBy,
		hostID, staffID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch events"})
	}
	defer rows.Close()

	type eventRow struct {
		ID             string     `json:"id"`
		Title          string     `json:"title"`
		EventType      string     `json:"event_type"`
		StartsAt       time.Time  `json:"starts_at"`
		EndsAt         *time.Time `json:"ends_at"`
		TicketName     string     `json:"ticket_name"`
		TicketPrice    float64    `json:"ticket_price"`
		TicketType     string     `json:"ticket_type"`
		MaxTickets     *int       `json:"max_tickets"`
		CardBgFrom     string     `json:"card_bg_from"`
		CardBgTo       string     `json:"card_bg_to"`
		CardBgImage    *string    `json:"card_bg_image"`
		LogoImage      *string    `json:"logo_image"`
		VenueAddress   *string    `json:"venue_address"`
		VenueFee       float64    `json:"venue_fee"`
		VenuePaid      bool       `json:"venue_paid"`
		IsActive       bool       `json:"is_active"`
		Expired        bool       `json:"expired"`
		CreatedAt      time.Time  `json:"created_at"`
		TicketCount    int        `json:"ticket_count"`
		AssignedTo     *string    `json:"assigned_to"`
		AssignedToName *string    `json:"assigned_to_name"`
		// Sales figures from the order books, in cents (Phase 2): what
		// buyers paid, the platform's commission net of refunds, refunds,
		// and the host's net.
		GrossCents       int64 `json:"gross_cents"`
		PlatformFeeCents int64 `json:"platform_fee_cents"`
		RefundedCents    int64 `json:"refunded_cents"`
		HostNetCents     int64 `json:"host_net_cents"`
	}

	events := []eventRow{}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(
			&e.ID, &e.Title, &e.EventType, &e.StartsAt, &e.EndsAt, &e.TicketName,
			&e.TicketPrice, &e.TicketType, &e.MaxTickets,
			&e.CardBgFrom, &e.CardBgTo, &e.CardBgImage, &e.LogoImage, &e.VenueAddress,
			&e.VenueFee, &e.VenuePaid, &e.IsActive, &e.Expired,
			&e.CreatedAt, &e.TicketCount,
			&e.AssignedTo, &e.AssignedToName,
			&e.GrossCents, &e.PlatformFeeCents, &e.RefundedCents,
		); err != nil {
			continue
		}
		e.HostNetCents = e.GrossCents - e.RefundedCents - e.PlatformFeeCents
		events = append(events, e)
	}

	return c.JSON(fiber.Map{"events": events})
}

// ListPublic returns venue-paid events available for ticket browsing.
// Public — no auth required. Used by the Tickets browse page.
func (h *EventHandler) ListPublic(c *fiber.Ctx) error {
	rows, err := h.DB.Query(context.Background(),
		`SELECT id, title, event_type, start_time, ends_at,
		        ticket_price, ticket_type, card_bg_from, card_bg_to, card_bg_image, venue_address
		 FROM events
		 WHERE venue_paid = true
		   AND is_active = true
		   AND ends_at > NOW()
		 ORDER BY start_time ASC
		 LIMIT 50`,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch events"})
	}
	defer rows.Close()

	type publicEvent struct {
		ID           string     `json:"id"`
		Title        string     `json:"title"`
		EventType    string     `json:"event_type"`
		StartsAt     time.Time  `json:"starts_at"`
		EndsAt       *time.Time `json:"ends_at"`
		TicketPrice  float64    `json:"ticket_price"`
		TicketType   string     `json:"ticket_type"`
		CardBgFrom   string     `json:"card_bg_from"`
		CardBgTo     string     `json:"card_bg_to"`
		CardBgImage  *string    `json:"card_bg_image"`
		VenueAddress *string    `json:"venue_address"`
	}

	events := []publicEvent{}
	for rows.Next() {
		var e publicEvent
		if err := rows.Scan(
			&e.ID, &e.Title, &e.EventType, &e.StartsAt, &e.EndsAt,
			&e.TicketPrice, &e.TicketType, &e.CardBgFrom, &e.CardBgTo, &e.CardBgImage, &e.VenueAddress,
		); err != nil {
			continue
		}
		events = append(events, e)
	}

	return c.JSON(fiber.Map{"events": events})
}

func (h *EventHandler) GetByID(c *fiber.Ctx) error {
	id := c.Params("id")

	var (
		eventID      string
		hostID       string
		title        string
		description  string
		startsAt     time.Time
		endsAt       *time.Time
		ticketPrice  float64
		ticketType   string
		venueAddress *string
		isActive     bool
		playbackURL  *string
		createdAt    time.Time
	)

	err := h.DB.QueryRow(context.Background(),
		`SELECT id, host_id, title, description, start_time, ends_at,
		        ticket_price, ticket_type, venue_address, is_active, aws_playback_url, created_at
		 FROM events WHERE id = $1`,
		id,
	).Scan(&eventID, &hostID, &title, &description, &startsAt, &endsAt,
		&ticketPrice, &ticketType, &venueAddress, &isActive, &playbackURL, &createdAt)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	expired := endsAt != nil && time.Now().After(*endsAt)

	return c.JSON(fiber.Map{
		"id":            eventID,
		"host_id":       hostID,
		"title":         title,
		"description":   description,
		"starts_at":     startsAt,
		"ends_at":       endsAt,
		"ticket_price":  ticketPrice,
		"ticket_type":   ticketType,
		"venue_address": venueAddress,
		"is_active":     isActive,
		"expired":       expired,
		// Public: whether the event has a stream channel, never its playback
		// URL. Playback is only issued by /tickets/enter after a ticket check.
		"has_stream": playbackURL != nil && *playbackURL != "",
		"created_at": createdAt,
	})
}
