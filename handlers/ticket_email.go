package handlers

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

// sendTicketEmail emails the buyer their ticket after it has been saved.
// Every purchase path (Stripe, PayPal, free tickets) calls this once the
// ticket row exists. It runs in the background so a slow mail server never
// holds up a checkout redirect or a payment webhook.
func sendTicketEmail(db *pgxpool.Pool, email *services.EmailService, toEmail, accessCode string) {
	if email == nil || toEmail == "" || accessCode == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var (
			t     = services.TicketEmail{AccessCode: accessCode}
			venue *string
		)
		err := db.QueryRow(ctx,
			`SELECT t.serial_no, e.id, e.title, e.start_time, e.ticket_name, e.ticket_type, e.venue_address
			 FROM tickets t JOIN events e ON e.id = t.event_id
			 WHERE t.access_token = $1`, accessCode,
		).Scan(&t.SerialNo, &t.EventID, &t.EventTitle, &t.StartsAt, &t.TicketName, &t.TicketType, &venue)
		if err != nil {
			log.Printf("ticket email: lookup for %s failed: %v", toEmail, err)
			return
		}
		if venue != nil {
			t.VenueAddress = *venue
		}
		if err := email.SendTicketConfirmation(toEmail, t); err != nil {
			log.Printf("ticket email to %s failed: %v", toEmail, err)
		}
	}()
}
