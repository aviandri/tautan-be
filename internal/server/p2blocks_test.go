package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestP2BlockValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	ok := func(typ string, data string) Block {
		t.Helper()
		var b Block
		do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok).expect(201).json(&b)
		return b
	}
	bad := func(typ string, data string) {
		t.Helper()
		r := do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok)
		if r.Code != 400 {
			t.Errorf("%s %s: got %d %s", typ, data, r.Code, r.Body)
		}
	}

	b := ok("link", `{"title":"Shop","url":"https://s.example","description":"d","layout":"grid2","icon":"🔥","label":"NEW","text_align":"left","dest_kind":"url","schedule":{"start_at":"2026-01-01T00:00:00Z"}}`)
	if !jsonEq(b.Data, `{"title":"Shop","url":"https://s.example","description":"d","layout":"grid2","icon":"🔥","label":"NEW","text_align":"left","dest_kind":"url","schedule":{"start_at":"2026-01-01T00:00:00Z"}}`) {
		t.Errorf("link data = %s", b.Data)
	}
	b = ok("link", `{"title":"Mail me","url":"me@example.com","dest_kind":"email"}`)
	if !strings.Contains(string(b.Data), `"mailto:me@example.com"`) {
		t.Errorf("email link not normalised: %s", b.Data)
	}
	ok("link", `{"title":"Go to shop","url":"","dest_kind":"tab","tab_id":"`+tab.ID+`"}`)
	ok("link", `{"title":"x","url":"https://x.example","icon":"https://cdn.example/i.png"}`)
	bad("link", `{"title":"x","url":"https://x.example","layout":"mosaic"}`)
	bad("link", `{"title":"x","url":"nope","dest_kind":"email"}`)
	bad("link", `{"title":"x","dest_kind":"tab"}`)
	bad("link", `{"title":"x","url":"https://x.example","icon":"this is not an emoji"}`)
	bad("link", `{"title":"x","url":"https://x.example","label":"`+strings.Repeat("L", 25)+`"}`)
	bad("link", `{"title":"x","url":"https://x.example","schedule":{"start_at":"2026-02-01T00:00:00Z","end_at":"2026-01-01T00:00:00Z"}}`)
	bad("link", `{"title":"x","url":"https://x.example","schedule":{"start_at":"tomorrow"}}`)

	ok("image", `{"url":"https://i.example/a.png","full_bleed":true,"lightbox":true}`)
	ok("folder", `{"title":"F","style":"inline"}`)
	bad("folder", `{"title":"F","style":"grid"}`)
	ok("faq", `{"heading":"FAQ","description":"d","items":[{"q":"a","a":"b"}]}`)
	ok("testimonial", `{"heading":"H","items":[{"quote":"q","author":"a","role":"CEO","url":"https://a.example"}]}`)
	bad("testimonial", `{"items":[{"quote":"q","author":"a","url":"javascript:x"}]}`)

	ok("heading", `{"title":"Section","text_align":"center"}`)
	ok("heading", `{}`)
	ok("divider", `{"style":"dashed"}`)
	bad("divider", `{"style":"wavy"}`)
	ok("file", `{"title":"Menu","url":"/uploads/abc.pdf","file_name":"menu.pdf","size":1234}`)
	bad("file", `{"title":"","url":"/uploads/abc.pdf"}`)
	ok("discount_code", `{"code":"SAVE20","description":"20% off","discount":"20%","url":"https://shop.example"}`)
	ok("discount_code", `{"code":""}`)
	bad("discount_code", `{"code":"`+strings.Repeat("C", 41)+`"}`)
	ok("countdown", `{"ends_at":"2026-12-31T23:59:00+07:00","label":"Launch"}`)
	bad("countdown", `{"label":"no date"}`)
	ok("booking", `{"url":"https://calendly.com/alice/30min"}`)
	ok("booking", `{"url":"https://cal.com/alice"}`)
	bad("booking", `{"url":"https://evil.example/calendly.com"}`)
	bad("booking", `{"url":"http://calendly.com/alice"}`)
	ok("quote", `{"quote":"Stay hungry","name":"S","role":"r","heading":"h"}`)
	bad("quote", `{"name":"no quote"}`)
	ok("email_signup", `{"title":"Join","button_label":"Subscribe"}`)
}

