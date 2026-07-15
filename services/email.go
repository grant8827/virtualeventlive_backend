package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type EmailService struct {
	APIKey    string
	FromEmail string
	SiteURL   string
}

func (e *EmailService) SendTicketConfirmation(toEmail, eventTitle, accessToken string, startsAt time.Time) error {
	if e.APIKey == "" {
		fmt.Printf("email: RESEND_API_KEY not set — skipping ticket email to %s\n", toEmail)
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
    <p style="color:#71717a;font-size:14px;margin-top:0">VirtualEventLive</p>

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

	body := map[string]interface{}{
		"from":    "VirtualEventLive <" + e.FromEmail + ">",
		"to":      []string{toEmail},
		"subject": "Your ticket for " + eventTitle,
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
