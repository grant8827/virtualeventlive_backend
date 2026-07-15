package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type HealthHandler struct {
	DB  *pgxpool.Pool
	RDB *redis.Client
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

	httpStatus := fiber.StatusOK
	if dbStatus != "ok" || redisStatus != "ok" {
		httpStatus = fiber.StatusServiceUnavailable
	}

	return c.Status(httpStatus).JSON(fiber.Map{
		"status":    "vertualeventlive api",
		"postgres":  dbStatus,
		"redis":     redisStatus,
		"timestamp": time.Now().UTC(),
	})
}
