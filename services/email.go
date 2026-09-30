package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"time"
)

// EmailService sends the platform's emails over SMTP when SMTPHost is set
// (e.g. Mailtrap), otherwise through the Resend API when APIKey is set. With
// neither configured, emails are skipped and logged.
type EmailService struct {
	APIKey    string
	FromEmail string
	SiteURL   string

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
}

// Enabled reports whether any email transport is configured.
func (e *EmailService) Enabled() bool {
	return e.smtpReady() || e.APIKey != ""
}

// smtpReady is true once SMTP has a host and, when a username is given, its
// password — so a half-filled .env falls back instead of failing every send.
func (e *EmailService) smtpReady() bool {
	return e.SMTPHost != "" && (e.SMTPUsername == "" || e.SMTPPassword != "")
}

// TicketEmail is everything the buyer's ticket email shows.
type TicketEmail struct {
	EventID      string
	EventTitle   string
	StartsAt     time.Time
	TicketName   string // e.g. "General Admission"
	TicketType   string // "Virtual" or "Virtual + Location"
	VenueAddress string // shown for in-person tickets
	AccessCode   string // unlocks the stream
	SerialNo     int64  // printed ticket number; door staff can type it to check in
}

// SendTicketConfirmation emails the buyer their ticket: the access code, a
// button straight into the stream, and for in-person tickets the ticket
// number and venue for the door.
func (e *EmailService) SendTicketConfirmation(toEmail string, t TicketEmail) error {
	if !e.Enabled() {
		fmt.Printf("email: not configured (SMTP_HOST / RESEND_API_KEY) — skipping ticket email to %s\n", toEmail)
		return nil
	}

	watchURL := fmt.Sprintf("%s/events/%s/watch?code=%s", e.SiteURL, url.PathEscape(t.EventID), url.QueryEscape(t.AccessCode))
	lookupURL := e.SiteURL + "/tickets"
	esc := html.EscapeString

	ticketName := t.TicketName
	if ticketName == "" {
		ticketName = "Ticket"
	}
	inPerson := ""
	if t.TicketType == "Virtual + Location" {
		venue := ""
		if t.VenueAddress != "" {
			venue = fmt.Sprintf(`<p style="color:#b3bcd6;font-size:13px;margin:8px 0 0">Venue: %s</p>`, esc(t.VenueAddress))
		}
		inPerson = fmt.Sprintf(`
    <div style="background:#0f1631;border:1px solid #252e52;border-radius:12px;padding:16px 20px;margin:0 0 24px">
      <p style="color:#8590b0;font-size:12px;margin:0 0 4px">Attending in person? Show this ticket number at the door</p>
      <p style="font-family:monospace;font-size:22px;font-weight:700;letter-spacing:0.08em;margin:0">#%d</p>%s
      <p style="color:#8590b0;font-size:12px;margin:8px 0 0">Each ticket can be used once — at the door or on the stream.</p>
    </div>`, t.SerialNo, venue)
	}

	body := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;background:#080d22;color:#fff;padding:40px 20px;margin:0">
  <div style="max-width:520px;margin:0 auto">
    <h1 style="font-size:22px;margin-bottom:4px">Your ticket is confirmed</h1>
    <p style="color:#8590b0;font-size:14px;margin-top:0">Virtual Event Plus</p>

    <div style="background:#0f1631;border:1px solid #252e52;border-radius:12px;padding:24px;margin:28px 0">
      <p style="color:#5a93fc;font-size:11px;font-weight:700;letter-spacing:0.08em;text-transform:uppercase;margin:0 0 6px">%s</p>
      <p style="font-size:18px;font-weight:600;margin:0 0 6px">%s</p>
      <p style="color:#8590b0;font-size:13px;margin:0">%s</p>
    </div>

    <p style="color:#b3bcd6;font-size:13px;margin-bottom:6px">Your access code</p>
    <p style="font-family:monospace;background:#0f1631;border:1px solid #252e52;border-radius:8px;padding:12px 16px;font-size:15px;letter-spacing:0.08em;margin:0 0 24px">%s</p>
%s
    <a href="%s" style="display:inline-block;background:#0067f9;color:#fff;padding:12px 28px;border-radius:9999px;font-size:14px;font-weight:600;text-decoration:none">
      Watch event
    </a>

    <hr style="border:none;border-top:1px solid #252e52;margin:32px 0">
    <p style="color:#8590b0;font-size:12px">
      Can't find this email later? Retrieve your tickets at
      <a href="%s" style="color:#5a93fc">%s</a> using your email address.
    </p>
  </div>
</body>
</html>`,
		esc(ticketName),
		esc(t.EventTitle),
		esc(t.StartsAt.Format("Monday, January 2, 2006 at 3:04 PM MST")),
		esc(t.AccessCode),
		inPerson,
		esc(watchURL),
		esc(lookupURL),
		esc(lookupURL),
	)

	return e.send(toEmail, "Your ticket for "+t.EventTitle, body)
}

// SendInvitation emails a registration link to someone a superuser invited.
func (e *EmailService) SendInvitation(toEmail, registerURL string, expiresAt time.Time) error {
	if !e.Enabled() {
		fmt.Printf("email: not configured (SMTP_HOST / RESEND_API_KEY) — skipping invitation email to %s\n", toEmail)
		return nil
	}
	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;background:#0a0a0a;color:#fff;padding:40px 20px;margin:0">
  <div style="max-width:520px;margin:0 auto">
    <h1 style="font-size:22px;margin-bottom:4px">You're invited to host on Virtual Event Plus</h1>
    <p style="color:#a1a1aa;font-size:14px;line-height:1.6">
      Create your host account to book events, sell tickets and go live.
      Once you register, we'll review and approve your account.
    </p>
    <a href="%s" style="display:inline-block;background:#fff;color:#000;padding:12px 28px;border-radius:9999px;font-size:14px;font-weight:600;text-decoration:none;margin:20px 0">
      Create your account
    </a>
    <p style="color:#52525b;font-size:12px">This link expires on %s.</p>
  </div>
</body>
</html>`, registerURL, expiresAt.Format("January 2, 2006"))
	return e.send(toEmail, "Your invitation to Virtual Event Plus", html)
}

