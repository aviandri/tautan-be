package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
)

// captureMailer records messages; fail makes Send return an error.
type captureMailer struct {
	mu   sync.Mutex
	sent []mail.Message
	fail error
}

func (c *captureMailer) Send(_ context.Context, m mail.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureMailer) take() []mail.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sent
	c.sent = nil
	return out
}

// mailServer is a test server with a capturing mailer and a settable clock.
func mailServer(t *testing.T) (*Server, http.Handler, *captureMailer, *time.Time) {
	t.Helper()
	s, h := newTestServer(t)
	m := &captureMailer{}
	s.mailer = m
	clock := time.Now()
	s.now = func() time.Time { return clock }
	return s, h, m, &clock
}

func deliver(t *testing.T, s *Server) int {
	t.Helper()
	n, err := s.DeliverPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var resetLinkRE = regexp.MustCompile(`/reset-password\?token=([A-Za-z0-9_%-]+)`)

func resetToken(t *testing.T, m mail.Message) string {
	t.Helper()
	sm := resetLinkRE.FindStringSubmatch(m.Text)
	if sm == nil {
		t.Fatalf("no reset link in %q", m.Text)
	}
	tok, _ := url.QueryUnescape(sm[1])
	return tok
}

func TestPasswordResetFlow(t *testing.T) {
	s, h, m, clock := mailServer(t)
	old := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "Rina@Example.com", "password": "old-password", "username": "rina"}).expect(201)
	var oldAuth authResponse
	old.json(&oldAuth)
	oldRefresh := refreshCookieFrom(t, old)

	// Unknown emails look exactly the same and send nothing.
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": "nobody@example.com"}).expect(202)
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": "RINA@example.com"}).expect(202)
	if n := deliver(t, s); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	msgs := m.take()
	if len(msgs) != 1 || msgs[0].To != "rina@example.com" || msgs[0].Subject != "Reset your Tautan password" || msgs[0].HTML == "" {
		t.Fatalf("sent = %+v", msgs)
	}
	tok := resetToken(t, msgs[0])

	// The sent row no longer holds the link.
	var status, body string
	_ = testPool.QueryRow(t.Context(), `SELECT status, text_body FROM email_outbox`).Scan(&status, &body)
	if status != "sent" || body != "" {
		t.Errorf("outbox row = %s %q", status, body)
	}

	*clock = clock.Add(5 * time.Second)
	if c := do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": tok, "new_password": "short"}).expect(400).errCode(); c != "invalid_password" {
		t.Errorf("code = %s", c)
	}
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": tok, "new_password": "brand-new-password"}).expect(200).json(&a)
	if a.User.ID != oldAuth.User.ID || a.AccessToken == "" {
		t.Fatalf("reset = %+v", a)
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(200)

	// Old sessions and the old password are dead; the link is single-use.
	do(t, h, "GET", "/api/v1/me", nil, bearer(oldAuth.AccessToken)).expect(401)
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(oldRefresh)).expect(401)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "rina@example.com", "password": "old-password"}).expect(401)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "rina@example.com", "password": "brand-new-password"}).expect(200)
	if c := do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": tok, "new_password": "again-password"}).expect(400).errCode(); c != "invalid_token" {
		t.Errorf("reused token code = %s", c)
	}
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": "made-up", "new_password": "again-password"}).expect(400)
}

func TestPasswordResetExpiryAndSiblings(t *testing.T) {
	s, h, m, clock := mailServer(t)
	signup(t, h, "budi")
	email := "budi@example.com"

	// Two links: using one kills the other.
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": email}).expect(202)
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": email}).expect(202)
	deliver(t, s)
	msgs := m.take()
	if len(msgs) != 2 {
		t.Fatalf("sent %d", len(msgs))
	}
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": resetToken(t, msgs[1]), "new_password": "first-new-pass"}).expect(200)
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": resetToken(t, msgs[0]), "new_password": "second-new-pass"}).expect(400)

	// Expired after 30 minutes.
	*clock = clock.Add(2 * time.Hour) // also clears the hourly cap
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": email}).expect(202)
	deliver(t, s)
	tok := resetToken(t, m.take()[0])
	*clock = clock.Add(resetTTL + time.Second)
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": tok, "new_password": "late-new-pass"}).expect(400)
}

func TestPasswordResetHourlyCap(t *testing.T) {
	s, h, m, _ := mailServer(t)
	signup(t, h, "cap")
	for range 5 {
		do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": "cap@example.com"}).expect(202)
	}
	deliver(t, s)
	if n := len(m.take()); n != resetPerHour {
		t.Fatalf("sent %d, want %d", n, resetPerHour)
	}
}

func TestPasswordResetGivesGoogleOnlyAccountAPassword(t *testing.T) {
	s, h, m, _ := mailServer(t)
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, testPool, func(tx pgx.Tx) error {
		_, _, err := createAccount(ctx, tx, "g@example.com", nil, tempUsername(), Onboarding{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": "g@example.com"}).expect(202)
	deliver(t, s)
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": resetToken(t, m.take()[0]), "new_password": "now-i-have-one"}).expect(200)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "g@example.com", "password": "now-i-have-one"}).expect(200)
}

func TestOutboxRetriesWithBackoffThenGivesUp(t *testing.T) {
	s, _, m, clock := mailServer(t)
	m.fail = errors.New("421 try later")
	if err := s.enqueueEmail(t.Context(), testPool, "test", mail.Message{To: "x@example.com", Subject: "hi", Text: "secret-link"}); err != nil {
		t.Fatal(err)
	}
	if n := deliver(t, s); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	// Not due again until the backoff passes.
	if n := deliver(t, s); n != 0 {
		t.Fatalf("re-claimed during backoff: %d", n)
	}
	*clock = clock.Add(outboxBackoff(1) + time.Second)
	m.fail = nil
	if n := deliver(t, s); n != 1 || len(m.take()) != 1 {
		t.Fatal("retry did not send")
	}

	// A message that keeps failing is marked failed and its body cleared.
	m.fail = errors.New("550 no such user")
	_ = s.enqueueEmail(t.Context(), testPool, "test", mail.Message{To: "y@example.com", Subject: "hi", Text: "secret-link"})
	for i := 1; i <= outboxMaxAttempts; i++ {
		if n := deliver(t, s); n != 1 {
			t.Fatalf("attempt %d claimed %d", i, n)
		}
		*clock = clock.Add(time.Hour + time.Second)
	}
	if n := deliver(t, s); n != 0 {
		t.Fatal("failed message retried")
	}
	var status, body, lastErr string
	var attempts int
	_ = testPool.QueryRow(t.Context(), `SELECT status, text_body, last_error, attempts FROM email_outbox WHERE to_email = 'y@example.com'`).
		Scan(&status, &body, &lastErr, &attempts)
	if status != "failed" || body != "" || lastErr != "550 no such user" || attempts != outboxMaxAttempts {
		t.Errorf("row = %s %q %q %d", status, body, lastErr, attempts)
	}
}

func TestOutboxBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 8: time.Hour, 20: time.Hour} {
		if got := outboxBackoff(n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}
