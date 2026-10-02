package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL string
	RedisURL    string
	JWTSecret   string
	// AppEnv is "production" (the default when unset), "development" or
	// "test". Only development/test may run with a weak JWT secret.
	AppEnv string
	// TrustedProxyHops: reverse proxies in front of the API (Railway's edge
	// = 1). The client IP is read from that position in X-Forwarded-For.
	// TRUSTED_PROXY_HOPS overrides; default 1 in production, 0 in dev/test.
	TrustedProxyHops int
	FrontendURL      string
	// Comma-separated list of origins allowed to call the API cross-origin.
	// Kept separate from FrontendURL, which is reused elsewhere (Stripe
	// Connect redirect links, email links) as a single URL and would break
	// if it became a list.
	CorsAllowedOrigins  string
	Port                string
	VenueFeeProvider    string
	StripeSecretKey     string
	StripeWebhookSecret string
	ResendAPIKey        string
	FromEmail           string
	// SMTP (e.g. Mailtrap). When SMTPHost is set, email goes out over SMTP
	// instead of the Resend API.
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	HourlyRate   float64
	// AllowPaymentBypass enables the "Bypass Payment & Activate" testing
	// button for hosts. Leave off in production.
	AllowPaymentBypass bool
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSRegion          string
	S3BucketName       string

	// IVS private playback. IVSPlaybackPrivateKey is the PEM private half of
	// an ECDSA P-384 playback key pair whose public half is imported into
	// IVS. With IVSPlaybackAuthRequired, new channels are created with
	// authorization on and playback URLs are only ever handed out signed.
	IVSPlaybackPrivateKey   string
	IVSPlaybackAuthRequired bool
	// Origin allowed to play signed streams, e.g. https://virtualeventplus.com
	// ("*" when empty, which mobile apps need).
	IVSPlaybackAllowOrigin string
	S3Region               string

	// WiPay — Caribbean payout rail. Host payouts are sent to WipayAccountNumber
	// via WipayAPIBaseURL once WiPay confirms their disbursement endpoint contract;
	// unset until then, so payouts stay queued as "pending" rather than firing blind.
	WipayAccountNumber string
	WipayCheckoutURL   string
	WipayCountryCode   string
	WipayCurrency      string
	WipayFeeStructure  string
	WipayAPIBaseURL    string
	WipayAPIKey        string
	WipayEnvironment   string
	// WipayVenueFeeEnabled turns on the legacy WiPay booking-fee checkout.
	// Its return route can't verify payment with WiPay, so it is refused in
	// production and off by default everywhere.
	WipayVenueFeeEnabled bool

	// PayPal Commerce Platform multiparty checkout.
	// PaypalEnabled must be set explicitly (PAYPAL_ENABLED=true) for any
	// PayPal flow to run; when it is, Validate requires a complete config.
	PaypalEnabled              bool
	PaypalClientID             string
	PaypalClientSecret         string
	PaypalEnvironment          string
	PaypalPartnerMerchantID    string
	PaypalPartnerAttributionID string
	PaypalWebhookID            string

	// Orders and accounting (Phase 2).
	// PlatformFeeBPS is the commission on ticket sales in basis points
	// (1000 = 10%). Each order stores the rule it was sold under.
	PlatformFeeBPS int
	// MaxTicketsPerBuyer caps paid tickets per buyer email per event
	// (issued plus reserved). Free tickets are always one per email.
	MaxTicketsPerBuyer int
	// ReconcileInterval runs provider reconciliation in the background
	// (e.g. "6h"); "0" turns it off. One instance runs it at a time.
	ReconcileInterval string
}

