package models

import "time"

type Ticket struct {
	ID              string    `json:"id"`
	EventID         string    `json:"event_id"`
	BuyerID         string    `json:"buyer_id"`
	AccessToken     string    `json:"access_token"`
	PurchasedAt     time.Time `json:"purchased_at"`
	PayoutProcessed bool      `json:"payout_processed"`
}
