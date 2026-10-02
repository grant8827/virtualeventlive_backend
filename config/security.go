package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
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
	if err := ValidateJWTSecret(c.JWTSecret, c.IsDevelopment()); err != nil {
		return err
	}
	// Requiring signed playback without a signing key would make every
	// stream unwatchable; refuse to start instead.
	if c.IVSPlaybackAuthRequired && strings.TrimSpace(c.IVSPlaybackPrivateKey) == "" {
		return errors.New("IVS_PLAYBACK_AUTH=required but IVS_PLAYBACK_PRIVATE_KEY is not set")
	}
	if err := c.validatePayments(); err != nil {
		return err
	}
	return nil
}

// TicketsPerBuyer is the per-buyer, per-event cap on paid tickets.
func (c *Config) TicketsPerBuyer() int {
	if c.MaxTicketsPerBuyer <= 0 {
		return 10
	}
	return c.MaxTicketsPerBuyer
}

// ReconcileEvery is the background reconciliation interval; 0 means off.
func (c *Config) ReconcileEvery() (time.Duration, error) {
	v := strings.TrimSpace(c.ReconcileInterval)
	if v == "" || v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 5*time.Minute {
		return 0, fmt.Errorf("RECONCILE_INTERVAL must be a duration of at least 5m, or 0 (got %q)", v)
	}
	return d, nil
}

// validatePayments keeps incomplete or unverifiable payment integrations
// from running by accident.
func (c *Config) validatePayments() error {
	if c.PaypalEnabled {
		var missing []string
		for name, v := range map[string]string{
			"PAYPAL_CLIENT_ID":              c.PaypalClientID,
			"PAYPAL_CLIENT_SECRET":          c.PaypalClientSecret,
			"PAYPAL_WEBHOOK_ID":             c.PaypalWebhookID,
			"PAYPAL_PARTNER_MERCHANT_ID":    c.PaypalPartnerMerchantID,
			"PAYPAL_PARTNER_ATTRIBUTION_ID": c.PaypalPartnerAttributionID,
		} {
			if strings.TrimSpace(v) == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("PAYPAL_ENABLED=true but these are not set: %s", strings.Join(missing, ", "))
		}
		// No silent default: production must say which PayPal it talks to.
		if c.PaypalEnvironment != "live" && c.PaypalEnvironment != "sandbox" {
			return errors.New("PAYPAL_ENVIRONMENT must be set to live or sandbox when PAYPAL_ENABLED=true")
		}
	}
	if c.PlatformFeeBPS < 0 || c.PlatformFeeBPS > 5000 {
		return fmt.Errorf("PLATFORM_FEE_BPS must be between 0 and 5000 (got %d)", c.PlatformFeeBPS)
	}
	if c.MaxTicketsPerBuyer < 0 {
		return fmt.Errorf("MAX_TICKETS_PER_BUYER must be at least 1 (got %d)", c.MaxTicketsPerBuyer)
	}
	if _, err := c.ReconcileEvery(); err != nil {
		return err
	}
	if c.WipayVenueFeeEnabled && !c.IsDevelopment() {
		return errors.New("WIPAY_VENUE_FEE_ENABLED is not allowed in production: WiPay payments can't be verified server-side yet")
	}
	return nil
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
