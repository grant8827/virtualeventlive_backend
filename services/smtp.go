package services

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

const smtpTimeout = 15 * time.Second

// sendSMTP delivers one HTML email over SMTP (e.g. Mailtrap on port 587).
// It upgrades the connection with STARTTLS whenever the server offers it and
// refuses to send credentials over an unencrypted connection.
func (e *EmailService) sendSMTP(toEmail, subject, html string) error {
	addr := net.JoinHostPort(e.SMTPHost, e.SMTPPort)
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))

	c, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: e.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if e.SMTPUsername != "" {
		// PlainAuth itself refuses to send the password unless the connection
		// is TLS-protected (or to localhost).
		if err := c.Auth(smtp.PlainAuth("", e.SMTPUsername, e.SMTPPassword, e.SMTPHost)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(e.FromEmail); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := c.Rcpt(toEmail); err != nil {
		return fmt.Errorf("smtp to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	msg, err := buildMessage(e.FromEmail, toEmail, subject, html)
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}

// buildMessage renders a single-part HTML email with quoted-printable body.
func buildMessage(from, to, subject, html string) ([]byte, error) {
	for _, v := range []string{from, to, subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("email header contains a line break")
		}
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "virtualeventplus.com"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		domain = from[at+1:]
	}

	var b bytes.Buffer
	fromHeader := (&mail.Address{Name: "Virtual Event Plus", Address: from}).String()
	fmt.Fprintf(&b, "From: %s\r\n", fromHeader)
	fmt.Fprintf(&b, "To: %s\r\n", (&mail.Address{Address: to}).String())
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")

	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(html)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
