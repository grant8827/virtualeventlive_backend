package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/services"
)

// SuperuserHandler backs the platform dashboard: analytics across every host,
// and control over every user and event. Every route runs behind
// middleware.RequireSuperuser.
type SuperuserHandler struct {
	DB    *pgxpool.Pool
	IVS   *services.IVSService
	Email *services.EmailService
}

const superuserPageSize = 25

func pageOffset(c *fiber.Ctx) (page, offset int) {
	page, _ = strconv.Atoi(c.Query("page", "1"))
	if page < 1 {
		page = 1
	}
	return page, (page - 1) * superuserPageSize
}

// likePattern turns a search box value into an ILIKE pattern, or nil for "no
// filter", escaping the wildcard characters people might type.
func likePattern(q string) *string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	q = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	p := "%" + q + "%"
	return &p
}

// ─── Overview ───────────────────────────────────────────────────────────────

type dailyPoint struct {
	Date    string  `json:"date"`
	Tickets int     `json:"tickets"`
	Gross   float64 `json:"gross"`
	Signups int     `json:"signups"`
}

type topHost struct {
	ID      string  `json:"id"`
	Name    *string `json:"name"`
	Email   string  `json:"email"`
	Events  int     `json:"events"`
	Tickets int     `json:"tickets"`
	Gross   float64 `json:"gross"`
}

type liveEvent struct {
	EventID   string     `json:"event_id"`
	Title     string     `json:"title"`
	HostName  *string    `json:"host_name"`
	HostEmail string     `json:"host_email"`
	Viewers   int64      `json:"viewers"`
	StartedAt *time.Time `json:"started_at"`
	Health    string     `json:"health"`
}

