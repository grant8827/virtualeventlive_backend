package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/refund"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

// Superuser payments console (Phase 2): orders, refunds and reconciliation.
// Every route runs behind middleware.RequireSuperuser.

// FinanceHandler serves the superuser payments pages.
type FinanceHandler struct {
	SuperuserHandler
	Cfg    *config.Config
	PayPal *services.PayPalService
}

type orderSummary struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Status          string     `json:"status"`
	EventID         string     `json:"event_id"`
	EventTitle      string     `json:"event_title"`
	HostEmail       string     `json:"host_email"`
	BuyerEmail      string     `json:"buyer_email"`
	Quantity        int        `json:"quantity"`
	Provider        string     `json:"provider"`
	AmountCents     int64      `json:"amount_cents"`
	PlatformFee     int64      `json:"platform_fee_cents"`
	RefundedCents   int64      `json:"refunded_cents"`
	DisputedCents   int64      `json:"disputed_cents"`
	ProviderFee     *int64     `json:"provider_fee_cents"`
	FeeRule         string     `json:"fee_rule"`
	FailureReason   *string    `json:"failure_reason"`
	CreatedAt       time.Time  `json:"created_at"`
	PaidAt          *time.Time `json:"paid_at"`
	CheckoutID      *string    `json:"provider_checkout_id"`
	PaymentID       *string    `json:"provider_payment_id"`
	OpenDisputes    int        `json:"open_disputes"`
	RefundableCents int64      `json:"refundable_cents"`
}

const orderSummarySelect = `
	SELECT o.id, o.kind, o.status, o.event_id, e.title, hu.email, o.buyer_email, o.quantity, o.provider,
	       o.amount_cents, o.platform_fee_cents, o.refunded_cents, o.disputed_cents, o.provider_fee_cents,
	       o.fee_rule, o.failure_reason, o.created_at, o.paid_at, o.provider_checkout_id, o.provider_payment_id,
	       (SELECT count(*) FROM payment_disputes d WHERE d.order_id = o.id AND d.outcome = 'open')
	FROM orders o JOIN events e ON e.id = o.event_id JOIN users hu ON hu.id = o.host_id`

func scanOrderSummary(row pgx.Row) (orderSummary, error) {
	var o orderSummary
	err := row.Scan(&o.ID, &o.Kind, &o.Status, &o.EventID, &o.EventTitle, &o.HostEmail, &o.BuyerEmail, &o.Quantity,
		&o.Provider, &o.AmountCents, &o.PlatformFee, &o.RefundedCents, &o.DisputedCents, &o.ProviderFee,
		&o.FeeRule, &o.FailureReason, &o.CreatedAt, &o.PaidAt, &o.CheckoutID, &o.PaymentID, &o.OpenDisputes)
	if err == nil && (o.Status == "paid" || o.Status == "needs_attention") &&
		(o.Provider == "stripe" || o.Provider == "paypal") && o.PaymentID != nil {
		o.RefundableCents = o.AmountCents - o.RefundedCents
	}
	return o, err
}

