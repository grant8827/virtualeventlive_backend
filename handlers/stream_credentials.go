package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

type StreamCredentialsHandler struct {
	DB  *pgxpool.Pool
	IVS *services.IVSService
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

// Status is public — ticket holders poll it from the Watch page to know
// whether the host is actually broadcasting right now, since a channel being
// provisioned (aws_channel_arn set at venue-fee time) says nothing about
// whether anyone is live on it at this exact moment.
func (h *StreamCredentialsHandler) Status(c *fiber.Ctx) error {
	eventID := c.Params("id")

	var channelARN *string
	if err := h.DB.QueryRow(context.Background(),
		`SELECT aws_channel_arn FROM events WHERE id = $1`, eventID,
	).Scan(&channelARN); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	if channelARN == nil || *channelARN == "" {
		return c.JSON(fiber.Map{"live": false})
	}

	live, err := h.IVS.IsLive(context.Background(), *channelARN)
	if err != nil {
		return c.JSON(fiber.Map{"live": false})
	}

	return c.JSON(fiber.Map{"live": live})
}
