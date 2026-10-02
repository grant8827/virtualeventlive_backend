package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"

	"vertualeventlive/backend/services"
)

// Ticket retrieval without accounts, done safely (TKT-01 / UX-03):
//
//   - After a purchase the buyer is sent to /ticket-success?claim=<token>. The
//     token is random, single-purchase and expires after ticketClaimTTL; it
//     reveals exactly the ticket from that purchase and nothing else. No email
//     address or other personal data goes in the URL.
//   - "Find my tickets" no longer returns tickets to whoever asks. It emails
//     them to the address, so only the inbox owner sees the access codes, and
//     it answers identically whether or not the address has tickets.

const ticketClaimTTL = 2 * time.Hour

// execer is satisfied by both *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// claimTarget says what a claim will resolve to. Exactly one is set: the
// ticket itself (free tickets, issued immediately) or the payment whose
// fulfilment will attach the ticket later.
type claimTarget struct {
	TicketID        *string
	StripeSessionID *string
	PayPalOrderID   *string
}

// newClaimToken returns a random claim token and the hash stored for it.
// Same construction as invitation tokens: 32 random bytes, SHA-256 at rest.
func newClaimToken() (token, hash string, err error) {
	return newInvitationToken()
}

func insertTicketClaim(ctx context.Context, db execer, hash, eventID string, target claimTarget) error {
	_, err := db.Exec(ctx,
		`INSERT INTO ticket_claims (token_hash, event_id, ticket_id, stripe_session_id, paypal_order_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, NOW() + make_interval(secs => $6))`,
		hash, eventID, target.TicketID, target.StripeSessionID, target.PayPalOrderID,
		ticketClaimTTL.Seconds(),
	)
	return err
}

// createTicketClaim makes a claim for an already-issued ticket and returns
// its token.
func createTicketClaim(ctx context.Context, db execer, eventID, ticketID string) (string, error) {
	token, hash, err := newClaimToken()
	if err != nil {
		return "", err
	}
	if err := insertTicketClaim(ctx, db, hash, eventID, claimTarget{TicketID: &ticketID}); err != nil {
		return "", err
	}
	return token, nil
}

// Claim exchanges a claim token for its ticket. 202 means the payment is
// still being processed (the page polls); 404/410 mean the link is invalid
// or expired.
func (h *TicketHandler) Claim(c *fiber.Ctx) error {
	token := strings.TrimSpace(c.Params("token"))
	if token == "" {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "This ticket link isn't valid."})
	}
	var (
		ticketID  *string
		expiresAt time.Time
	)
	err := h.DB.QueryRow(context.Background(),
		`SELECT ticket_id, expires_at FROM ticket_claims WHERE token_hash = $1`, hashInvitationToken(token),
	).Scan(&ticketID, &expiresAt)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "This ticket link isn't valid."})
	}
	if time.Now().After(expiresAt) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "This ticket link has expired. Your ticket is in your email, or use Find My Tickets."})
	}
	if ticketID == nil {
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"status": "pending"})
	}
	rows, err := h.loadTicketRows(context.Background(), `t.id = $1`, *ticketID)
	if err != nil || len(rows) == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "This ticket link isn't valid."})
	}
	return c.JSON(fiber.Map{"status": "ready", "tickets": rows})
}