func (h *SuperuserHandler) Overview(c *fiber.Ctx) error {
	ctx := context.Background()
	fail := func() error {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load analytics"})
	}

	// Only registered accounts count here. Staff/admins belong to their
	// host's team and are managed by that host, not by superusers.
	var users struct {
		Total     int `json:"total"`
		Buyers    int `json:"buyers"`
		Hosts     int `json:"hosts"`
		Suspended int `json:"suspended"`
		Pending   int `json:"pending"`
		New30d    int `json:"new_30d"`
	}
	if err := h.DB.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE role = 'buyer'),
		        count(*) FILTER (WHERE role = 'host'),
		        count(*) FILTER (WHERE status = 'suspended'),
		        count(*) FILTER (WHERE status = 'pending'),
		        count(*) FILTER (WHERE created_at > NOW() - INTERVAL '30 days')
		 FROM users WHERE account_owner_id IS NULL AND role <> 'superuser'`,
	).Scan(&users.Total, &users.Buyers, &users.Hosts, &users.Suspended, &users.Pending, &users.New30d); err != nil {
		return fail()
	}

	var events struct {
		Total    int `json:"total"`
		Upcoming int `json:"upcoming"`
		Ended    int `json:"ended"`
		Unpaid   int `json:"unpaid"`
		LiveNow  int `json:"live_now"`
	}
	if err := h.DB.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE venue_paid AND ends_at > NOW()),
		        count(*) FILTER (WHERE ends_at <= NOW()),
		        count(*) FILTER (WHERE NOT venue_paid AND ends_at > NOW())
		 FROM events`,
	).Scan(&events.Total, &events.Upcoming, &events.Ended, &events.Unpaid); err != nil {
		return fail()
	}

	var tickets struct {
		Total  int `json:"total"`
		Last30 int `json:"last_30d"`
	}
	if err := h.DB.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE purchased_at > NOW() - INTERVAL '30 days') FROM tickets`,
	).Scan(&tickets.Total, &tickets.Last30); err != nil {
		return fail()
	}

	// Ticket money comes from the ledger (paid sales only); venue fees from
	// events whose fee was really paid, not bypassed.
	var revenue struct {
		GrossSales     float64 `json:"gross_sales"`
		PlatformFees   float64 `json:"platform_fees"`
		ProcessingFees float64 `json:"processing_fees"`
		HostPayouts    float64 `json:"host_payouts"`
		PendingPayouts float64 `json:"pending_payouts"`
		VenueFees      float64 `json:"venue_fees"`
	}
	if err := h.DB.QueryRow(ctx,
		`SELECT COALESCE(SUM(gross_amount), 0)::float8,
		        COALESCE(SUM(platform_fee), 0)::float8,
		        COALESCE(SUM(stripe_fee), 0)::float8,
		        COALESCE(SUM(host_payout), 0)::float8,
		        COALESCE(SUM(host_payout) FILTER (WHERE payout_status = 'pending'), 0)::float8,
		        (SELECT COALESCE(SUM(venue_fee), 0)::float8 FROM events WHERE venue_paid AND NOT venue_bypassed)
		 FROM ledger_entries`,
	).Scan(&revenue.GrossSales, &revenue.PlatformFees, &revenue.ProcessingFees,
		&revenue.HostPayouts, &revenue.PendingPayouts, &revenue.VenueFees); err != nil {
		return fail()
	}

	// Last 30 days, one row per day including empty days.
	rows, err := h.DB.Query(ctx,
		`WITH days AS (
			SELECT generate_series(CURRENT_DATE - 29, CURRENT_DATE, INTERVAL '1 day')::date AS d
		 )
		 SELECT to_char(days.d, 'YYYY-MM-DD'),
		        (SELECT count(*) FROM tickets t WHERE t.purchased_at::date = days.d),
		        (SELECT COALESCE(SUM(gross_amount), 0)::float8 FROM ledger_entries l WHERE l.settled_at::date = days.d),
		        (SELECT count(*) FROM users u WHERE u.created_at::date = days.d)
		 FROM days ORDER BY days.d`,
	)
	if err != nil {
		return fail()
	}
	daily := []dailyPoint{}
	for rows.Next() {
		var p dailyPoint
		if err := rows.Scan(&p.Date, &p.Tickets, &p.Gross, &p.Signups); err != nil {
			rows.Close()
			return fail()
		}
		daily = append(daily, p)
	}
	rows.Close()

	rows, err = h.DB.Query(ctx,
		`SELECT u.id, u.full_name, u.email,
		        (SELECT count(*) FROM events e WHERE e.host_id = u.id),
		        (SELECT count(*) FROM tickets t JOIN events e ON e.id = t.event_id WHERE e.host_id = u.id),
		        (SELECT COALESCE(SUM(l.gross_amount), 0)::float8 FROM ledger_entries l
		           JOIN events e ON e.id = l.event_id WHERE e.host_id = u.id) AS gross
		 FROM users u WHERE u.role = 'host'
		 ORDER BY gross DESC, 5 DESC LIMIT 5`,
	)
	if err != nil {
		return fail()
	}
	hosts := []topHost{}
	for rows.Next() {
		var t topHost
		if err := rows.Scan(&t.ID, &t.Name, &t.Email, &t.Events, &t.Tickets, &t.Gross); err != nil {
			rows.Close()
			return fail()
		}
		hosts = append(hosts, t)
	}
	rows.Close()

	live, liveErr := h.liveEvents(ctx)
	events.LiveNow = len(live)

	resp := fiber.Map{
		"users": users, "events": events, "tickets": tickets, "revenue": revenue,
		"daily": daily, "top_hosts": hosts, "live": live,
		"ivs_enabled": h.IVS != nil && h.IVS.Enabled,
	}
	if liveErr != nil {
		resp["live_error"] = "could not reach the streaming service"
	}
	return c.JSON(resp)
}

// liveStreams returns what's on air, keyed by channel ARN. It asks IVS for
// every live stream in one call; if the AWS key isn't allowed ivs:ListStreams
// it falls back to checking, one by one, the events scheduled around now.
func (h *SuperuserHandler) liveStreams(ctx context.Context) (map[string]services.LiveStream, error) {
	if h.IVS == nil || !h.IVS.Enabled {
		return map[string]services.LiveStream{}, nil
	}
	if streams, err := h.IVS.ListLiveStreams(ctx); err == nil {
		return streams, nil
	}

	rows, err := h.DB.Query(ctx,
		`SELECT aws_channel_arn FROM events
		 WHERE aws_channel_arn IS NOT NULL AND aws_channel_arn <> '' AND venue_paid
		   AND start_time - INTERVAL '2 hours' <= NOW() AND ends_at + INTERVAL '2 hours' >= NOW()`,
	)
	if err != nil {
		return nil, err
	}
	var arns []string
	for rows.Next() {
		var arn string
		if err := rows.Scan(&arn); err == nil {
			arns = append(arns, arn)
		}
	}
	rows.Close()

	streams := map[string]services.LiveStream{}
	for _, arn := range arns {
		live, viewers, err := h.IVS.StreamState(ctx, arn)
		if err != nil {
			return streams, err
		}
		if live {
			streams[arn] = services.LiveStream{ViewerCount: viewers}
		}
	}
	return streams, nil
}

// liveEvents matches the on-air streams to events on the platform.
func (h *SuperuserHandler) liveEvents(ctx context.Context) ([]liveEvent, error) {
	out := []liveEvent{}
	streams, err := h.liveStreams(ctx)
	if err != nil || len(streams) == 0 {
		return out, err
	}
	arns := make([]string, 0, len(streams))
	for arn := range streams {
		arns = append(arns, arn)
	}
	rows, err := h.DB.Query(ctx,
		`SELECT e.id, e.title, e.aws_channel_arn, u.full_name, u.email
		 FROM events e JOIN users u ON u.id = e.host_id
		 WHERE e.aws_channel_arn = ANY($1)`, arns,
	)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ev  liveEvent
			arn string
		)
		if err := rows.Scan(&ev.EventID, &ev.Title, &arn, &ev.HostName, &ev.HostEmail); err != nil {
			return out, err
		}
		st := streams[arn]
		ev.Viewers, ev.StartedAt, ev.Health = st.ViewerCount, st.StartedAt, st.Health
		out = append(out, ev)
	}
	return out, nil
}

// ─── Users ──────────────────────────────────────────────────────────────────

type platformUser struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	FullName         *string   `json:"full_name"`
	OrganizationName *string   `json:"organization_name"`
	Role             string    `json:"role"`
	Status           string    `json:"status"`
	OwnerEmail       *string   `json:"owner_email"`
	Events           int       `json:"events"`
	Tickets          int       `json:"tickets"`
	Gross            float64   `json:"gross"`
	CreatedAt        time.Time `json:"created_at"`
}

const platformUserSelect = `
	SELECT u.id, u.email, u.full_name, u.organization_name, u.role, u.status, o.email,
	       (SELECT count(*) FROM events e WHERE e.host_id = u.id),
	       (SELECT count(*) FROM tickets t JOIN events e ON e.id = t.event_id WHERE e.host_id = u.id),
	       (SELECT COALESCE(SUM(l.gross_amount), 0)::float8 FROM ledger_entries l
	          JOIN events e ON e.id = l.event_id WHERE e.host_id = u.id),
	       u.created_at
	FROM users u LEFT JOIN users o ON o.id = u.account_owner_id`

func scanPlatformUser(row interface{ Scan(...any) error }) (platformUser, error) {
	var u platformUser
	err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.OrganizationName, &u.Role, &u.Status, &u.OwnerEmail,
		&u.Events, &u.Tickets, &u.Gross, &u.CreatedAt)
	return u, err
}

// ListUsers searches the accounts people registered themselves — not the
// staff/admins hosts add to their teams, and not superusers. Filters: q
// (name/email/organization), role (buyer, host) and status (pending, active,
// suspended, or approved = anything but pending). Accounts waiting for
// approval are listed first.
func (h *SuperuserHandler) ListUsers(c *fiber.Ctx) error {
	page, offset := pageOffset(c)

	var roles []string
	switch c.Query("role") {
	case "buyer", "host":
		roles = []string{c.Query("role")}
	}
	var status *string
	if s := c.Query("status"); s == "pending" || s == "active" || s == "suspended" || s == "approved" {
		status = &s
	}

	where := `
	WHERE u.account_owner_id IS NULL AND u.role <> 'superuser'
	  AND ($1::text IS NULL OR u.email ILIKE $1 OR u.full_name ILIKE $1 OR u.organization_name ILIKE $1)
	  AND ($2::text[] IS NULL OR u.role = ANY($2))
	  AND ($3::text IS NULL OR u.status = $3 OR ($3 = 'approved' AND u.status <> 'pending'))`
	args := []any{likePattern(c.Query("q")), roles, status}

	var total int
	if err := h.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM users u`+where, args...,
	).Scan(&total); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
	}

	rows, err := h.DB.Query(context.Background(),
		platformUserSelect+where+` ORDER BY (u.status = 'pending') DESC, u.created_at DESC LIMIT $4 OFFSET $5`,
		append(args, superuserPageSize, offset)...,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
	}
	defer rows.Close()

	users := []platformUser{}
	for rows.Next() {
		u, err := scanPlatformUser(rows)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load users"})
		}
		users = append(users, u)
	}
	return c.JSON(fiber.Map{"users": users, "total": total, "page": page, "page_size": superuserPageSize})
}

