package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"vertualeventlive/backend/services"
)

// Orders (PAY-03). Every checkout — paid ticket, free ticket, booking fee —
// is an order with an exact amount in cents, the fee rule it was sold
// under, the provider's references and a state. A pending ticket order
// holds its tickets until reserved_until, so paying buyers can't be
// oversold (PAY-09); fulfilment converts the reservation into tickets.

// reservationTTL outlasts the 30-minute Stripe checkout session, leaving a
// margin for the payment notification to arrive.
const reservationTTL = 35 * time.Minute

type order struct {
	ID                       string
	Kind                     string
	EventID                  string
	HostID                   string
	BuyerID                  *string
	BuyerEmail               string
	Quantity                 int
	Currency                 string
	UnitAmountCents          int64
	AmountCents              int64
	FeeRule                  string
	PlatformFeeBPS           int
	PlatformFeeCents         int64
	Provider                 string
	CheckoutID               *string
	PaymentID                *string
	PayoutGateway            string
	Status                   string
	RefundedCents            int64
	PlatformFeeRefundedCents int64
	DisputedCents            int64
}

const orderColumns = `id, kind, event_id, host_id, buyer_id, buyer_email, quantity, currency,
	unit_amount_cents, amount_cents, fee_rule, platform_fee_bps, platform_fee_cents, provider,
	provider_checkout_id, provider_payment_id, COALESCE(payout_gateway, ''), status,
	refunded_cents, platform_fee_refunded_cents, disputed_cents`

