package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v82"
)

// Refunds and disputes (PAY-05). Providers tell us about them, possibly
// more than once and out of order; these functions apply only forward
// progress, and book each change exactly once through keyed journals.

// refundUpdate is a provider's current view of one refund.
type refundUpdate struct {
	Provider    string
	RefundID    string
	AmountCents int64
	// pending, succeeded, failed or canceled
	Status      string
	Reason      string
	RequestedBy *string
}

// refundRank orders refund states: a refund can go pending → succeeded →
// failed (a refund can fail after it looked successful), never backwards.
func refundRank(status string) int {
	switch status {
	case "pending":
		return 1
	case "succeeded":
		return 2
	case "failed", "canceled":
		return 3
	}
	return 0
}

func refundEffective(status string) bool { return status == "pending" || status == "succeeded" }

// orderBooked reports whether the order's payment is in the ledger.
func orderBooked(ctx context.Context, tx pgx.Tx, o *order) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_journal WHERE idempotency_key IN ($1, $2))`,
		"sale:"+o.ID, "booking_fee:"+o.ID,
	).Scan(&ok)
	return ok, err
}

// applyRefund records u against o (locked) and books it. A refund that
// takes the order to fully refunded voids its tickets; one that later fails
// is reversed in the ledger and the tickets are restored.
func applyRefund(ctx context.Context, tx pgx.Tx, o *order, u refundUpdate) error {
	if o.Status != "paid" && o.Status != "needs_attention" {
		if o.Status == "pending" {
			// The payment notification hasn't been processed yet; the
			// provider retries this one later.
			return fmt.Errorf("refund %s arrived before order %s was paid", u.RefundID, o.ID)
		}
		return needsAttention("refund %s for order %s in state %s", u.RefundID, o.ID, o.Status)
	}
	if booked, err := orderBooked(ctx, tx, o); err != nil {
		return err
	} else if !booked && o.AmountCents > 0 {
		return needsAttention("refund %s for order %s, whose payment was never booked", u.RefundID, o.ID)
	}

	var (
		status           string
		amount, platform int64
		posted, reversed bool
	)
	err := tx.QueryRow(ctx,
		`SELECT status, amount_cents, platform_fee_cents, posted, reversed FROM payment_refunds
		 WHERE provider = $1 AND provider_refund_id = $2 FOR UPDATE`, u.Provider, u.RefundID,
	).Scan(&status, &amount, &platform, &posted, &reversed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO payment_refunds (order_id, provider, provider_refund_id, amount_cents, status, reason, requested_by)
			 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)`,
			o.ID, u.Provider, u.RefundID, u.AmountCents, u.Status, truncate(u.Reason, 500), u.RequestedBy,
		); err != nil {
			return fmt.Errorf("record refund: %w", err)
		}
		status, amount = u.Status, u.AmountCents
	case err != nil:
		return fmt.Errorf("load refund: %w", err)
	default:
		if refundRank(u.Status) <= refundRank(status) {
			return nil // old or repeated news
		}
		status = u.Status
		if _, err := tx.Exec(ctx,
			`UPDATE payment_refunds SET status = $3, updated_at = NOW() WHERE provider = $1 AND provider_refund_id = $2`,
			u.Provider, u.RefundID, status,
		); err != nil {
			return fmt.Errorf("update refund: %w", err)
		}
	}

	key := u.Provider + ":" + u.RefundID
	switch {
	case refundEffective(status) && !posted:
		if amount > o.AmountCents-o.RefundedCents {
			return needsAttention("refund %s of %d cents exceeds the %d cents left on order %s",
				u.RefundID, amount, o.AmountCents-o.RefundedCents, o.ID)
		}
		platform = o.refundSplit(amount)
		if err := postRefund(ctx, tx, o, key, amount, platform); err != nil {
			return err
		}
		o.RefundedCents += amount
		o.PlatformFeeRefundedCents += platform
		if _, err := tx.Exec(ctx,
			`UPDATE payment_refunds SET posted = true, platform_fee_cents = $3, updated_at = NOW()
			 WHERE provider = $1 AND provider_refund_id = $2`, u.Provider, u.RefundID, platform,
		); err != nil {
			return err
		}
		if o.RefundedCents == o.AmountCents {
			if err := voidOrderTickets(ctx, tx, o.ID, "refunded"); err != nil {
				return err
			}
		}
	case !refundEffective(status) && posted && !reversed:
		if err := postRefundReversal(ctx, tx, o, key, amount, platform); err != nil {
			return err
		}
		o.RefundedCents -= amount
		o.PlatformFeeRefundedCents -= platform
		if _, err := tx.Exec(ctx,
			`UPDATE payment_refunds SET reversed = true, updated_at = NOW()
			 WHERE provider = $1 AND provider_refund_id = $2`, u.Provider, u.RefundID,
		); err != nil {
			return err
		}
		if err := restoreOrderTickets(ctx, tx, o.ID, "refunded"); err != nil {
			return err
		}
	default:
		return nil
	}
	_, err = tx.Exec(ctx,
		`UPDATE orders SET refunded_cents = $2, platform_fee_refunded_cents = $3, updated_at = NOW() WHERE id = $1`,
		o.ID, o.RefundedCents, o.PlatformFeeRefundedCents)
	return err
}

