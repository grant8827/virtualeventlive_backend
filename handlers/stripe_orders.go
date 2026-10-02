package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"

	"vertualeventlive/backend/services"
)

// Stripe Checkout sessions → orders. Both the webhook and the booking-fee
// return page go through these functions; each is idempotent under the
// order's row lock, so duplicate, concurrent or out-of-order notifications
// converge on one paid order, one set of tickets and one set of journals.

func stripePaymentIntentID(sess *stripe.CheckoutSession) string {
	if sess != nil && sess.PaymentIntent != nil {
		return sess.PaymentIntent.ID
	}
	return ""
}

// orderForStripeSession finds (and locks) the order behind a session: by
// the order_id we put in its metadata, else by session ID. A session
// created by the previous release has no order yet; one is adopted from the
// session itself so the payment is still recorded.
func orderForStripeSession(ctx context.Context, tx pgx.Tx, sess *stripe.CheckoutSession, kind string, fee services.FeeRule) (*order, error) {
	if id := sess.Metadata["order_id"]; id != "" {
		o, err := lockOrder(ctx, tx, `id = $1 AND provider = 'stripe'`, id)
		if err != nil || o != nil {
			return o, err
		}
	}
	o, err := lockOrder(ctx, tx, `provider = 'stripe' AND provider_checkout_id = $1`, sess.ID)
	if err != nil || o != nil {
		return o, err
	}

	n := newOrder{
		Kind: kind, EventID: sess.Metadata["event_id"], Quantity: 1, UnitCents: sess.AmountTotal,
		Fee: fee, Provider: "stripe", Adopt: true, CheckoutID: sess.ID,
	}
	buyer := sess.Metadata["buyer_id"]
	if kind == "booking_fee" {
		buyer = sess.Metadata["host_id"]
	}
	if buyer != "" {
		n.BuyerID = &buyer
		if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, buyer).Scan(&n.BuyerEmail); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if n.BuyerEmail == "" {
		n.BuyerEmail = sess.Metadata["guest_email"]
	}
	if n.BuyerEmail == "" && sess.CustomerDetails != nil {
		n.BuyerEmail = sess.CustomerDetails.Email
	}
	if n.BuyerEmail == "" {
		n.BuyerEmail = "unknown"
	}
	o, err = insertOrder(ctx, tx, n, "pending")
	if errors.Is(err, errSaleNoEvent) {
		return nil, needsAttention("session %s is for unknown event %s", sess.ID, n.EventID)
	}
	return o, err
}

