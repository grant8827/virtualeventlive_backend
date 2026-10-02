package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/paymentintent"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/services"
)

// Reconciliation (PAY-05). Two passes:
//
//   - Books: the ledger, orders, refunds, disputes and tickets are checked
//     against each other, order by order. These invariants must hold after
//     any mix of success, failure, retry, refund and dispute.
//   - Provider: each recent paid order is compared with what Stripe or
//     PayPal says happened (amount, refunds, disputes); Stripe's actual
//     processing fee is booked; and paid Checkout Sessions with no paid
//     order (a webhook that never arrived) are found and, with Repair,
//     fulfilled through the normal idempotent path.

// ReconcileIssue is one thing that doesn't add up.
type ReconcileIssue struct {
	OrderID string `json:"order_id,omitempty"`
	Check   string `json:"check"`
	Detail  string `json:"detail"`
}

// ReconcileTotals are the books in cents.
type ReconcileTotals struct {
	TicketGross       int64            `json:"ticket_gross_cents"`
	BookingFees       int64            `json:"booking_fees_cents"`
	Refunded          int64            `json:"refunded_cents"`
	Commission        int64            `json:"commission_cents"`
	BookingFeeRevenue int64            `json:"booking_fee_revenue_cents"`
	ProcessingFees    int64            `json:"processing_fees_cents"`
	DisputeLosses     int64            `json:"dispute_losses_cents"`
	HostPayable       int64            `json:"host_payable_cents"`
	Cash              map[string]int64 `json:"cash_cents"`
	LedgerSum         int64            `json:"ledger_sum_cents"`
}

// ReconcileReport is the outcome of one run.
type ReconcileReport struct {
	Kind          string           `json:"kind"`
	RanAt         time.Time        `json:"ran_at"`
	OrdersChecked int              `json:"orders_checked"`
	Issues        []ReconcileIssue `json:"issues"`
	// Action items: not inconsistencies, but things an operator must do.
	NeedsAttention []string         `json:"needs_attention_orders"`
	FailedWebhooks int              `json:"failed_webhooks"`
	StalePending   int              `json:"stale_pending_orders"`
	Repaired       []string         `json:"repaired,omitempty"`
	FeesRecorded   int              `json:"fees_recorded,omitempty"`
	Totals         *ReconcileTotals `json:"totals,omitempty"`
}

func (r *ReconcileReport) issue(orderID, check, format string, args ...any) {
	r.Issues = append(r.Issues, ReconcileIssue{OrderID: orderID, Check: check, Detail: fmt.Sprintf(format, args...)})
}