type updatePlatformUserRequest struct {
	Status   *string `json:"status"`
	Role     *string `json:"role"`
	Password *string `json:"password"`
}

// UpdateUser approves, suspends or reactivates a registered account,
// switches it between buyer and host, or sets a new password. Superusers only
// control registered accounts: staff/admins are managed by their host on the
// host's Add User page, and other superusers from the CLI.
func (h *SuperuserHandler) UpdateUser(c *fiber.Ctx) error {
	actorID, _ := c.Locals("user_id").(string)
	targetID := c.Params("id")
	if targetID == actorID {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "you cannot change your own account here"})
	}

	var req updatePlatformUserRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	var currentRole, currentStatus, email string
	if err := h.DB.QueryRow(context.Background(),
		`SELECT role, status, email FROM users WHERE id = $1 AND account_owner_id IS NULL`, targetID,
	).Scan(&currentRole, &currentStatus, &email); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user not found"})
	}
	if currentRole == "superuser" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "superusers are managed from the command line"})
	}

	if req.Status != nil && *req.Status != "active" && *req.Status != "suspended" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "status must be active or suspended"})
	}
	if req.Role != nil {
		if *req.Role != "buyer" && *req.Role != "host" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "role must be buyer or host"})
		}
	}

	var passwordHash *string
	if req.Password != nil && *req.Password != "" {
		if len(*req.Password) < 8 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "password must be at least 8 characters"})
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to hash password"})
		}
		hashed := string(hash)
		passwordHash = &hashed
	}

	if _, err := h.DB.Exec(context.Background(),
		`UPDATE users SET status = COALESCE($1, status), role = COALESCE($2, role),
		        password_hash = COALESCE($3, password_hash), updated_at = NOW()
		 WHERE id = $4`,
		req.Status, req.Role, passwordHash, targetID,
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update user"})
	}

	// Approving a newly registered host: let them know they can sign in.
	if currentStatus == "pending" && req.Status != nil && *req.Status == "active" && h.Email != nil {
		if err := h.Email.SendAccountApproved(email); err != nil {
			fmt.Printf("approval email to %s failed: %v\n", email, err)
		}
	}

	u, err := scanPlatformUser(h.DB.QueryRow(context.Background(), platformUserSelect+` WHERE u.id = $1`, targetID))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load user"})
	}
	return c.JSON(u)
}

