package handlers

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// The legacy WiPay return route trusts a status in the URL. While disabled
// (the default) it must never activate an event.
func TestWiPayReturnCannotActivateWhileDisabled(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, testEvent{Paid: false, Active: false})
	h := &EventHandler{DB: db, Cfg: testConfig()} // WipayVenueFeeEnabled is false

	app := fiber.New()
	app.Get("/events/:id/wipay/complete", h.WiPayComplete)
	app.Get("/events/:id/wipay/launch", h.WiPayLaunch)

	status, _ := doReq(t, app, "GET", "/events/"+event+"/wipay/complete?state=anything&status=success", "")
	if status != fiber.StatusFound {
		t.Fatalf("complete: got %d, want redirect", status)
	}
	var paid bool
	_ = db.QueryRow(context.Background(), `SELECT venue_paid FROM events WHERE id = $1`, event).Scan(&paid)
	if paid {
		t.Fatal("disabled WiPay route activated an event")
	}
	if status, _ := doReq(t, app, "GET", "/events/"+event+"/wipay/launch?state=x", ""); status != fiber.StatusServiceUnavailable {
		t.Fatalf("launch: got %d, want 503", status)
	}
}