// ListOrders: newest first, filtered by status and a search over buyer,
// host, event and provider references.
func (h *FinanceHandler) ListOrders(c *fiber.Ctx) error {
	page, offset := pageOffset(c)
	status := c.Query("status")
	// $1 is always the status, so every branch references it.
	cond := map[string]string{
		"":         `$1::text = ''`,
		"refunded": `$1::text = 'refunded' AND o.refunded_cents > 0`,
		"disputed": `$1::text = 'disputed' AND EXISTS(SELECT 1 FROM payment_disputes d WHERE d.order_id = o.id)`,
	}[status]
	switch status {
	case "", "refunded", "disputed":
	case "pending", "paid", "failed", "expired", "needs_attention":
		cond = `o.status = $1::text`
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown status filter"})
	}
	where := `WHERE ` + cond + `
	            AND ($2::text IS NULL OR o.buyer_email ILIKE $2 OR hu.email ILIKE $2 OR e.title ILIKE $2
	                 OR o.provider_checkout_id ILIKE $2 OR o.provider_payment_id ILIKE $2 OR o.id::text ILIKE $2)`
	q := likePattern(c.Query("q"))
	var total int
	if err := h.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM orders o JOIN events e ON e.id = o.event_id JOIN users hu ON hu.id = o.host_id `+where,
		status, q).Scan(&total); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load orders"})
	}
	rows, err := h.DB.Query(context.Background(),
		orderSummarySelect+` `+where+` ORDER BY o.created_at DESC LIMIT $3 OFFSET $4`,
		status, q, superuserPageSize, offset)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load orders"})
	}
	defer rows.Close()
	orders := []orderSummary{}
	for rows.Next() {
		o, err := scanOrderSummary(rows)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load orders"})
		}
		orders = append(orders, o)
	}
	return c.JSON(fiber.Map{"orders": orders, "page": page, "page_size": superuserPageSize, "total": total})
}

type refundRow struct {
	ID          string    `json:"id"`
	ProviderID  string    `json:"provider_refund_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	Reason      *string   `json:"reason"`
	Reversed    bool      `json:"reversed"`
	CreatedAt   time.Time `json:"created_at"`
}

type disputeRow struct {
	ProviderID     string     `json:"provider_dispute_id"`
	AmountCents    int64      `json:"amount_cents"`
	WithdrawnCents int64      `json:"withdrawn_cents"`
	FeeCents       int64      `json:"fee_cents"`
	ProviderStatus string     `json:"provider_status"`
	Outcome        string     `json:"outcome"`
	Reason         *string    `json:"reason"`
	CreatedAt      time.Time  `json:"created_at"`
	ClosedAt       *time.Time `json:"closed_at"`
}

type journalRow struct {
	Key       string           `json:"key"`
	Kind      string           `json:"kind"`
	CreatedAt time.Time        `json:"created_at"`
	Postings  map[string]int64 `json:"postings"`
}

type ticketBrief struct {
	SerialNo   int64      `json:"serial_no"`
	VoidedAt   *time.Time `json:"voided_at"`
	VoidReason *string    `json:"void_reason"`
}

// GetOrder returns one order with its refunds, disputes, tickets and
// ledger journals. Ticket access codes are never included.
func (h *FinanceHandler) GetOrder(c *fiber.Ctx) error {
	ctx := context.Background()
	id := c.Params("id")
	o, err := scanOrderSummary(h.DB.QueryRow(ctx, orderSummarySelect+` WHERE o.id::text = $1`, id))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "order not found"})
	}

	refunds := []refundRow{}
	rows, err := h.DB.Query(ctx,
		`SELECT id, provider_refund_id, amount_cents, status, reason, reversed, created_at
		 FROM payment_refunds WHERE order_id = $1 ORDER BY created_at`, o.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
	}
	for rows.Next() {
		var r refundRow
		if err := rows.Scan(&r.ID, &r.ProviderID, &r.AmountCents, &r.Status, &r.Reason, &r.Reversed, &r.CreatedAt); err != nil {
			rows.Close()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
		}
		refunds = append(refunds, r)
	}
	rows.Close()

	disputes := []disputeRow{}
	rows, err = h.DB.Query(ctx,
		`SELECT provider_dispute_id, amount_cents, withdrawn_cents, fee_cents, provider_status, outcome, reason, created_at, closed_at
		 FROM payment_disputes WHERE order_id = $1 ORDER BY created_at`, o.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
	}
	for rows.Next() {
		var d disputeRow
		if err := rows.Scan(&d.ProviderID, &d.AmountCents, &d.WithdrawnCents, &d.FeeCents, &d.ProviderStatus, &d.Outcome, &d.Reason, &d.CreatedAt, &d.ClosedAt); err != nil {
			rows.Close()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
		}
		disputes = append(disputes, d)
	}
	rows.Close()

	tickets := []ticketBrief{}
	rows, err = h.DB.Query(ctx, `SELECT serial_no, voided_at, void_reason FROM tickets WHERE order_id = $1 ORDER BY serial_no`, o.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
	}
	for rows.Next() {
		var t ticketBrief
		if err := rows.Scan(&t.SerialNo, &t.VoidedAt, &t.VoidReason); err != nil {
			rows.Close()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
		}
		tickets = append(tickets, t)
	}
	rows.Close()

	journals := []journalRow{}
	rows, err = h.DB.Query(ctx,
		`SELECT j.idempotency_key, j.kind, j.created_at, p.account, p.amount_cents
		 FROM ledger_journal j JOIN ledger_postings p ON p.journal_id = j.id
		 WHERE j.order_id = $1 ORDER BY j.id, p.id`, o.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
	}
	for rows.Next() {
		var (
			key, kind, account string
			at                 time.Time
			cents              int64
		)
		if err := rows.Scan(&key, &kind, &at, &account, &cents); err != nil {
			rows.Close()
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to load order"})
		}
		if n := len(journals); n == 0 || journals[n-1].Key != key {
			journals = append(journals, journalRow{Key: key, Kind: kind, CreatedAt: at, Postings: map[string]int64{}})
		}
		journals[len(journals)-1].Postings[account] += cents
	}
	rows.Close()

	return c.JSON(fiber.Map{"order": o, "refunds": refunds, "disputes": disputes, "tickets": tickets, "journals": journals})
}