// loadTicketRows returns tickets (with their event details) matching where.
func (h *TicketHandler) loadTicketRows(ctx context.Context, where string, args ...any) ([]ticketRow, error) {
	rows, err := h.DB.Query(ctx,
		`SELECT t.id, t.access_token, t.serial_no, t.purchased_at,
		        t.checked_in_at, t.checked_in_channel,
		        e.id AS event_id, e.title, e.start_time, e.ticket_type, e.ticket_price,
		        e.card_bg_from, e.card_bg_to, e.card_bg_image, e.logo_image, e.venue_address,
		        e.is_active, e.ends_at
		 FROM tickets t
		 JOIN events e ON e.id = t.event_id
		 LEFT JOIN users u ON u.id = t.buyer_id
		 WHERE `+where+`
		 ORDER BY t.purchased_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ticketRow{}
	for rows.Next() {
		var t ticketRow
		var endsAt time.Time
		if err := rows.Scan(&t.ID, &t.AccessToken, &t.SerialNo, &t.PurchasedAt,
			&t.CheckedInAt, &t.CheckedInChannel,
			&t.EventID, &t.EventTitle, &t.EventStartsAt, &t.TicketType, &t.TicketPrice,
			&t.CardBgFrom, &t.CardBgTo, &t.CardBgImage, &t.LogoImage, &t.VenueAddress,
			&t.EventIsActive, &endsAt); err != nil {
			return nil, err
		}
		t.EventExpired = time.Now().After(endsAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ─── Find my tickets (by email) ─────────────────────────────────────────────

const lookupGenericMessage = "If there are tickets for that email address, we've emailed them. Check your inbox (and spam folder)."

type ticketLookupRequest struct {
	Email string `json:"email"`
}

// LookupByEmail emails a buyer their upcoming tickets. It never returns
// tickets or says whether any exist.
func (h *TicketHandler) LookupByEmail(c *fiber.Ctx) error {
	var req ticketLookupRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a valid email address"})
	}

	respond := func() error {
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"message": lookupGenericMessage})
	}
	// Limits: one email per address per window, and a cap per client IP, so
	// the endpoint can't be used to flood someone's inbox. Same response
	// either way.
	if !lookupLimiter.allow("ip:"+c.IP(), lookupPerIP, lookupWindow) ||
		!lookupLimiter.allow("email:"+hashEmail(email), 1, lookupEmailEvery) {
		return respond()
	}
	h.emailTicketList(email)
	return respond()
}

// LookupGone answers the retired GET /tickets/lookup?email= endpoint, which
// returned tickets and access codes to anyone who knew an email address.
func (h *TicketHandler) LookupGone(c *fiber.Ctx) error {
	return c.Status(fiber.StatusGone).JSON(fiber.Map{
		"error": "Ticket lookup now emails your tickets to you. Please update the app or refresh the page.",
	})
}

// emailTicketList sends the address its tickets for events that haven't
// ended, in the background. Nothing is sent if there are none.
func (h *TicketHandler) emailTicketList(email string) {
	if h.Email == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rows, err := h.DB.Query(ctx,
			`SELECT t.access_token, t.serial_no, e.id, e.title, e.start_time, e.ticket_name, e.ticket_type, e.venue_address
			 FROM tickets t
			 JOIN events e ON e.id = t.event_id
			 LEFT JOIN users u ON u.id = t.buyer_id
			 WHERE (lower(u.email) = $1 OR lower(t.guest_email) = $1) AND e.ends_at > NOW()
			 ORDER BY e.start_time`, email)
		if err != nil {
			log.Printf("ticket lookup email: query failed: %v", err)
			return
		}
		defer rows.Close()
		var tickets []services.TicketEmail
		for rows.Next() {
			var (
				t     services.TicketEmail
				venue *string
			)
			if err := rows.Scan(&t.AccessCode, &t.SerialNo, &t.EventID, &t.EventTitle, &t.StartsAt, &t.TicketName, &t.TicketType, &venue); err != nil {
				log.Printf("ticket lookup email: scan failed: %v", err)
				return
			}
			if venue != nil {
				t.VenueAddress = *venue
			}
			tickets = append(tickets, t)
		}
		if len(tickets) == 0 {
			return
		}
		if err := h.Email.SendTicketList(email, tickets); err != nil {
			log.Printf("ticket lookup email failed: %v", err)
		}
	}()
}

func hashEmail(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

// ─── Rate limiting ──────────────────────────────────────────────────────────

const (
	lookupWindow     = 15 * time.Minute
	lookupPerIP      = 10
	lookupEmailEvery = 2 * time.Minute
)

var lookupLimiter = newKeyedLimiter()

// keyedLimiter is a small in-memory fixed-window limiter. It is per server
// instance, which is enough to stop casual abuse in Phase 0.
type keyedLimiter struct {
	mu      sync.Mutex
	windows map[string]*limitWindow
}

type limitWindow struct {
	start time.Time
	count int
}

func newKeyedLimiter() *keyedLimiter {
	return &keyedLimiter{windows: map[string]*limitWindow{}}
}

// allow reports whether key may act again: at most max times per window.
func (l *keyedLimiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.windows) > 10000 {
		for k, w := range l.windows {
			if now.Sub(w.start) > window {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) > window {
		l.windows[key] = &limitWindow{start: now, count: 1}
		return true
	}
	if w.count >= max {
		return false
	}
	w.count++
	return true
}

// issueFreeTicket creates a free ticket for a signed-in buyer or a guest
// email, plus its claim link, in one transaction.
func (h *TicketHandler) issueFreeTicket(ctx context.Context, eventID string, buyerID, guestEmail *string) (accessToken, claim string, err error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)

	// The sales rule, checked with the event row locked so concurrent
	// requests can't oversell or slip past a cancellation.
	if err := canSellTickets(ctx, tx, eventID, true); err != nil {
		return "", "", err
	}

	accessToken = services.GenerateTicketCode()
	var ticketID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO tickets (event_id, buyer_id, guest_email, access_token) VALUES ($1, $2, $3, $4) RETURNING id`,
		eventID, buyerID, guestEmail, accessToken,
	).Scan(&ticketID); err != nil {
		return "", "", err
	}
	claim, err = createTicketClaim(ctx, tx, eventID, ticketID)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return accessToken, claim, nil
}
