package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbes(t *testing.T) {
	s, h := newTestServer(t)
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code
	}
	if c := get("/livez"); c != 200 {
		t.Fatalf("livez %d", c)
	}
	if c := get("/readyz"); c != 200 {
		t.Fatalf("readyz %d", c)
	}
	s.SetDraining()
	if c := get("/readyz"); c != 503 {
		t.Fatalf("readyz while draining = %d, want 503", c)
	}
	if c := get("/livez"); c != 200 {
		t.Fatalf("livez while draining = %d, want 200", c)
	}
}

func TestFlushPendingSendsImmediately(t *testing.T) {
	s, _ := newTestServer(t)
	got := make(chan []string, 1)
	fe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var c revalCall
		json.Unmarshal(body, &c)
		got <- c.Usernames
	}))
	defer fe.Close()
	s.cfg.RevalidateSecret, s.cfg.RevalidateURL = "whsec", fe.URL
	s.reval.debounce = time.Hour
	s.revalidate("alice")
	s.FlushPending()
	select {
	case u := <-got:
		if len(u) != 1 || u[0] != "alice" {
			t.Fatalf("usernames %v", u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FlushPending did not send the queued webhook")
	}
}
