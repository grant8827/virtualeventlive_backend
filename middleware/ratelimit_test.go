package middleware

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

func limitedApp(l *RateLimiter, max int64) *fiber.App {
	app := fiber.New()
	app.Post("/x", RateLimit(l, Limit{Name: "t" + strconv.FormatInt(time.Now().UnixNano(), 10), Max: max, Window: time.Minute}),
		func(c *fiber.Ctx) error { return c.SendStatus(200) })
	return app
}

func post(t *testing.T, app *fiber.App, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

func TestRateLimitBlocksAfterMaxWithRetryAfter(t *testing.T) {
	app := limitedApp(NewRateLimiter(nil), 3)
	for i := 1; i <= 3; i++ {
		if s, _ := post(t, app, nil); s != 200 {
			t.Fatalf("request %d: %d", i, s)
		}
	}
	s, retry := post(t, app, nil)
	if s != fiber.StatusTooManyRequests || retry == "" {
		t.Fatalf("4th request: %d, Retry-After %q", s, retry)
	}
}

// Behind one trusted proxy the client is the rightmost X-Forwarded-For entry;
// a client can't dodge limits by inventing addresses on the left.
func TestClientIPIgnoresSpoofedForwardedFor(t *testing.T) {
	SetTrustedProxyHops(1)
	defer SetTrustedProxyHops(0)
	app := limitedApp(NewRateLimiter(nil), 2)
	for i := 0; i < 5; i++ {
		post(t, app, map[string]string{"X-Forwarded-For": "10.0.0." + strconv.Itoa(i) + ", 203.0.113.7"})
	}
	if s, _ := post(t, app, map[string]string{"X-Forwarded-For": "198.51.100.99, 203.0.113.7"}); s != fiber.StatusTooManyRequests {
		t.Fatalf("spoofed left-hand addresses evaded the limit: %d", s)
	}
	// A genuinely different client (different proxy-appended address) isn't blocked.
	if s, _ := post(t, app, map[string]string{"X-Forwarded-For": "203.0.113.8"}); s != 200 {
		t.Fatalf("other client blocked: %d", s)
	}
}

func TestLimiterCountAndReset(t *testing.T) {
	l := NewRateLimiter(nil)
	ctx := context.Background()
	key := "k" + strconv.FormatInt(time.Now().UnixNano(), 10)
	l.Hit(ctx, key, time.Minute)
	l.Hit(ctx, key, time.Minute)
	if n := l.Count(ctx, key, time.Minute); n != 2 {
		t.Fatalf("count %d", n)
	}
	l.Reset(ctx, key, time.Minute)
	if n := l.Count(ctx, key, time.Minute); n != 0 {
		t.Fatalf("after reset %d", n)
	}
}

// Two server instances sharing Redis enforce one combined limit. Uses a
// local Redis only (TEST_REDIS_URL, e.g. redis://localhost:6379/15).
func TestRateLimitIsSharedAcrossInstancesViaRedis(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil || (opt.Addr != "localhost:6379" && opt.Addr != "127.0.0.1:6379") {
		t.Fatalf("refusing non-local redis %q", url)
	}
	ctx := context.Background()
	a, b := NewRateLimiter(redis.NewClient(opt)), NewRateLimiter(redis.NewClient(opt))
	key := "shared-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	a.Hit(ctx, key, time.Minute)
	a.Hit(ctx, key, time.Minute)
	if n, _ := b.Hit(ctx, key, time.Minute); n != 3 {
		t.Fatalf("instance B saw count %d, want 3", n)
	}
	a.Reset(ctx, key, time.Minute)
	if n := b.Count(ctx, key, time.Minute); n != 0 {
		t.Fatalf("reset not shared: %d", n)
	}
}
