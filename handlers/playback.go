package handlers

import (
	"errors"
	"log"

	"vertualeventlive/backend/services"
)

// PlaybackAccess turns a stored IVS playback URL into one a viewer may use.
// Callers must already have validated the viewer server-side (a valid ticket
// for an active event, or the event's host) — this is only ever reached
// after that check, and public endpoints never return playback URLs.
type PlaybackAccess struct {
	// Signer issues short-lived IVS playback tokens (nil when no key is set).
	Signer *services.PlaybackSigner
	// Required: never hand out an unsigned URL. Set once the IVS channels
	// have authorization turned on.
	Required bool
}

var errPlaybackUnavailable = errors.New("stream access isn't available right now")

// URL returns the URL to play, "" when the event has no stream yet, or
// errPlaybackUnavailable when signed playback is required but can't be issued.
func (p *PlaybackAccess) URL(playbackURL, channelARN *string) (string, error) {
	if playbackURL == nil || *playbackURL == "" {
		return "", nil
	}
	if p != nil && p.Signer != nil {
		arn := ""
		if channelARN != nil {
			arn = *channelARN
		}
		signed, _, err := p.Signer.SignURL(*playbackURL, arn)
		if err != nil {
			log.Printf("playback: could not sign URL: %v", err)
			return "", errPlaybackUnavailable
		}
		return signed, nil
	}
	if p != nil && p.Required {
		return "", errPlaybackUnavailable
	}
	// Signing not configured yet: the channel itself is still public in IVS,
	// so the plain URL is what plays. Still only returned after validation.
	return *playbackURL, nil
}
