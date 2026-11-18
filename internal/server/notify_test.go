package server

import (
	"net/mail"
	"strings"
	"testing"
	"time"

	tmail "github.com/aviandri/tautan-be/internal/mail"
)

type tmailMessage = tmail.Message

var tmailBuild = tmail.Build

const submissionPrefix = "New response:"

func takeSubmissions(m *captureMailer) []string {
	var subjects []string
	for _, msg := range m.take() {
		if strings.HasPrefix(msg.Subject, submissionPrefix) {
			subjects = append(subjects, msg.Subject)
		}
	}
	return subjects
}

func TestContactAndFormNotifyOwner(t *testing.T) {
	s, h, m, _ := mailServer(t)
	f := setupPage(t, h, "nina", true) // signup() marks the owner verified
	form := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "form", "data": map[string]any{
		"title": "Book a\r\nshoot",
		"fields": []map[string]any{
			{"id": "name", "type": "text", "label": "Name"},
			{"id": "email", "type": "email", "label": "Email"},
			{"id": "when", "type": "date", "label": "Date"},
			{"id": "ok", "type": "checkbox", "label": "Agree"},
		}}})

	do(t, h, "POST", "/api/v1/public/nina/contact", map[string]string{
		"block_id": f.form.ID, "name": "Budi <b>", "email": "budi@example.com", "message": "Hi!\n<script>x</script>",
	}).expect(201)
	do(t, h, "POST", "/api/v1/public/nina/form", map[string]any{"block_id": form.ID, "answers": map[string]any{
		"name": "Sari", "email": "sari@example.com", "when": "2026-12-01", "ok": true,
	}}).expect(201)
	deliver(t, s)
	var msgs []tmailMessage // skip the owner's signup confirmation email
	for _, x := range m.take() {
		if strings.HasPrefix(x.Subject, submissionPrefix) {
			msgs = append(msgs, x)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("sent %d: %+v", len(msgs), msgs)
	}
	contact, resp := msgs[0], msgs[1]
	if contact.To != "nina@example.com" || contact.ReplyTo != "budi@example.com" || !strings.Contains(contact.Subject, "Say hi") {
		t.Errorf("contact notice = %+v", contact)
	}
	if !strings.Contains(contact.Text, "Message: Hi!\n<script>x</script>") || !strings.Contains(contact.Text, "/dashboard/inbox") {
		t.Errorf("contact text = %q", contact.Text)
	}
	if strings.Contains(contact.HTML, "<script>x") || !strings.Contains(contact.HTML, "&lt;script&gt;") {
		t.Error("visitor text not escaped in HTML")
	}
	// Owner-controlled title flattened to one line in the subject.
	if resp.Subject != "New response: Book a shoot (nina)" || resp.ReplyTo != "sari@example.com" {
		t.Errorf("form notice subject=%q reply=%q", resp.Subject, resp.ReplyTo)
	}
	for _, want := range []string{"Name: Sari", "Date: 2026-12-01", "Agree: Yes", "/dashboard/forms"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("form text lacks %q: %q", want, resp.Text)
		}
	}
	// The built message carries Reply-To.
	from, _ := mail.ParseAddress("Tautan <hello@mail.tautan.id>")
	to, _ := mail.ParseAddress(resp.To)
	raw, err := buildForTest(from, to, resp)
	if err != nil || !strings.Contains(raw, "Reply-To: <sari@example.com>") {
		t.Errorf("Reply-To header missing (%v): %.300s", err, raw)
	}
}

func TestNotificationsOptOutAndPreconditions(t *testing.T) {
	s, h, m, clock := mailServer(t)
	f := setupPage(t, h, "omar", true)
	tok := bearer(f.tok)
	contact := func() {
		do(t, h, "POST", "/api/v1/public/omar/contact", map[string]string{
			"block_id": f.form.ID, "name": "V", "email": "v@example.com", "message": "hello",
		}).expect(201)
	}

	var me meResp
	do(t, h, "GET", "/api/v1/me", nil, tok).expect(200).json(&me)
	if !me.User.NotifySubmissions {
		t.Fatal("notifications should default to on")
	}
	do(t, h, "PATCH", "/api/v1/me/notifications", map[string]any{}, tok).expect(400)
	var out map[string]bool
	do(t, h, "PATCH", "/api/v1/me/notifications", map[string]any{"submissions": false}, tok).expect(200).json(&out)
	if out["submissions"] {
		t.Fatal("not turned off")
	}
	contact()
	deliver(t, s)
	if got := takeSubmissions(m); len(got) != 0 {
		t.Fatalf("opted-out owner got %v", got)
	}
	do(t, h, "PATCH", "/api/v1/me/notifications", map[string]any{"submissions": true}, tok).expect(200)

	// An unconfirmed owner address gets nothing (visitor data stays in the inbox).
	if _, err := testPool.Exec(t.Context(), `UPDATE users SET email_verified_at = NULL`); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(11 * time.Minute) // contact form limiter window
	contact()
	deliver(t, s)
	if got := takeSubmissions(m); len(got) != 0 {
		t.Fatalf("unverified owner got %v", got)
	}
	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM contact_submissions`).Scan(&n)
	if n != 2 {
		t.Errorf("submissions stored = %d", n)
	}
}

func TestNotificationsHourlyCap(t *testing.T) {
	s, h, m, clock := mailServer(t)
	f := setupPage(t, h, "flood", true)
	s.contactLimiter = newRateLimiter(1000, time.Minute) // isolate the per-owner cap
	for i := range notifyPerHour + 5 {
		do(t, h, "POST", "/api/v1/public/flood/contact", map[string]string{
			"block_id": f.form.ID, "name": "Bot", "email": "bot@example.com", "message": strings.Repeat("x", i+1),
		}).expect(201)
	}
	deliver(t, s)
	deliver(t, s)
	if got := len(takeSubmissions(m)); got != notifyPerHour {
		t.Fatalf("sent %d, want cap %d", got, notifyPerHour)
	}
	*clock = clock.Add(time.Hour + time.Minute)
	do(t, h, "POST", "/api/v1/public/flood/contact", map[string]string{
		"block_id": f.form.ID, "name": "Real", "email": "real@example.com", "message": "after the hour",
	}).expect(201)
	deliver(t, s)
	if got := len(takeSubmissions(m)); got != 1 {
		t.Fatalf("after an hour sent %d", got)
	}
}

func buildForTest(from, to *mail.Address, m tmailMessage) (string, error) {
	b, err := tmailBuild(from, to, m, time.Now())
	return string(b), err
}
