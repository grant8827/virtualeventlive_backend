package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"vertualeventlive/backend/services"
)

type HealthHandler struct {
	DB         *pgxpool.Pool
	RDB        *redis.Client
	IVSEnabled bool
	S3Enabled  bool
	S3         *services.S3Storage
	S3Missing  []string
}

func (h *HealthHandler) Check(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dbStatus := "ok"
	if err := h.DB.Ping(ctx); err != nil {
		dbStatus = "error: " + err.Error()
	}

	redisStatus := "ok"
	if err := h.RDB.Ping(ctx).Err(); err != nil {
		redisStatus = "error: " + err.Error()
	}

	s3Status := "not configured"
	if h.S3 != nil && h.S3Enabled {
		s3Status = "ok"
		if err := h.S3.Check(ctx); err != nil {
			s3Status = "error: " + err.Error()
		}
	}

	httpStatus := fiber.StatusOK
	if dbStatus != "ok" || redisStatus != "ok" {
		httpStatus = fiber.StatusServiceUnavailable
	}

	return c.Status(httpStatus).JSON(fiber.Map{
		"status":         "vertualeventlive api",
		"postgres":       dbStatus,
		"redis":          redisStatus,
		"ivs_enabled":    h.IVSEnabled,
		"s3_enabled":     h.S3Enabled,
		"s3":             s3Status,
		"s3_missing_env": h.S3Missing,
		"timestamp":      time.Now().UTC(),
	})
}