func TestScheduleFilteringOnPublic(t *testing.T) {
	s, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	mk := func(title, sched string, parent string) Block {
		body := `{"tab_id":"` + tab.ID + `","type":"link","data":{"title":"` + title + `","url":"https://x.example"` + sched + `}`
		if parent != "" {
			body += `,"parent_id":"` + parent + `"`
		}
		var b Block
		do(t, h, "POST", "/api/v1/blocks", body+`}`, tok).expect(201).json(&b)
		return b
	}
	always := mk("always", "", "")
	live := mk("live", `,"schedule":{"start_at":"2026-06-01T00:00:00Z","end_at":"2026-07-01T00:00:00Z"}`, "")
	future := mk("future", `,"schedule":{"start_at":"2026-06-15T12:00:01Z"}`, "")
	expired := mk("expired", `,"schedule":{"end_at":"2026-06-15T12:00:00Z"}`, "")
	var folder Block
	do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"folder","data":{"title":"F","schedule":{"start_at":"2027-01-01T00:00:00Z"}}}`, tok).expect(201).json(&folder)
	child := mk("child", "", folder.ID)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(200)

	var p pageResponse
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&p)
	got := map[string]bool{}
	for _, b := range p.Blocks {
		got[b.ID] = true
	}
	if !got[always.ID] || !got[live.ID] || got[future.ID] || got[expired.ID] || got[folder.ID] || got[child.ID] {
		t.Errorf("schedule filtering wrong: %v", got)
	}
	// The editor still shows everything.
	if n := len(editor(t, h, a.AccessToken).Blocks); n != 6 {
		t.Errorf("editor blocks = %d", n)
	}
	// Later, future is live.
	now = time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	p = pageResponse{}
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&p)
	found := false
	for _, b := range p.Blocks {
		found = found || b.ID == future.ID
	}
	if !found {
		t.Error("future block should be live now")
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	s, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	link := func(title, parent string) Block {
		body := map[string]any{"tab_id": tab.ID, "type": "link", "data": map[string]any{"title": title, "url": "https://x.example"}}
		if parent != "" {
			body["parent_id"] = parent
		}
		return createBlock(t, h, a.AccessToken, body)
	}
	l1 := link("one", "")
	folder := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "folder", "data": map[string]any{"title": "F"}})
	c1, c2 := link("c1", folder.ID), link("c2", folder.ID)
	l2 := link("two", "")

	deleted := func() []Block {
		var out struct{ Blocks []Block }
		do(t, h, "GET", "/api/v1/blocks/deleted", nil, tok).expect(200).json(&out)
		return out.Blocks
	}
	if d := deleted(); len(d) != 0 {
		t.Fatalf("deleted = %d", len(d))
	}

	// Delete a child on its own, then the folder (which takes c2 with it).
	do(t, h, "DELETE", "/api/v1/blocks/"+c1.ID, nil, tok).expect(204)
	s.now = func() time.Time { return time.Now().Add(time.Second) }
	do(t, h, "DELETE", "/api/v1/blocks/"+folder.ID, nil, tok).expect(204)
	do(t, h, "DELETE", "/api/v1/blocks/"+l1.ID, nil, tok).expect(204)

	if n := len(editor(t, h, a.AccessToken).Blocks); n != 1 {
		t.Fatalf("live blocks = %d, want 1 (l2)", n)
	}
	d := deleted()
	ids := []string{}
	for _, b := range d {
		if b.DeletedAt == nil {
			t.Error("deleted_at missing")
		}
		ids = append(ids, b.ID)
	}
	// c2 went with its folder, so it isn't listed separately.
	if len(d) != 3 || contains(ids, c2.ID) || !contains(ids, folder.ID) || !contains(ids, c1.ID) || !contains(ids, l1.ID) {
		t.Fatalf("deleted list = %v", ids)
	}
	// Deleted blocks can't be edited, reordered or deleted again.
	do(t, h, "PATCH", "/api/v1/blocks/"+l1.ID, map[string]any{"is_visible": false}, tok).expect(404)
	do(t, h, "DELETE", "/api/v1/blocks/"+l1.ID, nil, tok).expect(404)
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{l2.ID, l1.ID}}, tok).expect(400)
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{l2.ID}}, tok).expect(204)

	// Restoring c1 while its folder is deleted puts it at the top level.
	var r Block
	do(t, h, "POST", "/api/v1/blocks/"+c1.ID+"/restore", nil, tok).expect(200).json(&r)
	if r.ParentID != nil || r.DeletedAt != nil || r.Position != 1 {
		t.Errorf("restored c1 = %+v", r)
	}
	// Restoring the folder brings c2 back inside it.
	do(t, h, "POST", "/api/v1/blocks/"+folder.ID+"/restore", nil, tok).expect(200).json(&r)
	if r.Position != 2 {
		t.Errorf("folder position = %d", r.Position)
	}
	live := map[string]Block{}
	for _, b := range editor(t, h, a.AccessToken).Blocks {
		live[b.ID] = b
	}
	if b, ok := live[c2.ID]; !ok || b.ParentID == nil || *b.ParentID != folder.ID {
		t.Errorf("c2 not restored into folder: %+v", b)
	}
	do(t, h, "POST", "/api/v1/blocks/"+folder.ID+"/restore", nil, tok).expect(404)
	do(t, h, "POST", "/api/v1/blocks/nope/restore", nil, tok).expect(404)
	b := signup(t, h, "bob")
	do(t, h, "POST", "/api/v1/blocks/"+l1.ID+"/restore", nil, bearer(b.AccessToken)).expect(404)

	// Purge after 30 days.
	s.now = time.Now
	testPool.Exec(t.Context(), `UPDATE blocks SET deleted_at = now() - interval '31 days' WHERE id = $1`, l1.ID)
	if d := deleted(); len(d) != 0 {
		t.Errorf("not purged: %d", len(d))
	}
	var n int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM blocks WHERE id = $1`, l1.ID).Scan(&n)
	if n != 0 {
		t.Error("purged block still in table")
	}
}

