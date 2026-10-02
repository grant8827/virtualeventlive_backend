package middleware

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Every JWT the API signs carries this issuer and one audience naming what
// it's for. Validation pins the algorithm (HS256 only, so "none" or an
// RS256/HS256 confusion is rejected), the issuer, the audience, and requires
// an expiry — so a chat token can't be used as a payout token or vice versa.
// Sign-in itself no longer uses JWTs: it's a server-side session (session.go).
const (
	TokenIssuer    = "virtualeventplus"
	AudienceChat   = "chat"
	AudiencePayout = "payout"
)

// tokenLeeway tolerates small clock differences between servers.
const tokenLeeway = 30 * time.Second

var errNoSecret = errors.New("token secret is not configured")

// NewRegisteredClaims fills the standard claims for a token of the given
// audience that expires at expiresAt.
func NewRegisteredClaims(audience, subject string, expiresAt time.Time) jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Issuer:    TokenIssuer,
		Audience:  jwt.ClaimStrings{audience},
		Subject:   subject,
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
}

// SignToken signs claims with HS256.
func SignToken(secret string, claims jwt.Claims) (string, error) {
	if secret == "" {
		return "", errNoSecret
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// ParseToken verifies tokenStr was signed by us with HS256 for the given
// audience and is within its validity window, and decodes it into claims.
func ParseToken(secret, tokenStr, audience string, claims jwt.Claims) error {
	if secret == "" {
		return errNoSecret
	}
	token, err := jwt.ParseWithClaims(tokenStr, claims,
		func(t *jwt.Token) (interface{}, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(TokenIssuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(tokenLeeway),
	)
	if err != nil {
		return err
	}
	if !token.Valid {
		return errors.New("invalid token")
	}
	return nil
}
