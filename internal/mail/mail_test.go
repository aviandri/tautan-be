package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTP accepts one session and records the envelope, auth and data.
type fakeSMTP struct {
	addr                 string
	from, rcpt, authLine string
	data                 chan string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{addr: l.Addr().String(), data: make(chan string, 1)}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
		say("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				say("250-fake")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(up, "AUTH PLAIN"):
				f.authLine = line
				say("235 ok")
			case strings.HasPrefix(up, "MAIL FROM:"):
				f.from = line[len("MAIL FROM:"):]
				say("250 ok")
			case strings.HasPrefix(up, "RCPT TO:"):
				f.rcpt = line[len("RCPT TO:"):]
				say("250 ok")
			case up == "DATA":
				say("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				f.data <- b.String()
				say("250 queued")
			case up == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return f
}

func TestSMTPSendsMultipartMessage(t *testing.T) {
	f := newFakeSMTP(t)
	host, port, _ := net.SplitHostPort(f.addr)
	p, _ := strconv.Atoi(port)
	s, err := New(Config{Driver: "smtp", From: "Tautan <hello@mail.tautan.id>", Host: host, Port: p, Username: "resend", Password: "re_key"})
	if err != nil {
		t.Fatal(err)
	}
	m := PasswordReset("jane@example.com", "https://tautan.id/reset-password?token=abc")
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	raw := <-f.data
	if f.from != "<hello@mail.tautan.id>" || f.rcpt != "<jane@example.com>" {
		t.Errorf("envelope from=%q rcpt=%q", f.from, f.rcpt)
	}
	if dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(f.authLine, "AUTH PLAIN ")); string(dec) != "\x00resend\x00re_key" {
		t.Errorf("auth = %q", dec)
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); subj != "Reset your Tautan password" {
		t.Errorf("subject = %q", subj)
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatalf("content-type = %s", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(part) // NextPart decodes quoted-printable
		types = append(types, part.Header.Get("Content-Type"))
		if !strings.Contains(string(body), "https://tautan.id/reset-password?token=abc") {
			t.Errorf("%s part lacks the link: %s", part.Header.Get("Content-Type"), body)
		}
	}
	if len(types) != 2 {
		t.Errorf("parts = %v", types)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Driver: "carrier-pigeon"}); err == nil {
		t.Error("unknown driver accepted")
	}
	if _, err := New(Config{Driver: "smtp", From: "x@y.z"}); err == nil {
		t.Error("smtp without host accepted")
	}
	if _, err := New(Config{Driver: "smtp", Host: "h", Port: 587, From: "not an address"}); err == nil {
		t.Error("bad from accepted")
	}
	if s, err := New(Config{}); err != nil || s == nil {
		t.Error("log driver should be the default")
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	from, _ := mail.ParseAddress("a@b.c")
	to, _ := mail.ParseAddress("d@e.f")
	if _, err := Build(from, to, Message{Subject: "hi\r\nBcc: x@evil", Text: "x"}, time.Now()); err == nil {
		t.Fatal("newline in subject accepted")
	}
}