// ReconcileBooks checks the internal books.
func ReconcileBooks(ctx context.Context, db *pgxpool.Pool) (*ReconcileReport, error) {
	r := &ReconcileReport{Kind: "books", RanAt: time.Now().UTC(), Issues: []ReconcileIssue{}, NeedsAttention: []string{}}

	// Every journal balances (the database enforces this at commit; this
	// proves it for the record).
	rows, err := db.Query(ctx,
		`SELECT j.idempotency_key, COALESCE(SUM(p.amount_cents), 0), count(p.id)
		 FROM ledger_journal j LEFT JOIN ledger_postings p ON p.journal_id = j.id
		 GROUP BY j.id HAVING COALESCE(SUM(p.amount_cents), 0) <> 0 OR count(p.id) < 2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var sum, n int64
		if err := rows.Scan(&key, &sum, &n); err != nil {
			rows.Close()
			return nil, err
		}
		r.issue("", "journal_balanced", "journal %s has %d postings summing to %d", key, n, sum)
	}
	rows.Close()

	// Per order: each figure on the order must equal what the ledger,
	// refunds, disputes and tickets say.
	rows, err = db.Query(ctx, `
		SELECT o.id, o.kind, o.status, o.provider, COALESCE(o.payout_gateway, ''), o.quantity,
		       o.amount_cents, o.platform_fee_cents, o.platform_fee_refunded_cents, o.refunded_cents, o.disputed_cents,
		       EXISTS(SELECT 1 FROM ledger_journal j WHERE j.order_id = o.id AND j.kind IN ('sale', 'booking_fee')),
		       COALESCE((SELECT SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		                 WHERE j.order_id = o.id AND j.kind IN ('sale', 'booking_fee') AND p.account LIKE 'cash_%'), 0),
		       COALESCE((SELECT -SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		                 WHERE j.order_id = o.id AND p.account IN ('platform_commission', 'booking_fee_revenue')), 0),
		       COALESCE((SELECT -SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		                 WHERE j.order_id = o.id AND j.kind IN ('refund', 'refund_reversal') AND p.account LIKE 'cash_%'), 0),
		       COALESCE((SELECT -SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		                 WHERE j.order_id = o.id AND p.account = 'host_payable'), 0),
		       COALESCE((SELECT SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		                 WHERE j.order_id = o.id AND p.account = 'dispute_losses'), 0),
		       COALESCE((SELECT SUM(amount_cents) FROM payment_refunds WHERE order_id = o.id AND posted AND NOT reversed), 0),
		       COALESCE((SELECT SUM(withdrawn_cents) FROM payment_disputes WHERE order_id = o.id), 0),
		       EXISTS(SELECT 1 FROM payment_disputes WHERE order_id = o.id AND outcome = 'lost'),
		       (SELECT count(*) FROM tickets t WHERE t.order_id = o.id AND t.voided_at IS NULL),
		       (SELECT count(*) FROM tickets t WHERE t.order_id = o.id)
		FROM orders o
		WHERE o.status IN ('paid', 'needs_attention')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, kind, status, provider, gateway                  string
			qty                                                  int
			amount, fee, feeRefunded, refunded, disputed         int64
			booked                                               bool
			saleCash, revenue, refundCash, hostOwed, disputeLoss int64
			refundRows, disputeRows                              int64
			lost                                                 bool
			validTickets, allTickets                             int
		)
		if err := rows.Scan(&id, &kind, &status, &provider, &gateway, &qty, &amount, &fee, &feeRefunded, &refunded, &disputed,
			&booked, &saleCash, &revenue, &refundCash, &hostOwed, &disputeLoss, &refundRows, &disputeRows, &lost,
			&validTickets, &allTickets); err != nil {
			return nil, err
		}
		r.OrdersChecked++
		if status == "needs_attention" {
			r.NeedsAttention = append(r.NeedsAttention, id)
		}
		if amount > 0 && !booked {
			if status == "paid" {
				r.issue(id, "order_booked", "paid order of %d cents has no sale in the ledger", amount)
			}
			continue // unbooked needs_attention orders are action items
		}
		if amount == 0 {
			if saleCash != 0 || revenue != 0 || refundCash != 0 {
				r.issue(id, "free_order_has_money", "a free order has ledger postings")
			}
		} else {
			if saleCash != amount {
				r.issue(id, "sale_amount", "ledger received %d cents, order is %d", saleCash, amount)
			}
			if want := fee - feeRefunded; revenue != want {
				r.issue(id, "platform_revenue", "ledger revenue %d cents, order fee %d less refunded %d", revenue, fee, feeRefunded)
			}
			if refundCash != refunded {
				r.issue(id, "refund_ledger", "ledger refunded %d cents, order says %d", refundCash, refunded)
			}
			// The host's share is settled at checkout for Stripe and PayPal;
			// otherwise it stays owed until a manual payout.
			wantOwed := int64(0)
			if kind == "ticket" && gateway != "stripe" && gateway != "paypal" {
				wantOwed = (amount - fee) - (refunded - feeRefunded)
			}
			if hostOwed != wantOwed {
				r.issue(id, "host_payable", "host owed %d cents in the ledger, expected %d", hostOwed, wantOwed)
			}
		}
		if refundRows != refunded {
			r.issue(id, "refund_records", "refund records total %d cents, order says %d", refundRows, refunded)
		}
		if disputeRows != disputed || disputeLoss != disputed {
			r.issue(id, "dispute_records", "disputes hold %d cents, ledger %d, order %d", disputeRows, disputeLoss, disputed)
		}
		if refunded > amount {
			r.issue(id, "over_refunded", "refunded %d cents of %d", refunded, amount)
		}
		if kind == "ticket" {
			wantValid := qty
			if status == "needs_attention" || (amount > 0 && refunded == amount) || lost {
				wantValid = 0
			}
			if status == "paid" && allTickets != qty {
				r.issue(id, "tickets_issued", "%d tickets issued for quantity %d", allTickets, qty)
			}
			if validTickets != wantValid {
				r.issue(id, "tickets_valid", "%d valid tickets, expected %d", validTickets, wantValid)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tickets outside the order books.
	var orphans int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE order_id IS NULL`).Scan(&orphans); err != nil {
		return nil, err
	}
	if orphans > 0 {
		r.issue("", "tickets_without_order", "%d tickets have no order", orphans)
	}

	// Action items.
	if err := db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM stripe_webhook_events WHERE status IN ('failed', 'needs_attention'))
		      + (SELECT count(*) FROM paypal_webhook_events WHERE status IN ('failed', 'needs_attention'))`,
	).Scan(&r.FailedWebhooks); err != nil {
		return nil, err
	}
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM orders WHERE status = 'pending' AND created_at < NOW() - INTERVAL '2 hours'`,
	).Scan(&r.StalePending); err != nil {
		return nil, err
	}

	t := &ReconcileTotals{Cash: map[string]int64{}}
	if err := db.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_cents) FILTER (WHERE kind = 'ticket'), 0),
		        COALESCE(SUM(amount_cents) FILTER (WHERE kind = 'booking_fee'), 0),
		        COALESCE(SUM(refunded_cents), 0)
		 FROM orders WHERE status IN ('paid', 'needs_attention')
		   AND EXISTS(SELECT 1 FROM ledger_journal j WHERE j.order_id = orders.id AND j.kind IN ('sale', 'booking_fee'))`,
	).Scan(&t.TicketGross, &t.BookingFees, &t.Refunded); err != nil {
		return nil, err
	}
	accts, err := db.Query(ctx, `SELECT account, SUM(amount_cents) FROM ledger_postings GROUP BY account`)
	if err != nil {
		return nil, err
	}
	defer accts.Close()
	for accts.Next() {
		var acct string
		var sum int64
		if err := accts.Scan(&acct, &sum); err != nil {
			return nil, err
		}
		t.LedgerSum += sum
		switch acct {
		case acctPlatformCommission:
			t.Commission = -sum
		case acctBookingFeeRevenue:
			t.BookingFeeRevenue = -sum
		case acctProcessingFees:
			t.ProcessingFees = sum
		case acctDisputeLosses:
			t.DisputeLosses = sum
		case acctHostPayable:
			t.HostPayable = -sum
		default:
			if strings.HasPrefix(acct, "cash_") {
				t.Cash[strings.TrimPrefix(acct, "cash_")] = sum
			}
		}
	}
	if t.LedgerSum != 0 {
		r.issue("", "ledger_balanced", "all postings sum to %d cents, not 0", t.LedgerSum)
	}
	r.Totals = t
	return r, nil
}

