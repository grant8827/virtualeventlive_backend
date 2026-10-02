package handlers

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"

	"vertualeventlive/backend/database"
	"vertualeventlive/backend/middleware"
)

func verifyApp(t *testing.T) (*fiber.App, *AuthHandler) {
	db := testDB(t)
	h := &AuthHandler{DB: db, Cfg: testConfig(), Session: testSessionCfg, Limiter: middleware.NewRateLimiter(nil)}
	app := fiber.New()
	app.Post("/login", h.Login)
	app.Post("/verify", h.VerifyEmail)
	app.Post("/resend", h.ResendVerification)
	app.Put("/profile", middleware.RequireSession(db, testSessionCfg), h.UpdateProfile)
	return app, h
}

func createUnverifiedStaff(t *testing.T, password string) (id, email string) {
	t.Helper()
	db := testDB(t)
	owner := createTestHost(t, db)
	email = "staff-" + uniq() + "@phase1.test"
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role, status, account_owner_id) VALUES ($1, $2, 'staff', 'active', $3) RETURNING id`,
		email, string(hash), owner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, email
}

// issueVerification creates a link like the server does and returns its token.
func issueVerification(t *testing.T, h *AuthHandler, userID, email, purpose string) string {
	t.Helper()
	ctx := context.Background()
	if err := h.verifier().start(ctx, userID, email, purpose); err != nil {
		t.Fatal(err)
	}
	// start() only stores the hash; replace it with one whose token we know.
	token, hash, _ := newInvitationToken()
	if _, err := h.DB.Exec(ctx, `UPDATE email_verifications SET token_hash = $3 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose, hash); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestUnverifiedStaffCannotSignInUntilVerified(t *testing.T) {
	app, h := verifyApp(t)
	id, email := createUnverifiedStaff(t, "correct-horse-1")

	r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"correct-horse-1"}`, "")
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != fiber.StatusForbidden || !strings.Contains(string(body), "email_unverified") {
		t.Fatalf("unverified login: %d %s", r.StatusCode, body)
	}
	if r.Header.Get("Set-Cookie") != "" {
		t.Fatal("an unverified account got a session cookie")
	}
	var links int
	_ = h.DB.QueryRow(context.Background(), `SELECT count(*) FROM email_verifications WHERE user_id = $1 AND used_at IS NULL`, id).Scan(&links)
	if links != 1 {
		t.Fatalf("expected a verification link to be issued, found %d", links)
	}

	token := issueVerification(t, h, id, email, "verify")
	if r := send(t, app, "POST", "/verify", `{"token":"`+token+`"}`, ""); r.StatusCode != 200 {
		t.Fatalf("verify: %d", r.StatusCode)
	}
	if r := send(t, app, "POST", "/login", `{"email":"`+email+`","password":"correct-horse-1"}`, ""); r.StatusCode != 200 {
		t.Fatalf("login after verifying: %d", r.StatusCode)
	}
	// Links work once.
	if r := send(t, app, "POST", "/verify", `{"token":"`+token+`"}`, ""); r.StatusCode != 400 {
		t.Fatalf("reused link: %d", r.StatusCode)
	}
}

func TestVerificationLinksRejectForgedAndExpired(t *testing.T) {
	app, h := verifyApp(t)
	id, email := createUnverifiedStaff(t, "correct-horse-1")
	token := issueVerification(t, h, id, email, "verify")
	_, _ = h.DB.Exec(context.Background(), `UPDATE email_verifications SET expires_at = NOW() - INTERVAL '1 minute' WHERE user_id = $1`, id)

	for name, tok := range map[string]string{"expired": token, "forged": "not-a-real-token", "empty": ""} {
		if r := send(t, app, "POST", "/verify", `{"token":"`+tok+`"}`, ""); r.StatusCode != 400 {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	var verified bool
	_ = h.DB.QueryRow(context.Background(), `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&verified)
	if verified {
		t.Fatal("account verified by an invalid link")
	}
}

