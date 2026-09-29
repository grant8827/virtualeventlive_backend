package services

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"testing"
)

// fakeSMTP accepts one message and records what the client sent.
type fakeSMTP struct {
	auth, from, to string
	data           string
}

func startFakeSMTP(t *testing.T) (*fakeSMTP, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := &fakeSMTP{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
		say("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"):
				say("250-fake")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				got.auth = strings.TrimPrefix(line, "AUTH PLAIN ")
				say("235 ok")
			case strings.HasPrefix(line, "MAIL FROM:"):
				got.from = line
				say("250 ok")
			case strings.HasPrefix(line, "RCPT TO:"):
				got.to = line
				say("250 ok")
			case line == "DATA":
				say("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				got.data = b.String()
				say("250 queued")
			case line == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return got, ln.Addr().(*net.TCPAddr).AddrPort().String()
}

func TestSendSMTP(t *testing.T) {
	got, addr := startFakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	e := &EmailService{
		FromEmail:    "hello@virtualeventplus.com",
		SMTPHost:     host,
		SMTPPort:     port,
		SMTPUsername: "api",
		SMTPPassword: "secret-token",
	}
	if !e.Enabled() {
		t.Fatal("SMTP config should enable email")
	}

	html := `<p>Welcome — <a href="https://example.com/register?token=abc">register</a></p>`
	if err := e.send("host@example.com", "Your invitation to Virtual Event Plus", html); err != nil {
		t.Fatalf("send: %v", err)
	}

	auth, _ := base64.StdEncoding.DecodeString(got.auth)
	if string(auth) != "\x00api\x00secret-token" {
		t.Errorf("auth = %q", auth)
	}
	if got.from != "MAIL FROM:<hello@virtualeventplus.com>" {
		t.Errorf("from = %q", got.from)
	}
	if got.to != "RCPT TO:<host@example.com>" {
		t.Errorf("to = %q", got.to)
	}

	msg, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	if s := msg.Header.Get("Subject"); s != "Your invitation to Virtual Event Plus" {
		t.Errorf("subject = %q", s)
	}
	if f := msg.Header.Get("From"); f != `"Virtual Event Plus" <hello@virtualeventplus.com>` {
		t.Errorf("from header = %q", f)
	}
	if ct := msg.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	// SMTP terminates the body with a line break; the content before it must
	// match exactly.
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if strings.TrimRight(string(body), "\r\n") != html {
		t.Errorf("body round-trip mismatch:\n got %q\nwant %q", body, html)
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	if _, err := buildMessage("a@b.com", "c@d.com", "hi\r\nBcc: x@y.com", "<p>x</p>"); err == nil {
		t.Fatal("expected an error for a subject containing a line break")
	}
}
