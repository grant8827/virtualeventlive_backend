package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

const maxImageBytes = 8 << 20

var allowedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
}

type ImageHandler struct {
	DB      *pgxpool.Pool
	Storage *services.S3Storage
}

func eventImageKey(eventID, kind string) string {
	return fmt.Sprintf("events/%s/%s", eventID, kind)
}

func eventImagePath(eventID, kind string) string {
	return fmt.Sprintf("/api/v1/media/events/%s/%s", eventID, kind)
}

func validImageKind(kind string) bool {
	return kind == "ticket" || kind == "flyer" || kind == "logo"
}

func (h *ImageHandler) Upload(c *fiber.Ctx) error {
	if !h.Storage.Enabled {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "S3 image storage is not configured"})
	}

	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID, kind := c.Params("id"), c.Params("kind")
	if !validImageKind(kind) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image kind must be ticket, flyer, or logo"})
	}

	var owned bool
	if err := h.DB.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM events WHERE id = $1 AND host_id = $2 AND ends_at > NOW())`,
		eventID, hostID,
	).Scan(&owned); err != nil || !owned {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found or has ended"})
	}

	fileHeader, err := c.FormFile("image")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image file is required"})
	}
	if fileHeader.Size <= 0 || fileHeader.Size > maxImageBytes {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "image must be 8 MB or smaller"})
	}

	file, err := fileHeader.Open()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unable to read image"})
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unable to read image"})
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "image must be 8 MB or smaller"})
	}
	contentType := http.DetectContentType(data)
	if !allowedImageTypes[contentType] {
		return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "only JPEG, PNG, WebP, and GIF images are allowed"})
	}

	key := eventImageKey(eventID, kind)
	if err := h.Storage.Put(context.Background(), key, contentType, bytes.NewReader(data), int64(len(data))); err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}

	path := eventImagePath(eventID, kind)
	switch kind {
	case "ticket":
		if _, err := h.DB.Exec(context.Background(),
			`UPDATE events SET card_bg_image = $1 WHERE id = $2 AND host_id = $3`,
			path, eventID, hostID,
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "image uploaded but could not be saved"})
		}
	case "logo":
		if _, err := h.DB.Exec(context.Background(),
			`UPDATE events SET logo_image = $1 WHERE id = $2 AND host_id = $3`,
			path, eventID, hostID,
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "image uploaded but could not be saved"})
		}
	default: // flyer
		_, _ = h.DB.Exec(context.Background(),
			`UPDATE advertisements SET image_url = $1 WHERE event_id = $2 AND host_id = $3`,
			path, eventID, hostID,
		)
	}

	return c.JSON(fiber.Map{"image_url": path})
}

func (h *ImageHandler) Get(c *fiber.Ctx) error {
	eventID, kind := c.Params("id"), c.Params("kind")
	if !validImageKind(kind) {
		return c.SendStatus(fiber.StatusNotFound)
	}

	// Stream the object through the API instead of redirecting to a presigned
	// S3 URL. Ticket export loads these images into a canvas; a redirect makes
	// the final response subject to the bucket's CORS policy and prevents both
	// Download and Print when that policy is absent or misconfigured.
	object, err := h.Storage.Get(context.Background(), eventImageKey(eventID, kind))
	if err != nil {
		return c.SendStatus(fiber.StatusNotFound)
	}
	defer object.Body.Close()

	if object.ContentType != nil {
		c.Set(fiber.HeaderContentType, *object.ContentType)
	}
	c.Set(fiber.HeaderCacheControl, "public, max-age=3600")

	// SendStream defaults to an unknown size, which forces
	// Transfer-Encoding: chunked with no Content-Length. Railway's edge
	// proxy intermittently (and for some requests, consistently) fails
	// to relay that shape of response, returning its own 502 fallback
	// page without ever reaching this instance. S3 already tells us the
	// exact size, so give it to SendStream and skip chunked entirely.
	if object.ContentLength != nil {
		return c.SendStream(object.Body, int(*object.ContentLength))
	}
	return c.SendStream(object.Body)
}

func (h *ImageHandler) Delete(c *fiber.Ctx) error {
	hostID, ok := c.Locals("user_id").(string)
	if !ok || hostID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	eventID, kind := c.Params("id"), c.Params("kind")
	if !validImageKind(kind) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "image kind must be ticket, flyer, or logo"})
	}

	var owned bool
	if err := h.DB.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM events WHERE id = $1 AND host_id = $2)`,
		eventID, hostID,
	).Scan(&owned); err != nil || !owned {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}

	if err := h.Storage.Delete(context.Background(), eventImageKey(eventID, kind)); err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}
	switch kind {
	case "ticket":
		_, _ = h.DB.Exec(context.Background(), `UPDATE events SET card_bg_image = '' WHERE id = $1 AND host_id = $2`, eventID, hostID)
	case "logo":
		_, _ = h.DB.Exec(context.Background(), `UPDATE events SET logo_image = '' WHERE id = $1 AND host_id = $2`, eventID, hostID)
	default: // flyer
		_, _ = h.DB.Exec(context.Background(), `UPDATE advertisements SET image_url = '' WHERE event_id = $1 AND host_id = $2`, eventID, hostID)
	}
	return c.JSON(fiber.Map{"ok": true})
}
