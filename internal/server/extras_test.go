package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnnouncement(t *testing.T) {
	s, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	if string(a.Profile.Announcement) != "null" || string(a.Profile.Settings) != "{}" {
		t.Fatalf("defaults: %s %s", a.Profile.Announcement, a.Profile.Settings)
	}
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", `{"is_published":true,"announcement":{"layout":"popup","title":"Sale!","description":"20% off","cta":{"label":"Shop","url":"https://shop.example"},"bg_color":"#000","text_color":"#fff"}}`, tok).
		expect(200).json(&p)
	var ann Announcement
	json.Unmarshal(p.Announcement, &ann)
	if ann.ID == "" || ann.Layout != "popup" || ann.CTA == nil || ann.CTA.URL != "https://shop.example" {
		t.Fatalf("announcement = %s", p.Announcement)
	}
	firstID := ann.ID
	// Editing keeps the id (dismissals stick); delete + re-create gets a new one.
	do(t, h, "PATCH", "/api/v1/profile", `{"announcement":{"title":"Sale ends soon","id":"spoofed"}}`, tok).expect(200).json(&p)
	json.Unmarshal(p.Announcement, &ann)
	if ann.ID != firstID || ann.Layout != "banner" {
		t.Errorf("edit: %s", p.Announcement)
	}
	do(t, h, "PATCH", "/api/v1/profile", `{"announcement":null}`, tok).expect(200).json(&p)
	if string(p.Announcement) != "null" {
		t.Errorf("deleted = %s", p.Announcement)
	}
	do(t, h, "PATCH", "/api/v1/profile", `{"announcement":{"title":"New","cta":{"label":"Join","form_block_id":"`+editor(t, h, a.AccessToken).Tabs[0].ID+`"},"schedule":{"start_at":"2026-06-01T00:00:00Z","end_at":"2026-07-01T00:00:00Z"}}}`, tok).
		expect(200).json(&p)
	json.Unmarshal(p.Announcement, &ann)
	if ann.ID == firstID {
		t.Error("re-created announcement should get a new id")
	}
	// Public read honours the schedule; the editor always shows it.
	var pub pageResponse
	s.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&pub)
	if string(pub.Profile.Announcement) != "null" {
		t.Errorf("not yet live: %s", pub.Profile.Announcement)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC) }
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&pub)
	if !strings.Contains(string(pub.Profile.Announcement), `"New"`) {
		t.Errorf("live: %s", pub.Profile.Announcement)
	}
	s.now = time.Now
	if !strings.Contains(string(editor(t, h, a.AccessToken).Profile.Announcement), `"New"`) {
		t.Error("editor should always show the announcement")
	}
	for _, bad := range []string{
		`{"announcement":{"title":""}}`, `{"announcement":{"title":"x","layout":"modal"}}`,
		`{"announcement":{"title":"x","cta":{"label":"Go","url":"javascript:alert(1)"}}}`,
		`{"announcement":{"title":"x","cta":{"label":"","url":"https://a.example"}}}`,
		`{"announcement":{"title":"x","bg_color":"red;}"}}`, `{"announcement":"hi"}`,
		`{"announcement":{"title":"x","description":"` + strings.Repeat("d", 205) + `"}}`,
	} {
		if r := do(t, h, "PATCH", "/api/v1/profile", bad, tok); r.errCode() != "invalid_announcement" {
			t.Errorf("%.60s: %d %s", bad, r.Code, r.Body)
		}
	}
}

func TestSettings(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", `{"settings":{"ga4_id":"g-abc1234","meta_pixel_id":"123456789012","language":"id","show_share":true,"show_save_contact":true,"contact_email":"a@b.co","contact_phone":"+62 812 3456","junk":"<script>"}}`, tok).
		expect(200).json(&p)
	if !jsonEq(p.Settings, `{"ga4_id":"G-ABC1234","meta_pixel_id":"123456789012","language":"id","show_share":true,"show_save_contact":true,"contact_email":"a@b.co","contact_phone":"+62 812 3456"}`) {
		t.Errorf("settings = %s", p.Settings)
	}
	// An explicit false is stored and returned (Share is on by default in the FE, so false must
	// not collapse into "unset"); a missing key stays missing.
	do(t, h, "PATCH", "/api/v1/profile", `{"settings":{"show_share":false,"show_save_contact":false},"is_published":true}`, tok).expect(200).json(&p)
	if !jsonEq(p.Settings, `{"show_share":false,"show_save_contact":false}`) {
		t.Errorf("explicit false settings = %s", p.Settings)
	}
	var page pageResponse
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&page)
	if !jsonEq(page.Profile.Settings, `{"show_share":false,"show_save_contact":false}`) {
		t.Errorf("public settings = %s", page.Profile.Settings)
	}
	do(t, h, "PATCH", "/api/v1/profile", `{"settings":{"language":"en"}}`, tok).expect(200).json(&p)
	if !jsonEq(p.Settings, `{"language":"en"}`) {
		t.Errorf("unset show_share should be omitted: %s", p.Settings)
	}
	for _, bad := range []string{
		`{"ga4_id":"UA-12345-1"}`, `{"ga4_id":"G-ABC');alert(1);//"}`, `{"meta_pixel_id":"12ab"}`,
		`{"language":"Indonesian"}`, `{"contact_email":"nope"}`, `{"contact_phone":"call me"}`,
	} {
		if r := do(t, h, "PATCH", "/api/v1/profile", `{"settings":`+bad+`}`, tok); r.errCode() != "invalid_settings" {
			t.Errorf("%s: %d %s", bad, r.Code, r.Body)
		}
	}
}

