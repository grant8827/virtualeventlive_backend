package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/database"
	"vertualeventlive/backend/middleware"
)

// Authorization matrix (AUTH-06): the real routes, a local test database and
// one signed-in session per role. Runs only with TEST_DATABASE_URL pointing at
// a localhost database with "test" in its name (same guard as handlers tests).

const testOrigin = "http://frontend.test"

func matrixDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping authorization matrix")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
		t.Fatalf("refusing to run against non-local database host %q", h)
	}
	if !strings.Contains(strings.TrimPrefix(u.Path, "/"), "test") {
		t.Fatal(`refusing to run against a database without "test" in its name`)
	}
	db, err := database.ConnectPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

type principal struct {
	name    string
	id      string
	session string
}

type matrix struct {
	t      *testing.T
	db     *pgxpool.Pool
	app    *fiber.App
	cookie string

	anon, buyer, superuser                 principal
	ownerA, adminA, staffA                 principal
	ownerB, suspendedHost, memberOfSuspend principal

	eventA1, eventA2, eventB string // staffA is assigned eventA1 only
	adA1, adA2, adB          string
	ticketA1, ticketA2       string // door-check-in codes
}

func newMatrix(t *testing.T) *matrix {
	db := matrixDB(t)
	cfg := &config.Config{
		AppEnv:             "test",
		JWTSecret:          "authz-matrix-test-secret-0123456789abcdef",
		FrontendURL:        testOrigin,
		CorsAllowedOrigins: testOrigin,
		// Placeholder S3 settings so image routes reach their ownership check;
		// the requests below never get as far as calling S3.
		AWSAccessKeyID:     "authz-test",
		AWSSecretAccessKey: "authz-test",
		S3Region:           "us-east-1",
		S3BucketName:       "authz-test",
	}
	app := fiber.New()
	// nil Redis: the rate limiter falls back to fresh in-memory counters.
	Register(app, db, nil, cfg)

	m := &matrix{t: t, db: db, app: app, cookie: middleware.SessionConfig{Secure: !cfg.IsDevelopment()}.CookieName()}
	tag := fmt.Sprintf("%d%04d", time.Now().UnixNano(), rand.Intn(10000))
	var cleanup []string

	user := func(name, role, status string, owner *principal) principal {
		var ownerID *string
		if owner != nil {
			ownerID = &owner.id
		}
		p := principal{name: name}
		if err := db.QueryRow(context.Background(),
			`INSERT INTO users (email, password_hash, role, status, account_owner_id, email_verified_at)
			 VALUES ($1, 'x', $2, 'active', $3, NOW()) RETURNING id`,
			name+"-"+tag+"@authz.test", role, ownerID,
		).Scan(&p.id); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		cleanup = append(cleanup, p.id)
		tok, _, err := middleware.CreateSession(context.Background(), db, p.id, "127.0.0.1", "authz-test")
		if err != nil {
			t.Fatal(err)
		}
		p.session = tok
		// Suspend after signing in: an existing session must stop working.
		if status != "active" {
			if _, err := db.Exec(context.Background(), `UPDATE users SET status = $2 WHERE id = $1`, p.id, status); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	event := func(host principal, assigned *principal) string {
		var assignedTo *string
		if assigned != nil {
			assignedTo = &assigned.id
		}
		var id string
		if err := db.QueryRow(context.Background(),
			`INSERT INTO events (host_id, title, description, start_time, ends_at, ticket_price, ticket_type, venue_paid, is_active, assigned_to)
			 VALUES ($1, 'authz event', '', NOW() + INTERVAL '1 day', NOW() + INTERVAL '2 days', 0, 'Virtual + Location', true, true, $2)
			 RETURNING id`, host.id, assignedTo,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ad := func(host principal, eventID string) string {
		var id string
		if err := db.QueryRow(context.Background(),
			`INSERT INTO advertisements (host_id, event_id, headline) VALUES ($1, $2, 'original') RETURNING id`,
			host.id, eventID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ticket := func(eventID string) string {
		code := "AZ" + tag + fmt.Sprint(rand.Intn(1e6))
		if _, err := db.Exec(context.Background(),
			`INSERT INTO tickets (event_id, guest_email, access_token) VALUES ($1, 'guest@authz.test', $2)`, eventID, code,
		); err != nil {
			t.Fatal(err)
		}
		return code
	}

	m.anon = principal{name: "anon"}
	m.buyer = user("buyer", "buyer", "active", nil)
	m.superuser = user("superuser", "superuser", "active", nil)
	m.ownerA = user("ownerA", "host", "active", nil)
	m.adminA = user("adminA", "admin", "active", &m.ownerA)
	m.staffA = user("staffA", "staff", "active", &m.ownerA)
	m.ownerB = user("ownerB", "host", "active", nil)
	m.suspendedHost = user("suspendedHost", "host", "suspended", nil)
	m.memberOfSuspend = user("memberOfSuspendedHost", "admin", "active", &m.suspendedHost)

	m.eventA1 = event(m.ownerA, &m.staffA)
	m.eventA2 = event(m.ownerA, nil)
	m.eventB = event(m.ownerB, nil)
	m.adA1, m.adA2, m.adB = ad(m.ownerA, m.eventA1), ad(m.ownerA, m.eventA2), ad(m.ownerB, m.eventB)
	m.ticketA1, m.ticketA2 = ticket(m.eventA1), ticket(m.eventA2)

	t.Cleanup(func() {
		ctx := context.Background()
		// Team members first, then the accounts (events, ads and tickets cascade).
		_, _ = db.Exec(ctx, `DELETE FROM users WHERE id = ANY($1) AND account_owner_id IS NOT NULL`, cleanup)
		_, _ = db.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, cleanup)
	})
	return m
}

func (m *matrix) do(p principal, method, path, body string) (int, string) {
	m.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	if p.session != "" {
		req.AddCookie(&http.Cookie{Name: m.cookie, Value: p.session})
	}
	resp, err := m.app.Test(req, -1)
	if err != nil {
		m.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// concrete fills route parameters so every route can be requested.
func (m *matrix) concrete(path string) string {
	r := strings.NewReplacer(":id", m.eventA1, ":kind", "logo", ":token", "x")
	return r.Replace(path)
}

func registeredRoutes(app *fiber.App) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range app.GetRoutes(true) {
		if r.Method == fiber.MethodHead {
			continue // Fiber adds HEAD for every GET
		}
		k := r.Method + " " + r.Path
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Every registered route has an access class, and the policy lists no
// routes that don't exist.
func TestEveryRouteHasAnAccessPolicy(t *testing.T) {
	app := fiber.New()
	Register(app, nil, nil, &config.Config{AppEnv: "test", JWTSecret: "x", FrontendURL: testOrigin})
	routes := registeredRoutes(app)
	registered := map[string]bool{}
	for _, k := range routes {
		registered[k] = true
		if _, ok := AccessPolicy[k]; !ok {
			t.Errorf("route %q has no entry in AccessPolicy", k)
		}
	}
	for k := range AccessPolicy {
		if !registered[k] {
			t.Errorf("AccessPolicy lists %q, which is not registered", k)
		}
	}
}

// Negative cases for every protected route, per its class.
func TestAccessClassesDenyEveryWrongRole(t *testing.T) {
	m := newMatrix(t)
	type deny struct {
		who  principal
		want int
	}
	for _, k := range registeredRoutes(m.app) {
		class := AccessPolicy[k]
		if class == Public || class == "" {
			continue
		}
		method, path, _ := strings.Cut(k, " ")
		cases := []deny{{m.anon, 401}, {m.suspendedHost, 401}}
		switch class {
		case HostStaff, HostStaffEvent, HostAdmin, HostAdminPayout:
			cases = append(cases, deny{m.buyer, 403}, deny{m.superuser, 403}, deny{m.memberOfSuspend, 401})
		case Superuser:
			cases = append(cases, deny{m.buyer, 403}, deny{m.ownerA, 403}, deny{m.adminA, 403}, deny{m.staffA, 403})
		}
		if class == HostAdmin || class == HostAdminPayout {
			cases = append(cases, deny{m.staffA, 403})
		}
		for _, c := range cases {
			if got, body := m.do(c.who, method, m.concrete(path), `{}`); got != c.want {
				t.Errorf("%s as %s: got %d, want %d (%s)", k, c.who.name, got, c.want, body)
			}
		}
	}
}

// Event-scoped routes: staff reach only assigned events, and no host reaches
// another host's event. Both answer 404.
func TestEventScopedRoutesRejectOtherEvents(t *testing.T) {
	m := newMatrix(t)
	for _, k := range registeredRoutes(m.app) {
		if AccessPolicy[k] != HostStaffEvent {
			continue
		}
		method, path, _ := strings.Cut(k, " ")
		for _, c := range []struct {
			who   principal
			event string
		}{
			{m.staffA, m.eventA2}, // same account, not assigned
			{m.staffA, m.eventB},  // other account
			{m.ownerB, m.eventA1},
			{m.ownerA, m.eventB},
			{m.adminA, m.eventB},
		} {
			p := strings.NewReplacer(":id", c.event, ":kind", "logo").Replace(path)
			if got, body := m.do(c.who, method, p, `{}`); got != 404 {
				t.Errorf("%s as %s on another event: got %d, want 404 (%s)", k, c.who.name, got, body)
			}
		}
	}
}

func isAllowed(status int) bool {
	return status != 401 && status != 403 && status != 404 && status < 500
}

// Positive cases: each role reaches what its class admits.
func TestAccessClassesAdmitTheRightRoles(t *testing.T) {
	m := newMatrix(t)
	cases := []struct {
		who          principal
		method, path string
		body         string
	}{
		// Session
		{m.buyer, "GET", "/api/v1/auth/me", ""},
		{m.ownerA, "GET", "/api/v1/auth/me", ""},
		{m.adminA, "GET", "/api/v1/auth/me", ""},
		{m.staffA, "GET", "/api/v1/auth/me", ""},
		{m.superuser, "GET", "/api/v1/auth/me", ""},
		{m.buyer, "GET", "/api/v1/tickets/mine", ""},
		// HostStaff
		{m.ownerA, "GET", "/api/v1/events", ""},
		{m.adminA, "GET", "/api/v1/events", ""},
		{m.staffA, "GET", "/api/v1/events", ""},
		{m.ownerB, "GET", "/api/v1/events", ""},
		{m.staffA, "GET", "/api/v1/advertisements/mine", ""},
		// HostStaffEvent
		{m.ownerA, "PATCH", "/api/v1/events/" + m.eventA2 + "/ticket", `{"ticket_type":"Virtual + Location"}`},
		{m.adminA, "PATCH", "/api/v1/events/" + m.eventA2 + "/ticket", `{"ticket_type":"Virtual + Location"}`},
		{m.staffA, "PATCH", "/api/v1/events/" + m.eventA1 + "/ticket", `{"ticket_type":"Virtual + Location"}`},
		{m.staffA, "GET", "/api/v1/events/" + m.eventA1 + "/viewers", ""},
		// An unknown image kind is rejected by the handler (400), i.e. after
		// the guards let staff through; a real delete would call S3.
		{m.staffA, "DELETE", "/api/v1/events/" + m.eventA1 + "/images/bogus", ""},
		// HostAdmin
		{m.ownerA, "GET", "/api/v1/team", ""},
		{m.adminA, "GET", "/api/v1/team", ""},
		{m.ownerA, "GET", "/api/v1/connect/security/status", ""},
		{m.adminA, "GET", "/api/v1/connect/security/status", ""},
		// Superuser
		{m.superuser, "GET", "/api/v1/superuser/overview", ""},
		{m.superuser, "GET", "/api/v1/superuser/users", ""},
		{m.superuser, "GET", "/api/v1/superuser/security-events", ""},
	}
	for _, c := range cases {
		if got, body := m.do(c.who, c.method, c.path, c.body); !isAllowed(got) {
			t.Errorf("%s %s as %s: got %d, want allowed (%s)", c.method, c.path, c.who.name, got, body)
		}
	}
}

// Staff list only their assigned events; hosts list only their own.
func TestEventListIsScopedToAccountAndAssignment(t *testing.T) {
	m := newMatrix(t)
	ids := func(p principal) map[string]bool {
		code, body := m.do(p, "GET", "/api/v1/events", "")
		if code != 200 {
			t.Fatalf("list as %s: %d %s", p.name, code, body)
		}
		var out struct {
			Events []struct {
				ID string `json:"id"`
			} `json:"events"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, e := range out.Events {
			got[e.ID] = true
		}
		return got
	}
	if got := ids(m.staffA); !got[m.eventA1] || got[m.eventA2] || got[m.eventB] {
		t.Errorf("staff list = %v, want only %s", got, m.eventA1)
	}
	if got := ids(m.adminA); !got[m.eventA1] || !got[m.eventA2] || got[m.eventB] {
		t.Errorf("admin list = %v, want A1 and A2 only", got)
	}
	if got := ids(m.ownerB); got[m.eventA1] || got[m.eventA2] || !got[m.eventB] {
		t.Errorf("ownerB list = %v, want B only", got)
	}
}

// Object-level checks (BOLA) on routes whose IDs aren't events.
func TestObjectLevelAuthorization(t *testing.T) {
	m := newMatrix(t)
	ctx := context.Background()
	headline := func(id string) string {
		var h string
		_ = m.db.QueryRow(ctx, `SELECT headline FROM advertisements WHERE id = $1`, id).Scan(&h)
		return h
	}
	exists := func(q, id string) bool {
		var ok bool
		_ = m.db.QueryRow(ctx, `SELECT EXISTS(`+q+`)`, id).Scan(&ok)
		return ok
	}
	adBody := `{"headline":"changed","cta_text":"Go"}`

	// Flyers
	for _, c := range []struct {
		who  principal
		ad   string
		want int
	}{
		{m.ownerB, m.adA1, 404},
		{m.ownerA, m.adB, 404},
		{m.staffA, m.adA2, 404}, // flyer for an event not assigned to them
		{m.staffA, m.adB, 404},
	} {
		if got, _ := m.do(c.who, "PUT", "/api/v1/advertisements/"+c.ad, adBody); got != c.want {
			t.Errorf("update flyer as %s: got %d, want %d", c.who.name, got, c.want)
		}
		if got, _ := m.do(c.who, "DELETE", "/api/v1/advertisements/"+c.ad, ""); got != c.want {
			t.Errorf("delete flyer as %s: got %d, want %d", c.who.name, got, c.want)
		}
	}
	for _, id := range []string{m.adA1, m.adA2, m.adB} {
		if headline(id) != "original" {
			t.Errorf("flyer %s was changed or deleted by another account", id)
		}
	}
	if got, _ := m.do(m.staffA, "PUT", "/api/v1/advertisements/"+m.adA1, adBody); got != 200 || headline(m.adA1) != "changed" {
		t.Errorf("staff updating an assigned event's flyer: got %d", got)
	}
	if got, _ := m.do(m.adminA, "DELETE", "/api/v1/advertisements/"+m.adA2, ""); got != 200 {
		t.Errorf("admin deleting own account's flyer: got %d", got)
	}

	// Events
	if got, _ := m.do(m.ownerB, "DELETE", "/api/v1/events/"+m.eventA1, ""); got != 404 {
		t.Errorf("deleting another host's event: got %d, want 404", got)
	}
	if !exists(`SELECT 1 FROM events WHERE id = $1 AND is_active`, m.eventA1) {
		t.Error("another host deactivated eventA1")
	}
	if got, _ := m.do(m.ownerB, "POST", "/api/v1/events/"+m.eventA1+"/checkout", `{}`); got != 404 {
		t.Errorf("checkout for another host's event: got %d, want 404", got)
	}

	// Team members
	if got, _ := m.do(m.ownerB, "PATCH", "/api/v1/team/"+m.staffA.id, `{"full_name":"Taken over"}`); got != 404 {
		t.Errorf("editing another account's member: got %d, want 404", got)
	}
	if got, _ := m.do(m.ownerB, "DELETE", "/api/v1/team/"+m.staffA.id, ""); got != 404 {
		t.Errorf("deleting another account's member: got %d, want 404", got)
	}
	if got, _ := m.do(m.ownerB, "PATCH", "/api/v1/team/"+m.ownerA.id, `{"full_name":"Taken over"}`); got != 404 {
		t.Errorf("editing another host owner via team: got %d, want 404", got)
	}
	if !exists(`SELECT 1 FROM users WHERE id = $1 AND account_owner_id IS NOT NULL`, m.staffA.id) {
		t.Fatal("another host removed staffA")
	}
	if got, _ := m.do(m.adminA, "PATCH", "/api/v1/team/"+m.staffA.id, `{"full_name":"Staff A"}`); got != 200 {
		t.Errorf("admin editing own account's member: got %d", got)
	}

	// Door check-in
	for _, c := range []struct {
		who  principal
		code string
		want int
	}{
		{m.ownerB, m.ticketA1, 403},
		{m.staffA, m.ticketA2, 403}, // event not assigned to them
		{m.staffA, m.ticketA1, 200},
		{m.ownerA, m.ticketA2, 200},
	} {
		if got, body := m.do(c.who, "POST", "/api/v1/tickets/checkin", `{"code":"`+c.code+`"}`); got != c.want {
			t.Errorf("check-in as %s: got %d, want %d (%s)", c.who.name, got, c.want, body)
		}
	}
}

// A cookie-authenticated change from an untrusted origin is refused (CSRF).
func TestCrossOriginCookieRequestIsRefused(t *testing.T) {
	m := newMatrix(t)
	req := httptest.NewRequest("PATCH", "/api/v1/team/"+m.staffA.id, strings.NewReader(`{"full_name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: m.cookie, Value: m.ownerA.session})
	resp, err := m.app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("cross-origin PATCH: got %d, want 403", resp.StatusCode)
	}
}
