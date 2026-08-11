package handlers

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

type EventHandler struct {
	DB    *pgxpool.Pool
	Cfg   *config.Config
	IVS   *services.IVSService
	WiPay *services.WiPayService
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

	hours := int(math.Ceil(duration.Hours()))
	venueFee := float64(hours) * h.Cfg.HourlyRate

	var eventID string
	err := h.DB.QueryRow(context.Background(),
		`INSERT INTO events
			(host_id, title, event_type, description, start_time, ends_at, ticket_price, venue_fee, is_active, package_type)
		 VALUES ($1,$2,$3,$4,$5,$6,0,$7,false,'revenue_share')
		 RETURNING id`,
		hostID, req.Title, req.EventType, req.Description, req.StartsAt, req.EndsAt, venueFee,
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
	})
}

func (h *EventHandler) Checkout(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID := c.Params("id")
	var title string
	var venueFee float64
	var venuePaid bool

	err := h.DB.QueryRow(context.Background(),
		`SELECT title, venue_fee, venue_paid FROM events WHERE id = $1 AND host_id = $2`,
		eventID, hostID,
	).Scan(&title, &venueFee, &venuePaid)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if venuePaid {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "venue fee already paid"})
	}

	provider := strings.ToLower(strings.TrimSpace(h.Cfg.VenueFeeProvider))
	if provider == "" {
		provider = "auto"
	}

	if provider == "wipay" || (provider == "auto" && h.WiPay != nil && h.WiPay.CheckoutEnabled()) {
		state, err := h.signVenueFeeState(eventID, hostID, venueFee)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to prepare WiPay checkout"})
		}

		launchURL := fmt.Sprintf("%s/api/v1/events/%s/wipay/launch?state=%s",
			strings.TrimRight(c.BaseURL(), "/"),
			url.PathEscape(eventID),
			url.QueryEscape(state),
		)

		return c.JSON(fiber.Map{
			"checkout_provider": "wipay",
			"checkout_url":      launchURL,
		})
	}

	// No payment provider configured — bypass payment and mark event as paid directly
	if h.Cfg.StripeSecretKey == "" {
		if _, err := h.DB.Exec(context.Background(),
			`UPDATE events SET venue_paid = true, is_active = true WHERE id = $1`, eventID,
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to activate event"})
		}
		return c.JSON(fiber.Map{"checkout_url": h.Cfg.FrontendURL + "/dashboard?venue_paid=1"})
	}

	stripe.Key = h.Cfg.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String("usd"),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name:        stripe.String("Venue rental — " + title),
						Description: stripe.String("VirtualEventLive streaming venue fee"),
					},
					UnitAmount: stripe.Int64(int64(venueFee * 100)),
				},
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(h.Cfg.FrontendURL + "/dashboard?venue_paid=1"),
		CancelURL:  stripe.String(h.Cfg.FrontendURL + "/dashboard"),
		Metadata: map[string]string{
			"type":     "venue_fee",
			"event_id": eventID,
			"host_id":  hostID,
		},
	}

	s, err := session.New(params)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create checkout session"})
	}

	return c.JSON(fiber.Map{"checkout_url": s.URL})
}

func (h *EventHandler) WiPayLaunch(c *fiber.Ctx) error {
	eventID := c.Params("id")
	state := c.Query("state")
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
		Description: "VirtualEventLive venue fee - " + title,
		Reference:   "venue_fee:" + eventID,
		SuccessURL:  successURL,
		CancelURL:   cancelURL,
	})
	if err != nil {
		return c.Status(fiber.StatusBadGateway).SendString(err.Error())
	}

	return c.Type("html").SendString(renderHostedCheckoutHTML(checkout))
}

func (h *EventHandler) WiPayComplete(c *fiber.Ctx) error {
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

func (h *EventHandler) signVenueFeeState(eventID, hostID string, venueFee float64) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"event_id":     eventID,
		"host_id":      hostID,
		"amount_cents": int64(math.Round(venueFee * 100)),
		"exp":          time.Now().Add(30 * time.Minute).Unix(),
	})
	return token.SignedString([]byte(h.Cfg.JWTSecret))
}