// RejectUser removes an account that registered but was never approved. It
// only works while the account is pending, so no events, tickets or money
// can be attached to it.
func (h *SuperuserHandler) RejectUser(c *fiber.Ctx) error {
	ctx := context.Background()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reject user"})
	}
	defer tx.Rollback(ctx)

	// Drop their used invitation too, so the list doesn't show them as
	// registered and the email can be invited again later.
	if _, err := tx.Exec(ctx,
		`DELETE FROM invitations WHERE user_id = $1
		   AND EXISTS(SELECT 1 FROM users WHERE id = $1 AND status = 'pending')`, c.Params("id"),
	); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reject user"})
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM users WHERE id = $1 AND status = 'pending' AND account_owner_id IS NULL`, c.Params("id"),
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reject user"})
	}
	if tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no pending account with that id"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to reject user"})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ─── Events ─────────────────────────────────────────────────────────────────

type platformEvent struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	EventType     string     `json:"event_type"`
	StartsAt      time.Time  `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at"`
	HostID        string     `json:"host_id"`
	HostName      *string    `json:"host_name"`
	HostEmail     string     `json:"host_email"`
	VenueFee      float64    `json:"venue_fee"`
	VenuePaid     bool       `json:"venue_paid"`
	VenueBypassed bool       `json:"venue_bypassed"`
	IsActive      bool       `json:"is_active"`
	Expired       bool       `json:"expired"`
	Cancelled     bool       `json:"cancelled"`
	Tickets       int        `json:"tickets"`
	Gross         float64    `json:"gross"`
	PlatformFee   float64    `json:"platform_fee"`
	Live          bool       `json:"live"`
	Viewers       int64      `json:"viewers"`
	channelARN    *string
}