func Load() *Config {
	return &Config{
		DatabaseURL: getEnv("DATABASE_URL", ""),
		RedisURL:    getEnv("REDIS_URL", "redis://localhost:6379"),
		// No fallback: a missing secret must stop the server (see Validate),
		// never silently sign tokens with a known value.
		JWTSecret:        getEnv("JWT_SECRET", ""),
		AppEnv:           strings.ToLower(strings.TrimSpace(getEnv("APP_ENV", "production"))),
		TrustedProxyHops: trustedProxyHops(),
		FrontendURL:      getEnv("FRONTEND_URL", "http://localhost:3000"),
		CorsAllowedOrigins: getEnv("CORS_ALLOWED_ORIGINS",
			"https://virtualeventplus.com,https://www.virtualeventplus.com,https://virtualeventlivefrontend-production.up.railway.app,http://localhost:5173"),
		Port:                getEnv("PORT", "8080"),
		VenueFeeProvider:    getEnv("VENUE_FEE_PROVIDER", "auto"),
		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		ResendAPIKey:        getEnv("RESEND_API_KEY", ""),
		FromEmail:           getEnv("FROM_EMAIL", "tickets@vertualeventlive.com"),
		SMTPHost:            getEnv("SMTP_HOST", ""),
		SMTPPort:            getEnv("SMTP_PORT", "587"),
		SMTPUsername:        getEnv("SMTP_USERNAME", ""),
		SMTPPassword:        getEnv("SMTP_PASSWORD", ""),
		HourlyRate:          getFloat("HOURLY_RATE", 15.0),
		AllowPaymentBypass:  getEnv("ALLOW_PAYMENT_BYPASS", "false") == "true",
		AWSAccessKeyID:      getEnv("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey:  getEnv("AWS_SECRET_ACCESS_KEY", ""),
		AWSRegion:           getEnv("AWS_REGION", "us-east-1"),
		S3BucketName:        getEnv("S3_BUCKET_NAME", ""),

		IVSPlaybackPrivateKey:   getEnv("IVS_PLAYBACK_PRIVATE_KEY", ""),
		IVSPlaybackAuthRequired: getEnv("IVS_PLAYBACK_AUTH", "") == "required",
		IVSPlaybackAllowOrigin:  getEnv("IVS_PLAYBACK_ALLOW_ORIGIN", ""),
		S3Region:                getEnv("S3_REGION", getEnv("AWS_REGION", "us-east-1")),

		WipayAccountNumber:   getEnv("WIPAY_ACCOUNT_NUMBER", ""),
		WipayCheckoutURL:     getEnv("WIPAY_CHECKOUT_URL", ""),
		WipayCountryCode:     getEnv("WIPAY_COUNTRY_CODE", "JM"),
		WipayCurrency:        getEnv("WIPAY_CURRENCY", "USD"),
		WipayFeeStructure:    getEnv("WIPAY_FEE_STRUCTURE", "merchant_absorb"),
		WipayAPIBaseURL:      getEnv("WIPAY_API_BASE_URL", ""),
		WipayAPIKey:          getEnv("WIPAY_API_KEY", ""),
		WipayEnvironment:     getEnv("WIPAY_ENVIRONMENT", "sandbox"),
		WipayVenueFeeEnabled: getEnv("WIPAY_VENUE_FEE_ENABLED", "") == "true",

		PaypalClientID:             getEnv("PAYPAL_CLIENT_ID", ""),
		PaypalClientSecret:         getEnv("PAYPAL_CLIENT_SECRET", ""),
		PaypalEnvironment:          strings.ToLower(getEnv("PAYPAL_ENVIRONMENT", "")),
		PaypalEnabled:              getEnv("PAYPAL_ENABLED", "") == "true",
		PaypalPartnerMerchantID:    getEnv("PAYPAL_PARTNER_MERCHANT_ID", ""),
		PaypalPartnerAttributionID: getEnv("PAYPAL_PARTNER_ATTRIBUTION_ID", ""),
		PaypalWebhookID:            getEnv("PAYPAL_WEBHOOK_ID", ""),

		PlatformFeeBPS:     getInt("PLATFORM_FEE_BPS", 1000),
		MaxTicketsPerBuyer: getInt("MAX_TICKETS_PER_BUYER", 10),
		ReconcileInterval:  getEnv("RECONCILE_INTERVAL", "6h"),
	}
}

func trustedProxyHops() int {
	if v, err := strconv.Atoi(os.Getenv("TRUSTED_PROXY_HOPS")); err == nil && v >= 0 {
		return v
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV"))) {
	case "development", "test":
		return 0
	}
	return 1
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return fallback
}

func getFloat(key string, fallback float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return fallback
	}
	return f
}

func getInt(key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return v
}

func (c *Config) MissingS3EnvKeys() []string {
	missing := []string{}
	if c.AWSAccessKeyID == "" {
		missing = append(missing, "AWS_ACCESS_KEY_ID")
	}
	if c.AWSSecretAccessKey == "" {
		missing = append(missing, "AWS_SECRET_ACCESS_KEY")
	}
	if c.S3BucketName == "" {
		missing = append(missing, "S3_BUCKET_NAME")
	}
	if c.S3Region == "" {
		missing = append(missing, "S3_REGION")
	}
	return missing
}
