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
	StripeSecretKey     string
	StripeWebhookSecret string
	ResendAPIKey        string
	FromEmail           string
	HourlyRate          float64
	AWSAccessKeyID      string
	AWSSecretAccessKey  string
	AWSRegion           string
}

func Load() *Config {
	return &Config{
		DatabaseURL:         getEnv("DATABASE_URL", ""),
		RedisURL:            getEnv("REDIS_URL", "redis://localhost:6379"),
		JWTSecret:           getEnv("JWT_SECRET", "change-me-in-production"),
		FrontendURL:         getEnv("FRONTEND_URL", "http://localhost:3000"),
		Port:                getEnv("PORT", "8080"),
		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		ResendAPIKey:        getEnv("RESEND_API_KEY", ""),
		FromEmail:           getEnv("FROM_EMAIL", "tickets@vertualeventlive.com"),
		HourlyRate:          getFloat("HOURLY_RATE", 20.0),
		AWSAccessKeyID:      getEnv("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey:  getEnv("AWS_SECRET_ACCESS_KEY", ""),
		AWSRegion:           getEnv("AWS_REGION", "us-east-1"),
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