// ProviderReconciler compares orders with Stripe and PayPal.
type ProviderReconciler struct {
	DB     *pgxpool.Pool
	Cfg    *config.Config
	PayPal *services.PayPalService
	Email  *services.EmailService
	IVS    *services.IVSService
	// Window is how far back to look (default 7 days).
	Window time.Duration
	// Repair fulfils paid sessions whose webhook never arrived.
	Repair bool
}

func (p *ProviderReconciler) window() time.Duration {
	if p.Window > 0 {
		return p.Window
	}
	return 7 * 24 * time.Hour
}

// Run does one provider pass.
func (p *ProviderReconciler) Run(ctx context.Context) (*ReconcileReport, error) {
	r := &ReconcileReport{Kind: "provider", RanAt: time.Now().UTC(), Issues: []ReconcileIssue{}, NeedsAttention: []string{}}
	if p.Cfg.StripeSecretKey != "" {
		if err := p.stripePass(ctx, r); err != nil {
			return nil, err
		}
	}
	if p.PayPal != nil && p.PayPal.Enabled() {
		if err := p.paypalPass(ctx, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (p *ProviderReconciler) stripePass(ctx context.Context, r *ReconcileReport) error {
	stripe.Key = p.Cfg.StripeSecretKey
	since := time.Now().Add(-p.window())

	// 1. Paid sessions we have no paid order for.
	params := &stripe.CheckoutSessionListParams{Status: stripe.String(string(stripe.CheckoutSessionStatusComplete))}
	params.CreatedRange = &stripe.RangeQueryParams{GreaterThanOrEqual: since.Unix()}
	params.Limit = stripe.Int64(100)
	it := session.List(params)
	for it.Next() {
		sess := it.CheckoutSession()
		kind := sess.Metadata["type"]
		if sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid || (kind != "ticket" && kind != "venue_fee") {
			continue
		}
		var status string
		err := p.DB.QueryRow(ctx,
			`SELECT status FROM orders WHERE provider = 'stripe' AND (provider_checkout_id = $1 OR id::text = $2)`,
			sess.ID, sess.Metadata["order_id"]).Scan(&status)
		if err == nil && (status == "paid" || status == "needs_attention") {
			continue
		}
		if !p.Repair {
			r.issue(sess.Metadata["order_id"], "stripe_session_unrecorded", "paid session %s has no paid order", sess.ID)
			continue
		}
		h := &StripeHandler{DB: p.DB, Cfg: p.Cfg, Email: p.Email, IVS: p.IVS}
		var ferr error
		if kind == "ticket" {
			_, ferr = h.fulfillTicket(ctx, sess)
		} else {
			_, ferr = h.fulfillVenueFee(ctx, sess)
		}
		var na errNeedsAttention
		switch {
		case ferr == nil:
			r.Repaired = append(r.Repaired, sess.ID)
		case errors.As(ferr, &na):
			r.Repaired = append(r.Repaired, sess.ID+" (needs attention: "+na.reason+")")
		default:
			r.issue(sess.Metadata["order_id"], "stripe_session_unrecorded", "paid session %s could not be recorded: %v", sess.ID, ferr)
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("list Stripe sessions: %w", err)
	}

	// 2. Each recent paid order against its PaymentIntent and charge.
	rows, err := p.DB.Query(ctx,
		`SELECT id FROM orders WHERE provider = 'stripe' AND status IN ('paid', 'needs_attention')
		   AND provider_payment_id IS NOT NULL AND (paid_at >= $1 OR provider_fee_cents IS NULL)
		 ORDER BY paid_at DESC LIMIT 500`, since)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := p.checkStripeOrder(ctx, r, id); err != nil {
			r.issue(id, "stripe_lookup", "%v", err)
		}
	}
	return nil
}

func (p *ProviderReconciler) checkStripeOrder(ctx context.Context, r *ReconcileReport, orderID string) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	o, err := lockOrder(ctx, tx, `id = $1`, orderID)
	if err != nil || o == nil || o.PaymentID == nil {
		return err
	}
	params := &stripe.PaymentIntentParams{}
	params.AddExpand("latest_charge.balance_transaction")
	pi, err := paymentintent.Get(*o.PaymentID, params)
	if err != nil {
		return fmt.Errorf("payment %s: %w", *o.PaymentID, err)
	}
	r.OrdersChecked++
	if pi.Amount != o.AmountCents || !strings.EqualFold(string(pi.Currency), o.Currency) {
		r.issue(o.ID, "stripe_amount", "Stripe charged %d %s, order is %d %s", pi.Amount, pi.Currency, o.AmountCents, o.Currency)
	}
	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		r.issue(o.ID, "stripe_status", "PaymentIntent is %s", pi.Status)
	}
	ch := pi.LatestCharge
	if ch == nil {
		r.issue(o.ID, "stripe_charge", "PaymentIntent has no charge")
		return tx.Commit(ctx)
	}
	if ch.AmountRefunded != o.RefundedCents {
		r.issue(o.ID, "stripe_refunds", "Stripe refunded %d cents, books say %d", ch.AmountRefunded, o.RefundedCents)
	}
	var openDisputes int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_disputes WHERE order_id = $1`, o.ID).Scan(&openDisputes); err != nil {
		return err
	}
	if ch.Disputed && openDisputes == 0 {
		r.issue(o.ID, "stripe_dispute", "charge is disputed but no dispute is recorded")
	}

	// The actual processing fee, booked once.
	if bt := ch.BalanceTransaction; bt != nil && bt.Fee > 0 {
		posted, err := postJournal(ctx, tx, journal{Key: "processing_fee:" + o.ID, Kind: "processing_fee", Order: o,
			Memo: "Stripe fee", Postings: []posting{{acctProcessingFees, bt.Fee}, {cashAccount(o.Provider), -bt.Fee}}})
		if err != nil {
			return err
		}
		if posted {
			r.FeesRecorded++
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET provider_fee_cents = $2 WHERE id = $1 AND provider_fee_cents IS NULL`, o.ID, bt.Fee); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *ProviderReconciler) paypalPass(ctx context.Context, r *ReconcileReport) error {
	rows, err := p.DB.Query(ctx,
		`SELECT id, provider_payment_id, amount_cents, refunded_cents FROM orders
		 WHERE provider = 'paypal' AND status IN ('paid', 'needs_attention') AND provider_payment_id IS NOT NULL
		   AND (paid_at >= $1 OR provider_fee_cents IS NULL)
		 ORDER BY paid_at DESC LIMIT 500`, time.Now().Add(-p.window()))
	if err != nil {
		return err
	}
	type ppOrder struct {
		id, capture      string
		amount, refunded int64
	}
	var list []ppOrder
	for rows.Next() {
		var o ppOrder
		if err := rows.Scan(&o.id, &o.capture, &o.amount, &o.refunded); err != nil {
			rows.Close()
			return err
		}
		list = append(list, o)
	}
	rows.Close()
	for _, o := range list {
		c, err := p.PayPal.GetCapture(o.capture)
		if err != nil {
			r.issue(o.id, "paypal_lookup", "%v", err)
			continue
		}
		r.OrdersChecked++
		if c.AmountCents != o.amount || c.Currency != "USD" {
			r.issue(o.id, "paypal_amount", "PayPal captured %d %s, order is %d USD", c.AmountCents, c.Currency, o.amount)
		}
		want := "COMPLETED"
		switch {
		case o.refunded == o.amount && o.amount > 0:
			want = "REFUNDED"
		case o.refunded > 0:
			want = "PARTIALLY_REFUNDED"
		}
		if c.Status != want && !(want == "REFUNDED" && c.Status == "REVERSED") {
			r.issue(o.id, "paypal_status", "PayPal capture is %s, books expect %s", c.Status, want)
		}
		// PayPal's fee is charged to the host (the seller), not the
		// platform: recorded on the order for reference, not booked.
		if _, err := p.DB.Exec(ctx, `UPDATE orders SET provider_fee_cents = $2 WHERE id = $1 AND provider_fee_cents IS NULL`, o.id, c.FeeCents); err != nil {
			return err
		}
	}
	return nil
}

