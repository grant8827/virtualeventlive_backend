package config

import (
	"errors"
	"fmt"
	"strings"
)

// minProductionSecretLen is the shortest JWT secret accepted outside
// development/test: 32 bytes is HS256's key size.
const minProductionSecretLen = 32

// knownWeakSecrets are values that have appeared as defaults or examples and
// must never sign production tokens.
var knownWeakSecrets = []string{
	"change-me-in-production",
	"change-me-to-a-long-random-secret",
	"secret",
	"changeme",
}

// IsDevelopment reports whether the server runs in an explicitly
// non-production mode. An unset APP_ENV counts as production.
func (c *Config) IsDevelopment() bool {
	return c.AppEnv == "development" || c.AppEnv == "test"
}

// Validate checks settings the server must not start without. main() calls
// it before serving and exits on error.
func (c *Config) Validate() error {
	switch c.AppEnv {
	case "production", "development", "test":
	default:
		return fmt.Errorf("APP_ENV must be production, development or test (got %q)", c.AppEnv)
	}
	return ValidateJWTSecret(c.JWTSecret, c.IsDevelopment())
}

// ValidateJWTSecret rejects missing, placeholder and low-entropy secrets.
// In development/test any explicitly configured, non-empty secret is allowed.
// It never includes the secret in its error.
func ValidateJWTSecret(secret string, development bool) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("JWT_SECRET is not set")
	}
	if development {
		return nil
	}
	lower := strings.ToLower(secret)
	if strings.Contains(lower, "change-me") || strings.Contains(lower, "changeme") {
		return errors.New("JWT_SECRET is a known placeholder value; generate a random one")
	}
	for _, weak := range knownWeakSecrets {
		if lower == weak {
			return errors.New("JWT_SECRET is a known placeholder value; generate a random one")
		}
	}
	if len(secret) < minProductionSecretLen {
		return fmt.Errorf("JWT_SECRET must be at least %d characters in production", minProductionSecretLen)
	}
	distinct := map[rune]struct{}{}
	for _, r := range secret {
		distinct[r] = struct{}{}
	}
	// A random hex string uses ~16 symbols, base64 ~60; human phrases and
	// repeated characters fall well below this.
	if len(distinct) < 12 {
		return errors.New("JWT_SECRET is not random enough; generate one with: openssl rand -base64 48")
	}
	return nil
}
