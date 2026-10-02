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
	// Per-buyer limits (PAY-11).
	errSaleHasFreeTicket = errors.New("You already have a free ticket for this event. Check your email, or use Find My Tickets.")
	errSaleBuyerLimit    = errors.New("You've reached the ticket limit for this event.")
)

// saleState is what the sales rule needs to know about an event.
type saleState struct {
	VenuePaid  bool
	Active     bool
	Cancelled  bool
	EndsAt     time.Time
	MaxTickets *int
	// Sold counts valid (not voided) tickets plus tickets held by other
	// buyers' unexpired checkouts (PAY-09).
	Sold int
	// Quantity being bought now; 0 means 1.
	Quantity int
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
	case s.MaxTickets != nil && *s.MaxTickets > 0 && s.Sold+max(s.Quantity, 1) > *s.MaxTickets:
		return errSaleSoldOut
	}
	return nil
}

// rowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// canSellTickets loads the event and applies the rule for one ticket. With
// lock=true (inside a transaction) the event row is locked, so concurrent
// purchases can't both take the last ticket.
func canSellTickets(ctx context.Context, q rowQuerier, eventID string, lock bool) error {
	return checkSale(ctx, q, eventID, saleCheck{Lock: lock})
}

// saleCheck says how to apply the rule.
type saleCheck struct {
	Lock     bool
	Quantity int
	// ExcludeOrder is the order being fulfilled: its own reservation is not
	// counted against it.
	ExcludeOrder *string
}

func checkSale(ctx context.Context, q rowQuerier, eventID string, opts saleCheck) error {
	if opts.Lock {
		// Lock first, in its own statement. Under READ COMMITTED a
		// statement that waits for a row lock still evaluates its
		// subqueries with the snapshot taken before the wait, so counting
		// in the same statement would miss the tickets and reservations the
		// previous lock holder just committed — and oversell.
		var one int
		if err := q.QueryRow(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errSaleNoEvent
			}
			return err
		}
	}
	sql := `SELECT e.venue_paid, e.is_active, e.cancelled_at IS NOT NULL, e.ends_at, e.max_tickets,
	               (SELECT count(*) FROM tickets t WHERE t.event_id = e.id AND t.voided_at IS NULL)
	             + (SELECT COALESCE(SUM(o.quantity), 0) FROM orders o
	                WHERE o.event_id = e.id AND o.kind = 'ticket' AND o.status = 'pending'
	                  AND o.reserved_until > NOW() AND ($2::uuid IS NULL OR o.id <> $2::uuid))
	        FROM events e WHERE e.id = $1`
	s := saleState{Quantity: opts.Quantity}
	if err := q.QueryRow(ctx, sql, eventID, opts.ExcludeOrder).Scan(
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
	for _, e := range []error{errSaleVenueUnpaid, errSaleDisabled, errSaleCancelled, errSaleEnded, errSaleSoldOut, errSaleNoEvent, errSaleHasFreeTicket, errSaleBuyerLimit} {
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
