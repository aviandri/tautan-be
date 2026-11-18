package server

import (
	"testing"
	"time"
)

func TestLoginRateLimits(t *testing.T) {
	s, h := newTestServer(t)
	signup(t, h, "victim")
	login := func(email, pw string) resp {
		return do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": email, "password": pw})
	}
	// Failed attempts count per account (any IP, any case).
	for i := 0; i < 10; i++ {
		login("victim@example.com", "wrong-password").expect(401)
	}
	if c := login("VICTIM@example.com", "wrong-password").expect(429).errCode(); c != "rate_limited" {
		t.Errorf("code = %s", c)
	}
	login("victim@example.com", "password123").expect(429) // locked for the window, even with the right password
	// Other accounts are unaffected; successes don't count against the account.
	s.loginFailLimiter = newRateLimiter(10, 15*time.Minute)
	for i := 0; i < 12; i++ {
		login("victim@example.com", "password123").expect(200)
	}
	// Every attempt counts per IP.
	s.loginIPLimiter = newRateLimiter(3, time.Minute)
	login("victim@example.com", "password123").expect(200)
	login("nobody@example.com", "x").expect(401)
	login("victim@example.com", "password123").expect(200)
	login("victim@example.com", "password123").expect(429)
	// Sign-ups per IP.
	s.signupLimiter = newRateLimiter(1, time.Hour)
	signup(t, h, "first")
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "second@example.com", "password": "password123", "username": "second"}).expect(429)
}
