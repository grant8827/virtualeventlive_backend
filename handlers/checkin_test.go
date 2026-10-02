package handlers

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// PAY-10: a "Virtual + Location" ticket admits exactly once, across door
// scanners and the stream, however many requests race. Voided (refunded
// or charged-back) tickets admit nobody.

func checkinApp(t *testing.T, hostID string) (*fiber.App, *TicketHandler) {
	h := &TicketHandler{DB: testDB(t), Cfg: testConfig(), Playback: &PlaybackAccess{}}
	app := fiber.New()
	app.Post("/checkin", func(c *fiber.Ctx) error {
		c.Locals("user_id", hostID)
		return h.CheckIn(c)
	})
	app.Get("/enter", h.Enter)
	return app, h
}

func doorEvent(t *testing.T, host string) string {
	t.Helper()
	db := testDB(t)
	event := createTestEvent(t, db, host, onSale)
	if _, err := db.Exec(context.Background(), `UPDATE events SET ticket_type = 'Virtual + Location' WHERE id = $1`, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestConcurrentDoorScansAdmitOnce(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := doorEvent(t, host)
	_, code := createGuestTicket(t, db, event, "door-"+uniq()+"@phase2.test")
	app, _ := checkinApp(t, host)

	var admitted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch status, _ := doReq(t, app, "POST", "/checkin", fmt.Sprintf(`{"code":%q}`, code)); status {
			case 200:
				admitted.Add(1)
			case 409:
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 || refused.Load() != 19 {
		t.Fatalf("%d admitted, %d refused; want 1 and 19", admitted.Load(), refused.Load())
	}
}

func TestDoorAndStreamRaceHasOneWinner(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := doorEvent(t, host)
	app, _ := checkinApp(t, host)

	for round := 0; round < 10; round++ {
		_, code := createGuestTicket(t, db, event, "race-"+uniq()+"@phase2.test")
		var door, stream int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); door, _ = doReq(t, app, "POST", "/checkin", fmt.Sprintf(`{"code":%q}`, code)) }()
		go func() { defer wg.Done(); stream, _ = doReq(t, app, "GET", "/enter?code="+code, "") }()
		wg.Wait()
		doorWon, streamWon := door == 200, stream == 200
		if doorWon == streamWon {
			t.Fatalf("round %d: door %d, stream %d — exactly one channel must win", round, door, stream)
		}
		var channel string
		_ = db.QueryRow(context.Background(), `SELECT checked_in_channel FROM tickets WHERE access_token = $1`, code).Scan(&channel)
		if (doorWon && channel != "physical") || (streamWon && channel != "virtual") {
			t.Fatalf("round %d: winner and recorded channel %q disagree", round, channel)
		}
	}
}

func TestVoidedTicketsAdmitNobody(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := doorEvent(t, host)
	app, _ := checkinApp(t, host)
	ticketID, code := createGuestTicket(t, db, event, "void-"+uniq()+"@phase2.test")
	if _, err := db.Exec(context.Background(), `UPDATE tickets SET voided_at = NOW(), void_reason = 'refunded' WHERE id = $1`, ticketID); err != nil {
		t.Fatal(err)
	}
	if status, _ := doReq(t, app, "POST", "/checkin", fmt.Sprintf(`{"code":%q}`, code)); status != 409 {
		t.Errorf("door scan of a voided ticket: %d, want 409", status)
	}
	if status, _ := doReq(t, app, "GET", "/enter?code="+code, ""); status != 403 {
		t.Errorf("stream entry with a voided ticket: %d, want 403", status)
	}
	var channel *string
	_ = db.QueryRow(context.Background(), `SELECT checked_in_channel FROM tickets WHERE id = $1`, ticketID).Scan(&channel)
	if channel != nil {
		t.Error("a voided ticket was checked in")
	}
}
