package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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

func (e *EmailService) SendTicketConfirmation(toEmail, eventTitle, accessToken string, startsAt time.Time) error {
	if !e.Enabled() {
		fmt.Printf("email: not configured (SMTP_HOST / RESEND_API_KEY) — skipping ticket email to %s\n", toEmail)
		return nil
	}

	watchURL := e.SiteURL + "/watch/" + accessToken
	lookupURL := e.SiteURL + "/tickets"

	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;background:#0a0a0a;color:#fff;padding:40px 20px;margin:0">
  <div style="max-width:520px;margin:0 auto">
    <h1 style="font-size:22px;margin-bottom:4px">Your ticket is confirmed</h1>
    <p style="color:#71717a;font-size:14px;margin-top:0">Virtual Event Plus</p>

    <div style="background:#18181b;border:1px solid #27272a;border-radius:12px;padding:24px;margin:28px 0">
      <p style="font-size:18px;font-weight:600;margin:0 0 6px">%s</p>
      <p style="color:#71717a;font-size:13px;margin:0">%s</p>
    </div>

    <p style="color:#a1a1aa;font-size:13px;margin-bottom:6px">Your access code</p>
    <p style="font-family:monospace;background:#18181b;border:1px solid #27272a;border-radius:8px;padding:12px 16px;font-size:13px;letter-spacing:0.05em;margin:0 0 24px">%s</p>

    <a href="%s" style="display:inline-block;background:#fff;color:#000;padding:12px 28px;border-radius:9999px;font-size:14px;font-weight:600;text-decoration:none">
      Watch event
    </a>

    <hr style="border:none;border-top:1px solid #27272a;margin:32px 0">
    <p style="color:#52525b;font-size:12px">
      Can't find this email later? Retrieve your tickets at
      <a href="%s" style="color:#a1a1aa">%s</a> using your email address.
    </p>
  </div>
</body>
</html>`,
		eventTitle,
		startsAt.Format("Monday, January 2, 2006 at 3:04 PM MST"),
		accessToken,
		watchURL,
		lookupURL,
		lookupURL,
	)

	return e.send(toEmail, "Your ticket for "+eventTitle, html)
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
