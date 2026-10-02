package handlers

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/database"
)

// Database-backed tests run only against a disposable local database named in
// TEST_DATABASE_URL, e.g.
//
//	TEST_DATABASE_URL=postgres://localhost:5432/vep_phase0_test go test ./...
//
// They are skipped when it isn't set, and refuse any database that isn't on
// localhost with "test" in its name, so they can never touch a real one.

var (
	testPoolOnce sync.Once
	testPool     *pgxpool.Pool
	testPoolErr  error
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	testPoolOnce.Do(func() {
		u, err := url.Parse(dsn)
		if err != nil {
			testPoolErr = err
			return
		}
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			testPoolErr = fmt.Errorf("refusing to run tests against non-local database host %q", host)
			return
		}
		if !strings.Contains(strings.TrimPrefix(u.Path, "/"), "test") {
			testPoolErr = fmt.Errorf("refusing to run tests against a database without \"test\" in its name")
			return
		}
		testPool, testPoolErr = database.ConnectPostgres(dsn)
		if testPoolErr == nil {
			testPoolErr = database.RunMigrations(testPool)
		}
	})
	if testPoolErr != nil {
		t.Fatalf("test database: %v", testPoolErr)
	}
	return testPool
}

func testConfig() *config.Config {
	return &config.Config{
		AppEnv:      "test",
		JWTSecret:   "test-secret-for-handlers",
		FrontendURL: "http://frontend.test",
	}
}

func uniq() string {
	return fmt.Sprintf("%d%04d", time.Now().UnixNano(), rand.Intn(10000))
}

// createTestHost inserts an active host and removes it (with its events,
// tickets and claims, via ON DELETE CASCADE) when the test ends.
func createTestHost(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var id string
	email := "host-" + uniq() + "@phase0.test"
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role, status) VALUES ($1, 'x', 'host', 'active') RETURNING id`, email,
	).Scan(&id); err != nil {
		t.Fatalf("create host: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

type testEvent struct {
	Price     float64
	Paid      bool
	Active    bool
	Cancelled bool
	Ended     bool
	MaxTix    *int
}

// onSale is an event that should be able to sell tickets.
var onSale = testEvent{Paid: true, Active: true}

func createTestEvent(t *testing.T, db *pgxpool.Pool, hostID string, e testEvent) string {
	t.Helper()
	start, end := time.Now().Add(24*time.Hour), time.Now().Add(26*time.Hour)
	if e.Ended {
		start, end = time.Now().Add(-3*time.Hour), time.Now().Add(-1*time.Hour)
	}
	var cancelledAt *time.Time
	if e.Cancelled {
		now := time.Now()
		cancelledAt = &now
	}
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO events (host_id, title, description, start_time, ends_at, ticket_price, venue_paid, is_active, cancelled_at, max_tickets)
		 VALUES ($1, 'Phase 0 test event', '', $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		hostID, start, end, e.Price, e.Paid, e.Active, cancelledAt, e.MaxTix,
	).Scan(&id); err != nil {
		t.Fatalf("create event: %v", err)
	}
	return id
}

func createGuestTicket(t *testing.T, db *pgxpool.Pool, eventID, email string) (ticketID, accessCode string) {
	t.Helper()
	accessCode = "T" + uniq()
	if err := db.QueryRow(context.Background(),
		`INSERT INTO tickets (event_id, guest_email, access_token) VALUES ($1, $2, $3) RETURNING id`,
		eventID, email, accessCode,
	).Scan(&ticketID); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	return ticketID, accessCode
}
