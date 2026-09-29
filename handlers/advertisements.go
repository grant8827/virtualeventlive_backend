package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/middleware"
)

// staffFilter is the signed-in staff member's ID, or nil for the owner and
// admins. Queries use it as ($n::uuid IS NULL OR e.assigned_to = $n) so staff
// only touch flyers for events assigned to them.
func staffFilter(c *fiber.Ctx) *string {
	if id, ok := middleware.AssignedStaff(c); ok {
		return &id
	}
	return nil
}

type AdvertisementHandler struct {
	DB *pgxpool.Pool
}

type createAdRequest struct {
	EventID  *string `json:"event_id"`
	Headline string  `json:"headline"`
	Body     string  `json:"body"`
	ImageURL string  `json:"image_url"`
	CTAText  string  `json:"cta_text"`
}

func (h *AdvertisementHandler) Create(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req createAdRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.EventID == nil || *req.EventID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event is required"})
	}

	var expired bool
	if err := h.DB.QueryRow(context.Background(),
		`SELECT ends_at < NOW() FROM events
		 WHERE id = $1 AND host_id = $2 AND ($3::uuid IS NULL OR assigned_to = $3)`,
		*req.EventID, hostID, staffFilter(c),
	).Scan(&expired); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if expired {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cannot create a flyer for an event that has ended"})
	}

	if req.CTAText == "" {
		req.CTAText = "Get Tickets"
	}

	var adID string
	err := h.DB.QueryRow(context.Background(),
		`INSERT INTO advertisements (host_id, event_id, headline, body, image_url, cta_text)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		hostID, req.EventID, req.Headline, req.Body, req.ImageURL, req.CTAText,
	).Scan(&adID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create advertisement"})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"id":       adID,
		"headline": req.Headline,
		"body":     req.Body,
	})
}

type updateAdRequest struct {
	Headline string `json:"headline"`
	Body     string `json:"body"`
	ImageURL string `json:"image_url"`
	CTAText  string `json:"cta_text"`
}

func (h *AdvertisementHandler) Update(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	adID := c.Params("id")

	var req updateAdRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.CTAText == "" {
		req.CTAText = "Get Tickets"
	}

	result, err := h.DB.Exec(context.Background(),
		`UPDATE advertisements a SET headline = $1, body = $2, image_url = $3, cta_text = $4
		 WHERE a.id = $5 AND a.host_id = $6
		   AND ($7::uuid IS NULL OR EXISTS(SELECT 1 FROM events e WHERE e.id = a.event_id AND e.assigned_to = $7))`,
		req.Headline, req.Body, req.ImageURL, req.CTAText, adID, hostID, staffFilter(c),
	)
	if err != nil || result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ad not found"})
	}

	return c.JSON(fiber.Map{"ok": true})
}

func (h *AdvertisementHandler) ListPublic(c *fiber.Ctx) error {
	rows, err := h.DB.Query(context.Background(),
		`SELECT a.id, a.headline, a.body, a.image_url, a.cta_text,
		        a.event_id, e.title AS event_title, a.created_at
		 FROM advertisements a
		 JOIN events e ON e.id = a.event_id
		 WHERE a.is_active = true
		   AND e.venue_paid = true
		   AND e.is_active = true
		   AND e.ends_at > NOW()
		 ORDER BY a.created_at DESC
		 LIMIT 20`,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch advertisements"})
	}
	defer rows.Close()

	type adRow struct {
		ID         string    `json:"id"`
		Headline   string    `json:"headline"`
		Body       string    `json:"body"`
		ImageURL   string    `json:"image_url"`
		CTAText    string    `json:"cta_text"`
		EventID    *string   `json:"event_id"`
		EventTitle *string   `json:"event_title"`
		CreatedAt  time.Time `json:"created_at"`
	}

	ads := []adRow{}
	for rows.Next() {
		var a adRow
		if err := rows.Scan(
			&a.ID, &a.Headline, &a.Body, &a.ImageURL, &a.CTAText,
			&a.EventID, &a.EventTitle, &a.CreatedAt,
		); err != nil {
			continue
		}
		ads = append(ads, a)
	}

	return c.JSON(fiber.Map{"ads": ads})
}

func (h *AdvertisementHandler) ListByHost(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	rows, err := h.DB.Query(context.Background(),
		`SELECT a.id, a.headline, a.body, a.image_url, a.cta_text,
		        a.event_id, e.title AS event_title, a.is_active, a.created_at, e.ends_at
		 FROM advertisements a
		 LEFT JOIN events e ON e.id = a.event_id
		 WHERE a.host_id = $1 AND ($2::uuid IS NULL OR e.assigned_to = $2)
		 ORDER BY a.created_at DESC`,
		hostID, staffFilter(c),
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch advertisements"})
	}
	defer rows.Close()

	type adRow struct {
		ID           string    `json:"id"`
		Headline     string    `json:"headline"`
		Body         string    `json:"body"`
		ImageURL     string    `json:"image_url"`
		CTAText      string    `json:"cta_text"`
		EventID      *string   `json:"event_id"`
		EventTitle   *string   `json:"event_title"`
		IsActive     bool      `json:"is_active"`
		CreatedAt    time.Time `json:"created_at"`
		EventExpired bool      `json:"event_expired"`
	}

	ads := []adRow{}
	for rows.Next() {
		var a adRow
		var endsAt *time.Time
		if err := rows.Scan(
			&a.ID, &a.Headline, &a.Body, &a.ImageURL, &a.CTAText,
			&a.EventID, &a.EventTitle, &a.IsActive, &a.CreatedAt, &endsAt,
		); err != nil {
			continue
		}
		a.EventExpired = endsAt != nil && time.Now().After(*endsAt)
		ads = append(ads, a)
	}

	return c.JSON(fiber.Map{"ads": ads})
}

func (h *AdvertisementHandler) Delete(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	adID := c.Params("id")

	result, err := h.DB.Exec(context.Background(),
		`DELETE FROM advertisements a
		 WHERE a.id = $1 AND a.host_id = $2
		   AND ($3::uuid IS NULL OR EXISTS(SELECT 1 FROM events e WHERE e.id = a.event_id AND e.assigned_to = $3))`,
		adID, hostID, staffFilter(c),
	)
	if err != nil || result.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "ad not found"})
	}
	return c.JSON(fiber.Map{"ok": true})
}
