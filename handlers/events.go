package handlers

import (
	"context"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"

	"vertualeventlive/backend/config"
)

type EventHandler struct {
	DB  *pgxpool.Pool
	Cfg *config.Config
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

	// Stripe not configured — bypass payment and mark event as paid directly
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

type ticketSetupRequest struct {
	TicketName  string  `json:"ticket_name"`
	TicketPrice float64 `json:"ticket_price"`
	TicketType  string  `json:"ticket_type"`
	CardBgFrom  string  `json:"card_bg_from"`
	CardBgTo    string  `json:"card_bg_to"`
	CardBgImage string  `json:"card_bg_image"`
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

	result, err := h.DB.Exec(context.Background(),
		`UPDATE events
		 SET ticket_name = $1, ticket_price = $2, ticket_type = $3,
		     card_bg_from = $4, card_bg_to = $5, card_bg_image = $6
		 WHERE id = $7 AND host_id = $8 AND ends_at > NOW()`,
		req.TicketName, req.TicketPrice, req.TicketType,
		req.CardBgFrom, req.CardBgTo, req.CardBgImage,
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

func (h *EventHandler) ListByHost(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT e.id, e.title, e.event_type, e.start_time, e.ends_at, e.ticket_name,
		        e.ticket_price, e.ticket_type, e.max_tickets,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image,
		        e.venue_fee, e.venue_paid, e.is_active, (e.ends_at < NOW()) AS expired,
		        e.created_at, COUNT(t.id) AS ticket_count
		 FROM events e
		 LEFT JOIN tickets t ON t.event_id = e.id
		 WHERE e.host_id = $1
		 GROUP BY e.id
		 ORDER BY e.created_at DESC`,
		hostID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch events"})
	}
	defer rows.Close()

	type eventRow struct {
		ID          string     `json:"id"`
		Title       string     `json:"title"`
		EventType   string     `json:"event_type"`
		StartsAt    time.Time  `json:"starts_at"`
		EndsAt      *time.Time `json:"ends_at"`
		TicketName  string     `json:"ticket_name"`
		TicketPrice float64    `json:"ticket_price"`
		TicketType  string     `json:"ticket_type"`
		MaxTickets  *int       `json:"max_tickets"`
		CardBgFrom  string     `json:"card_bg_from"`
		CardBgTo    string     `json:"card_bg_to"`
		CardBgImage *string    `json:"card_bg_image"`
		VenueFee    float64    `json:"venue_fee"`
		VenuePaid   bool       `json:"venue_paid"`
		IsActive    bool       `json:"is_active"`
		Expired     bool       `json:"expired"`
		CreatedAt   time.Time  `json:"created_at"`
		TicketCount int        `json:"ticket_count"`
	}

	events := []eventRow{}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(
			&e.ID, &e.Title, &e.EventType, &e.StartsAt, &e.EndsAt, &e.TicketName,
			&e.TicketPrice, &e.TicketType, &e.MaxTickets,
			&e.CardBgFrom, &e.CardBgTo, &e.CardBgImage,
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
		        ticket_price, ticket_type, card_bg_from, card_bg_to
		 FROM events
		 WHERE venue_paid = true AND ends_at > NOW()
		 ORDER BY start_time ASC
		 LIMIT 50`,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch events"})
	}
	defer rows.Close()

	type publicEvent struct {
		ID          string     `json:"id"`
		Title       string     `json:"title"`
		EventType   string     `json:"event_type"`
		StartsAt    time.Time  `json:"starts_at"`
		EndsAt      *time.Time `json:"ends_at"`
		TicketPrice float64    `json:"ticket_price"`
		TicketType  string     `json:"ticket_type"`
		CardBgFrom  string     `json:"card_bg_from"`
		CardBgTo    string     `json:"card_bg_to"`
	}

	events := []publicEvent{}
	for rows.Next() {
		var e publicEvent
		if err := rows.Scan(
			&e.ID, &e.Title, &e.EventType, &e.StartsAt, &e.EndsAt,
			&e.TicketPrice, &e.TicketType, &e.CardBgFrom, &e.CardBgTo,
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
		eventID     string
		hostID      string
		title       string
		description string
		startsAt    time.Time
		endsAt      *time.Time
		ticketPrice float64
		isActive    bool
		playbackURL *string
		createdAt   time.Time
	)

	err := h.DB.QueryRow(context.Background(),
		`SELECT id, host_id, title, description, start_time, ends_at,
		        ticket_price, is_active, aws_playback_url, created_at
		 FROM events WHERE id = $1`,
		id,
	).Scan(&eventID, &hostID, &title, &description, &startsAt, &endsAt,
		&ticketPrice, &isActive, &playbackURL, &createdAt)
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
		"is_active":        isActive,
		"expired":          expired,
		"aws_playback_url": playbackURL,
		"created_at":       createdAt,
	})
}
