package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v82"
)

// Stripe webhook handling (PAY-02).
//
// Every verified event gets a row in stripe_webhook_events. Delivery rules:
//   - already processed / ignored / needs_attention → 200 (duplicate, no-op)
//   - processed now, or nothing to do → 200
//   - can't be fixed by retrying (e.g. paid for an event that has since been
//     cancelled) → 200, recorded as needs_attention for a manual refund
//   - temporary failure (database, IVS...) → 500, recorded as failed, so
//     Stripe retries; the retry runs the same idempotent fulfilment again
//
// Fulfilment itself is idempotent too: every payment belongs to one order,
// processed under the order's row lock, and every ledger journal has a
// unique key. Refunds and disputes are keyed by Stripe's own IDs and apply
// only forward-moving changes, so out-of-order deliveries converge.

type webhookOutcome string

const (
	outcomeProcessed      webhookOutcome = "processed"
	outcomeIgnored        webhookOutcome = "ignored"
	outcomeNeedsAttention webhookOutcome = "needs_attention"
)

// errNeedsAttention wraps a reason that retrying won't fix.
type errNeedsAttention struct{ reason string }

func (e errNeedsAttention) Error() string { return e.reason }

func needsAttention(format string, args ...any) error {
	return errNeedsAttention{reason: fmt.Sprintf(format, args...)}
}

func (h *StripeHandler) Webhook(c *fiber.Ctx) error {
	event, err := constructStripeWebhookEvent(c.Body(), c.Get("Stripe-Signature"), h.Cfg.StripeWebhookSecret)
	if err != nil {
		// Logged without secret material: the generic 400 response Stripe sees
		// doesn't distinguish a bad signature from other ConstructEvent failures.
		log.Printf("stripe webhook verify failed: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid webhook signature"})
	}
	ctx := context.Background()

	// Duplicate delivery of an event we've already finished with.
	var prior string
	switch err := h.DB.QueryRow(ctx, `SELECT status FROM stripe_webhook_events WHERE event_id = $1`, event.ID).Scan(&prior); {
	case err == nil && (prior == string(outcomeProcessed) || prior == string(outcomeIgnored) || prior == string(outcomeNeedsAttention)):
		_, _ = h.DB.Exec(ctx, `UPDATE stripe_webhook_events SET attempts = attempts + 1, last_attempt_at = NOW() WHERE event_id = $1`, event.ID)
		return c.JSON(fiber.Map{"received": true, "duplicate": true})
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		log.Printf("stripe webhook %s: could not read event record: %v", event.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "temporarily unable to process"})
	}

	if _, err := h.DB.Exec(ctx,
		`INSERT INTO stripe_webhook_events (event_id, event_type, object_id, status, attempts)
		 VALUES ($1, $2, $3, 'processing', 1)
		 ON CONFLICT (event_id) DO UPDATE
		   SET status = 'processing', attempts = stripe_webhook_events.attempts + 1, last_attempt_at = NOW()`,
		event.ID, string(event.Type), stripeObjectID(event),
	); err != nil {
		log.Printf("stripe webhook %s: could not record event: %v", event.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "temporarily unable to process"})
	}

	process := h.process
	if process == nil {
		process = h.processStripeEvent
	}
	outcome, procErr := process(ctx, event)

	var na errNeedsAttention
	switch {
	case procErr == nil:
		h.recordWebhookOutcome(ctx, event.ID, string(outcome), "")
		return c.JSON(fiber.Map{"received": true})
	case errors.As(procErr, &na):
		log.Printf("stripe webhook %s (%s) needs attention: %s", event.ID, event.Type, na.reason)
		h.recordWebhookOutcome(ctx, event.ID, string(outcomeNeedsAttention), na.reason)
		return c.JSON(fiber.Map{"received": true})
	default:
		log.Printf("stripe webhook %s (%s) failed, Stripe will retry: %v", event.ID, event.Type, procErr)
		h.recordWebhookOutcome(ctx, event.ID, "failed", procErr.Error())
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "fulfillment failed; will retry"})
	}
}

func (h *StripeHandler) recordWebhookOutcome(ctx context.Context, eventID, status, errMsg string) {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	var lastErr *string
	if errMsg != "" {
		lastErr = &errMsg
	}
	processedAt := "NULL"
	if status != "failed" {
		processedAt = "NOW()"
	}
	if _, err := h.DB.Exec(ctx,
		`UPDATE stripe_webhook_events SET status = $2, last_error = $3, processed_at = `+processedAt+`
		 WHERE event_id = $1`, eventID, status, lastErr); err != nil {
		log.Printf("stripe webhook %s: could not record outcome %s: %v", eventID, status, err)
	}
}

// stripeObjectID returns the event's object ID (e.g. the Checkout Session
// ID) for diagnosis — never amounts, emails or card data.
func stripeObjectID(event stripe.Event) *string {
	var obj struct {
		ID string `json:"id"`
	}
	if event.Data == nil || json.Unmarshal(event.Data.Raw, &obj) != nil || obj.ID == "" {
		return nil
	}
	return &obj.ID
}

// processStripeEvent does the work for one verified event. A plain error
// means "retry"; errNeedsAttention means "don't retry, fix by hand".
func (h *StripeHandler) processStripeEvent(ctx context.Context, event stripe.Event) (webhookOutcome, error) {
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			return "", needsAttention("unreadable checkout session")
		}
		// Delayed payment methods complete the session before the money
		// arrives; they're fulfilled on async_payment_succeeded instead.
		if sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
			return outcomeIgnored, nil
		}
		switch sess.Metadata["type"] {
		case "venue_fee":
			return h.fulfillVenueFee(ctx, &sess)
		case "ticket":
			return h.fulfillTicket(ctx, &sess)
		}
		return outcomeIgnored, nil

	case "checkout.session.expired", "checkout.session.async_payment_failed":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			return "", needsAttention("unreadable checkout session")
		}
		if event.Type == "checkout.session.expired" {
			return h.endStripeCheckout(ctx, &sess, "expired", "checkout expired")
		}
		return h.endStripeCheckout(ctx, &sess, "failed", "delayed payment failed")

	case "refund.created", "refund.updated", "refund.failed", "charge.refund.updated":
		var r stripe.Refund
		if err := json.Unmarshal(event.Data.Raw, &r); err != nil {
			return "", needsAttention("unreadable refund")
		}
		return h.applyStripeRefund(ctx, &r)

	case "charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed",
		"charge.dispute.funds_withdrawn", "charge.dispute.funds_reinstated":
		var d stripe.Dispute
		if err := json.Unmarshal(event.Data.Raw, &d); err != nil {
			return "", needsAttention("unreadable dispute")
		}
		return h.applyStripeDispute(ctx, &d)

	case "account.updated":
		var acc stripe.Account
		if err := json.Unmarshal(event.Data.Raw, &acc); err != nil {
			return "", needsAttention("unreadable account")
		}
		if _, err := h.DB.Exec(ctx,
			`UPDATE connected_accounts SET payout_enabled = $1 WHERE stripe_account_id = $2`,
			acc.PayoutsEnabled, acc.ID,
		); err != nil {
			return "", fmt.Errorf("update connected account: %w", err)
		}
		return outcomeProcessed, nil
	}
	return outcomeIgnored, nil
}

func (h *StripeHandler) fulfillVenueFee(ctx context.Context, sess *stripe.CheckoutSession) (webhookOutcome, error) {
	if err := recordBookingFee(ctx, h.DB, h.IVS, sess); err != nil {
		return "", err
	}
	return outcomeProcessed, nil
}