type refundOrderRequest struct {
	// AmountCents to refund; omitted or 0 refunds everything left.
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
}

// RefundOrder refunds a paid order through its provider. Ticket refunds
// take the host's share back from them (Stripe reverse_transfer / PayPal
// seller refund) and return the platform fee in proportion. The provider's
// answer is booked at once; its later notifications only confirm it.
func (h *FinanceHandler) RefundOrder(c *fiber.Ctx) error {
	ctx := context.Background()
	var req refundOrderRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > 300 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "give a reason for the refund (up to 300 characters)"})
	}
	actor := actorOf(c)

	// Read the order (unlocked: the provider call happens outside the
	// transaction; the idempotency key makes a double click one refund).
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "refund failed"})
	}
	o, err := lockOrder(ctx, tx, `id::text = $1`, c.Params("id"))
	_ = tx.Rollback(ctx)
	if err != nil || o == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "order not found"})
	}
	if o.Status != "paid" && o.Status != "needs_attention" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "only paid orders can be refunded"})
	}
	if (o.Provider != "stripe" && o.Provider != "paypal") || o.PaymentID == nil {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "this order has no provider payment to refund"})
	}
	remaining := o.AmountCents - o.RefundedCents
	amount := req.AmountCents
	if amount == 0 {
		amount = remaining
	}
	if amount <= 0 || amount > remaining {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("refund between 1 and %d cents", remaining)})
	}
	key := "refund-" + o.ID + "-" + strconv.FormatInt(o.RefundedCents, 10) + "-" + strconv.FormatInt(amount, 10)

	var u refundUpdate
	switch o.Provider {
	case "stripe":
		if h.Cfg.StripeSecretKey == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "Stripe is not configured"})
		}
		stripe.Key = h.Cfg.StripeSecretKey
		params := &stripe.RefundParams{
			PaymentIntent: stripe.String(*o.PaymentID),
			Amount:        stripe.Int64(amount),
			Metadata:      map[string]string{"order_id": o.ID, "requested_by": actor},
		}
		if o.settlesInstantly() {
			params.ReverseTransfer = stripe.Bool(true)
			params.RefundApplicationFee = stripe.Bool(true)
		}
		params.SetIdempotencyKey(key)
		r, err := refund.New(params)
		if err != nil {
			log.Printf("refund order %s: stripe: %v", o.ID, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Stripe refused the refund: " + stripeErrorMessage(err)})
		}
		u = refundUpdate{Provider: "stripe", RefundID: r.ID, AmountCents: r.Amount, Status: stripeRefundStatus(r.Status)}
	case "paypal":
		if h.PayPal == nil || !h.PayPal.Enabled() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "PayPal is not configured"})
		}
		var merchant string
		_ = h.DB.QueryRow(ctx, `SELECT merchant_id FROM paypal_ticket_orders WHERE app_order_id = $1`, o.ID).Scan(&merchant)
		r, err := h.PayPal.RefundCapture(*o.PaymentID, amount, o.refundSplit(amount), merchant, key, req.Reason)
		if err != nil {
			log.Printf("refund order %s: paypal: %v", o.ID, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "PayPal refused the refund"})
		}
		u = refundUpdate{Provider: "paypal", RefundID: r.ID, AmountCents: r.AmountCents, Status: paypalRefundStatus(r.Status)}
	}
	u.Reason, u.RequestedBy = req.Reason, &actor

	tx, err = h.DB.Begin(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "refund sent but not recorded; it will be recorded from the provider's notification"})
	}
	defer tx.Rollback(ctx)
	o, err = lockOrder(ctx, tx, `id = $1`, o.ID)
	if err == nil && o != nil {
		err = applyRefund(ctx, tx, o, u)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		var na errNeedsAttention
		msg := "refund sent but not recorded; it will be recorded from the provider's notification"
		if errors.As(err, &na) {
			msg = "refund sent, but it needs attention: " + na.reason
		}
		log.Printf("refund order %s: recording refund %s: %v", c.Params("id"), u.RefundID, err)
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"error": msg})
	}
	recordSecurityEvent(ctx, h.DB, c, "superuser_refund_issued", actor, "", map[string]any{
		"order_id": o.ID, "amount_cents": amount, "provider": o.Provider, "refund_id": u.RefundID,
	})
	return h.GetOrder(c)
}

