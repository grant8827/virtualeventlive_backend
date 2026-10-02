package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

// errActivateCancelled: a booking fee arrived for an event a superuser has
// cancelled. The event stays cancelled; the payment needs a manual refund.
var errActivateCancelled = errors.New("event was cancelled")

// activateVenuePaidEvent marks the booking fee paid and provisions the IVS
// channel. It runs under a row lock and is idempotent, so the webhook and the
// return page (or duplicate webhooks) can't activate twice or create two
// channels. It never re-activates a cancelled event.
func activateVenuePaidEvent(ctx context.Context, db *pgxpool.Pool, ivs *services.IVSService, eventID string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var (
		title      string
		channelARN *string
		cancelled  bool
	)
	if err := tx.QueryRow(ctx,
		`SELECT title, aws_channel_arn, cancelled_at IS NOT NULL FROM events WHERE id = $1 FOR UPDATE`,
		eventID,
	).Scan(&title, &channelARN, &cancelled); err != nil {
		return err
	}
	if cancelled {
		return errActivateCancelled
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET venue_paid = true, is_active = true WHERE id = $1`, eventID,
	); err != nil {
		return err
	}

	if ivs != nil && ivs.Enabled && (channelARN == nil || *channelARN == "") {
		creds, err := ivs.ProvisionChannel(ctx, title)
		if err != nil {
			// Non-fatal: the host can retry channel setup from Go Live.
			fmt.Printf("IVS provision failed (non-fatal): %v\n", err)
		} else if _, err := tx.Exec(ctx,
			`UPDATE events SET
				aws_channel_arn   = $1,
				stream_ingest_url = $2,
				stream_key_value  = $3,
				aws_playback_url  = $4
			 WHERE id = $5`,
			creds.ChannelARN, creds.IngestURL, creds.StreamKey, creds.PlaybackURL, eventID,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
