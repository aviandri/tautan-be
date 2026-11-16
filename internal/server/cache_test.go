package server

import "testing"

func TestPublicPageETag(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "cachy", true)
	r := do(t, h, "GET", "/api/v1/public/cachy", nil).expect(200)
	etag := r.Header().Get("ETag")
	if etag == "" || r.Header().Get("Cache-Control") != "public, s-maxage=60, stale-while-revalidate=600" {
		t.Fatalf("headers = %v", r.Header())
	}
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		c := do(t, h, "GET", "/api/v1/public/cachy", nil, withHeader("If-None-Match", inm))
		if c.Code != 304 || c.Body.Len() != 0 || c.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %s: %d %q", inm, c.Code, c.Body)
		}
	}
	do(t, h, "GET", "/api/v1/public/cachy", nil, withHeader("If-None-Match", `"stale"`)).expect(200)

	// Any change to the page changes the ETag.
	do(t, h, "PATCH", "/api/v1/blocks/"+f.link.ID, map[string]any{"data": map[string]any{"title": "New", "url": "https://x.example"}}, bearer(f.tok)).expect(200)
	r2 := do(t, h, "GET", "/api/v1/public/cachy", nil, withHeader("If-None-Match", etag)).expect(200)
	if r2.Header().Get("ETag") == etag {
		t.Error("ETag did not change after an edit")
	}
}
