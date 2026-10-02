package config

import (
	"strings"
	"testing"
)

const strongSecret = "q7Vx9mT2pL4wZr8Ks1Nb6Hc3Jd5Fg0YeUaXiOoPt"

func TestProductionRefusesMissingSecret(t *testing.T) {
	c := &Config{AppEnv: "production", JWTSecret: ""}
	if err := c.Validate(); err == nil {
		t.Fatal("production must refuse to start without JWT_SECRET")
	}
}

func TestUnsetAppEnvIsTreatedAsProduction(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("JWT_SECRET", "")
	c := Load()
	if c.AppEnv != "production" {
		t.Fatalf("APP_ENV default = %q, want production", c.AppEnv)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("an unset APP_ENV with no secret must fail closed")
	}
}

func TestNoBuiltInFallbackSecret(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	if s := Load().JWTSecret; s != "" {
		t.Fatal("Load must not supply a default JWT secret")
	}
}

func TestProductionRejectsWeakSecrets(t *testing.T) {
	weak := map[string]string{
		"old default":         "change-me-in-production",
		"example placeholder": "change-me-to-a-long-random-secret",
		"short":               "abc123XYZ",
		"long but repetitive": strings.Repeat("ab", 20),
		"generic":             "secret",
	}
	for name, s := range weak {
		if err := ValidateJWTSecret(s, false); err == nil {
			t.Errorf("%s: production accepted a weak secret", name)
		}
	}
}

func TestProductionAcceptsStrongSecret(t *testing.T) {
	c := &Config{AppEnv: "production", JWTSecret: strongSecret}
	if err := c.Validate(); err != nil {
		t.Fatalf("strong secret rejected: %v", err)
	}
	// 64 random hex characters (openssl rand -hex 32) must also pass.
	hex := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if err := ValidateJWTSecret(hex, false); err != nil {
		t.Fatalf("hex secret rejected: %v", err)
	}
}

func TestDevelopmentAllowsExplicitDevSecretButNotEmpty(t *testing.T) {
	dev := &Config{AppEnv: "development", JWTSecret: "local-dev-secret"}
	if err := dev.Validate(); err != nil {
		t.Fatalf("development should allow an explicit dev secret: %v", err)
	}
	empty := &Config{AppEnv: "test", JWTSecret: ""}
	if err := empty.Validate(); err == nil {
		t.Fatal("even test mode needs an explicitly configured secret")
	}
}

func TestUnknownAppEnvRejected(t *testing.T) {
	c := &Config{AppEnv: "staging-ish", JWTSecret: strongSecret}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown APP_ENV must be rejected")
	}
}

func TestValidationErrorNeverContainsSecret(t *testing.T) {
	secret := "change-me-SUPERSECRETVALUE"
	err := ValidateJWTSecret(secret, false)
	if err == nil || strings.Contains(err.Error(), "SUPERSECRETVALUE") {
		t.Fatalf("error must exist and must not echo the secret: %v", err)
	}
}

func fullPayPal(c *Config) {
	c.PaypalEnabled = true
	c.PaypalClientID, c.PaypalClientSecret, c.PaypalWebhookID = "id", "secret", "wh"
	c.PaypalPartnerMerchantID, c.PaypalPartnerAttributionID = "merchant", "attribution"
	c.PaypalEnvironment = "sandbox"
}

func TestPayPalOffByDefault(t *testing.T) {
	t.Setenv("PAYPAL_ENABLED", "")
	t.Setenv("PAYPAL_CLIENT_ID", "would-otherwise-enable-it")
	t.Setenv("PAYPAL_CLIENT_SECRET", "x")
	c := Load()
	if c.PaypalEnabled {
		t.Fatal("PayPal must stay off unless PAYPAL_ENABLED=true")
	}
	if c.PaypalEnvironment != "" {
		t.Fatal("PAYPAL_ENVIRONMENT must not silently default to sandbox")
	}
}

func TestPayPalRefusesIncompleteOrAmbiguousConfig(t *testing.T) {
	base := func() *Config { c := &Config{AppEnv: "production", JWTSecret: strongSecret}; fullPayPal(c); return c }
	if err := base().Validate(); err != nil {
		t.Fatalf("complete PayPal config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no webhook id":    func(c *Config) { c.PaypalWebhookID = "" },
		"no secret":        func(c *Config) { c.PaypalClientSecret = "" },
		"no partner id":    func(c *Config) { c.PaypalPartnerMerchantID = "" },
		"no environment":   func(c *Config) { c.PaypalEnvironment = "" },
		"typo environment": func(c *Config) { c.PaypalEnvironment = "production" },
	} {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: incomplete PayPal config accepted", name)
		}
	}
}

func TestWiPayVenueFeeRefusedInProduction(t *testing.T) {
	prod := &Config{AppEnv: "production", JWTSecret: strongSecret, WipayVenueFeeEnabled: true}
	if err := prod.Validate(); err == nil {
		t.Fatal("unverifiable WiPay activation must not be enabled in production")
	}
	dev := &Config{AppEnv: "development", JWTSecret: "dev", WipayVenueFeeEnabled: true}
	if err := dev.Validate(); err != nil {
		t.Fatalf("development may opt in explicitly: %v", err)
	}
}

func TestPaymentLimitsValidated(t *testing.T) {
	base := func() *Config {
		return &Config{AppEnv: "production", JWTSecret: strongSecret, PlatformFeeBPS: 1000, ReconcileInterval: "6h"}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"negative fee":       func(c *Config) { c.PlatformFeeBPS = -1 },
		"fee over 50%":       func(c *Config) { c.PlatformFeeBPS = 5001 },
		"negative per-buyer": func(c *Config) { c.MaxTicketsPerBuyer = -3 },
		"bad interval":       func(c *Config) { c.ReconcileInterval = "soon" },
		"interval too short": func(c *Config) { c.ReconcileInterval = "30s" },
	} {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	off := base()
	off.ReconcileInterval = "0"
	if d, err := off.ReconcileEvery(); err != nil || d != 0 {
		t.Errorf("RECONCILE_INTERVAL=0: %v %v", d, err)
	}
	if base().TicketsPerBuyer() != 10 {
		t.Error("per-buyer default is not 10")
	}
}