// SendPasswordReset emails a one-time link to choose a new password.
func (e *EmailService) SendPasswordReset(toEmail, resetURL string) error {
	if !e.Enabled() {
		fmt.Printf("email: not configured (SMTP_HOST / RESEND_API_KEY) — skipping password reset email to %s\n", toEmail)
		return nil
	}
	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;background:#0a0a0a;color:#fff;padding:40px 20px;margin:0">
  <div style="max-width:520px;margin:0 auto">
    <h1 style="font-size:22px;margin-bottom:4px">Reset your password</h1>
    <p style="color:#a1a1aa;font-size:14px;line-height:1.6">
      We received a request to reset the password for your Virtual Event Plus account.
    </p>
    <a href="%s" style="display:inline-block;background:#0067F9;color:#fff;padding:12px 28px;border-radius:9999px;font-size:14px;font-weight:600;text-decoration:none;margin:20px 0">
      Choose a new password
    </a>
    <p style="color:#52525b;font-size:12px;line-height:1.6">
      This link expires in 1 hour and can only be used once. If you didn't ask
      for this, you can ignore this email — your password won't change.
    </p>
  </div>
</body>
</html>`, resetURL)
	return e.send(toEmail, "Reset your Virtual Event Plus password", html)
}

// SendAccountApproved tells a newly registered host they can sign in.
func (e *EmailService) SendAccountApproved(toEmail string) error {
	if !e.Enabled() {
		fmt.Printf("email: not configured (SMTP_HOST / RESEND_API_KEY) — skipping approval email to %s\n", toEmail)
		return nil
	}
	loginURL := e.SiteURL + "/login"
	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;background:#0a0a0a;color:#fff;padding:40px 20px;margin:0">
  <div style="max-width:520px;margin:0 auto">
    <h1 style="font-size:22px;margin-bottom:4px">Your account is approved</h1>
    <p style="color:#a1a1aa;font-size:14px;line-height:1.6">You can now sign in and set up your first event.</p>
    <a href="%s" style="display:inline-block;background:#fff;color:#000;padding:12px 28px;border-radius:9999px;font-size:14px;font-weight:600;text-decoration:none;margin:20px 0">
      Sign in
    </a>
  </div>
</body>
</html>`, loginURL)
	return e.send(toEmail, "Your Virtual Event Plus account is approved", html)
}

// send delivers one HTML email over SMTP if configured, else through Resend.
func (e *EmailService) send(toEmail, subject, html string) error {
	if e.smtpReady() {
		return e.sendSMTP(toEmail, subject, html)
	}
	body := map[string]interface{}{
		"from":    "Virtual Event Plus <" + e.FromEmail + ">",
		"to":      []string{toEmail},
		"subject": subject,
		"html":    html,
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", "https://api.resend.com/emails", bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("resend API error: status %d", resp.StatusCode)
	}
	return nil
}
