package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLeadMagnetEmail(t *testing.T) {
	s, _ := newTestServer(t)
	m := &captureMailer{}
	s.mailer = m
	s.cfg.UploadDir, s.cfg.PublicBaseURL, s.cfg.AppBaseURL = t.TempDir(), "http://api.test", "http://app.test"
	h := s.Routes()
	os.WriteFile(filepath.Join(s.cfg.UploadDir, "0123456789abcdef0123456789abcdef.pdf"), []byte("%PDF-guide"), 0o644)
	f := setupPage(t, h, "leadmail", true)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "lead_magnet", "data": map[string]any{
		"title": "Free\nguide", "file_url": "http://api.test/uploads/0123456789abcdef0123456789abcdef.pdf"}})
	lead := func(email string) bool {
		var out struct {
			DownloadURL string `json:"download_url"`
			Emailed     bool   `json:"emailed"`
		}
		do(t, h, "POST", "/api/v1/public/leadmail/blocks/"+card.ID+"/lead", map[string]any{"email": email}).expect(200).json(&out)
		if out.DownloadURL == "" {
			t.Fatal("on-page link missing")
		}
		return out.Emailed
	}
	setVerified := func(v bool) {
		if _, err := testPool.Exec(t.Context(), `UPDATE users SET email_verified_at = CASE WHEN $2 THEN now() END
			WHERE id = (SELECT user_id FROM profiles WHERE username = $1)`, "leadmail", v); err != nil {
			t.Fatal(err)
		}
	}

	// MAIL_DRIVER=log: nothing is emailed, the page link is the only one.
	s.cfg.MailDriver = "log"
	setVerified(true)
	if lead("a@example.com") {
		t.Error("emailed with the log driver")
	}
	// SMTP, but the owner hasn't confirmed their email.
	s.cfg.MailDriver = "smtp"
	setVerified(false)
	if lead("b@example.com") {
		t.Error("emailed for an unconfirmed owner")
	}
	setVerified(true)
	if !lead("Reader@Example.com") {
		t.Fatal("not emailed")
	}
	deliver(t, s)
	msgs := m.takeSubject("Your download: Free guide")
	if len(msgs) != 1 || msgs[0].To != "reader@example.com" || msgs[0].Subject != "Your download: Free guide" {
		t.Fatalf("emails = %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "http://app.test/leadmail") {
		t.Errorf("no page URL in %q", msgs[0].Text)
	}
	link := regexp.MustCompile(`http://api\.test(/api/v1/public/leadmail/download/[^\s"]+)`).FindStringSubmatch(msgs[0].Text)
	if link == nil {
		t.Fatalf("no link in %q", msgs[0].Text)
	}
	// The emailed link outlives the 15-minute on-page one.
	s.now = func() time.Time { return time.Now().Add(6 * 24 * time.Hour) }
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", link[1], nil))
	if rec.Code != 200 || rec.Body.String() != "%PDF-guide" {
		t.Errorf("emailed link after 6 days = %d", rec.Code)
	}
	s.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", link[1], nil))
	if rec.Code != 410 {
		t.Errorf("emailed link after 8 days = %d", rec.Code)
	}
	s.now = time.Now

	// At most 3 a day to one address, whatever page asks.
	s.formLimiter = newRateLimiter(100, time.Hour)
	lead("reader@example.com")
	lead("reader@example.com")
	if lead("reader@example.com") {
		t.Error("4th email to the same address in a day")
	}
	// Per-card cap.
	s.leadPageLimiter = newRateLimiter(1, time.Hour)
	lead("c@example.com")
	if lead("d@example.com") {
		t.Error("card cap not applied")
	}
}