// disputeUpdate is a provider's current view of one dispute.
type disputeUpdate struct {
	Provider       string
	DisputeID      string
	AmountCents    int64
	ProviderStatus string
	Reason         string
	// Money: what the provider currently holds back, and its fees, as of
	// the Seen'th balance movement. HasMoney=false means this notification
	// carries no money information (status only).
	HasMoney  bool
	Withdrawn int64
	Fee       int64
	Seen      int
}

// disputeOutcome maps a provider status to open/won/lost.
func disputeOutcome(provider, status string) string {
	s := strings.ToLower(status)
	switch {
	case s == "won" || s == "resolved_seller_favour" || s == "resolved_seller_favor":
		return "won"
	case s == "lost" || s == "resolved_buyer_favour" || s == "resolved_buyer_favor":
		return "lost"
	}
	return "open"
}

// applyDispute records u against o (locked). Money held back by the
// provider is booked as a dispute loss, reinstated money reverses it, and
// a lost dispute voids the order's tickets.
func applyDispute(ctx context.Context, tx pgx.Tx, o *order, u disputeUpdate) error {
	if o.Status == "pending" {
		return fmt.Errorf("dispute %s arrived before order %s was paid", u.DisputeID, o.ID)
	}
	outcome := disputeOutcome(u.Provider, u.ProviderStatus)

	var (
		prevOutcome          string
		withdrawn, fee, seen int64
	)
	err := tx.QueryRow(ctx,
		`SELECT outcome, withdrawn_cents, fee_cents, balance_txns_seen FROM payment_disputes
		 WHERE provider = $1 AND provider_dispute_id = $2 FOR UPDATE`, u.Provider, u.DisputeID,
	).Scan(&prevOutcome, &withdrawn, &fee, &seen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO payment_disputes (order_id, provider, provider_dispute_id, amount_cents, provider_status, outcome, reason)
			 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))`,
			o.ID, u.Provider, u.DisputeID, u.AmountCents, u.ProviderStatus, outcome, truncate(u.Reason, 100),
		); err != nil {
			return fmt.Errorf("record dispute: %w", err)
		}
		prevOutcome = "open"
	case err != nil:
		return fmt.Errorf("load dispute: %w", err)
	}

	// A decided dispute never reopens because of an older notification.
	if prevOutcome != "open" {
		outcome = prevOutcome
	}
	if _, err := tx.Exec(ctx,
		`UPDATE payment_disputes SET provider_status = CASE WHEN outcome = 'open' THEN $3::text ELSE provider_status END,
		        outcome = $4::text, amount_cents = GREATEST(amount_cents, $5::bigint), updated_at = NOW(),
		        closed_at = CASE WHEN $4::text <> 'open' THEN COALESCE(closed_at, NOW()) END
		 WHERE provider = $1 AND provider_dispute_id = $2`,
		u.Provider, u.DisputeID, u.ProviderStatus, outcome, u.AmountCents,
	); err != nil {
		return fmt.Errorf("update dispute: %w", err)
	}

	if u.HasMoney && int64(u.Seen) > seen {
		cash := cashAccount(o.Provider)
		key := fmt.Sprintf("dispute:%s:%s:%d", u.Provider, u.DisputeID, u.Seen)
		if d := u.Withdrawn - withdrawn; d > 0 {
			if _, err := postJournal(ctx, tx, journal{Key: key + ":withdrawal", Kind: "dispute_withdrawal", Order: o, Postings: []posting{
				{acctDisputeLosses, d}, {cash, -d},
			}}); err != nil {
				return err
			}
		} else if d < 0 {
			if _, err := postJournal(ctx, tx, journal{Key: key + ":reinstatement", Kind: "dispute_reinstatement", Order: o, Postings: []posting{
				{cash, -d}, {acctDisputeLosses, d},
			}}); err != nil {
				return err
			}
		}
		if d := u.Fee - fee; d != 0 {
			if _, err := postJournal(ctx, tx, journal{Key: key + ":fee", Kind: "dispute_fee", Order: o, Postings: []posting{
				{acctProcessingFees, d}, {cash, -d},
			}}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE payment_disputes SET withdrawn_cents = $3, fee_cents = $4, balance_txns_seen = $5
			 WHERE provider = $1 AND provider_dispute_id = $2`,
			u.Provider, u.DisputeID, u.Withdrawn, u.Fee, u.Seen,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE orders SET disputed_cents = (SELECT COALESCE(SUM(withdrawn_cents), 0) FROM payment_disputes WHERE order_id = $1),
		        updated_at = NOW() WHERE id = $1`, o.ID,
	); err != nil {
		return err
	}
	if outcome == "lost" {
		return voidOrderTickets(ctx, tx, o.ID, "chargeback")
	}
	return nil
}

// stripeRefundStatus maps Stripe's refund states to ours.
func stripeRefundStatus(s stripe.RefundStatus) string {
	switch s {
	case stripe.RefundStatusSucceeded:
		return "succeeded"
	case stripe.RefundStatusFailed:
		return "failed"
	case stripe.RefundStatusCanceled:
		return "canceled"
	}
	return "pending" // pending, requires_action
}

// lockStripeOrderByPayment finds the order a Stripe object belongs to, by
// the order_id in its metadata or by PaymentIntent.
func lockStripeOrderByPayment(ctx context.Context, tx pgx.Tx, orderID, paymentIntent string) (*order, error) {
	if orderID != "" {
		if o, err := lockOrder(ctx, tx, `id::text = $1 AND provider = 'stripe'`, orderID); err != nil || o != nil {
			return o, err
		}
	}
	if paymentIntent == "" {
		return nil, nil
	}
	return lockOrder(ctx, tx, `provider = 'stripe' AND provider_payment_id = $1`, paymentIntent)
}

func (h *StripeHandler) applyStripeRefund(ctx context.Context, r *stripe.Refund) (webhookOutcome, error) {
	if !strings.EqualFold(string(r.Currency), "usd") {
		return "", needsAttention("refund %s is not in USD", r.ID)
	}
	var pi string
	if r.PaymentIntent != nil {
		pi = r.PaymentIntent.ID
	}
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	o, err := lockStripeOrderByPayment(ctx, tx, r.Metadata["order_id"], pi)
	if err != nil {
		return "", err
	}
	if o == nil {
		// Out of order: the payment it refunds isn't recorded yet. Stripe
		// retries, by which time it normally is.
		return "", fmt.Errorf("refund %s: no order for payment %s yet", r.ID, pi)
	}
	var requestedBy *string
	if v := r.Metadata["requested_by"]; v != "" {
		requestedBy = &v
	}
	if err := applyRefund(ctx, tx, o, refundUpdate{
		Provider: "stripe", RefundID: r.ID, AmountCents: r.Amount, Status: stripeRefundStatus(r.Status),
		Reason: string(r.Reason), RequestedBy: requestedBy,
	}); err != nil {
		var na errNeedsAttention
		if errors.As(err, &na) {
			return "", err
		}
		return "", fmt.Errorf("apply refund: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcomeProcessed, nil
}

func (h *StripeHandler) applyStripeDispute(ctx context.Context, d *stripe.Dispute) (webhookOutcome, error) {
	var pi string
	if d.PaymentIntent != nil {
		pi = d.PaymentIntent.ID
	}
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	o, err := lockStripeOrderByPayment(ctx, tx, d.Metadata["order_id"], pi)
	if err != nil {
		return "", err
	}
	if o == nil {
		return "", fmt.Errorf("dispute %s: no order for payment %s yet", d.ID, pi)
	}
	u := disputeUpdate{
		Provider: "stripe", DisputeID: d.ID, AmountCents: d.Amount,
		ProviderStatus: string(d.Status), Reason: string(d.Reason),
		HasMoney: true, Seen: len(d.BalanceTransactions),
	}
	// Each balance transaction moves money: a withdrawal (negative amount)
	// when the dispute opens, a reinstatement (positive) if it's won.
	for _, bt := range d.BalanceTransactions {
		u.Withdrawn -= bt.Amount
		u.Fee += bt.Fee
	}
	if err := applyDispute(ctx, tx, o, u); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcomeProcessed, nil
}