const platformEventSelect = `
	SELECT e.id, e.title, e.event_type, e.start_time, e.ends_at, e.host_id, u.full_name, u.email,
	       e.venue_fee::float8, e.venue_paid, e.venue_bypassed, e.is_active, (e.ends_at < NOW()),
	       (e.cancelled_at IS NOT NULL),
	       (SELECT count(*) FROM tickets t WHERE t.event_id = e.id),
	       (SELECT COALESCE(SUM(l.gross_amount), 0)::float8 FROM ledger_entries l WHERE l.event_id = e.id),
	       (SELECT COALESCE(SUM(l.platform_fee), 0)::float8 FROM ledger_entries l WHERE l.event_id = e.id),
	       e.aws_channel_arn
	FROM events e JOIN users u ON u.id = e.host_id`

func scanPlatformEvent(row interface{ Scan(...any) error }) (platformEvent, error) {
	var e platformEvent
	err := row.Scan(&e.ID, &e.Title, &e.EventType, &e.StartsAt, &e.EndsAt, &e.HostID, &e.HostName, &e.HostEmail,
		&e.VenueFee, &e.VenuePaid, &e.VenueBypassed, &e.IsActive, &e.Expired, &e.Cancelled,
		&e.Tickets, &e.Gross, &e.PlatformFee, &e.channelARN)
	return e, err
}

// ListEvents lists every event. Filters: q (title/host) and status
// (upcoming, unpaid, disabled, ended, cancelled).
func (h *SuperuserHandler) ListEvents(c *fiber.Ctx) error {
	ctx := context.Background()
	page, offset := pageOffset(c)

	statusFilter := "TRUE"
	switch c.Query("status") {
	case "upcoming":
		statusFilter = "e.venue_paid AND e.is_active AND e.ends_at > NOW()"
	case "unpaid":
		statusFilter = "NOT e.venue_paid AND e.ends_at > NOW()"
	case "disabled":
		statusFilter = "NOT e.is_active AND e.venue_paid AND e.ends_at > NOW()"
	case "ended":
		statusFilter = "e.ends_at <= NOW() AND e.cancelled_at IS NULL"
	case "cancelled":
		statusFilter = "e.cancelled_at IS NOT NULL"
	}
	where := `
	WHERE ($1::text IS NULL OR e.title ILIKE $1 OR u.email ILIKE $1 OR u.full_name ILIKE $1)
	  AND ` + statusFilter
	q := likePattern(c.Query("q"))

	var total int
	if err := h.DB.QueryRow(ctx,
		`SELECT count(*) FROM events e JOIN users u ON u.id = e.host_id`+where, q,
	).Scan(&total); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load events"})
	}

	rows, err := h.DB.Query(ctx,
		platformEventSelect+where+` ORDER BY e.start_time DESC LIMIT $2 OFFSET $3`,
		q, superuserPageSize, offset,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load events"})
	}
	defer rows.Close()

	events := []platformEvent{}
	for rows.Next() {
		e, err := scanPlatformEvent(rows)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load events"})
		}
		events = append(events, e)
	}

	// Mark which of these are on air right now.
	if streams, err := h.liveStreams(ctx); err == nil {
		for i := range events {
			if arn := events[i].channelARN; arn != nil {
				if st, ok := streams[*arn]; ok {
					events[i].Live, events[i].Viewers = true, st.ViewerCount
				}
			}
		}
	}
	return c.JSON(fiber.Map{"events": events, "total": total, "page": page, "page_size": superuserPageSize})
}

