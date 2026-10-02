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

	"vertualeventlive/backend/services"
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
// Fulfilment itself is idempotent too: one ticket per Checkout Session
// (unique stripe_checkout_session_id) and venue activation under a row lock.

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
	eventID := sess.Metadata["event_id"]
	var venueFee float64
	if err := h.DB.QueryRow(ctx, `SELECT venue_fee FROM events WHERE id = $1`, eventID).Scan(&venueFee); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", needsAttention("booking fee paid for unknown event %s (session %s)", eventID, sess.ID)
		}
		return "", fmt.Errorf("load event: %w", err)
	}
	// Paid, in full, in USD, for this event.
	if !venueFeeStripeSessionMatches(sess, eventID, venueFee) {
		return "", needsAttention("session %s is not a full booking-fee payment for event %s", sess.ID, eventID)
	}
	if err := activateVenuePaidEvent(ctx, h.DB, h.IVS, eventID); err != nil {
		if errors.Is(err, errActivateCancelled) {
			return "", needsAttention("booking fee paid for cancelled event %s (session %s): refund", eventID, sess.ID)
		}
		return "", fmt.Errorf("activate event: %w", err)
	}
	return outcomeProcessed, nil
}

// fulfillTicket issues exactly one ticket per paid Checkout Session.
func (h *StripeHandler) fulfillTicket(ctx context.Context, sess *stripe.CheckoutSession) (webhookOutcome, error) {
	if !stripeTicketSessionPaid(sess) {
		return "", needsAttention("session %s is not a completed USD ticket payment", sess.ID)
	}
	eventID := sess.Metadata["event_id"]
	buyerID := sess.Metadata["buyer_id"]
	guestEmail := sess.Metadata["guest_email"]
	if buyerID == "" && guestEmail == "" {
		return "", needsAttention("session %s has no buyer", sess.ID)
	}

	buyerEmail := guestEmail
	if buyerID != "" {
		var userEmail string
		_ = h.DB.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, buyerID).Scan(&userEmail)
		if userEmail != "" {
			buyerEmail = userEmail
		}
	}

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin ticket transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Already fulfilled (an earlier delivery, or completed + async_succeeded
	// for the same session): nothing to do, and no second email.
	var existing string
	switch err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE stripe_checkout_session_id = $1`, sess.ID).Scan(&existing); {
	case err == nil:
		return outcomeProcessed, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("check existing ticket: %w", err)
	}

	// The sales rule, with the event locked. The buyer has paid, so a
	// refusal here means a manual refund rather than a retry.
	if err := canSellTickets(ctx, tx, eventID, true); err != nil {
		if isSaleRuleError(err) {
			return "", needsAttention("paid session %s for event %s that can no longer sell (%v): refund", sess.ID, eventID, err)
		}
		return "", fmt.Errorf("sales rule: %w", err)
	}

	accessToken := services.GenerateTicketCode()
	var buyer, guest *string
	if buyerID != "" {
		buyer = &buyerID
	} else {
		guest = &guestEmail
	}
	var ticketID string
	err = tx.QueryRow(ctx,
		`INSERT INTO tickets (event_id, buyer_id, guest_email, access_token, stripe_checkout_session_id)
		 VALUES ($1, $2, $3, $4, $5) ON CONFLICT (stripe_checkout_session_id) DO NOTHING RETURNING id`,
		eventID, buyer, guest, accessToken, sess.ID,
	).Scan(&ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcomeProcessed, nil // a concurrent delivery won the race
	}
	if err != nil {
		return "", fmt.Errorf("insert ticket: %w", err)
	}

	var payoutGateway string
	_ = tx.QueryRow(ctx,
		`SELECT ca.payout_gateway FROM connected_accounts ca
		 JOIN events e ON e.host_id = ca.user_id WHERE e.id = $1`, eventID,
	).Scan(&payoutGateway)
	if payoutGateway == "" {
		payoutGateway = "stripe"
	}
	// Stripe hosts are paid via destination charge at checkout, so their
	// entries settle immediately. WiPay/PayPal hosts wait for a manual payout.
	payoutStatus := "paid"
	if payoutGateway != "stripe" {
		payoutStatus = "pending"
	}
	split := services.CalculateSplit(float64(sess.AmountTotal) / 100)
	hostPayout := split.HostPayout
	if payoutGateway == "stripe" {
		hostPayout = split.StripeDestinationPayout()
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (ticket_id, event_id, gross_amount, stripe_fee, platform_fee, host_payout, payout_gateway, payout_status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ticketID, eventID, split.GrossAmount, split.StripeFee, split.PlatformFee, hostPayout, payoutGateway, payoutStatus,
	); err != nil {
		return "", fmt.Errorf("insert ledger: %w", err)
	}
	// The buyer's ticket-success page is waiting on this claim.
	if _, err := tx.Exec(ctx, `UPDATE ticket_claims SET ticket_id = $1 WHERE stripe_session_id = $2`, ticketID, sess.ID); err != nil {
		return "", fmt.Errorf("attach claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit ticket: %w", err)
	}

	sendTicketEmail(h.DB, h.Email, buyerEmail, accessToken)
	return outcomeProcessed, nil
}
