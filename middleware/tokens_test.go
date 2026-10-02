package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "q7Vx9mT2pL4wZr8Ks1Nb6Hc3Jd5Fg0YeUaXiOoPt"

// Chat and payout tokens are the JWTs the API still signs. These cover the
// forged/invalid cases for them (sign-in sessions are covered in
// session_test.go).

type testClaims struct {
	EventID string `json:"event_id"`
	jwt.RegisteredClaims
}

func chatToken(t *testing.T, secret string, method jwt.SigningMethod, mutate func(*testClaims)) string {
	t.Helper()
	c := &testClaims{EventID: "event-1", RegisteredClaims: NewRegisteredClaims(AudienceChat, "event-1", time.Now().Add(time.Hour))}
	if mutate != nil {
		mutate(c)
	}
	var key any = []byte(secret)
	if method == jwt.SigningMethodNone {
		key = jwt.UnsafeAllowNoneSignatureType
	}
	s, err := jwt.NewWithClaims(method, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidChatTokenAccepted(t *testing.T) {
	if err := ParseToken(testSecret, chatToken(t, testSecret, jwt.SigningMethodHS256, nil), AudienceChat, &testClaims{}); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestForgedAndInvalidTokensRejected(t *testing.T) {
	hs := jwt.SigningMethodHS256
	valid := chatToken(t, testSecret, hs, nil)
	cases := map[string]string{
		"garbage":            "not.a.jwt",
		"wrong secret":       chatToken(t, "a-completely-different-secret-value-123", hs, nil),
		"alg none":           chatToken(t, testSecret, jwt.SigningMethodNone, nil),
		"other HMAC alg":     chatToken(t, testSecret, jwt.SigningMethodHS512, nil),
		"tampered signature": valid[:len(valid)-2] + "xx",
		"expired":            chatToken(t, testSecret, hs, func(c *testClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }),
		"no expiry":          chatToken(t, testSecret, hs, func(c *testClaims) { c.ExpiresAt = nil }),
		"wrong issuer":       chatToken(t, testSecret, hs, func(c *testClaims) { c.Issuer = "someone-else" }),
		"payout audience":    chatToken(t, testSecret, hs, func(c *testClaims) { c.Audience = jwt.ClaimStrings{AudiencePayout} }),
		"no audience":        chatToken(t, testSecret, hs, func(c *testClaims) { c.Audience = nil; c.Issuer = "" }),
		"issued in future":   chatToken(t, testSecret, hs, func(c *testClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) }),
	}
	for name, tok := range cases {
		if err := ParseToken(testSecret, tok, AudienceChat, &testClaims{}); err == nil {
			t.Errorf("%s: accepted", name)
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
	if got := run(sign(AudienceChat, "user-1")); got != 403 {
		t.Errorf("chat-audience token used as payout token: got %d, want 403", got)
	}
	if got := run(sign(AudiencePayout, "user-2")); got != 403 {
		t.Errorf("another user's payout token: got %d, want 403", got)
	}
}

func TestSigningWithoutSecretFails(t *testing.T) {
	if _, err := SignToken("", &testClaims{}); err == nil {
		t.Fatal("signing with an empty secret must fail")
	}
	if err := ParseToken("", "x", AudienceChat, &testClaims{}); err == nil {
		t.Fatal("parsing with an empty secret must fail")
	}
}
