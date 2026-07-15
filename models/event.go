package models

import "time"

type Event struct {
	ID             string    `json:"id"`
	HostID         string    `json:"host_id"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	StartTime      time.Time `json:"start_time"`
	TicketPrice    float64   `json:"ticket_price"`
	IsActive       bool      `json:"is_active"`
	PackageType    string    `json:"package_type"` // "flat_rental" | "revenue_share"
	TicketBgType   string    `json:"ticket_bg_type"`
	TicketBgValue  string    `json:"ticket_bg_value"`
	AWSStreamKey   string    `json:"aws_stream_key,omitempty"`
	AWSPlaybackURL string    `json:"aws_playback_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
