package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type StreamCredentialsHandler struct {
	DB *pgxpool.Pool
}

func (h *StreamCredentialsHandler) Get(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	eventID := c.Params("id")

	var (
		streamIngestURL *string
		streamKeyValue  *string
		playbackURL     *string
		venuePaid       bool
		title           string
		endsAt          time.Time
	)

	err := h.DB.QueryRow(context.Background(),
		`SELECT title, stream_ingest_url, stream_key_value, aws_playback_url, venue_paid, ends_at
		 FROM events WHERE id = $1 AND host_id = $2`,
		eventID, hostID,
	).Scan(&title, &streamIngestURL, &streamKeyValue, &playbackURL, &venuePaid, &endsAt)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	if !venuePaid {
		return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{
			"error": "venue fee not paid — pay to unlock stream credentials",
		})
	}
	if time.Now().After(endsAt) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "This event has ended — stream credentials are no longer available.",
		})
	}

	return c.JSON(fiber.Map{
		"event_id":         eventID,
		"title":            title,
		"stream_ingest_url": streamIngestURL,
		"stream_key_value": streamKeyValue,
		"playback_url":     playbackURL,
		"ivs_ready":        streamIngestURL != nil && streamKeyValue != nil,
	})
}