// recordBookingFee books a paid booking-fee session and activates the
// event. A fee paid for a cancelled event, or a second fee for an event
// that is already paid, is booked and flagged for a refund.
func recordBookingFee(ctx context.Context, db *pgxpool.Pool, ivs *services.IVSService, sess *stripe.CheckoutSession) error {
	eventID := sess.Metadata["event_id"]
	var (
		feeCents  int64
		cancelled bool
	)
	if err := db.QueryRow(ctx,
		`SELECT (venue_fee * 100)::bigint, cancelled_at IS NOT NULL FROM events WHERE id = $1`, eventID,
	).Scan(&feeCents, &cancelled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return needsAttention("booking fee paid for unknown event %s (session %s)", eventID, sess.ID)
		}
		return fmt.Errorf("load event: %w", err)
	}
	// Paid, in full, in USD, for this event.
	if !venueFeeStripeSessionMatches(sess, eventID, feeCents) {
		return needsAttention("session %s is not a full booking-fee payment for event %s", sess.ID, eventID)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	o, err := orderForStripeSession(ctx, tx, sess, "booking_fee", services.FeeRule{})
	if err != nil {
		return err
	}
	paid := fulfillment{CheckoutID: sess.ID, PaymentID: stripePaymentIntentID(sess)}
	switch {
	case o.Status == "paid":
		// Already booked; make sure the event is active (idempotent).
	case o.Status == "needs_attention":
		return tx.Commit(ctx)
	case o.Kind != "booking_fee" || o.EventID != eventID || o.AmountCents != sess.AmountTotal:
		if err := markNeedsAttention(ctx, tx, o, "booking-fee session does not match its order; not booked"); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return needsAttention("session %s does not match order %s", sess.ID, o.ID)
	default:
		var alreadyPaid bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM orders WHERE event_id = $1 AND kind = 'booking_fee' AND status = 'paid' AND id <> $2)`,
			eventID, o.ID,
		).Scan(&alreadyPaid); err != nil {
			return err
		}
		reason := ""
		switch {
		case cancelled:
			reason = "booking fee paid for a cancelled event: refund"
		case alreadyPaid:
			reason = "second booking fee paid for the same event: refund"
		}
		if reason != "" {
			if err := recordUnfulfilledPayment(ctx, tx, o, paid, reason); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return needsAttention("%s (event %s, session %s)", reason, eventID, sess.ID)
		}
		if _, err := markOrderPaid(ctx, tx, o, paid, nil); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if err := activateVenuePaidEvent(ctx, db, ivs, eventID); err != nil {
		if errors.Is(err, errActivateCancelled) {
			// Cancelled between booking and activation.
			if _, uerr := db.Exec(ctx,
				`UPDATE orders SET status = 'needs_attention', failure_reason = 'booking fee paid for a cancelled event: refund', updated_at = NOW()
				 WHERE id = $1`, o.ID); uerr != nil {
				return uerr
			}
			return needsAttention("booking fee paid for cancelled event %s (session %s): refund", eventID, sess.ID)
		}
		return fmt.Errorf("activate event: %w", err)
	}
	return nil
}

// fulfillStripeTicket issues the tickets for one paid Checkout Session.
func (h *StripeHandler) fulfillTicket(ctx context.Context, sess *stripe.CheckoutSession) (webhookOutcome, error) {
	if !stripeTicketSessionPaid(sess) {
		return "", needsAttention("session %s is not a completed USD ticket payment", sess.ID)
	}
	eventID := sess.Metadata["event_id"]
	if sess.Metadata["order_id"] == "" && sess.Metadata["buyer_id"] == "" && sess.Metadata["guest_email"] == "" {
		return "", needsAttention("session %s has no buyer", sess.ID)
	}

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin ticket transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	o, err := orderForStripeSession(ctx, tx, sess, "ticket", feeRule(h.Cfg))
	if err != nil {
		return "", err
	}
	// Already fulfilled (an earlier delivery, or completed + async_succeeded
	// for the same session) or already flagged: nothing more to do.
	if o.Status == "paid" || o.Status == "needs_attention" {
		return outcomeProcessed, nil
	}
	if o.Kind != "ticket" || o.EventID != eventID || o.AmountCents != sess.AmountTotal {
		if err := markNeedsAttention(ctx, tx, o, fmt.Sprintf("session paid %d cents for order of %d cents; not booked", sess.AmountTotal, o.AmountCents)); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", needsAttention("session %s does not match order %s", sess.ID, o.ID)
	}

	paid := fulfillment{CheckoutID: sess.ID, PaymentID: stripePaymentIntentID(sess)}
	// The sales rule, with the event locked; the order's own reservation is
	// not counted against it. The buyer has paid, so a refusal here is booked
	// and flagged for a refund rather than retried.
	if err := checkSale(ctx, tx, eventID, saleCheck{Lock: true, Quantity: o.Quantity, ExcludeOrder: &o.ID}); err != nil {
		if !isSaleRuleError(err) {
			return "", fmt.Errorf("sales rule: %w", err)
		}
		if err := recordUnfulfilledPayment(ctx, tx, o, paid, "paid for an event that can no longer sell ("+err.Error()+"): refund"); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", needsAttention("paid session %s for event %s that can no longer sell (%v): refund", sess.ID, eventID, err)
	}

	codes, err := markOrderPaid(ctx, tx, o, paid, &sess.ID)
	if err != nil {
		if isUniqueViolation(err, "idx_tickets_stripe_checkout_session_id") {
			return outcomeProcessed, nil // a concurrent delivery won the race
		}
		return "", err
	}
	ticketID, err := firstTicketID(ctx, tx, o.ID)
	if err != nil {
		return "", fmt.Errorf("load ticket: %w", err)
	}
	// The buyer's ticket-success page is waiting on this claim.
	if _, err := tx.Exec(ctx, `UPDATE ticket_claims SET ticket_id = $1 WHERE stripe_session_id = $2`, ticketID, sess.ID); err != nil {
		return "", fmt.Errorf("attach claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit ticket: %w", err)
	}
	for _, code := range codes {
		sendTicketEmail(h.DB, h.Email, o.BuyerEmail, code)
	}
	return outcomeProcessed, nil
}

// endStripeCheckout handles a session that expired or whose delayed payment
// failed: the order ends and its reserved tickets go back on sale.
func (h *StripeHandler) endStripeCheckout(ctx context.Context, sess *stripe.CheckoutSession, status, reason string) (webhookOutcome, error) {
	tag, err := h.DB.Exec(ctx,
		`UPDATE orders SET status = $3, failure_reason = $4, reserved_until = NULL, updated_at = NOW()
		 WHERE provider = 'stripe' AND status = 'pending'
		   AND (id::text = $1 OR provider_checkout_id = $2)`,
		sess.Metadata["order_id"], sess.ID, status, reason)
	if err != nil {
		return "", fmt.Errorf("end checkout: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return outcomeIgnored, nil
	}
	return outcomeProcessed, nil
}
