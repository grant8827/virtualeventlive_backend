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
		"event_id":          eventID,
		"title":             title,
		"stream_ingest_url": streamIngestURL,
		"stream_key_value":  streamKeyValue,
		"playback_url":      playbackURL,
		"ivs_ready":         streamIngestURL != nil && streamKeyValue != nil,
	})
}

// Reprovision retries IVS channel creation for events whose venue fee was
// paid before AWS credentials existed (or before this env had them), so
// stream_ingest_url/stream_key_value were never set. Provisioning only ever
// runs automatically once, at venue-fee webhook time — this is the manual
// fallback for events that missed that window.
func (h *StreamCredentialsHandler) Reprovision(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	eventID := c.Params("id")

	var (
		title              string
		venuePaid          bool
		existingChannelARN *string
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT title, venue_paid, aws_channel_arn FROM events WHERE id = $1 AND host_id = $2`,
		eventID, hostID,
	).Scan(&title, &venuePaid, &existingChannelARN)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if !venuePaid {
		return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{"error": "venue fee not paid — pay to unlock stream credentials"})
	}
	if existingChannelARN != nil && *existingChannelARN != "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "stream channel is already provisioned for this event"})
	}
	if !h.IVS.Enabled {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "AWS IVS is not configured on the server"})
	}

	creds, err := h.IVS.ProvisionChannel(context.Background(), title)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}

	if _, err := h.DB.Exec(context.Background(),
		`UPDATE events SET
			aws_channel_arn   = $1,
			stream_ingest_url = $2,
			stream_key_value  = $3,
			aws_playback_url  = $4
		 WHERE id = $5`,
		creds.ChannelARN, creds.IngestURL, creds.StreamKey, creds.PlaybackURL, eventID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "provisioned but failed to save credentials — contact support"})
	}

	return c.JSON(fiber.Map{"provisioned": true, "ivs_ready": true})
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
