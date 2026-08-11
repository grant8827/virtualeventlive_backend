package config

import (
	"os"
	"strconv"
)

type Config struct {
	DatabaseURL         string
	RedisURL            string
	JWTSecret           string
	FrontendURL         string
	Port                string
	VenueFeeProvider    string
	StripeSecretKey     string
	StripeWebhookSecret string
	ResendAPIKey        string
	FromEmail           string
	HourlyRate          float64
	AWSAccessKeyID      string
	AWSSecretAccessKey  string
	AWSRegion           string
	S3BucketName        string
	S3Region            string

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

	// PayPal — Payouts API (https://developer.paypal.com/docs/payouts/).
	PaypalClientID     string
	PaypalClientSecret string
	PaypalEnvironment  string
}

func Load() *Config {
	return &Config{
		DatabaseURL:         getEnv("DATABASE_URL", ""),
		RedisURL:            getEnv("REDIS_URL", "redis://localhost:6379"),
		JWTSecret:           getEnv("JWT_SECRET", "change-me-in-production"),
		FrontendURL:         getEnv("FRONTEND_URL", "http://localhost:3000"),
		Port:                getEnv("PORT", "8080"),
		VenueFeeProvider:    getEnv("VENUE_FEE_PROVIDER", "auto"),
		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		ResendAPIKey:        getEnv("RESEND_API_KEY", ""),
		FromEmail:           getEnv("FROM_EMAIL", "tickets@vertualeventlive.com"),
		HourlyRate:          getFloat("HOURLY_RATE", 20.0),
		AWSAccessKeyID:      getEnv("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey:  getEnv("AWS_SECRET_ACCESS_KEY", ""),
		AWSRegion:           getEnv("AWS_REGION", "us-east-1"),
		S3BucketName:        getEnv("S3_BUCKET_NAME", ""),
		S3Region:            getEnv("S3_REGION", getEnv("AWS_REGION", "us-east-1")),

		WipayAccountNumber: getEnv("WIPAY_ACCOUNT_NUMBER", ""),
		WipayCheckoutURL:   getEnv("WIPAY_CHECKOUT_URL", ""),
		WipayCountryCode:   getEnv("WIPAY_COUNTRY_CODE", "JM"),
		WipayCurrency:      getEnv("WIPAY_CURRENCY", "USD"),
		WipayFeeStructure:  getEnv("WIPAY_FEE_STRUCTURE", "merchant_absorb"),
		WipayAPIBaseURL:    getEnv("WIPAY_API_BASE_URL", ""),
		WipayAPIKey:        getEnv("WIPAY_API_KEY", ""),
		WipayEnvironment:   getEnv("WIPAY_ENVIRONMENT", "sandbox"),

		PaypalClientID:     getEnv("PAYPAL_CLIENT_ID", ""),
		PaypalClientSecret: getEnv("PAYPAL_CLIENT_SECRET", ""),
		PaypalEnvironment:  getEnv("PAYPAL_ENVIRONMENT", "sandbox"),
	}
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
