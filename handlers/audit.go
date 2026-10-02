package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/middleware"
)

// recordSecurityEvent appends to the security audit log. It never fails the
// request: a logging problem is logged, not surfaced. actorID / targetUserID
// may be empty. details must not contain secrets or plain email addresses.
func recordSecurityEvent(ctx context.Context, db *pgxpool.Pool, c *fiber.Ctx, eventType, actorID, targetUserID string, details map[string]any) {
	if db == nil {
		return
	}
	var ip, ua *string
	if c != nil {
		i := middleware.ClientIP(c)
		u := c.Get(fiber.HeaderUserAgent)
		if len(u) > 300 {
			u = u[:300]
		}
		ip, ua = &i, &u
	}
	if details == nil {
		details = map[string]any{}
	}
	payload, _ := json.Marshal(details)
	if _, err := db.Exec(ctx,
		`INSERT INTO security_events (event_type, actor_user_id, target_user_id, ip, user_agent, details)
		 VALUES ($1, NULLIF($2, '')::uuid, NULLIF($3, '')::uuid, $4, $5, $6)`,
		eventType, actorID, targetUserID, ip, ua, payload,
	); err != nil {
		log.Printf("security audit: could not record %s: %v", eventType, err)
	}
}

// actorOf is the signed-in user behind the request (the real person, not the
// host account a team member acts for).
func actorOf(c *fiber.Ctx) string {
	if id, ok := c.Locals("actor_id").(string); ok && id != "" {
		return id
	}
	id, _ := c.Locals("user_id").(string)
	return id
}

type securityEventRow struct {
	ID           int64           `json:"id"`
	OccurredAt   time.Time       `json:"occurred_at"`
	EventType    string          `json:"event_type"`
	ActorUserID  *string         `json:"actor_user_id"`
	ActorEmail   *string         `json:"actor_email"`
	TargetUserID *string         `json:"target_user_id"`
	TargetEmail  *string         `json:"target_email"`
	IP           *string         `json:"ip"`
	Details      json.RawMessage `json:"details"`
}

// ListSecurityEvents (superuser) returns the most recent audit events,
// optionally filtered by ?type= and ?user= (actor or target).
func (h *SuperuserHandler) ListSecurityEvents(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", "100"))
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var eventType, user *string
	if v := c.Query("type"); v != "" {
		eventType = &v
	}
	if v := c.Query("user"); v != "" {
		user = &v
	}
	rows, err := h.DB.Query(context.Background(),
		`SELECT se.id, se.occurred_at, se.event_type, se.actor_user_id, a.email, se.target_user_id, t.email, se.ip, se.details
		 FROM security_events se
		 LEFT JOIN users a ON a.id = se.actor_user_id
		 LEFT JOIN users t ON t.id = se.target_user_id
		 WHERE ($1::text IS NULL OR se.event_type = $1)
		   AND ($2::uuid IS NULL OR se.actor_user_id = $2 OR se.target_user_id = $2)
		 ORDER BY se.occurred_at DESC, se.id DESC
		 LIMIT $3`, eventType, user, limit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load security events"})
	}
	defer rows.Close()
	out := []securityEventRow{}
	for rows.Next() {
		var r securityEventRow
		if err := rows.Scan(&r.ID, &r.OccurredAt, &r.EventType, &r.ActorUserID, &r.ActorEmail,
			&r.TargetUserID, &r.TargetEmail, &r.IP, &r.Details); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load security events"})
		}
		out = append(out, r)
	}
	return c.JSON(fiber.Map{"events": out})
}