func stripeErrorMessage(err error) string {
	var se *stripe.Error
	if errors.As(err, &se) && se.Msg != "" {
		return se.Msg
	}
	return "see the backend logs"
}

// Reconciliation runs the books check now and returns it with the latest
// provider run.
func (h *FinanceHandler) Reconciliation(c *fiber.Ctx) error {
	ctx := context.Background()
	books, err := ReconcileBooks(ctx, h.DB)
	if err != nil {
		log.Printf("reconciliation (books): %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reconciliation failed"})
	}
	var (
		last   json.RawMessage
		lastAt *time.Time
	)
	_ = h.DB.QueryRow(ctx,
		`SELECT report, finished_at FROM reconciliation_runs WHERE kind = 'provider' ORDER BY id DESC LIMIT 1`,
	).Scan(&last, &lastAt)
	resp := fiber.Map{"books": books, "last_provider_run": nil, "last_provider_run_at": lastAt}
	if len(last) > 0 {
		resp["last_provider_run"] = last
	}
	return c.JSON(resp)
}

// RunProviderReconciliation compares recent orders with Stripe/PayPal now,
// repairing payments whose notification never arrived.
func (h *FinanceHandler) RunProviderReconciliation(c *fiber.Ctx) error {
	ctx := context.Background()
	p := &ProviderReconciler{DB: h.DB, Cfg: h.Cfg, PayPal: h.PayPal, Email: h.Email, IVS: h.IVS, Repair: true}
	r, err := p.Run(ctx)
	if err != nil {
		log.Printf("reconciliation (provider): %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "could not reach the payment provider"})
	}
	saveReconcileRun(ctx, h.DB, r)
	recordSecurityEvent(ctx, h.DB, c, "superuser_reconciliation_run", actorOf(c), "", map[string]any{
		"orders_checked": r.OrdersChecked, "issues": len(r.Issues), "repaired": len(r.Repaired),
	})
	return c.JSON(r)
}
