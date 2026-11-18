package server

import (
	"fmt"
	"testing"
)

type pageFixture struct {
	tok                       string
	tab                       Tab
	link, hidden, form, child Block
	folderHidden              Block
}

func setupPage(t *testing.T, h httpHandler, username string, publish bool) pageFixture {
	t.Helper()
	a := signup(t, h, username)
	f := pageFixture{tok: a.AccessToken}
	f.tab = editor(t, h, a.AccessToken).Tabs[0]
	mk := func(typ string, data map[string]any, extra map[string]any) Block {
		body := map[string]any{"tab_id": f.tab.ID, "type": typ, "data": data}
		for k, v := range extra {
			body[k] = v
		}
		return createBlock(t, h, a.AccessToken, body)
	}
	f.link = mk("link", map[string]any{"title": "Site", "url": "https://x.example"}, nil)
	f.hidden = mk("text", map[string]any{"content": "secret"}, nil)
	f.form = mk("contact_form", map[string]any{"title": "Say hi", "button_label": "Send"}, nil)
	f.folderHidden = mk("folder", map[string]any{"title": "F"}, nil)
	f.child = mk("link", map[string]any{"title": "c", "url": "https://c.example"}, map[string]any{"parent_id": f.folderHidden.ID})
	for _, id := range []string{f.hidden.ID, f.folderHidden.ID} {
		do(t, h, "PATCH", "/api/v1/blocks/"+id, map[string]any{"is_visible": false}, bearer(a.AccessToken)).expect(200)
	}
	if publish {
		do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, bearer(a.AccessToken)).expect(200)
	}
	return f
}

func TestPublicPage(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	setupPage(t, h, "draft", false)

	var p pageResponse
	do(t, h, "GET", "/api/v1/public/Alice", nil).expect(200).json(&p)
	if p.Profile.Username != "alice" || len(p.Tabs) != 1 {
		t.Fatalf("page = %+v", p.Profile)
	}
	ids := map[string]bool{}
	for _, b := range p.Blocks {
		ids[b.ID] = true
	}
	if !ids[f.link.ID] || !ids[f.form.ID] || ids[f.hidden.ID] || ids[f.folderHidden.ID] || ids[f.child.ID] {
		t.Errorf("visibility filtering wrong: %v", ids)
	}
	do(t, h, "GET", "/api/v1/public/draft", nil).expect(404)
	do(t, h, "GET", "/api/v1/public/nobody", nil).expect(404)
}

func TestPublicEvents(t *testing.T) {
	s, _ := newTestServer(t)
	// httptest requests come from 192.0.2.1: trust it so CF-IPCountry counts.
	s.cfg.TrustedProxies = []string{"192.0.2.0/24"}
	s.proxy = newProxyResolver(s.cfg)
	h := s.Routes()
	f := setupPage(t, h, "alice", true)
	setupPage(t, h, "draft", false)
	other := setupPage(t, h, "bob", true)
	ua := withHeader("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile/15E148")

	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view", "referrer": "https://www.Instagram.com/p/abc"},
		ua, withHeader("CF-IPCountry", "id"), withHeader("Referer", "http://localhost:3000/alice")).expect(204)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view"},
		withHeader("Referer", "http://localhost:3000/alice"), withHeader("User-Agent", "Mozilla/5.0 (Windows NT 10.0)")).expect(204)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "click", "block_id": f.link.ID, "referrer": ""},
		withHeader("Referer", "https://t.co/xyz"), withHeader("User-Agent", "Mozilla/5.0 (iPad; CPU OS 17_0)")).expect(204)
	// Header fallback when body has no referrer.
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view"}, withHeader("Referer", "https://t.co/xyz")).expect(204)
	// Bots: accepted, not recorded.
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view"}, withHeader("User-Agent", "Googlebot/2.1")).expect(204)

	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "like"}).expect(400)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "click"}).expect(400)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "click", "block_id": other.link.ID}).expect(400)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "click", "block_id": "nope"}).expect(400)
	do(t, h, "POST", "/api/v1/public/draft/events", map[string]any{"kind": "view"}).expect(404)

	rows, err := testPool.Query(t.Context(), `SELECT kind, COALESCE(block_id::text, ''), referrer, country, device FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var k, b, r, c, d string
		rows.Scan(&k, &b, &r, &c, &d)
		if b != "" {
			b = "B"
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s", k, b, r, c, d))
	}
	want := []string{
		"view||instagram.com|ID|mobile",
		"view||||desktop",
		"click|B|||tablet",
		"view||t.co||unknown",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
}

func TestEventRateLimit(t *testing.T) {
	s, h := newTestServer(t)
	setupPage(t, h, "alice", true)
	s.eventLimiter = newRateLimiter(2, 60e9)
	for i := 0; i < 4; i++ {
		do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view"}).expect(204)
	}
	var n int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM events`).Scan(&n)
	if n != 2 {
		t.Errorf("recorded %d events, want 2", n)
	}
}

