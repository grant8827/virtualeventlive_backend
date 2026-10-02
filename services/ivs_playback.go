package services

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PlaybackSigner issues Amazon IVS playback authorization tokens.
//
// When an IVS channel has "authorization" turned on, IVS refuses to play it
// unless the playback URL carries ?token=<JWT> signed (ES384) by a playback
// key pair whose public key was imported into IVS. The server signs tokens
// only after it has validated the viewer's ticket, so a copied playback URL
// is useless on its own and stops working when its token expires.
type PlaybackSigner struct {
	key         *ecdsa.PrivateKey
	ttl         time.Duration
	allowOrigin string
}

// DefaultPlaybackTokenTTL bounds how long a signed playback URL can be used
// to start playback.
const DefaultPlaybackTokenTTL = 10 * time.Minute

// NewPlaybackSigner parses a P-384 private key in PEM form (PKCS#8 or SEC 1).
// Env vars often hold PEM with literal "\n"; those are accepted too.
// allowOrigin restricts which web origin may play ("*" when empty).
func NewPlaybackSigner(privateKeyPEM, allowOrigin string, ttl time.Duration) (*PlaybackSigner, error) {
	privateKeyPEM = strings.ReplaceAll(strings.TrimSpace(privateKeyPEM), `\n`, "\n")
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("IVS playback key: not PEM encoded")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("IVS playback key: not an ECDSA key")
		}
		key = ec
	} else if ec, err2 := x509.ParseECPrivateKey(block.Bytes); err2 == nil {
		key = ec
	} else {
		return nil, errors.New("IVS playback key: could not parse private key")
	}
	if key.Curve != elliptic.P384() {
		return nil, errors.New("IVS playback key: must be an ECDSA P-384 (secp384r1) key")
	}
	if ttl <= 0 {
		ttl = DefaultPlaybackTokenTTL
	}
	if allowOrigin == "" {
		allowOrigin = "*"
	}
	return &PlaybackSigner{key: key, ttl: ttl, allowOrigin: allowOrigin}, nil
}

// SignURL returns playbackURL with a short-lived authorization token for the
// given channel, and when that token expires.
func (s *PlaybackSigner) SignURL(playbackURL, channelARN string) (string, time.Time, error) {
	if channelARN == "" {
		return "", time.Time{}, errors.New("channel ARN is required to authorize playback")
	}
	expires := time.Now().Add(s.ttl)
	token, err := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"aws:channel-arn":                 channelARN,
		"aws:access-control-allow-origin": s.allowOrigin,
		"exp":                             expires.Unix(),
	}).SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign playback token: %w", err)
	}
	u, err := url.Parse(playbackURL)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("playback URL: %w", err)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), expires, nil
}

// PublicKey exposes the verification key (used by tests).
func (s *PlaybackSigner) PublicKey() *ecdsa.PublicKey { return &s.key.PublicKey }
