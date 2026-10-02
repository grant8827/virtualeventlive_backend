package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func ticketTestApp(h *TicketHandler) *fiber.App {
	app := fiber.New()
	app.Post("/tickets/lookup", h.LookupByEmail)
	app.Get("/tickets/lookup", h.LookupGone)
	app.Get("/tickets/claim/:token", h.Claim)
	return app
}

func doReq(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An attacker who knows a customer's email must not be able to read their
// ticket or access code through the old GET lookup.
func TestOldEmailLookupNoLongerReturnsTickets(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	victim := "victim-" + uniq() + "@phase0.test"
	_, code := createGuestTicket(t, db, event, victim)

	app := ticketTestApp(&TicketHandler{DB: db, Cfg: testConfig()})
	status, body := doReq(t, app, "GET", "/tickets/lookup?email="+victim, "")
	if status != fiber.StatusGone {
		t.Fatalf("GET lookup: got %d, want 410", status)
	}
	if strings.Contains(body, code) || strings.Contains(body, victim) {
		t.Fatal("GET lookup leaked the victim's ticket data")
	}
}

// The new lookup emails the owner; the HTTP response never contains tickets
// and is identical for an address with tickets and one without (no
// enumeration of who has bought tickets).
func TestEmailLookupRevealsNothingAndCannotEnumerate(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	victim := "victim-" + uniq() + "@phase0.test"
	_, code := createGuestTicket(t, db, event, victim)
	nobody := "nobody-" + uniq() + "@phase0.test"

	app := ticketTestApp(&TicketHandler{DB: db, Cfg: testConfig()})
	s1, withTickets := doReq(t, app, "POST", "/tickets/lookup", `{"email":"`+victim+`"}`)
	s2, withoutTickets := doReq(t, app, "POST", "/tickets/lookup", `{"email":"`+nobody+`"}`)
	if s1 != fiber.StatusAccepted || s2 != fiber.StatusAccepted {
		t.Fatalf("statuses %d / %d, want 202 / 202", s1, s2)
	}
	if strings.Contains(withTickets, code) || strings.Contains(withTickets, "access_token") || strings.Contains(withTickets, event) {
		t.Fatal("POST lookup response leaked ticket data")
	}
	if withTickets != withoutTickets {
		t.Fatal("responses differ between an address with tickets and one without")
	}
}

func TestEmailLookupRateLimitedWithSameAnswer(t *testing.T) {
	db := testDB(t)
	app := ticketTestApp(&TicketHandler{DB: db, Cfg: testConfig()})
	email := "flood-" + uniq() + "@phase0.test"
	_, first := doReq(t, app, "POST", "/tickets/lookup", `{"email":"`+email+`"}`)
	status, second := doReq(t, app, "POST", "/tickets/lookup", `{"email":"`+email+`"}`)
	if status != fiber.StatusAccepted || first != second {
		t.Fatal("a rate-limited request must get the same generic answer")
	}
}

// A claim token shows exactly its own purchase. Guessing tokens, or holding
// the claim for someone else's purchase, never reveals another ticket.
func TestClaimShowsOnlyItsOwnTicket(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	email := "buyer-" + uniq() + "@phase0.test"
	h := &TicketHandler{DB: db, Cfg: testConfig()}
	app := ticketTestApp(h)

	// Two tickets for the same email; each claim must return only its own.
	codeA, claimA, err := h.issueFreeTicket(ctx, event, nil, &email)
	if err != nil {
		t.Fatal(err)
	}
	codeB, _, err := h.issueFreeTicket(ctx, event, nil, &email)
	if err != nil {
		t.Fatal(err)
	}

	status, body := doReq(t, app, "GET", "/tickets/claim/"+claimA, "")
	if status != 200 {
		t.Fatalf("own claim: got %d", status)
	}
	var resp struct {
		Tickets []ticketRow `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Tickets) != 1 || resp.Tickets[0].AccessToken != codeA {
		t.Fatalf("claim A returned %d tickets / wrong ticket", len(resp.Tickets))
	}
	if strings.Contains(body, codeB) {
		t.Fatal("claim A also revealed the other ticket for the same email")
	}

	for _, bad := range []string{"guess", strings.Repeat("A", 43), claimA + "x", email} {
		if status, body := doReq(t, app, "GET", "/tickets/claim/"+bad, ""); status != fiber.StatusNotFound || strings.Contains(body, codeA) {
			t.Errorf("forged claim %q: got %d", bad, status)
		}
	}
}

func TestClaimExpiresAndPendingClaimRevealsNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	app := ticketTestApp(&TicketHandler{DB: db, Cfg: testConfig()})

	// Paid checkout not yet fulfilled: 202, no ticket data.
	token, hash, _ := newClaimToken()
	session := "cs_test_" + uniq()
	if err := insertTicketClaim(ctx, db, hash, event, claimTarget{StripeSessionID: &session}); err != nil {
		t.Fatal(err)
	}
	if status, body := doReq(t, app, "GET", "/tickets/claim/"+token, ""); status != fiber.StatusAccepted || strings.Contains(body, "access_token") {
		t.Fatalf("pending claim: got %d %s", status, body)
	}

	// Expired claim: 410, no ticket data.
	email := "late-" + uniq() + "@phase0.test"
	code, claim, err := (&TicketHandler{DB: db}).issueFreeTicket(ctx, event, nil, &email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE ticket_claims SET expires_at = NOW() - INTERVAL '1 minute' WHERE token_hash = $1`, hashInvitationToken(claim)); err != nil {
		t.Fatal(err)
	}
	if status, body := doReq(t, app, "GET", "/tickets/claim/"+claim, ""); status != fiber.StatusGone || strings.Contains(body, code) {
		t.Fatalf("expired claim: got %d", status)
	}
}
