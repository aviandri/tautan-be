package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type resp struct {
	*httptest.ResponseRecorder
	t *testing.T
}

func (r resp) json(dst any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), dst); err != nil {
		r.t.Fatalf("decode %q: %v", r.Body.String(), err)
	}
}

func (r resp) errCode() string {
	var e struct{ Error struct{ Code string } }
	_ = json.Unmarshal(r.Body.Bytes(), &e)
	return e.Error.Code
}

func (r resp) expect(status int) resp {
	r.t.Helper()
	if r.Code != status {
		r.t.Fatalf("status = %d, want %d; body: %s", r.Code, status, r.Body)
	}
	return r
}

type reqOpt func(*http.Request)

func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withCookie(c *http.Cookie) reqOpt { return func(r *http.Request) { r.AddCookie(c) } }

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func do(t *testing.T, h http.Handler, method, path string, body any, opts ...reqOpt) resp {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return resp{rec, t}
}

func refreshCookieFrom(t *testing.T, r resp) *http.Cookie {
	t.Helper()
	for _, c := range r.Result().Cookies() {
		if c.Name == refreshCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie set", refreshCookie)
	return nil
}

// signup creates a user and returns the auth response.
func signup(t *testing.T, h http.Handler, username string) authResponse {
	t.Helper()
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{
		"email": username + "@example.com", "password": "password123", "username": username,
	}).expect(201).json(&a)
	// Most tests publish pages; verification has its own tests (verify_test.go).
	if _, err := testPool.Exec(context.Background(), `UPDATE users SET email_verified_at = now() WHERE id = $1`, a.User.ID); err != nil {
		t.Fatal(err)
	}
	return a
}
func urlq(s string) string { return url.QueryEscape(s) }
