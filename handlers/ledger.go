package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"vertualeventlive/backend/services"
)

// The ledger (PAY-05) is append-only and double-entry: every financial fact
// is one journal with an idempotency key and postings that sum to zero
// (enforced by the database at commit). Debits are positive, credits
// negative. Replaying a webhook re-posts nothing, because the key already
// exists.

const (
	acctHostPayable        = "host_payable"
	acctPlatformCommission = "platform_commission"
	acctBookingFeeRevenue  = "booking_fee_revenue"
	acctProcessingFees     = "processing_fees"
	acctDisputeLosses      = "dispute_losses"
)

// cashAccount is the platform's funds at the order's provider.
func cashAccount(provider string) string {
	switch provider {
	case "stripe", "paypal":
		return "cash_" + provider
	}
	return "cash_legacy"
}

type posting struct {
	Account string
	Cents   int64
}

type journal struct {
	Key      string
	Kind     string
	Order    *order
	Memo     string
	Postings []posting
}

// postJournal writes j once. It returns false, with nothing written, when
// the key was already posted or every posting is zero.
func postJournal(ctx context.Context, tx pgx.Tx, j journal) (bool, error) {
	lines := make([]posting, 0, len(j.Postings))
	var sum int64
	for _, p := range j.Postings {
		if p.Cents != 0 {
			lines = append(lines, p)
			sum += p.Cents
		}
	}
	if len(lines) == 0 {
		return false, nil
	}
	if sum != 0 || len(lines) < 2 {
		return false, fmt.Errorf("journal %s is unbalanced", j.Key)
	}
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO ledger_journal (idempotency_key, kind, order_id, event_id, host_id, currency, memo)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
		j.Key, j.Kind, j.Order.ID, j.Order.EventID, j.Order.HostID, j.Order.Currency, j.Memo,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("post journal %s: %w", j.Key, err)
	}
	for _, p := range lines {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_postings (journal_id, account, amount_cents) VALUES ($1, $2, $3)`,
			id, p.Account, p.Cents,
		); err != nil {
			return false, fmt.Errorf("post journal %s: %w", j.Key, err)
		}
	}
	return true, nil
}

// settlesInstantly: the host's share leaves the platform at checkout (Stripe
// destination charge, PayPal multiparty), so host_payable is cleared at once.
func (o *order) settlesInstantly() bool {
	return o.Kind == "ticket" && (o.PayoutGateway == "stripe" || o.PayoutGateway == "paypal")
}

// postSale books a paid order: the money in, the platform's commission (or
// the whole booking fee) and the host's share, settled to the host straight
// away when the gateway pays them at checkout.
func postSale(ctx context.Context, tx pgx.Tx, o *order) error {
	cash := cashAccount(o.Provider)
	if o.Kind == "booking_fee" {
		_, err := postJournal(ctx, tx, journal{Key: "booking_fee:" + o.ID, Kind: "booking_fee", Order: o, Postings: []posting{
			{cash, o.AmountCents}, {acctBookingFeeRevenue, -o.AmountCents},
		}})
		return err
	}
	hostShare := o.AmountCents - o.PlatformFeeCents
	if _, err := postJournal(ctx, tx, journal{Key: "sale:" + o.ID, Kind: "sale", Order: o, Postings: []posting{
		{cash, o.AmountCents}, {acctHostPayable, -hostShare}, {acctPlatformCommission, -o.PlatformFeeCents},
	}}); err != nil {
		return err
	}
	if o.settlesInstantly() {
		if _, err := postJournal(ctx, tx, journal{Key: "host_transfer:" + o.ID, Kind: "host_transfer", Order: o, Postings: []posting{
			{acctHostPayable, hostShare}, {cash, -hostShare},
		}}); err != nil {
			return err
		}
	}
	return nil
}

// refundSplit is how much of a refund comes out of the platform's
// commission: the sale's proportion, with the last refund taking whatever
// commission is left so a full refund returns exactly the original fee.
func (o *order) refundSplit(amount int64) (platform int64) {
	if o.Kind == "booking_fee" {
		return amount
	}
	remainingFee := o.PlatformFeeCents - o.PlatformFeeRefundedCents
	if o.RefundedCents+amount >= o.AmountCents {
		return remainingFee
	}
	return min(services.ProportionalShare(amount, o.PlatformFeeCents, o.AmountCents), remainingFee)
}

// postRefund books a refund of amount, platformPart of it from commission.
// Instantly-settled hosts have their share clawed back (Stripe
// reverse_transfer / PayPal refund from the seller).
func postRefund(ctx context.Context, tx pgx.Tx, o *order, refundKey string, amount, platformPart int64) error {
	cash := cashAccount(o.Provider)
	revenue := acctPlatformCommission
	if o.Kind == "booking_fee" {
		revenue = acctBookingFeeRevenue
	}
	hostPart := amount - platformPart
	if _, err := postJournal(ctx, tx, journal{Key: "refund:" + refundKey, Kind: "refund", Order: o, Postings: []posting{
		{revenue, platformPart}, {acctHostPayable, hostPart}, {cash, -amount},
	}}); err != nil {
		return err
	}
	if o.settlesInstantly() {
		if _, err := postJournal(ctx, tx, journal{Key: "host_transfer_reversal:" + refundKey, Kind: "host_transfer_reversal", Order: o, Postings: []posting{
			{cash, hostPart}, {acctHostPayable, -hostPart},
		}}); err != nil {
			return err
		}
	}
	return nil
}

// postRefundReversal undoes postRefund for a refund that later failed.
func postRefundReversal(ctx context.Context, tx pgx.Tx, o *order, refundKey string, amount, platformPart int64) error {
	cash := cashAccount(o.Provider)
	revenue := acctPlatformCommission
	if o.Kind == "booking_fee" {
		revenue = acctBookingFeeRevenue
	}
	hostPart := amount - platformPart
	if _, err := postJournal(ctx, tx, journal{Key: "refund_reversal:" + refundKey, Kind: "refund_reversal", Order: o, Postings: []posting{
		{revenue, -platformPart}, {acctHostPayable, -hostPart}, {cash, amount},
	}}); err != nil {
		return err
	}
	if o.settlesInstantly() {
		if _, err := postJournal(ctx, tx, journal{Key: "host_transfer_restore:" + refundKey, Kind: "host_transfer_restore", Order: o, Postings: []posting{
			{acctHostPayable, hostPart}, {cash, -hostPart},
		}}); err != nil {
			return err
		}
	}
	return nil
}
