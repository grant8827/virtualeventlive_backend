package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

type StreamHandler struct {
	DB    *pgxpool.Pool
	Guard *services.SessionGuard
}

type streamRequest struct {
	TicketToken       string `json:"ticket_token"`
	DeviceFingerprint string `json:"device_fingerprint"`
}

// Watch validates the ticket, runs the Redis session lock, and returns the HLS playback URL.
func (h *StreamHandler) Watch(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req streamRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.TicketToken == "" || req.DeviceFingerprint == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "ticket_token and device_fingerprint are required",
		})
	}

	// Verify ticket belongs to this buyer and fetch the event's playback URL
	var playbackURL *string
	err := h.DB.QueryRow(context.Background(),
		`SELECT e.aws_playback_url
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 WHERE t.access_token = $1
		   AND t.buyer_id     = $2
		   AND e.is_active    = true`,
		req.TicketToken, buyerID,
	).Scan(&playbackURL)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "ticket not found or event is no longer active",
		})
	}

	if playbackURL == nil || *playbackURL == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "stream is not live yet — no playback URL assigned",
		})
	}

	result, err := h.Guard.EvaluateAndLock(req.TicketToken, req.DeviceFingerprint)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "session check failed"})
	}

	if result == services.LockConflict {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"status": "forbidden",
			"error":  "This ticket pass is currently active on another device.",
		})
	}

	return c.JSON(fiber.Map{
		"status":           "authorized",
		"playback_url":     *playbackURL,
		"lease_expires_in": int(services.LeaseTTL.Seconds()),
	})
}

// Heartbeat extends the Redis lease every 30 seconds while the player is active.
func (h *StreamHandler) Heartbeat(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req streamRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.TicketToken == "" || req.DeviceFingerprint == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "ticket_token and device_fingerprint are required",
		})
	}

	// Lightweight ownership check
	var exists bool
	_ = h.DB.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM tickets WHERE access_token = $1 AND buyer_id = $2)`,
		req.TicketToken, buyerID,
	).Scan(&exists)
	if !exists {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "invalid ticket"})
	}

	result, err := h.Guard.EvaluateAndLock(req.TicketToken, req.DeviceFingerprint)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "session check failed"})
	}

	if result == services.LockConflict {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"status": "forbidden",
			"error":  "Session has been claimed by another device.",
		})
	}

	return c.JSON(fiber.Map{
		"status":           "renewed",
		"lease_expires_in": int(services.LeaseTTL.Seconds()),
	})
}

// Release explicitly drops the Redis lock so the viewer can switch devices without the 45s wait.
func (h *StreamHandler) Release(c *fiber.Ctx) error {
	buyerID, ok := c.Locals("user_id").(string)
	if !ok || buyerID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	var req streamRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	var exists bool
	_ = h.DB.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM tickets WHERE access_token = $1 AND buyer_id = $2)`,
		req.TicketToken, buyerID,
	).Scan(&exists)
	if !exists {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "invalid ticket"})
	}

	_ = h.Guard.Release(req.TicketToken)
	return c.JSON(fiber.Map{"status": "released"})
}