func TestUsernameRedirects(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	b := signup(t, h, "bob")
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "alicia"}, tok).expect(200)

	r := do(t, h, "GET", "/api/v1/public/alice", nil).expect(301)
	if r.Header().Get("Location") != "/api/v1/public/alicia" || !strings.Contains(r.Body.String(), `"redirect_to":"alicia"`) {
		t.Errorf("redirect = %s %s", r.Header().Get("Location"), r.Body)
	}
	do(t, h, "GET", "/api/v1/public/alicia", nil).expect(200)
	// Chain: alice and alicia both lead to the newest name.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "ali"}, tok).expect(200)
	for _, old := range []string{"alice", "Alicia"} {
		if loc := do(t, h, "GET", "/api/v1/public/"+old, nil).expect(301).Header().Get("Location"); loc != "/api/v1/public/ali" {
			t.Errorf("%s → %s", old, loc)
		}
	}
	// Others can't take a retired name; the owner can reclaim it.
	var av struct{ Available bool }
	do(t, h, "GET", "/api/v1/auth/username-available?u=alice", nil).expect(200).json(&av)
	if av.Available {
		t.Error("retired name reported available")
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "alice"}, bearer(b.AccessToken)).expect(409)
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "c@example.com", "password": "password123", "username": "alicia"}).expect(409)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "alice"}, tok).expect(200)
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200)
	if loc := do(t, h, "GET", "/api/v1/public/ali", nil).expect(301).Header().Get("Location"); loc != "/api/v1/public/alice" {
		t.Errorf("ali → %s", loc)
	}
	// Unpublished target: plain 404.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": false}, tok).expect(200)
	do(t, h, "GET", "/api/v1/public/ali", nil).expect(404)
	// At most 5 redirects are kept.
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"} {
		do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": n + "xx"}, tok).expect(200)
	}
	var count int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM username_redirects`).Scan(&count)
	if count != 5 {
		t.Errorf("redirects kept = %d", count)
	}
	// Temporary usernames never leave a redirect.
	var tmp authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "t@example.com", "password": "password123"}).expect(201).json(&tmp)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "tina"}, bearer(tmp.AccessToken)).expect(200)
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM username_redirects WHERE old_username LIKE 'tmp.%'`).Scan(&count)
	if count != 0 {
		t.Error("temp username left a redirect")
	}
}

func TestVCard(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	s.cfg.PublicBaseURL = "http://api.test"
	s.cfg.AppBaseURL = "https://tautan.test"
	h := s.Routes()
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, multipartReq(t, "file", "a.png", pngBytes(), a.AccessToken))
	var up struct{ URL string }
	resp{rec, t}.expect(201).json(&up)

	do(t, h, "GET", "/api/v1/public/alice/vcard", nil).expect(404)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{
		"is_published": true, "display_name": "Alice; Smith, PhD", "bio": "Line one\nLine two " + strings.Repeat("é", 60),
		"avatar_url": up.URL, "socials": []map[string]string{{"platform": "instagram", "url": "https://instagram.com/alice"}},
		"settings": map[string]any{"contact_email": "alice@example.com", "contact_phone": "+62 812"},
	}, tok).expect(200)
	r := do(t, h, "GET", "/api/v1/public/alice/vcard", nil).expect(200)
	body := r.Body.String()
	if r.Header().Get("Content-Type") != "text/vcard; charset=utf-8" || !strings.Contains(r.Header().Get("Content-Disposition"), `alice.vcf`) {
		t.Errorf("headers = %v", r.Header())
	}
	unfolded := strings.ReplaceAll(body, "\r\n ", "")
	for _, want := range []string{
		"BEGIN:VCARD\r\n", "VERSION:3.0\r\n", `FN:Alice\; Smith\, PhD`, `NOTE:Line one\nLine two`, "URL:https://tautan.test/alice\r\n",
		"EMAIL;TYPE=INTERNET:alice@example.com", "TEL;TYPE=CELL:+62 812", "PHOTO;ENCODING=b;TYPE=PNG:",
		"X-SOCIALPROFILE;TYPE=instagram:https://instagram.com/alice", "END:VCARD\r\n",
	} {
		if !strings.Contains(unfolded, want) {
			t.Errorf("vcard missing %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\r\n") {
		if len(line) > 75 {
			t.Errorf("unfolded line (%d octets): %.40s…", len(line), line)
		}
	}
}
