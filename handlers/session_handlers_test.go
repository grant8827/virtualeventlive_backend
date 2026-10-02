package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/middleware"
)

var testSessionCfg = middleware.SessionConfig{Secure: true}

func createLoginUser(t *testing.T, password string) (id, email string) {
	t.Helper()
	db := testDB(t)
	email = "login-" + uniq() + "@phase1.test"
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role, status, email_verified_at) VALUES ($1, $2, 'host', 'active', NOW()) RETURNING id`,
		email, string(hash),
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id, email
}

func authApp(t *testing.T) (*fiber.App, *AuthHandler) {
	db := testDB(t)
	h := &AuthHandler{DB: db, Cfg: testConfig(), Session: testSessionCfg}
	app := fiber.New()
	requireSession := middleware.RequireSession(db, testSessionCfg)
	app.Post("/login", h.Login)
	app.Post("/logout", h.Logout)
	app.Get("/me", requireSession, h.Me)
	app.Post("/change-password", requireSession, h.ChangePassword)
	return app, h
}

func send(t *testing.T, app *fiber.App, method, path, body, session string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: testSessionCfg.CookieName(), Value: session})
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func loginSession(t *testing.T, app *fiber.App, email, password string) string {
	t.Helper()
	resp := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"`+password+`"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == testSessionCfg.CookieName() {
			return c.Value
		}
	}
	t.Fatal("login set no session cookie")
	return ""
}

func TestLoginSetsCookieAndReturnsNoToken(t *testing.T) {
	app, _ := authApp(t)
	_, email := createLoginUser(t, "correct-horse-1")
	resp := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"correct-horse-1"}`, "")
	body := make([]byte, 2048)
	n, _ := resp.Body.Read(body)
	if resp.StatusCode != 200 || strings.Contains(string(body[:n]), "token") {
		t.Fatalf("login response must not contain a token: %d %s", resp.StatusCode, body[:n])
	}
	cookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(strings.ToLower(cookie), "httponly") || !strings.Contains(cookie, "__Host-vep_session=") {
		t.Fatalf("session cookie missing or not HttpOnly: %q", cookie)
	}
	// The same failure answer for a wrong password and an unknown email.
	bad := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"wrong"}`, "")
	unknown := send(t, app, "POST", "/login", `{"email":"nobody-`+uniq()+`@phase1.test","password":"wrong"}`, "")
	if bad.StatusCode != 401 || unknown.StatusCode != 401 {
		t.Fatalf("bad password %d / unknown email %d, want 401/401", bad.StatusCode, unknown.StatusCode)
	}
}

func TestLogoutRevokesSessionServerSide(t *testing.T) {
	app, _ := authApp(t)
	_, email := createLoginUser(t, "correct-horse-1")
	session := loginSession(t, app, email, "correct-horse-1")
	if r := send(t, app, "GET", "/me", "", session); r.StatusCode != 200 {
		t.Fatalf("me before logout: %d", r.StatusCode)
	}
	send(t, app, "POST", "/logout", "", session)
	// A copy of the cookie kept after logout is useless.
	if r := send(t, app, "GET", "/me", "", session); r.StatusCode != 401 {
		t.Fatalf("session usable after logout: %d", r.StatusCode)
	}
}

func TestPasswordChangeSignsOutOtherDevices(t *testing.T) {
	app, _ := authApp(t)
	_, email := createLoginUser(t, "correct-horse-1")
	laptop := loginSession(t, app, email, "correct-horse-1")
	phone := loginSession(t, app, email, "correct-horse-1")

	r := send(t, app, "POST", "/change-password",
		`{"current_password":"correct-horse-1","new_password":"battery-staple-2","confirm_password":"battery-staple-2"}`, laptop)
	if r.StatusCode != 200 {
		t.Fatalf("change password: %d", r.StatusCode)
	}
	if r := send(t, app, "GET", "/me", "", laptop); r.StatusCode != 200 {
		t.Error("the device that changed the password was signed out")
	}
	if r := send(t, app, "GET", "/me", "", phone); r.StatusCode != 401 {
		t.Error("another device stayed signed in after a password change")
	}
}

func TestPasswordResetSignsOutEverywhere(t *testing.T) {
	db := testDB(t)
	app, h := authApp(t)
	id, email := createLoginUser(t, "correct-horse-1")
	session := loginSession(t, app, email, "correct-horse-1")

	token, hash, _ := newInvitationToken()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO password_resets (user_id, token_hash, expires_at) VALUES ($1, $2, NOW() + INTERVAL '1 hour')`, id, hash); err != nil {
		t.Fatal(err)
	}
	resetApp := fiber.New()
	resetApp.Post("/reset", h.ResetPassword)
	if r := send(t, resetApp, "POST", "/reset", `{"token":"`+token+`","password":"brand-new-pass-3"}`, ""); r.StatusCode != 200 {
		t.Fatalf("reset: %d", r.StatusCode)
	}
	if r := send(t, app, "GET", "/me", "", session); r.StatusCode != 401 {
		t.Fatal("session survived a password reset")
	}
}

func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	db := testDB(t)
	h := &AuthHandler{DB: db, Cfg: testConfig(), Session: testSessionCfg, Limiter: middleware.NewRateLimiter(nil)}
	app := fiber.New()
	app.Post("/login", h.Login)
	_, email := createLoginUser(t, "correct-horse-1")

	for i := 0; i < loginMaxFailures; i++ {
		if r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"wrong"}`, ""); r.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i+1, r.StatusCode)
		}
	}
	// Locked: even the right password is refused for now.
	if r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"correct-horse-1"}`, ""); r.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: %d", r.StatusCode)
	}
	// Unknown emails lock the same way, so lockout doesn't reveal accounts.
	ghost := "ghost-" + uniq() + "@phase1.test"
	for i := 0; i < loginMaxFailures; i++ {
		send(t, app, "POST", "/login", `{"email":"`+ghost+`","password":"wrong"}`, "")
	}
	if r := send(t, app, "POST", "/login", `{"email":"`+ghost+`","password":"wrong"}`, ""); r.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("unknown email not locked the same way: %d", r.StatusCode)
	}
}

func TestSuccessfulLoginResetsFailureCount(t *testing.T) {
	db := testDB(t)
	h := &AuthHandler{DB: db, Cfg: testConfig(), Session: testSessionCfg, Limiter: middleware.NewRateLimiter(nil)}
	app := fiber.New()
	app.Post("/login", h.Login)
	_, email := createLoginUser(t, "correct-horse-1")
	for i := 0; i < loginMaxFailures-1; i++ {
		send(t, app, "POST", "/login", `{"email":"`+email+`","password":"wrong"}`, "")
	}
	if r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"correct-horse-1"}`, ""); r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	if r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"wrong"}`, ""); r.StatusCode != 401 {
		t.Fatalf("failure count was not reset: %d", r.StatusCode)
	}
}
