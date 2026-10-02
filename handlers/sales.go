package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

// The single server-side rule for whether an event may sell or issue
// tickets (EVT-03). Every ticket path — free tickets, Stripe checkout
// creation and fulfilment, PayPal checkout creation and capture — goes
// through canSellTickets, so a disabled, cancelled, ended, unpaid or sold-out
// event can never issue a ticket.

// ErrNotOnSale reasons. Their messages are safe to show buyers.
var (
	errSaleVenueUnpaid = errors.New("Tickets for this event aren't on sale yet.")
	errSaleDisabled    = errors.New("Ticket sales for this event are currently closed.")
	errSaleCancelled   = errors.New("This event was cancelled.")
	errSaleEnded       = errors.New("This event has ended.")
	errSaleSoldOut     = errors.New("This event is sold out.")
	errSaleNoEvent     = errors.New("Event not found.")
)

// saleState is what the sales rule needs to know about an event.
type saleState struct {
	VenuePaid  bool
	Active     bool
	Cancelled  bool
	EndsAt     time.Time
	MaxTickets *int
	Sold       int
}

// checkCanSell applies the sales rule. It is pure so it can be unit-tested.
func checkCanSell(s saleState, now time.Time) error {
	switch {
	case s.Cancelled:
		return errSaleCancelled
	case !now.Before(s.EndsAt):
		return errSaleEnded
	case !s.VenuePaid:
		return errSaleVenueUnpaid
	case !s.Active:
		return errSaleDisabled
	case s.MaxTickets != nil && *s.MaxTickets > 0 && s.Sold >= *s.MaxTickets:
		return errSaleSoldOut
	}
	return nil
}

// rowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// canSellTickets loads the event and applies the rule. With lock=true (inside
// a transaction) the event row is locked, so concurrent purchases can't both
// take the last ticket.
func canSellTickets(ctx context.Context, q rowQuerier, eventID string, lock bool) error {
	sql := `SELECT e.venue_paid, e.is_active, e.cancelled_at IS NOT NULL, e.ends_at, e.max_tickets,
	               (SELECT count(*) FROM tickets t WHERE t.event_id = e.id)
	        FROM events e WHERE e.id = $1`
	if lock {
		sql += ` FOR UPDATE OF e`
	}
	var s saleState
	if err := q.QueryRow(ctx, sql, eventID).Scan(
		&s.VenuePaid, &s.Active, &s.Cancelled, &s.EndsAt, &s.MaxTickets, &s.Sold,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errSaleNoEvent
		}
		return err
	}
	return checkCanSell(s, time.Now())
}

// isSaleRuleError reports whether err is a "not on sale" outcome (as opposed
// to a database failure).
func isSaleRuleError(err error) bool {
	for _, e := range []error{errSaleVenueUnpaid, errSaleDisabled, errSaleCancelled, errSaleEnded, errSaleSoldOut, errSaleNoEvent} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// saleRuleResponse answers a purchase that the sales rule refused (409 with
// the buyer-facing reason), or a genuine failure (500).
func saleRuleResponse(c *fiber.Ctx, err error) error {
	if isSaleRuleError(err) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create ticket"})
}
