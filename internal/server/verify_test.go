package server

import (
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
)

const verifySubject = "Confirm your email for Tautan"

var verifyLinkRE = regexp.MustCompile(`/verify-email\?token=([A-Za-z0-9_%-]+)`)

func verifyToken(t *testing.T, msgs []mail.Message) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("no verification email")
	}
	sm := verifyLinkRE.FindStringSubmatch(msgs[len(msgs)-1].Text)
	if sm == nil {
		t.Fatalf("no link in %q", msgs[len(msgs)-1].Text)
	}
	tok, _ := url.QueryUnescape(sm[1])
	return tok
}

type meResp struct {
	User    meUser
	Profile Profile
}

func TestSignupVerificationGatesPublishing(t *testing.T) {
	s, h, m, clock := mailServer(t)
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "Dewi@Example.com", "password": "password123", "username": "dewi"}).
		expect(201).json(&a)
	tok := bearer(a.AccessToken)
	deliver(t, s)
	msgs := m.takeSubject(verifySubject)
	if len(msgs) != 1 || msgs[0].To != "dewi@example.com" {
		t.Fatalf("verification emails = %+v", msgs)
	}

	var me meResp
	do(t, h, "GET", "/api/v1/me", nil, tok).expect(200).json(&me)
	if me.User.EmailVerified {
		t.Fatal("new signup should be unverified")
	}
	// Editing works; publishing doesn't.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"bio": "hello"}, tok).expect(200)
	if c := do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(403).errCode(); c != "email_unverified" {
		t.Errorf("code = %s", c)
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": false}, tok).expect(200)

	// Resend: 202, capped per hour, then the link from any message works.
	do(t, h, "POST", "/api/v1/me/verification-email", nil, tok).expect(202)
	do(t, h, "POST", "/api/v1/me/verification-email", nil, tok).expect(202)
	if c := do(t, h, "POST", "/api/v1/me/verification-email", nil, tok).expect(429).errCode(); c != "rate_limited" {
		t.Errorf("code = %s", c) // signup's email counts towards the 3/hour
	}
	do(t, h, "POST", "/api/v1/me/verification-email", nil).expect(401)
	deliver(t, s)
	link := verifyToken(t, m.takeSubject(verifySubject))

	*clock = clock.Add(time.Minute)
	if c := do(t, h, "POST", "/api/v1/auth/verify-email", map[string]string{"token": "nope"}).expect(400).errCode(); c != "invalid_token" {
		t.Errorf("code = %s", c)
	}
	var v map[string]bool
	do(t, h, "POST", "/api/v1/auth/verify-email", map[string]string{"token": link}).expect(200).json(&v)
	if !v["verified"] {
		t.Fatalf("verify = %v", v)
	}
	do(t, h, "POST", "/api/v1/auth/verify-email", map[string]string{"token": link}).expect(400) // single use
	do(t, h, "GET", "/api/v1/me", nil, tok).expect(200).json(&me)
	if !me.User.EmailVerified {
		t.Fatal("still unverified")
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(200)
	var again map[string]bool
	do(t, h, "POST", "/api/v1/me/verification-email", nil, tok).expect(200).json(&again)
	if !again["already_verified"] {
		t.Errorf("resend after verify = %v", again)
	}
}

func TestVerificationLinkExpires(t *testing.T) {
	s, h, m, clock := mailServer(t)
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "late@example.com", "password": "password123", "username": "late"}).expect(201)
	deliver(t, s)
	link := verifyToken(t, m.takeSubject(verifySubject))
	*clock = clock.Add(verifyTTL + time.Second)
	do(t, h, "POST", "/api/v1/auth/verify-email", map[string]string{"token": link}).expect(400)
}

func TestAlreadyPublishedPagesAreGrandfathered(t *testing.T) {
	_, h, _, _ := mailServer(t)
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "old@example.com", "password": "password123", "username": "oldtimer"}).
		expect(201).json(&a)
	tok := bearer(a.AccessToken)
	// A page published before verification existed.
	if _, err := testPool.Exec(t.Context(), `UPDATE profiles SET is_published = true WHERE user_id = $1`, a.User.ID); err != nil {
		t.Fatal(err)
	}
	// Re-sending is_published: true (as editors do) and other edits keep working.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true, "bio": "still here"}, tok).expect(200)
	do(t, h, "GET", "/api/v1/public/oldtimer", nil).expect(200)
	// Unpublishing and re-publishing now needs a confirmed email.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": false}, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(403)
}

func TestGoogleAndResetCountAsVerified(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	m := &captureMailer{}
	s.mailer = m

	// New Google account: verified, can publish once it claims a username.
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-v", "email": "gv@gmail.com"})
	var g googleResp
	googleCallback(t, h, c, code, state).expect(201).json(&g)
	var me meResp
	do(t, h, "GET", "/api/v1/me", nil, bearer(g.AccessToken)).expect(200).json(&me)
	if !me.User.EmailVerified {
		t.Error("Google account should be verified")
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "gvpage", "is_published": true}, bearer(g.AccessToken)).expect(200)
	if _, err := s.DeliverPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(m.takeSubject(verifySubject)); n != 0 {
		t.Errorf("Google signup sent %d verification emails", n)
	}

	// A password reset proves the address too.
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "rs@example.com", "password": "password123", "username": "rspage"}).expect(201)
	do(t, h, "POST", "/api/v1/auth/password/forgot", map[string]string{"email": "rs@example.com"}).expect(202)
	_, _ = s.DeliverPending(t.Context())
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/password/reset", map[string]string{"token": resetToken(t, m.takeSubject(resetSubject)[0]), "new_password": "new-password"}).
		expect(200).json(&a)
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, bearer(a.AccessToken)).expect(200)
}

func TestGoogleLinkKeepsPasswordOfVerifiedAccount(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	var a authResponse
	r := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "kept@gmail.com", "password": "my-password", "username": "kept"}).expect(201)
	r.json(&a)
	if err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error { return markVerified(t.Context(), tx, a.User.ID, clock) }); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Second)
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-kept", "email": "kept@gmail.com"})
	var g googleResp
	googleCallback(t, h, c, code, state).expect(200).json(&g)
	if g.PasswordRemoved {
		t.Fatal("verified account lost its password")
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(200)
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(refreshCookieFrom(t, r))).expect(200)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "kept@gmail.com", "password": "my-password"}).expect(200)
}
