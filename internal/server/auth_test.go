package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSignupAndLogin(t *testing.T) {
	_, h := newTestServer(t)

	r := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{
		"email": "Alice@Example.com", "password": "password123", "username": "Alice",
	}).expect(201)
	var a authResponse
	r.json(&a)
	if a.AccessToken == "" || a.User.Email != "alice@example.com" || a.Profile.Username != "alice" {
		t.Fatalf("unexpected signup response: %+v", a)
	}
	if a.Profile.AvatarURL != nil {
		t.Errorf("avatar_url should be null, got %q", *a.Profile.AvatarURL)
	}
	var theme map[string]any
	_ = json.Unmarshal(a.Profile.Theme, &theme)
	if theme["preset"] == nil || theme["button"] == nil {
		t.Errorf("default theme incomplete: %s", a.Profile.Theme)
	}
	c := refreshCookieFrom(t, r)
	if !c.HttpOnly || c.Path != "/api/v1/auth" || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("bad cookie attrs: %+v", c)
	}

	// Login, case-insensitive email.
	var l authResponse
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "ALICE@example.com", "password": "password123"}).
		expect(200).json(&l)
	if l.Profile.ID != a.Profile.ID {
		t.Fatal("login returned different profile")
	}
	if code := do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "alice@example.com", "password": "nope-nope"}).
		expect(401).errCode(); code != "invalid_credentials" {
		t.Errorf("code = %s", code)
	}
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "password123"}).expect(401)

	// /me
	var me struct {
		User    User
		Profile Profile
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(l.AccessToken)).expect(200).json(&me)
	if me.User.ID != a.User.ID || me.Profile.Username != "alice" {
		t.Fatalf("me = %+v", me)
	}
	do(t, h, "GET", "/api/v1/me", nil).expect(401)
	do(t, h, "GET", "/api/v1/me", nil, bearer("garbage")).expect(401)

	// Signup tabs: one default tab exists.
	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM tabs WHERE profile_id = $1`, a.Profile.ID).Scan(&n)
	if n != 1 {
		t.Errorf("tabs = %d, want 1", n)
	}
}

func TestSignupValidation(t *testing.T) {
	_, h := newTestServer(t)
	signup(t, h, "bob")
	cases := []struct {
		name, email, pass, user, code string
		status                        int
	}{
		{"bad email", "nope", "password123", "carol", "invalid_email", 400},
		{"short password", "c@example.com", "short", "carol", "invalid_password", 400},
		{"bad username", "c@example.com", "password123", "ca", "invalid_username", 400},
		{"username chars", "c@example.com", "password123", "car-ol", "invalid_username", 400},
		{"reserved", "c@example.com", "password123", "dashboard", "invalid_username", 400},
		{"email taken", "BOB@example.com", "password123", "carol", "email_taken", 409},
		{"username taken", "c@example.com", "password123", "BOB", "username_taken", 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": tc.email, "password": tc.pass, "username": tc.user}).
				expect(tc.status).errCode()
			if code != tc.code {
				t.Errorf("code = %s, want %s", code, tc.code)
			}
		})
	}
	do(t, h, "POST", "/api/v1/auth/signup", "{not json").expect(400)
}

func TestRefreshRotationAndLogout(t *testing.T) {
	s, h := newTestServer(t)
	r := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "d@example.com", "password": "password123", "username": "dave"}).expect(201)
	c1 := refreshCookieFrom(t, r)

	do(t, h, "POST", "/api/v1/auth/refresh", nil).expect(401)

	r2 := do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(c1)).expect(200)
	var out struct {
		AccessToken string `json:"access_token"`
	}
	r2.json(&out)
	if _, _, err := s.parseAccessToken(out.AccessToken); err != nil {
		t.Fatalf("refreshed token invalid: %v", err)
	}
	c2 := refreshCookieFrom(t, r2)
	if c2.Value == c1.Value {
		t.Fatal("refresh token not rotated")
	}
	// Old token is revoked after rotation.
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(c1)).expect(401)

	lo := do(t, h, "POST", "/api/v1/auth/logout", nil, withCookie(c2)).expect(204)
	if refreshCookieFrom(t, lo).MaxAge >= 0 {
		t.Error("logout should clear cookie")
	}
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(c2)).expect(401)
}

func TestAccessTokenExpiry(t *testing.T) {
	s, h := newTestServer(t)
	a := signup(t, h, "erin")
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(401)
}

func TestUsernameAvailable(t *testing.T) {
	_, h := newTestServer(t)
	signup(t, h, "frank")
	for u, want := range map[string]bool{"frank": false, "FRANK": false, "frankie": true, "ab": false, "admin": false, "a b c": false} {
		var out struct{ Available bool }
		do(t, h, "GET", "/api/v1/auth/username-available?u="+urlq(u), nil).expect(200).json(&out)
		if out.Available != want {
			t.Errorf("%q available = %v, want %v", u, out.Available, want)
		}
	}
}
