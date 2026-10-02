package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"

	"vertualeventlive/backend/services"
)

// PayPal webhooks (PAY-02/PAY-04), handled like Stripe's: every verified
// event gets a row in paypal_webhook_events; duplicates are acknowledged
// without reprocessing; temporary failures answer 500 so PayPal retries;
// failures retrying can't fix are recorded as needs_attention.
//
// Money only moves on capture, refund and reversal events. Dispute events
// record the dispute's state; when PayPal takes the money back it sends a
// reversal, which is booked like a refund.

type paypalWebhookEvent struct {
	ID           string          `json:"id"`
	EventType    string          `json:"event_type"`
	ResourceType string          `json:"resource_type"`
	Resource     json.RawMessage `json:"resource"`
}

// PayPalWebhook is also the fallback for buyers who approve payment and
// close the PayPal window before the browser return reaches PayPalComplete.
// PayPal signs the event, and its verification API validates that signature
// before we act.
func (h *TicketHandler) PayPalWebhook(c *fiber.Ctx) error {
	if h.PayPal == nil {
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	raw := append([]byte(nil), c.Body()...)
	var event paypalWebhookEvent
	if err := json.Unmarshal(raw, &event); err != nil || event.ID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid PayPal webhook"})
	}
	verified, err := h.PayPal.VerifyWebhookSignature(
		c.Get("PayPal-Transmission-Id"),
		c.Get("PayPal-Transmission-Time"),
		c.Get("PayPal-Cert-Url"),
		c.Get("PayPal-Auth-Algo"),
		c.Get("PayPal-Transmission-Sig"),
		json.RawMessage(raw),
	)
	if err != nil {
		fmt.Printf("paypal webhook verification error: %v\n", err)
		return c.SendStatus(fiber.StatusBadGateway)
	}
	if !verified {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid PayPal webhook signature"})
	}

	ctx := context.Background()
	var prior string
	switch err := h.DB.QueryRow(ctx, `SELECT status FROM paypal_webhook_events WHERE event_id = $1`, event.ID).Scan(&prior); {
	case err == nil && prior != "failed" && prior != "processing":
		_, _ = h.DB.Exec(ctx, `UPDATE paypal_webhook_events SET attempts = attempts + 1, last_attempt_at = NOW() WHERE event_id = $1`, event.ID)
		return c.JSON(fiber.Map{"received": true, "duplicate": true})
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		log.Printf("paypal webhook %s: could not read event record: %v", event.ID, err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	var resource struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(event.Resource, &resource)
	if _, err := h.DB.Exec(ctx,
		`INSERT INTO paypal_webhook_events (event_id, event_type, resource_id, status, attempts)
		 VALUES ($1, $2, NULLIF($3, ''), 'processing', 1)
		 ON CONFLICT (event_id) DO UPDATE
		   SET status = 'processing', attempts = paypal_webhook_events.attempts + 1, last_attempt_at = NOW()`,
		event.ID, event.EventType, resource.ID,
	); err != nil {
		log.Printf("paypal webhook %s: could not record event: %v", event.ID, err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	outcome, procErr := h.processPayPalEvent(ctx, event)
	var na errNeedsAttention
	switch {
	case procErr == nil:
		h.recordPayPalOutcome(ctx, event.ID, string(outcome), "")
		return c.SendStatus(fiber.StatusOK)
	case errors.As(procErr, &na):
		log.Printf("paypal webhook %s (%s) needs attention: %s", event.ID, event.EventType, na.reason)
		h.recordPayPalOutcome(ctx, event.ID, string(outcomeNeedsAttention), na.reason)
		return c.SendStatus(fiber.StatusOK)
	default:
		log.Printf("paypal webhook %s (%s) failed, PayPal will retry: %v", event.ID, event.EventType, procErr)
		h.recordPayPalOutcome(ctx, event.ID, "failed", procErr.Error())
		// A non-2xx response asks PayPal to retry a transient failure.
		return c.SendStatus(fiber.StatusInternalServerError)
	}
}

func (h *TicketHandler) recordPayPalOutcome(ctx context.Context, eventID, status, errMsg string) {
	processedAt := "NULL"
	if status != "failed" {
		processedAt = "NOW()"
	}
	if _, err := h.DB.Exec(ctx,
		`UPDATE paypal_webhook_events SET status = $2, last_error = NULLIF($3, ''), processed_at = `+processedAt+`
		 WHERE event_id = $1`, eventID, status, truncate(errMsg, 500)); err != nil {
		log.Printf("paypal webhook %s: could not record outcome %s: %v", eventID, status, err)
	}
}

// paypalAmount is PayPal's money object.
type paypalAmount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

// paypalLink is one HATEOAS link on a PayPal resource.
type paypalLink struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

// captureIDFromLinks finds the capture a refund belongs to ("up" link).
func captureIDFromLinks(links []paypalLink) string {
	for _, l := range links {
		if l.Rel == "up" && strings.Contains(l.Href, "/captures/") {
			id := l.Href[strings.LastIndex(l.Href, "/captures/")+len("/captures/"):]
			if i := strings.IndexAny(id, "/?"); i >= 0 {
				id = id[:i]
			}
			return id
		}
	}
	return ""
}

func paypalRefundStatus(s string) string {
	switch strings.ToUpper(s) {
	case "COMPLETED":
		return "succeeded"
	case "FAILED":
		return "failed"
	case "CANCELLED":
		return "canceled"
	}
	return "pending"
}

func (h *TicketHandler) processPayPalEvent(ctx context.Context, event paypalWebhookEvent) (webhookOutcome, error) {
	switch event.EventType {
	case "CHECKOUT.ORDER.APPROVED":
		var r struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Resource, &r); err != nil || r.ID == "" {
			return "", needsAttention("PayPal order ID is missing")
		}
		return h.completeFromWebhook(ctx, r.ID)

	case "PAYMENT.CAPTURE.COMPLETED":
		// Normally our own capture; completes an order whose browser return
		// never arrived. The capture call is idempotent (PayPal-Request-Id).
		var r struct {
			SupplementaryData struct {
				RelatedIDs struct {
					OrderID string `json:"order_id"`
				} `json:"related_ids"`
			} `json:"supplementary_data"`
		}
		if err := json.Unmarshal(event.Resource, &r); err != nil || r.SupplementaryData.RelatedIDs.OrderID == "" {
			return outcomeIgnored, nil
		}
		return h.completeFromWebhook(ctx, r.SupplementaryData.RelatedIDs.OrderID)

	case "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.DECLINED":
		var r struct {
			SupplementaryData struct {
				RelatedIDs struct {
					OrderID string `json:"order_id"`
				} `json:"related_ids"`
			} `json:"supplementary_data"`
		}
		_ = json.Unmarshal(event.Resource, &r)
		tag, err := h.DB.Exec(ctx,
			`UPDATE orders SET status = 'failed', failure_reason = 'PayPal capture denied', reserved_until = NULL, updated_at = NOW()
			 WHERE provider = 'paypal' AND status = 'pending' AND provider_checkout_id = $1`,
			r.SupplementaryData.RelatedIDs.OrderID)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return outcomeIgnored, nil
		}
		return outcomeProcessed, nil

	case "PAYMENT.CAPTURE.REFUNDED", "PAYMENT.CAPTURE.REVERSED":
		// The resource is the refund (or, for a reversal, the reversal):
		// its own ID and amount, linked "up" to the capture.
		var r struct {
			ID       string       `json:"id"`
			Status   string       `json:"status"`
			CustomID string       `json:"custom_id"`
			Amount   paypalAmount `json:"amount"`
			Links    []paypalLink `json:"links"`
		}
		if err := json.Unmarshal(event.Resource, &r); err != nil || r.ID == "" {
			return "", needsAttention("unreadable PayPal refund")
		}
		if r.Amount.CurrencyCode != "USD" {
			return "", needsAttention("PayPal refund %s is not in USD", r.ID)
		}
		cents, err := services.ParseDecimalCents(r.Amount.Value)
		if err != nil || cents == 0 {
			return "", needsAttention("PayPal refund %s has an unreadable amount", r.ID)
		}
		reversal := event.EventType == "PAYMENT.CAPTURE.REVERSED"
		status := paypalRefundStatus(r.Status)
		reason := "refunded through PayPal"
		if reversal {
			status, reason = "succeeded", "PayPal reversal (chargeback)"
		}
		return h.applyPayPalMoneyBack(ctx, captureIDFromLinks(r.Links), r.CustomID, refundUpdate{
			Provider: "paypal", RefundID: r.ID, AmountCents: cents, Status: status, Reason: reason,
		}, reversal)

	case "CUSTOMER.DISPUTE.CREATED", "CUSTOMER.DISPUTE.UPDATED", "CUSTOMER.DISPUTE.RESOLVED":
		var r struct {
			DisputeID            string       `json:"dispute_id"`
			Status               string       `json:"status"`
			Reason               string       `json:"reason"`
			DisputeAmount        paypalAmount `json:"dispute_amount"`
			DisputedTransactions []struct {
				SellerTransactionID string `json:"seller_transaction_id"`
			} `json:"disputed_transactions"`
			DisputeOutcome struct {
				OutcomeCode string `json:"outcome_code"`
			} `json:"dispute_outcome"`
		}
		if err := json.Unmarshal(event.Resource, &r); err != nil || r.DisputeID == "" {
			return "", needsAttention("unreadable PayPal dispute")
		}
		if len(r.DisputedTransactions) == 0 {
			return "", needsAttention("PayPal dispute %s names no transaction", r.DisputeID)
		}
		cents, _ := services.ParseDecimalCents(r.DisputeAmount.Value)
		status := r.Status
		if r.DisputeOutcome.OutcomeCode != "" {
			status = r.DisputeOutcome.OutcomeCode
		}
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			return "", err
		}
		defer tx.Rollback(ctx)
		o, err := lockOrder(ctx, tx, `provider = 'paypal' AND provider_payment_id = $1`, r.DisputedTransactions[0].SellerTransactionID)
		if err != nil {
			return "", err
		}
		if o == nil {
			return "", fmt.Errorf("PayPal dispute %s: no order for capture yet", r.DisputeID)
		}
		// Status only: PayPal moves money with a reversal event.
		if err := applyDispute(ctx, tx, o, disputeUpdate{
			Provider: "paypal", DisputeID: r.DisputeID, AmountCents: cents, ProviderStatus: status, Reason: r.Reason,
		}); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return outcomeProcessed, nil
	}
	return outcomeIgnored, nil
}

func (h *TicketHandler) completeFromWebhook(ctx context.Context, paypalOrderID string) (webhookOutcome, error) {
	if _, _, err := h.completePayPalTicketOrder(ctx, paypalOrderID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return outcomeIgnored, nil // not one of our ticket orders
		}
		if isSaleRuleError(err) {
			// Not captured, so the buyer wasn't charged.
			return outcomeProcessed, nil
		}
		return "", err
	}
	return outcomeProcessed, nil
}

// applyPayPalMoneyBack books a PayPal refund or reversal against the order
// that owns the capture.
func (h *TicketHandler) applyPayPalMoneyBack(ctx context.Context, captureID, orderID string, u refundUpdate, chargeback bool) (webhookOutcome, error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var o *order
	if captureID != "" {
		if o, err = lockOrder(ctx, tx, `provider = 'paypal' AND provider_payment_id = $1`, captureID); err != nil {
			return "", err
		}
	}
	if o == nil && orderID != "" {
		if o, err = lockOrder(ctx, tx, `provider = 'paypal' AND id::text = $1`, orderID); err != nil {
			return "", err
		}
	}
	if o == nil {
		return "", fmt.Errorf("PayPal refund %s: no order for capture %s yet", u.RefundID, captureID)
	}
	if err := applyRefund(ctx, tx, o, u); err != nil {
		return "", err
	}
	if chargeback {
		if err := applyDispute(ctx, tx, o, disputeUpdate{
			Provider: "paypal", DisputeID: "reversal:" + u.RefundID, AmountCents: u.AmountCents, ProviderStatus: "lost",
			Reason: "chargeback",
		}); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcomeProcessed, nil
}
