package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "q7Vx9mT2pL4wZr8Ks1Nb6Hc3Jd5Fg0YeUaXiOoPt"

func sessionToken(t *testing.T, secret string, mutate func(*Claims)) string {
	t.Helper()
	c := &Claims{UserID: "user-1", Role: "host",
		RegisteredClaims: NewRegisteredClaims(AudienceSession, "user-1", time.Now().Add(time.Hour))}
	if mutate != nil {
		mutate(c)
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// protectedStatus runs a request through Protected and returns the status.
func protectedStatus(t *testing.T, token string) int {
	t.Helper()
	app := fiber.New()
	app.Get("/x", Protected(testSecret), func(c *fiber.Ctx) error { return c.SendStatus(200) })
	req := httptest.NewRequest("GET", "/x", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestValidSessionTokenAccepted(t *testing.T) {
	if got := protectedStatus(t, sessionToken(t, testSecret, nil)); got != 200 {
		t.Fatalf("valid token: got %d", got)
	}
}

func TestForgedAndInvalidTokensRejected(t *testing.T) {
	noneToken, _ := jwt.NewWithClaims(jwt.SigningMethodNone, &Claims{UserID: "user-1", Role: "superuser",
		RegisteredClaims: NewRegisteredClaims(AudienceSession, "user-1", time.Now().Add(time.Hour))}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, &Claims{UserID: "user-1",
		RegisteredClaims: NewRegisteredClaims(AudienceSession, "user-1", time.Now().Add(time.Hour))}).
		SignedString([]byte(testSecret))
	valid := sessionToken(t, testSecret, nil)

	cases := map[string]string{
		"no token":           "",
		"garbage":            "not.a.jwt",
		"wrong secret":       sessionToken(t, "a-completely-different-secret-value-123", nil),
		"alg none":           noneToken,
		"other HMAC alg":     hs512,
		"tampered signature": valid[:len(valid)-2] + "xx",
		"expired":            sessionToken(t, testSecret, func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }),
		"no expiry":          sessionToken(t, testSecret, func(c *Claims) { c.ExpiresAt = nil }),
		"wrong issuer":       sessionToken(t, testSecret, func(c *Claims) { c.Issuer = "someone-else" }),
		"chat audience":      sessionToken(t, testSecret, func(c *Claims) { c.Audience = jwt.ClaimStrings{AudienceChat} }),
		"payout audience":    sessionToken(t, testSecret, func(c *Claims) { c.Audience = jwt.ClaimStrings{AudiencePayout} }),
		"legacy no audience": sessionToken(t, testSecret, func(c *Claims) { c.Audience = nil; c.Issuer = "" }),
		"empty user id":      sessionToken(t, testSecret, func(c *Claims) { c.UserID = "" }),
		"issued in future":   sessionToken(t, testSecret, func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) }),
	}
	for name, tok := range cases {
		if got := protectedStatus(t, tok); got != 401 {
			t.Errorf("%s: got %d, want 401", name, got)
		}
	}
}

func TestPayoutTokenRequiresPayoutAudienceAndSameUser(t *testing.T) {
	sign := func(aud, user string) string {
		s, _ := SignToken(testSecret, &PayoutClaims{UserID: user, Scope: "payout",
			RegisteredClaims: NewRegisteredClaims(aud, user, time.Now().Add(time.Minute))})
		return s
	}
	run := func(payoutTok string) int {
		app := fiber.New()
		app.Get("/x", func(c *fiber.Ctx) error { c.Locals("user_id", "user-1"); return c.Next() },
			RequirePayoutUnlock(testSecret), func(c *fiber.Ctx) error { return c.SendStatus(200) })
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("X-Payout-Token", payoutTok)
		resp, _ := app.Test(req)
		return resp.StatusCode
	}
	if got := run(sign(AudiencePayout, "user-1")); got != 200 {
		t.Fatalf("valid payout token: got %d", got)
	}
	if got := run(sign(AudienceSession, "user-1")); got != 403 {
		t.Errorf("session-audience token used as payout token: got %d, want 403", got)
	}
	if got := run(sign(AudiencePayout, "user-2")); got != 403 {
		t.Errorf("another user's payout token: got %d, want 403", got)
	}
}

func TestSigningWithoutSecretFails(t *testing.T) {
	if _, err := SignToken("", &Claims{}); err == nil {
		t.Fatal("signing with an empty secret must fail")
	}
	if err := ParseToken("", "x", AudienceSession, &Claims{}); err == nil {
		t.Fatal("parsing with an empty secret must fail")
	}
}
