package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A cross-site HTML form (enctype=text/plain) can build a valid JSON body but
// can't set application/json: sign-in endpoints refuse it, so no other site can
// log a visitor into the attacker's account (audit M2).
func TestAuthEndpointsRequireJSON(t *testing.T) {
	_, h := newTestServer(t)
	signup(t, h, "csrfvictim")
	post := func(path, ct, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	// What <form enctype="text/plain"> sends for name=`{"email":"…","password":"…","x":"` value=`"}`.
	forged := `{"email":"csrfvictim@example.com","password":"password123","x":"="}`
	for _, ct := range []string{"text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		rec := post("/api/v1/auth/login", ct, forged)
		if rec.Code != 415 || strings.Contains(rec.Header().Get("Set-Cookie"), refreshCookie+"=") && !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
			t.Errorf("login with %q = %d, Set-Cookie %q", ct, rec.Code, rec.Header().Get("Set-Cookie"))
		}
	}
	if rec := post("/api/v1/auth/login", "application/json; charset=utf-8", forged); rec.Code != 200 {
		t.Errorf("JSON login = %d", rec.Code)
	}
	for _, p := range []string{"/api/v1/auth/signup", "/api/v1/auth/google/callback", "/api/v1/auth/password/forgot", "/api/v1/auth/password/reset",
		"/api/v1/auth/verify-email", "/api/v1/auth/email-change/confirm", "/api/v1/auth/email-change/revert"} {
		if rec := post(p, "text/plain", `{}`); rec.Code != 415 {
			t.Errorf("%s with text/plain = %d", p, rec.Code)
		}
	}
	// Public card posts and the analytics beacon keep taking text/plain.
	if rec := post("/api/v1/public/nobody/events", "text/plain;charset=UTF-8", `{"kind":"view"}`); rec.Code == 415 {
		t.Error("beacon refused text/plain")
	}
}
