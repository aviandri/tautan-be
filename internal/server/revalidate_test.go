package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type revalCall struct {
	Usernames []string `json:"usernames"`
}

func TestRevalidateWebhook(t *testing.T) {
	s, _ := newTestServer(t)
	calls := make(chan revalCall, 10)
	fe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-Tautan-Timestamp")
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil ||
			r.Header.Get("X-Tautan-Signature") != "sha256="+revalidateSignature("whsec", ts, body) {
			t.Errorf("bad signature: %s %s", ts, r.Header.Get("X-Tautan-Signature"))
		}
		var c revalCall
		json.Unmarshal(body, &c)
		calls <- c
		w.WriteHeader(204)
	}))
	defer fe.Close()
	s.cfg.RevalidateSecret, s.cfg.RevalidateURL = "whsec", fe.URL
	s.reval.debounce = 30 * time.Millisecond
	h := s.Routes()

	expect := func(want ...string) {
		t.Helper()
		select {
		case c := <-calls:
			if len(c.Usernames) != len(want) {
				t.Fatalf("usernames = %v, want %v", c.Usernames, want)
			}
			for i := range want {
				if c.Usernames[i] != want[i] {
					t.Fatalf("call = %+v, want %v", c, want)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no revalidate call, want %v", want)
		}
	}
	none := func() {
		t.Helper()
		select {
		case c := <-calls:
			t.Fatalf("unexpected revalidate %+v", c)
		case <-time.After(150 * time.Millisecond):
		}
	}

	f := setupPage(t, h, "alice", false) // edits while unpublished: no calls
	none()
	tok := bearer(f.tok)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(200)
	expect("alice")

	// A burst of edits is one call; failed requests and reads send nothing.
	do(t, h, "PATCH", "/api/v1/blocks/"+f.link.ID, map[string]any{"is_visible": false}, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/blocks/"+f.link.ID, map[string]any{"is_visible": true}, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"theme": map[string]any{"background": "#ffffff"}}, tok).expect(200)
	expect("alice")
	do(t, h, "PATCH", "/api/v1/blocks/"+f.link.ID, map[string]any{"data": map[string]any{"url": "javascript:x"}}, tok).expect(400)
	do(t, h, "GET", "/api/v1/profile/editor", nil, tok).expect(200)
	none()

	// Rename includes the old username(s).
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "alice2"}, tok).expect(200)
	expect("alice", "alice2")
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "alice3"}, tok).expect(200)
	expect("alice", "alice2", "alice3")

	// Unpublishing revalidates (the page must disappear); later edits don't.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": false}, tok).expect(200)
	expect("alice3")
	do(t, h, "DELETE", "/api/v1/blocks/"+f.link.ID, nil, tok).expect(204)
	none()

	// Large batches are split into calls of at most 20 names.
	names := make([]string, 45)
	for i := range names {
		names[i] = "u" + strconv.Itoa(100+i)
	}
	s.revalidate(names...)
	got := 0
	for k := 0; k < 3; k++ {
		select {
		case c := <-calls:
			if len(c.Usernames) > 20 {
				t.Fatalf("batch of %d", len(c.Usernames))
			}
			got += len(c.Usernames)
		case <-time.After(3 * time.Second):
			t.Fatal("missing batch")
		}
	}
	if got != 45 {
		t.Errorf("sent %d names, want 45", got)
	}
}
