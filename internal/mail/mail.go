// Package mail sends transactional email. Callers don't send directly: the
// server writes to the email_outbox table and a worker delivers through a
// Sender, so requests never wait on (or reveal timing through) the provider.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Message is one email. Text is required; HTML is optional.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	ReplyTo string // optional, e.g. the visitor who filled in a form
}

// Sender delivers a message or returns an error (the outbox retries).
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Config selects and configures a driver.
type Config struct {
	Driver   string // "log" (default) or "smtp"
	From     string // e.g. "Tautan <hello@mail.tautan.id>"
	Host     string
	Port     int    // 587 (STARTTLS) or 465 (implicit TLS); 25/1025 for local test servers
	Username string // empty: no AUTH
	Password string
	// ShowBodies lets the log driver print full messages (development only:
	// bodies carry live reset/verification links).
	ShowBodies bool
}

// New builds the Sender for cfg.
func New(cfg Config) (Sender, error) {
	switch cfg.Driver {
	case "", "log":
		return Log{ShowBodies: cfg.ShowBodies}, nil
	case "smtp":
		if cfg.Host == "" || cfg.Port == 0 {
			return nil, errors.New("MAIL_DRIVER=smtp needs SMTP_HOST and SMTP_PORT")
		}
		from, err := mail.ParseAddress(cfg.From)
		if err != nil {
			return nil, fmt.Errorf("MAIL_FROM is not a valid address (%q): %w", cfg.From, err)
		}
		if strings.HasSuffix(strings.ToLower(from.Address), "@localhost") {
			return nil, errors.New("MAIL_DRIVER=smtp needs MAIL_FROM set to a real sender, e.g. \"Tautan <hello@mail.tautan.id>\"")
		}
		return &SMTP{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("MAIL_DRIVER must be log or smtp (got %q)", cfg.Driver)
	}
}

// Log writes messages to the log instead of sending them.
type Log struct{ ShowBodies bool }

func (l Log) Send(_ context.Context, m Message) error {
	if l.ShowBodies {
		slog.Info("mail (log driver, not sent)", "to", m.To, "subject", m.Subject, "text", m.Text)
	} else {
		slog.Info("mail (log driver, not sent)", "to", m.To, "subject", m.Subject)
	}
	return nil
}

// SMTP sends through any SMTP relay (Resend, SES, Postmark, Mailgun, …).
type SMTP struct{ cfg Config }

func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return err
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("bad recipient: %w", err)
	}
	raw, err := Build(from, to, m, time.Now())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if s.cfg.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
				return err
			}
		}
	}
	if s.cfg.Username != "" {
		// net/smtp refuses PLAIN auth over an unencrypted link except to localhost.
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Build renders an RFC 5322 message: multipart/alternative when HTML is set,
// quoted-printable bodies, encoded headers.
func Build(from, to *mail.Address, m Message, now time.Time) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("subject contains a newline")
	}
	var b bytes.Buffer
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from.String())
	hdr("To", to.String())
	if m.ReplyTo != "" {
		rt, err := mail.ParseAddress(m.ReplyTo)
		if err != nil {
			return nil, fmt.Errorf("bad reply-to: %w", err)
		}
		hdr("Reply-To", rt.String())
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	hdr("Auto-Submitted", "auto-generated")

	part := func(w *bytes.Buffer, body string) error {
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(body)); err != nil {
			return err
		}
		return qp.Close()
	}
	if m.HTML == "" {
		hdr("Content-Type", `text/plain; charset="utf-8"`)
		hdr("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := part(&b, m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	for _, p := range []struct{ ct, s string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.ct + `; charset="utf-8"`},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		var pb bytes.Buffer
		if err := part(&pb, p.s); err != nil {
			return nil, err
		}
		if _, err := w.Write(pb.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	b.Write(body.Bytes())
	return b.Bytes(), nil
}
