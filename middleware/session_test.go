package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/database"
)

// Same guard as the handler tests: only a local database with "test" in its
// name, from TEST_DATABASE_URL; skipped otherwise.
var (
	mwPoolOnce sync.Once
	mwPool     *pgxpool.Pool
	mwPoolErr  error
)

func sessionTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	mwPoolOnce.Do(func() {
		u, err := url.Parse(dsn)
		if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || !strings.Contains(u.Path, "test") {
			mwPoolErr = fmt.Errorf("refusing non-local or non-test database")
			return
		}
		if mwPool, mwPoolErr = database.ConnectPostgres(dsn); mwPoolErr == nil {
			mwPoolErr = database.RunMigrations(mwPool)
		}
	})
	if mwPoolErr != nil {
		t.Fatal(mwPoolErr)
	}
	return mwPool
}

func sessionTestUser(t *testing.T, db *pgxpool.Pool, status string) string {
	t.Helper()
	var id string
	email := fmt.Sprintf("sess-%d@phase1.test", time.Now().UnixNano())
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role, status) VALUES ($1, 'x', 'host', $2) RETURNING id`, email, status,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

var prodCookies = SessionConfig{Secure: true}

func protectedApp(db *pgxpool.Pool) *fiber.App {
	app := fiber.New()
	app.Get("/me", RequireSession(db, prodCookies), func(c *fiber.Ctx) error {
		return c.SendString(c.Locals("user_id").(string))
	})
	return app
}

func call(t *testing.T, app *fiber.App, method, path, cookie string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: prodCookies.CookieName(), Value: cookie})
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func sessionCookieFrom(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == prodCookies.CookieName() {
			return c
		}
	}
	return nil
}

func TestSessionCookieIsHttpOnlySecureLaxHostPrefixed(t *testing.T) {
	app := fiber.New()
	exp := time.Now().Add(time.Hour)
	app.Get("/", func(c *fiber.Ctx) error { SetSessionCookie(c, prodCookies, "tok", exp); return nil })
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	raw := resp.Header.Get("Set-Cookie")
	// Cookie attribute names are case-insensitive (Fiber writes "secure").
	for _, want := range []string{"__Host-vep_session=tok", "httponly", "secure", "samesite=lax", "path=/"} {
		if !strings.Contains(strings.ToLower(raw), strings.ToLower(want)) {
			t.Errorf("cookie %q missing %q", raw, want)
		}
	}
	if strings.Contains(strings.ToLower(raw), "domain=") {
		t.Error("__Host- cookie must not set a Domain")
	}
}

func TestRequireSessionAcceptsOnlyValidSessions(t *testing.T) {
	db := sessionTestDB(t)
	ctx := context.Background()
	user := sessionTestUser(t, db, "active")
	token, _, err := CreateSession(ctx, db, user, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	app := protectedApp(db)

	if r := call(t, app, "GET", "/me", token, nil); r.StatusCode != 200 {
		t.Fatalf("valid session: %d", r.StatusCode)
	}
	if r := call(t, app, "GET", "/me", "", nil); r.StatusCode != 401 {
		t.Errorf("no cookie: %d", r.StatusCode)
	}
	if r := call(t, app, "GET", "/me", "made-up-session-id", nil); r.StatusCode != 401 {
		t.Errorf("unknown session: %d", r.StatusCode)
	}
	// Bearer tokens (the old scheme) are not accepted at all.
	if r := call(t, app, "GET", "/me", "", map[string]string{"Authorization": "Bearer " + token}); r.StatusCode != 401 {
		t.Errorf("bearer header: %d", r.StatusCode)
	}
	// The stored value is a hash; it doesn't work as a session ID.
	if r := call(t, app, "GET", "/me", HashSessionToken(token), nil); r.StatusCode != 401 {
		t.Errorf("hash used as session: %d", r.StatusCode)
	}
}

func TestRevokedIdleAndExpiredSessionsRejected(t *testing.T) {
	db := sessionTestDB(t)
	ctx := context.Background()
	user := sessionTestUser(t, db, "active")
	app := protectedApp(db)

	cases := map[string]string{
		"revoked":         `UPDATE sessions SET revoked_at = NOW() WHERE token_hash = $1`,
		"idle 25h":        `UPDATE sessions SET last_seen_at = NOW() - INTERVAL '25 hours' WHERE token_hash = $1`,
		"absolute expiry": `UPDATE sessions SET expires_at = NOW() - INTERVAL '1 minute' WHERE token_hash = $1`,
	}
	for name, sql := range cases {
		token, _, _ := CreateSession(ctx, db, user, "", "")
		if _, err := db.Exec(ctx, sql, HashSessionToken(token)); err != nil {
			t.Fatal(err)
		}
		if r := call(t, app, "GET", "/me", token, nil); r.StatusCode != 401 {
			t.Errorf("%s session: %d", name, r.StatusCode)
		}
	}
}

func TestSuspendedUserSessionIsRevoked(t *testing.T) {
	db := sessionTestDB(t)
	ctx := context.Background()
	user := sessionTestUser(t, db, "active")
	token, _, _ := CreateSession(ctx, db, user, "", "")
	_, _ = db.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, user)

	app := protectedApp(db)
	if r := call(t, app, "GET", "/me", token, nil); r.StatusCode != 401 {
		t.Fatalf("suspended: %d", r.StatusCode)
	}
	// Reactivating doesn't revive the old session.
	_, _ = db.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, user)
	if r := call(t, app, "GET", "/me", token, nil); r.StatusCode != 401 {
		t.Fatalf("session survived suspension: %d", r.StatusCode)
	}
}

func TestSessionRotationIssuesNewIDAndRetiresOld(t *testing.T) {
	db := sessionTestDB(t)
	ctx := context.Background()
	user := sessionTestUser(t, db, "active")
	oldToken, _, _ := CreateSession(ctx, db, user, "", "")
	_, _ = db.Exec(ctx, `UPDATE sessions SET rotated_at = NOW() - INTERVAL '2 hours' WHERE token_hash = $1`, HashSessionToken(oldToken))

	app := protectedApp(db)
	r := call(t, app, "GET", "/me", oldToken, nil)
	fresh := sessionCookieFrom(r)
	if r.StatusCode != 200 || fresh == nil || fresh.Value == "" || fresh.Value == oldToken {
		t.Fatalf("rotation: status %d, new cookie %v", r.StatusCode, fresh)
	}
	// In-flight requests with the old ID still work during the grace window...
	if r := call(t, app, "GET", "/me", oldToken, nil); r.StatusCode != 200 {
		t.Fatalf("old ID within grace: %d", r.StatusCode)
	}
	// ...and not after it.
	_, _ = db.Exec(ctx, `UPDATE sessions SET previous_valid_until = NOW() - INTERVAL '1 second' WHERE token_hash = $1`, HashSessionToken(fresh.Value))
	if r := call(t, app, "GET", "/me", oldToken, nil); r.StatusCode != 401 {
		t.Fatalf("old ID after grace: %d", r.StatusCode)
	}
	if r := call(t, app, "GET", "/me", fresh.Value, nil); r.StatusCode != 200 {
		t.Fatalf("new ID: %d", r.StatusCode)
	}
}

func TestRevokeUserSessionsKeepsOnlyTheExcepted(t *testing.T) {
	db := sessionTestDB(t)
	ctx := context.Background()
	user := sessionTestUser(t, db, "active")
	keep, _, _ := CreateSession(ctx, db, user, "", "")
	other, _, _ := CreateSession(ctx, db, user, "", "")
	s, err := LookupSession(ctx, db, keep)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := RevokeUserSessions(ctx, db, user, "test", s.SessionID); err != nil || n != 1 {
		t.Fatalf("revoked %d (%v), want 1", n, err)
	}
	app := protectedApp(db)
	if r := call(t, app, "GET", "/me", keep, nil); r.StatusCode != 200 {
		t.Error("excepted session was revoked")
	}
	if r := call(t, app, "GET", "/me", other, nil); r.StatusCode != 401 {
		t.Error("other session survived")
	}
}

func TestTrustedOriginCSRFCheck(t *testing.T) {
	allowed := NormalizeOrigins("https://virtualeventplus.com,http://localhost:5173")
	app := fiber.New()
	app.Post("/x", RequireTrustedOrigin(allowed, prodCookies.CookieName()), func(c *fiber.Ctx) error { return c.SendStatus(200) })
	app.Get("/x", RequireTrustedOrigin(allowed, prodCookies.CookieName()), func(c *fiber.Ctx) error { return c.SendStatus(200) })

	cases := []struct {
		name, method, cookie string
		headers              map[string]string
		want                 int
	}{
		{"own origin", "POST", "s", map[string]string{"Origin": "https://virtualeventplus.com"}, 200},
		{"evil origin", "POST", "s", map[string]string{"Origin": "https://evil.example"}, 403},
		{"lookalike origin", "POST", "s", map[string]string{"Origin": "https://virtualeventplus.com.evil.example"}, 403},
		{"no origin, own referer", "POST", "s", map[string]string{"Referer": "https://virtualeventplus.com/dashboard"}, 200},
		{"no origin, no referer", "POST", "s", nil, 403},
		{"null origin", "POST", "s", map[string]string{"Origin": "null"}, 403},
		{"no cookie (no ambient credential)", "POST", "", map[string]string{"Origin": "https://evil.example"}, 200},
		{"safe method", "GET", "s", map[string]string{"Origin": "https://evil.example"}, 200},
	}
	for _, tc := range cases {
		if r := call(t, app, tc.method, "/x", tc.cookie, tc.headers); r.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, r.StatusCode, tc.want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	app := fiber.New()
	app.Use(SecurityHeaders(true))
	app.Get("/x", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })
	r := call(t, app, "GET", "/x", "", nil)
	for h, want := range map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
	} {
		if got := r.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
}