// ActivateEvent turns on an event whose venue fee hasn't been paid — for
// comps and support cases. It is recorded as bypassed so it doesn't count as
// venue-fee revenue.
func (h *SuperuserHandler) ActivateEvent(c *fiber.Ctx) error {
	ctx := context.Background()
	eventID := c.Params("id")

	var paid, over bool
	if err := h.DB.QueryRow(ctx,
		`SELECT venue_paid, (ends_at <= NOW() OR cancelled_at IS NOT NULL) FROM events WHERE id = $1`, eventID,
	).Scan(&paid, &over); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if paid {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event is already activated"})
	}
	if over {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event has ended or was cancelled"})
	}
	if err := activateVenuePaidEvent(ctx, h.DB, h.IVS, eventID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to activate event"})
	}
	if _, err := h.DB.Exec(ctx, `UPDATE events SET venue_bypassed = true WHERE id = $1`, eventID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to activate event"})
	}
	return h.respondEvent(c, eventID)
}

type updatePlatformEventRequest struct {
	IsActive *bool `json:"is_active"`
}

// UpdateEvent disables or re-enables a paid event. A disabled event drops off
// the public listings and stops selling tickets; nothing is deleted.
func (h *SuperuserHandler) UpdateEvent(c *fiber.Ctx) error {
	var req updatePlatformEventRequest
	if err := c.BodyParser(&req); err != nil || req.IsActive == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "is_active is required"})
	}
	tag, err := h.DB.Exec(context.Background(),
		`UPDATE events SET is_active = $1
		 WHERE id = $2 AND venue_paid AND cancelled_at IS NULL AND ends_at > NOW()`, *req.IsActive, c.Params("id"),
	)
	if err != nil || tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "only paid, upcoming events can be disabled or enabled"})
	}
	return h.respondEvent(c, c.Params("id"))
}

// CancelEvent ends an event for good: it stops selling tickets, drops off
// the public listings and its flyers are removed. Like a host deleting their
// event, tickets and payment records are kept; refunds are not automatic.
func (h *SuperuserHandler) CancelEvent(c *fiber.Ctx) error {
	ctx := context.Background()
	eventID := c.Params("id")
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to cancel event"})
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE events SET is_active = false, cancelled_at = NOW(), ends_at = LEAST(ends_at, NOW())
		 WHERE id = $1 AND cancelled_at IS NULL AND ends_at > NOW()`, eventID,
	)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to cancel event"})
	}
	if tag.RowsAffected() == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event not found, already ended or already cancelled"})
	}
	if _, err := tx.Exec(ctx, `DELETE FROM advertisements WHERE event_id = $1`, eventID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to cancel event"})
	}
	if err := tx.Commit(ctx); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to cancel event"})
	}
	return h.respondEvent(c, eventID)
}

func (h *SuperuserHandler) respondEvent(c *fiber.Ctx, eventID string) error {
	e, err := scanPlatformEvent(h.DB.QueryRow(context.Background(), platformEventSelect+` WHERE e.id = $1`, eventID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load event"})
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	return c.JSON(e)
}
