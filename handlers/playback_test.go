package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"vertualeventlive/backend/services"
)

const testPlaybackURL = "https://test.playback.live-video.net/api/video/v1/phase0.m3u8"
const testChannelARN = "arn:aws:ivs:us-east-1:000000000000:channel/phase0test"

func testSigner(t *testing.T) *services.PlaybackSigner {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	s, err := services.NewPlaybackSigner(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func giveEventStream(t *testing.T, eventID string) {
	t.Helper()
	if _, err := testDB(t).Exec(context.Background(),
		`UPDATE events SET aws_playback_url = $1, aws_channel_arn = $2 WHERE id = $3`,
		testPlaybackURL, testChannelARN, eventID); err != nil {
		t.Fatal(err)
	}
}

// A public visitor gets no playback URL from the event API.
func TestPublicEventAPIDoesNotExposePlayback(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	giveEventStream(t, event)

	app := fiber.New()
	app.Get("/events/:id", (&EventHandler{DB: db, Cfg: testConfig()}).GetByID)
	status, body := doReq(t, app, "GET", "/events/"+event, "")
	if status != 200 {
		t.Fatalf("got %d", status)
	}
	if strings.Contains(body, "live-video.net") || strings.Contains(body, "aws_playback_url") || strings.Contains(body, "phase0.m3u8") {
		t.Fatalf("public event API exposed playback info: %s", body)
	}
	if !strings.Contains(body, `"has_stream":true`) {
		t.Fatalf("expected has_stream=true: %s", body)
	}
}

func enterApp(db *TicketHandler) *fiber.App {
	app := fiber.New()
	app.Get("/tickets/enter", db.Enter)
	return app
}

// Playback is issued only after a server-side ticket check, and only as a
// signed, short-lived URL when signing is configured.
func TestPlaybackRequiresValidTicket(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	signer := testSigner(t)
	h := &TicketHandler{DB: db, Cfg: testConfig(), Playback: &PlaybackAccess{Signer: signer, Required: true}}
	app := enterApp(h)

	live := createTestEvent(t, db, host, onSale)
	giveEventStream(t, live)
	_, code := createGuestTicket(t, db, live, "viewer-"+uniq()+"@phase0.test")

	// No code / a made-up code: no playback information at all.
	for _, path := range []string{"/tickets/enter", "/tickets/enter?code=NOTATICKET", "/tickets/enter?code=" + strings.ToLower(code)} {
		status, body := doReq(t, app, "GET", path, "")
		if status == 200 || strings.Contains(body, "live-video.net") {
			t.Errorf("%s: got %d and/or a playback URL", path, status)
		}
	}

	// Valid ticket: a signed URL whose token is bound to this channel.
	status, body := doReq(t, app, "GET", "/tickets/enter?code="+code, "")
	if status != 200 {
		t.Fatalf("valid ticket: got %d %s", status, body)
	}
	var resp struct {
		PlaybackURL string `json:"playback_url"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	u, err := url.Parse(resp.PlaybackURL)
	if err != nil || u.Query().Get("token") == "" || !strings.HasPrefix(resp.PlaybackURL, testPlaybackURL+"?") {
		t.Fatalf("expected a signed playback URL, got %q", resp.PlaybackURL)
	}
}

func TestPlaybackDeniedForCancelledEndedOrInactiveEvents(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	h := &TicketHandler{DB: db, Cfg: testConfig(), Playback: &PlaybackAccess{Signer: testSigner(t), Required: true}}
	app := enterApp(h)

	for name, ev := range map[string]testEvent{
		"cancelled": {Paid: true, Active: true, Cancelled: true},
		"ended":     {Paid: true, Active: true, Ended: true},
		"disabled":  {Paid: true, Active: false},
		"unpaid":    {Paid: false, Active: true},
	} {
		event := createTestEvent(t, db, host, ev)
		giveEventStream(t, event)
		_, code := createGuestTicket(t, db, event, "viewer-"+uniq()+"@phase0.test")
		status, body := doReq(t, app, "GET", "/tickets/enter?code="+code, "")
		if status != fiber.StatusForbidden || strings.Contains(body, "live-video.net") {
			t.Errorf("%s event: got %d, body %s", name, status, body)
		}
	}
}

// With signing required but no key, nothing is handed out (fail closed).
func TestPlaybackFailsClosedWhenSigningRequiredButMissing(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	giveEventStream(t, event)
	_, code := createGuestTicket(t, db, event, "viewer-"+uniq()+"@phase0.test")

	app := enterApp(&TicketHandler{DB: db, Cfg: testConfig(), Playback: &PlaybackAccess{Required: true}})
	status, body := doReq(t, app, "GET", "/tickets/enter?code="+code, "")
	if status != fiber.StatusServiceUnavailable || strings.Contains(body, "live-video.net") {
		t.Fatalf("got %d %s; want 503 and no URL", status, body)
	}
}

func TestPlaybackAccessRules(t *testing.T) {
	plain, arn := testPlaybackURL, testChannelARN
	if u, err := (&PlaybackAccess{}).URL(nil, nil); u != "" || err != nil {
		t.Error("no stream should give no URL and no error")
	}
	if _, err := (&PlaybackAccess{Required: true}).URL(&plain, &arn); err == nil {
		t.Error("required without a signer must refuse")
	}
	if _, err := (&PlaybackAccess{Signer: testSigner(t)}).URL(&plain, nil); err == nil {
		t.Error("signing without a channel ARN must refuse")
	}
}