func (h *EventHandler) verifyVenueFeeState(tokenString, eventID, hostID string, venueFee float64) error {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		return []byte(h.Cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || !token.Valid {
		return fmt.Errorf("invalid state token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return fmt.Errorf("invalid state claims")
	}
	if claims["event_id"] != eventID || claims["host_id"] != hostID {
		return fmt.Errorf("state mismatch")
	}

	amountCents, ok := claims["amount_cents"].(float64)
	if !ok || int64(amountCents) != int64(math.Round(venueFee*100)) {
		return fmt.Errorf("amount mismatch")
	}

	return nil
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
	TicketName   string  `json:"ticket_name"`
	TicketPrice  float64 `json:"ticket_price"`
	TicketType   string  `json:"ticket_type"`
	CardBgFrom   string  `json:"card_bg_from"`
	CardBgTo     string  `json:"card_bg_to"`
	CardBgImage  string  `json:"card_bg_image"`
	VenueAddress string  `json:"venue_address"`
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

	result, err := h.DB.Exec(context.Background(),
		`UPDATE events
		 SET ticket_name = $1, ticket_price = $2, ticket_type = $3,
		     card_bg_from = $4, card_bg_to = $5, card_bg_image = $6, venue_address = $7
		 WHERE id = $8 AND host_id = $9 AND ends_at > NOW()`,
		req.TicketName, req.TicketPrice, req.TicketType,
		req.CardBgFrom, req.CardBgTo, req.CardBgImage, nullIfEmpty(req.VenueAddress),
		eventID, hostID,
	)
	if err != nil || result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or has already ended"})
	}

	return c.JSON(fiber.Map{"ok": true})
}

// BypassActivate marks an event as paid and active without going through
// Stripe. Dev/testing use only — activate from the frontend bypass button.
func (h *EventHandler) BypassActivate(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID := c.Params("id")

	result, err := h.DB.Exec(context.Background(),
		`UPDATE events SET venue_paid = true, is_active = true
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

	rows, err := h.DB.Query(context.Background(),
		`SELECT e.id, e.title, e.event_type, e.start_time, e.ends_at, e.ticket_name,
		        e.ticket_price, e.ticket_type, e.max_tickets,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image, e.logo_image, e.venue_address,
		        e.venue_fee, e.venue_paid, e.is_active, (e.ends_at < NOW()) AS expired,
		        e.created_at, COUNT(t.id) AS ticket_count
		 FROM events e
		 LEFT JOIN tickets t ON t.event_id = e.id
		 WHERE e.host_id = $1 AND `+dateFilter+`
		 GROUP BY e.id
		 ORDER BY `+orderBy,
		hostID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch events"})
	}
	defer rows.Close()

	type eventRow struct {
		ID           string     `json:"id"`
		Title        string     `json:"title"`
		EventType    string     `json:"event_type"`
		StartsAt     time.Time  `json:"starts_at"`
		EndsAt       *time.Time `json:"ends_at"`
		TicketName   string     `json:"ticket_name"`
		TicketPrice  float64    `json:"ticket_price"`
		TicketType   string     `json:"ticket_type"`
		MaxTickets   *int       `json:"max_tickets"`
		CardBgFrom   string     `json:"card_bg_from"`
		CardBgTo     string     `json:"card_bg_to"`
		CardBgImage  *string    `json:"card_bg_image"`
		LogoImage    *string    `json:"logo_image"`
		VenueAddress *string    `json:"venue_address"`
		VenueFee     float64    `json:"venue_fee"`
		VenuePaid    bool       `json:"venue_paid"`
		IsActive     bool       `json:"is_active"`
		Expired      bool       `json:"expired"`
		CreatedAt    time.Time  `json:"created_at"`
		TicketCount  int        `json:"ticket_count"`
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
		); err != nil {
			continue
		}
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
		"id":               eventID,
		"host_id":          hostID,
		"title":            title,
		"description":      description,
		"starts_at":        startsAt,
		"ends_at":          endsAt,
		"ticket_price":     ticketPrice,
		"ticket_type":      ticketType,
		"venue_address":    venueAddress,
		"is_active":        isActive,
		"expired":          expired,
		"aws_playback_url": playbackURL,
		"created_at":       createdAt,
	})
}