// Changing the email doesn't take effect (or allow sign-in with the new
// address) until the new address is confirmed.
func TestEmailChangeNeedsConfirmation(t *testing.T) {
	app, h := verifyApp(t)
	id, email := createLoginUser(t, "correct-horse-1")
	session := loginSession(t, app, email, "correct-horse-1")
	newEmail := "new-" + uniq() + "@phase1.test"

	profile := `{"email":"` + newEmail + `","full_name":"A","phone":"1","address_line1":"a","city":"c","state":"s","postal_code":"p","country":"x"}`
	if r := send(t, app, "PUT", "/profile", profile, session); r.StatusCode != 200 {
		t.Fatalf("profile update: %d", r.StatusCode)
	}
	var current string
	_ = h.DB.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, id).Scan(&current)
	if current != email {
		t.Fatal("email changed before confirmation")
	}
	if r := send(t, app, "POST", "/login", `{"email":"`+newEmail+`","password":"correct-horse-1"}`, ""); r.StatusCode == 200 {
		t.Fatal("signed in with an unconfirmed address")
	}

	token := issueVerification(t, h, id, newEmail, "change")
	if r := send(t, app, "POST", "/verify", `{"token":"`+token+`"}`, ""); r.StatusCode != 200 {
		t.Fatalf("confirm change: %d", r.StatusCode)
	}
	_ = h.DB.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, id).Scan(&current)
	if current != newEmail {
		t.Fatalf("email after confirmation: %s", current)
	}

	// Another account's address can't be claimed.
	_, other := createLoginUser(t, "x-password-9")
	taken := `{"email":"` + other + `","full_name":"A","phone":"1","address_line1":"a","city":"c","state":"s","postal_code":"p","country":"x"}`
	session = loginSession(t, app, newEmail, "correct-horse-1")
	if r := send(t, app, "PUT", "/profile", taken, session); r.StatusCode != fiber.StatusConflict {
		t.Fatalf("taken email: %d", r.StatusCode)
	}
}

func TestResendVerificationIsGeneric(t *testing.T) {
	app, _ := verifyApp(t)
	_, email := createUnverifiedStaff(t, "correct-horse-1")
	r1 := send(t, app, "POST", "/resend", `{"email":"`+email+`"}`, "")
	b1, _ := io.ReadAll(r1.Body)
	r2 := send(t, app, "POST", "/resend", `{"email":"nobody-`+uniq()+`@phase1.test"}`, "")
	b2, _ := io.ReadAll(r2.Body)
	if r1.StatusCode != 202 || r2.StatusCode != 202 || string(b1) != string(b2) {
		t.Fatalf("resend reveals account existence: %d %s / %d %s", r1.StatusCode, b1, r2.StatusCode, b2)
	}
}

// Migrations re-run on every start; the one-time grandfathering must not
// verify accounts created afterwards.
func TestGrandfatheringRunsOnlyOnce(t *testing.T) {
	db := testDB(t)
	id, _ := createUnverifiedStaff(t, "correct-horse-1")
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	var verified bool
	_ = db.QueryRow(context.Background(), `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&verified)
	if verified {
		t.Fatal("re-running migrations verified a new account")
	}
}

// Audit events are written for sign-ins, without plain email addresses.
func TestLoginEventsAreAudited(t *testing.T) {
	app, h := verifyApp(t)
	id, email := createLoginUser(t, "correct-horse-1")
	send(t, app, "POST", "/login", `{"email":"`+email+`","password":"wrong"}`, "")
	loginSession(t, app, email, "correct-horse-1")

	var failed, success int
	var leaked bool
	ctx := context.Background()
	_ = h.DB.QueryRow(ctx, `SELECT count(*) FROM security_events WHERE event_type = 'login_failed' AND target_user_id = $1`, id).Scan(&failed)
	_ = h.DB.QueryRow(ctx, `SELECT count(*) FROM security_events WHERE event_type = 'login_success' AND actor_user_id = $1`, id).Scan(&success)
	_ = h.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM security_events WHERE details::text ILIKE '%' || $1 || '%')`, email).Scan(&leaked)
	if failed != 1 || success != 1 {
		t.Fatalf("audit: %d failed / %d success events", failed, success)
	}
	if leaked {
		t.Fatal("audit details contain a plain email address")
	}
}