// saveReconcileRun stores a report for the dashboard.
func saveReconcileRun(ctx context.Context, db *pgxpool.Pool, r *ReconcileReport) {
	body, _ := json.Marshal(r)
	if _, err := db.Exec(ctx,
		`INSERT INTO reconciliation_runs (kind, started_at, finished_at, checked, issues, report)
		 VALUES ($1, $2, NOW(), $3, $4, $5)`, r.Kind, r.RanAt, r.OrdersChecked, len(r.Issues), body); err != nil {
		log.Printf("reconciliation: could not save %s run: %v", r.Kind, err)
	}
}

// reconcileLockID makes sure only one instance reconciles at a time.
const reconcileLockID = 727_301_003

// RunScheduledReconciliation runs both passes now, if no other instance
// is, and stores the reports.
func RunScheduledReconciliation(ctx context.Context, p *ProviderReconciler) {
	conn, err := p.DB.Acquire(ctx)
	if err != nil {
		log.Printf("reconciliation: %v", err)
		return
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, reconcileLockID).Scan(&got); err != nil || !got {
		return
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, reconcileLockID) //nolint:errcheck

	if pr, err := p.Run(ctx); err != nil {
		log.Printf("reconciliation: provider pass failed: %v", err)
	} else {
		saveReconcileRun(ctx, p.DB, pr)
		if len(pr.Issues) > 0 || len(pr.Repaired) > 0 {
			log.Printf("reconciliation: provider pass checked %d orders, %d issues, %d repaired",
				pr.OrdersChecked, len(pr.Issues), len(pr.Repaired))
		}
	}
	if br, err := ReconcileBooks(ctx, p.DB); err != nil {
		log.Printf("reconciliation: books pass failed: %v", err)
	} else {
		saveReconcileRun(ctx, p.DB, br)
		if len(br.Issues) > 0 || len(br.NeedsAttention) > 0 || br.FailedWebhooks > 0 {
			log.Printf("reconciliation: books checked %d orders: %d issues, %d orders need attention, %d webhook problems",
				br.OrdersChecked, len(br.Issues), len(br.NeedsAttention), br.FailedWebhooks)
		}
	}
}

// StartReconciler runs reconciliation every interval until ctx ends.
func StartReconciler(ctx context.Context, p *ProviderReconciler, every time.Duration) {
	if every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunScheduledReconciliation(ctx, p)
			}
		}
	}()
}