func TestContactAndInbox(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	other := setupPage(t, h, "bob", true)
	s.contactLimiter = newRateLimiter(1000, 60e9)
	post := func(body map[string]any) resp { return do(t, h, "POST", "/api/v1/public/alice/contact", body) }
	good := func(i int) map[string]any {
		return map[string]any{"block_id": f.form.ID, "name": "Vis", "email": "v@example.com", "message": fmt.Sprintf("hello %d", i)}
	}

	post(good(0)).expect(201)
	for name, mut := range map[string]func(m map[string]any){
		"no name":       func(m map[string]any) { m["name"] = " " },
		"bad email":     func(m map[string]any) { m["email"] = "nope" },
		"no message":    func(m map[string]any) { m["message"] = "" },
		"link block":    func(m map[string]any) { m["block_id"] = f.link.ID },
		"other's block": func(m map[string]any) { m["block_id"] = other.form.ID },
		"bad uuid":      func(m map[string]any) { m["block_id"] = "x" },
	} {
		m := good(1)
		mut(m)
		if r := post(m); r.Code != 400 {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	// Hidden contact form doesn't accept submissions.
	do(t, h, "PATCH", "/api/v1/blocks/"+f.form.ID, map[string]any{"is_visible": false}, bearer(f.tok)).expect(200)
	post(good(2)).expect(400)
	do(t, h, "PATCH", "/api/v1/blocks/"+f.form.ID, map[string]any{"is_visible": true}, bearer(f.tok)).expect(200)

	for i := 1; i <= 24; i++ {
		post(good(i)).expect(201)
	}

	// Inbox pagination: 25 total → 20 + 5, newest first.
	var page struct {
		Items      []ContactSubmission `json:"items"`
		NextCursor *string             `json:"next_cursor"`
	}
	do(t, h, "GET", "/api/v1/contact-submissions", nil, bearer(f.tok)).expect(200).json(&page)
	if len(page.Items) != 20 || page.NextCursor == nil || page.Items[0].Message != "hello 24" {
		t.Fatalf("page1: %d items, next=%v, first=%q", len(page.Items), page.NextCursor, page.Items[0].Message)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.ID] = true
	}
	cur := *page.NextCursor
	page.Items, page.NextCursor = nil, nil
	do(t, h, "GET", "/api/v1/contact-submissions?cursor="+cur, nil, bearer(f.tok)).expect(200).json(&page)
	if len(page.Items) != 5 || page.NextCursor != nil || page.Items[4].Message != "hello 0" {
		t.Fatalf("page2: %d items next=%v", len(page.Items), page.NextCursor)
	}
	for _, it := range page.Items {
		if seen[it.ID] {
			t.Error("duplicate across pages")
		}
	}
	do(t, h, "GET", "/api/v1/contact-submissions?cursor=garbage", nil, bearer(f.tok)).expect(400)

	// Bob's inbox is empty and returns [] not null.
	r := do(t, h, "GET", "/api/v1/contact-submissions", nil, bearer(other.tok)).expect(200)
	if r.Body.String() != "{\"items\":[],\"next_cursor\":null}\n" {
		t.Errorf("empty inbox = %s", r.Body)
	}

	// Rate limit.
	s.contactLimiter = newRateLimiter(1, 60e9)
	post(good(99)).expect(201)
	if c := post(good(100)).expect(429).errCode(); c != "rate_limited" {
		t.Errorf("code = %s", c)
	}
}