func scanOrder(row pgx.Row) (*order, error) {
	var o order
	err := row.Scan(&o.ID, &o.Kind, &o.EventID, &o.HostID, &o.BuyerID, &o.BuyerEmail, &o.Quantity, &o.Currency,
		&o.UnitAmountCents, &o.AmountCents, &o.FeeRule, &o.PlatformFeeBPS, &o.PlatformFeeCents, &o.Provider,
		&o.CheckoutID, &o.PaymentID, &o.PayoutGateway, &o.Status,
		&o.RefundedCents, &o.PlatformFeeRefundedCents, &o.DisputedCents)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// lockOrder loads one order matching where, locked for update. It returns
// (nil, nil) when there is none.
func lockOrder(ctx context.Context, tx pgx.Tx, where string, args ...any) (*order, error) {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE `+where+` FOR UPDATE`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// newOrder describes a checkout about to start.
type newOrder struct {
	Kind       string
	EventID    string
	BuyerID    *string
	BuyerEmail string
	Quantity   int
	UnitCents  int64
	Fee        services.FeeRule
	Provider   string
	// Reserve holds the tickets until the reservation expires.
	Reserve bool
	// PerBuyerLimit caps tickets per email per event (0 = no cap).
	PerBuyerLimit int
	// Adopt records a payment that started before orders existed (a
	// checkout created by the previous release): no sales rule or limit,
	// since the money has already been taken.
	Adopt      bool
	CheckoutID string
}

func (n newOrder) amount() int64 { return n.UnitCents * int64(max(n.Quantity, 1)) }

// createPendingOrder applies the sales rule and per-buyer limit with the
// event locked, then records the order. For ticket orders the tickets are
// held from here on, so two buyers can't both pay for the last seat.
func createPendingOrder(ctx context.Context, db txBeginner, n newOrder) (*order, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	o, err := insertOrder(ctx, tx, n, "pending")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// insertOrder runs inside tx: sales rule (tickets only), buyer limit, insert.
func insertOrder(ctx context.Context, tx pgx.Tx, n newOrder, status string) (*order, error) {
	qty := max(n.Quantity, 1)
	email := strings.ToLower(strings.TrimSpace(n.BuyerEmail))
	if n.Kind == "ticket" && !n.Adopt {
		if err := checkSale(ctx, tx, n.EventID, saleCheck{Lock: true, Quantity: qty}); err != nil {
			return nil, err
		}
		if n.PerBuyerLimit > 0 {
			held, err := ticketsHeldBy(ctx, tx, n.EventID, email)
			if err != nil {
				return nil, err
			}
			if held+qty > n.PerBuyerLimit {
				return nil, errSaleBuyerLimit
			}
		}
	}
	amount := n.amount()
	fee, rule, bps := n.Fee.PlatformFee(amount), n.Fee.Name(), n.Fee.BasisPoints
	if n.Kind == "booking_fee" {
		// The whole booking fee is platform revenue.
		fee, rule, bps = amount, "booking-fee", 10_000
	}
	var reserved *time.Time
	if n.Reserve && n.Kind == "ticket" {
		t := time.Now().Add(reservationTTL)
		reserved = &t
	}
	var paidAt *time.Time
	if status == "paid" {
		now := time.Now()
		paidAt = &now
	}
	o, err := scanOrder(tx.QueryRow(ctx,
		`INSERT INTO orders (kind, event_id, host_id, buyer_id, buyer_email, quantity, unit_amount_cents,
		                     amount_cents, fee_rule, platform_fee_bps, platform_fee_cents, provider,
		                     payout_gateway, status, reserved_until, paid_at, provider_checkout_id)
		 SELECT $1, e.id, e.host_id, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		        (SELECT payout_gateway FROM connected_accounts ca WHERE ca.user_id = e.host_id),
		        $12, $13, $14, NULLIF($15, '')
		 FROM events e WHERE e.id = $2
		 RETURNING `+orderColumns,
		n.Kind, n.EventID, n.BuyerID, email, qty, n.UnitCents, amount, rule, bps, fee,
		n.Provider, status, reserved, paidAt, n.CheckoutID,
	))
	if err != nil {
		if isUniqueViolation(err, "idx_orders_one_free_ticket") {
			return nil, errSaleHasFreeTicket
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errSaleNoEvent
		}
		return nil, err
	}
	return o, nil
}

// ticketsHeldBy counts an email's valid tickets plus its live reservations
// for an event.
func ticketsHeldBy(ctx context.Context, tx pgx.Tx, eventID, email string) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM tickets t JOIN orders o ON o.id = t.order_id
		         WHERE t.event_id = $1 AND t.voided_at IS NULL AND lower(o.buyer_email) = $2)
		      + (SELECT COALESCE(SUM(quantity), 0) FROM orders
		         WHERE event_id = $1 AND kind = 'ticket' AND status = 'pending'
		           AND reserved_until > NOW() AND lower(buyer_email) = $2)`,
		eventID, email,
	).Scan(&n)
	return n, err
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

// txBeginner is satisfied by *pgxpool.Pool.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// setOrderCheckout records the provider's checkout reference.
func setOrderCheckout(ctx context.Context, db execer, orderID, checkoutID string) error {
	_, err := db.Exec(ctx,
		`UPDATE orders SET provider_checkout_id = $2, updated_at = NOW() WHERE id = $1`, orderID, checkoutID)
	return err
}

// endPendingOrder marks a checkout that never completed (failed, expired)
// and releases its reservation. Paid orders are never touched.
func endPendingOrder(ctx context.Context, db execer, orderID, status, reason string) error {
	_, err := db.Exec(ctx,
		`UPDATE orders SET status = $2, failure_reason = NULLIF($3, ''), reserved_until = NULL, updated_at = NOW()
		 WHERE id = $1 AND status = 'pending'`, orderID, status, truncate(reason, 500))
	return err
}

// markNeedsAttention flags an order whose money arrived but can't be
// fulfilled; an operator refunds or fixes it.
func markNeedsAttention(ctx context.Context, tx pgx.Tx, o *order, reason string) error {
	_, err := tx.Exec(ctx,
		`UPDATE orders SET status = 'needs_attention', failure_reason = $2, reserved_until = NULL, updated_at = NOW()
		 WHERE id = $1 AND status IN ('pending', 'failed', 'expired', 'needs_attention')`, o.ID, truncate(reason, 500))
	return err
}

// fulfillment is what a provider reports about a completed payment.
type fulfillment struct {
	CheckoutID string
	PaymentID  string
}

// markOrderPaid issues the order's tickets (ticket orders), records the
// payment and books it. Run with the order row locked. Returns the issued
// access codes.
func markOrderPaid(ctx context.Context, tx pgx.Tx, o *order, f fulfillment, stripeSessionID *string) ([]string, error) {
	var codes []string
	if o.Kind == "ticket" {
		var guestEmail *string
		if o.BuyerID == nil {
			guestEmail = &o.BuyerEmail
		}
		for i := 0; i < o.Quantity; i++ {
			code := services.GenerateTicketCode()
			// The first ticket carries the Stripe session ID (unique), a
			// second guard against minting twice for one payment.
			var sessionID *string
			if i == 0 {
				sessionID = stripeSessionID
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO tickets (event_id, buyer_id, guest_email, access_token, order_id, stripe_checkout_session_id)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				o.EventID, o.BuyerID, guestEmail, code, o.ID, sessionID,
			); err != nil {
				return nil, fmt.Errorf("issue ticket: %w", err)
			}
			codes = append(codes, code)
		}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE orders SET status = 'paid', paid_at = COALESCE(paid_at, NOW()), reserved_until = NULL,
		        failure_reason = NULL, updated_at = NOW(),
		        provider_checkout_id = COALESCE(NULLIF($2, ''), provider_checkout_id),
		        provider_payment_id = COALESCE(NULLIF($3, ''), provider_payment_id)
		 WHERE id = $1 AND status <> 'paid'`, o.ID, f.CheckoutID, f.PaymentID)
	if err != nil {
		return nil, fmt.Errorf("mark order paid: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("order %s was already paid", o.ID)
	}
	o.Status = "paid"
	if err := postSale(ctx, tx, o); err != nil {
		return nil, err
	}
	return codes, nil
}

// recordUnfulfilledPayment books money that arrived for an order that can't
// be fulfilled (event cancelled or sold out meanwhile, duplicate booking
// fee). The money really moved — for destination charges the host has
// already received their share — so it is booked like a sale, and the order
// is flagged for an operator to refund. No ticket is issued.
func recordUnfulfilledPayment(ctx context.Context, tx pgx.Tx, o *order, f fulfillment, reason string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status = 'needs_attention', failure_reason = $4, paid_at = COALESCE(paid_at, NOW()),
		        reserved_until = NULL, updated_at = NOW(),
		        provider_checkout_id = COALESCE(NULLIF($2, ''), provider_checkout_id),
		        provider_payment_id = COALESCE(NULLIF($3, ''), provider_payment_id)
		 WHERE id = $1`, o.ID, f.CheckoutID, f.PaymentID, truncate(reason, 500)); err != nil {
		return err
	}
	o.Status = "needs_attention"
	return postSale(ctx, tx, o)
}

// firstTicketID returns the order's first ticket, for its claim link.
func firstTicketID(ctx context.Context, tx pgx.Tx, orderID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE order_id = $1 ORDER BY serial_no LIMIT 1`, orderID).Scan(&id)
	return id, err
}

// voidOrderTickets stops an order's tickets admitting anyone (full refund,
// lost chargeback). They stay on record.
func voidOrderTickets(ctx context.Context, tx pgx.Tx, orderID, reason string) error {
	_, err := tx.Exec(ctx,
		`UPDATE tickets SET voided_at = NOW(), void_reason = $2 WHERE order_id = $1 AND voided_at IS NULL`,
		orderID, reason)
	return err
}

// restoreOrderTickets undoes a void for reason (a refund that failed).
func restoreOrderTickets(ctx context.Context, tx pgx.Tx, orderID, reason string) error {
	_, err := tx.Exec(ctx,
		`UPDATE tickets SET voided_at = NULL, void_reason = NULL WHERE order_id = $1 AND void_reason = $2`,
		orderID, reason)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
