package server

import (
	"testing"
	"time"
)

func TestRefreshTokenReuseRevokesFamily(t *testing.T) {
	_, h, _, clock := mailServer(t)
	login := func() (string, resp) {
		r := do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "reuse@example.com", "password": "password123"}).expect(200)
		var a authResponse
		r.json(&a)
		return a.AccessToken, r
	}
	signup(t, h, "reuse")
	refresh := func(r resp) (resp, string) {
		var out map[string]string
		res := do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(refreshCookieFrom(t, r)))
		if res.Code == 200 {
			res.json(&out)
		}
		return res, out["access_token"]
	}

	// Device A and device B are separate sign-ins (separate families).
	_, a0 := login()
	_, b0 := login()

	// A rotates twice; the stolen first token is presented later by a thief.
	*clock = clock.Add(time.Minute)
	a1, aTok := refresh(a0)
	a1.expect(200)
	*clock = clock.Add(time.Minute)
	a2, _ := refresh(a1)
	a2.expect(200)

	// Inside the grace window a stale token is just refused (two tabs racing).
	if r, _ := refresh(a1); r.Code != 401 {
		t.Fatalf("racing refresh = %d", r.Code)
	}
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(refreshCookieFrom(t, a2))).expect(200)

	// Past the grace window: reuse of a rotated token kills the whole family…
	*clock = clock.Add(time.Minute)
	if r, _ := refresh(a0); r.Code != 401 {
		t.Fatalf("reused token = %d", r.Code)
	}
	var live int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM refresh_tokens t JOIN users u ON u.id = t.user_id
		WHERE u.email = 'reuse@example.com' AND t.revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 2 { // device B's and the signup session's tokens survive
		t.Fatalf("live refresh tokens = %d, want 2 (device B + signup)", live)
	}
	// …and every access token issued before now, including A's.
	do(t, h, "GET", "/api/v1/me", nil, bearer(aTok)).expect(401)
	// Device B (another family) refreshes into a fresh, working access token.
	*clock = clock.Add(2 * time.Second)
	b1, bTok := refresh(b0)
	b1.expect(200)
	do(t, h, "GET", "/api/v1/me", nil, bearer(bTok)).expect(200)

	// A token revoked by logout (not rotation) doesn't trigger anything.
	_, c0 := login()
	do(t, h, "POST", "/api/v1/auth/logout", nil, withCookie(refreshCookieFrom(t, c0))).expect(204)
	*clock = clock.Add(time.Minute)
	if r, _ := refresh(c0); r.Code != 401 {
		t.Fatalf("logged-out token = %d", r.Code)
	}
	b2, bTok2 := refresh(b1)
	b2.expect(200)
	do(t, h, "GET", "/api/v1/me", nil, bearer(bTok2)).expect(200)
}
