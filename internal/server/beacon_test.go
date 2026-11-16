package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// navigator.sendBeacon posts a "simple" CORS request (text/plain), so browsers
// skip the preflight. The events endpoint must accept it.
func TestEventsAcceptBeacon(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "beacon", true)
	for _, ct := range []string{"text/plain;charset=UTF-8", "text/plain", ""} {
		req := httptest.NewRequest("POST", "/api/v1/public/beacon/events",
			strings.NewReader(`{"kind":"click","block_id":"`+f.link.ID+`","meta":{"action":"buy"}}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("User-Agent", "Mozilla/5.0 test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
			rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Errorf("content-type %q: %d %v", ct, rec.Code, rec.Header())
		}
	}
	var n int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE action = 'buy'`).Scan(&n)
	if n != 3 {
		t.Errorf("recorded %d beacon events, want 3", n)
	}
}