func TestFileUpload(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	h := s.Routes()
	a := signup(t, h, "alice")
	pdf := []byte("%PDF-1.4\n%âãÏÓ\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF")
	req := multipartReq(t, "file", "My Menu.pdf", pdf, a.AccessToken)
	req.URL.RawQuery = "kind=file"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		URL, FileName, ContentType string
		Size                       int64
	}
	r := resp{rec, t}.expect(201)
	_ = json.Unmarshal(r.Body.Bytes(), &out)
	var raw map[string]any
	r.json(&raw)
	if !strings.HasSuffix(raw["url"].(string), ".pdf") || raw["file_name"] != "My Menu.pdf" || raw["content_type"] != "application/pdf" || raw["size"].(float64) != float64(len(pdf)) {
		t.Fatalf("upload = %v", raw)
	}
	path := raw["url"].(string)[strings.Index(raw["url"].(string), "/uploads/"):]
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 || rec.Header().Get("Content-Disposition") != "attachment" {
		t.Errorf("serve pdf: %d %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	// Without kind=file a PDF is rejected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, multipartReq(t, "file", "m.pdf", pdf, a.AccessToken))
	if rec.Code != 415 {
		t.Errorf("pdf as image: %d", rec.Code)
	}
	// HTML is never accepted.
	req = multipartReq(t, "file", "x.txt", []byte("<html><script>alert(1)</script></html>"), a.AccessToken)
	req.URL.RawQuery = "kind=file"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Errorf("html: %d", rec.Code)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
