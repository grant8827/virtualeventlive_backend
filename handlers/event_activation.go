package handlers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/services"
)

func activateVenuePaidEvent(ctx context.Context, db *pgxpool.Pool, ivs *services.IVSService, eventID string) error {
	if _, err := db.Exec(ctx,
		`UPDATE events SET venue_paid = true, is_active = true WHERE id = $1`,
		eventID,
	); err != nil {
		return err
	}

	if ivs == nil || !ivs.Enabled {
		return nil
	}

	var (
		title      string
		channelARN *string
	)
	if err := db.QueryRow(ctx,
		`SELECT title, aws_channel_arn FROM events WHERE id = $1`,
		eventID,
	).Scan(&title, &channelARN); err != nil {
		return err
	}
	if channelARN != nil && *channelARN != "" {
		return nil
	}

	creds, err := ivs.ProvisionChannel(ctx, title)
	if err != nil {
		fmt.Printf("IVS provision failed (non-fatal): %v\n", err)
		return nil
	}

	_, _ = db.Exec(ctx,
		`UPDATE events SET
			aws_channel_arn   = $1,
			stream_ingest_url = $2,
			stream_key_value  = $3,
			aws_playback_url  = $4
		 WHERE id = $5`,
		creds.ChannelARN, creds.IngestURL, creds.StreamKey, creds.PlaybackURL, eventID,
	)

	return nil
}
