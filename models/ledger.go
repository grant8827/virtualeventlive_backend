package models

import "time"

type LedgerEntry struct {
	ID          string    `json:"id"`
	TicketID    string    `json:"ticket_id"`
	EventID     string    `json:"event_id"`
	GrossAmount float64   `json:"gross_amount"`
	StripeFee   float64   `json:"stripe_fee"`
	PlatformFee float64   `json:"platform_fee"`
	HostPayout  float64   `json:"host_payout"`
	SettledAt   time.Time `json:"settled_at"`
}
