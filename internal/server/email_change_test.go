package server

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/aviandri/tautan-be/internal/mail"
)

const (
	changeSubject  = "Confirm your new email for Tautan"
	changedSubject = "Your Tautan email was changed"
)

func linkToken(t *testing.T, re *regexp.Regexp, msgs []mail.Message) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("no email")
	}
	sm := re.FindStringSubmatch(msgs[len(msgs)-1].Text)
	if sm == nil {
		t.Fatalf("no link in %q", msgs[len(msgs)-1].Text)
	}
	tok, _ := url.QueryUnescape(sm[1])
	return tok
}

var (
	confirmChangeRE = regexp.MustCompile(`/confirm-email-change\?token=([A-Za-z0-9_%-]+)`)
	revertChangeRE  = regexp.MustCompile(`/revert-email-change\?token=([A-Za-z0-9_%-]+)`)
)


func TestEmailChangeConfirmNotifyRevert(t *testing.T) {
	s, h, m, clock := mailServer(t)
	var a authResponse
	res := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "nadia@example.com", "password": "password123", "username": "nadia"}).expect(201)
	res.json(&a)
	oldRefresh := refreshCookieFrom(t, res)
	tok := bearer(a.AccessToken)
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "taken@example.com", "password": "password123", "username": "takenone"}).expect(201)

	// Validation.
	body := func(e, p string) map[string]string { return map[string]string{"new_email": e, "password": p} }
	do(t, h, "POST", "/api/v1/me/email", body("nadia.new@example.com", "password123")).expect(401)
	for _, c := range []struct {
		in   map[string]string
		code int
		err  string
	}{
		{body("not an email", "password123"), 400, "invalid_email"},
		{body("nadia.new@example.com", "wrong-password"), 403, "invalid_password"},
		{body("Nadia@Example.com", "password123"), 400, "same_email"},
		{body("taken@example.com", "password123"), 409, "email_taken"},
	} {
		if got := do(t, h, "POST", "/api/v1/me/email", c.in, tok).expect(c.code).errCode(); got != c.err {
			t.Errorf("%v: code = %s", c.in, got)
		}
	}

	// A request changes nothing yet; /me shows it pending.
	var pend map[string]string
	do(t, h, "POST", "/api/v1/me/email", body("  Nadia.New@Example.com ", "password123"), tok).expect(202).json(&pend)
	if pend["pending_email"] != "nadia.new@example.com" {
		t.Fatalf("pending = %v", pend)
	}
	var me meResp
	do(t, h, "GET", "/api/v1/me", nil, tok).expect(200).json(&me)
	if me.User.Email != "nadia@example.com" || me.User.PendingEmail == nil || *me.User.PendingEmail != "nadia.new@example.com" {
		t.Fatalf("me = %+v", me.User)
	}
	deliver(t, s)
	msgs := m.takeSubject(changeSubject)
	if len(msgs) != 1 || msgs[0].To != "nadia.new@example.com" {
		t.Fatalf("confirm emails = %+v", msgs)
	}
	first := linkToken(t, confirmChangeRE, msgs)

	// A second request replaces the first link.
	do(t, h, "POST", "/api/v1/me/email", body("nadia2@example.com", "password123"), tok).expect(202)
	deliver(t, s)
	second := linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": first}).expect(400)

	*clock = clock.Add(5 * time.Second)
	var c authResponse
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": second}).expect(200).json(&c)
	if c.User.Email != "nadia2@example.com" || c.AccessToken == "" {
		t.Fatalf("confirm = %+v", c)
	}
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": second}).expect(400) // single use
	// Every old session ends; the new one works; sign-in uses the new email.
	do(t, h, "GET", "/api/v1/me", nil, tok).expect(401)
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(oldRefresh)).expect(401)
	do(t, h, "GET", "/api/v1/me", nil, bearer(c.AccessToken)).expect(200).json(&me)
	if me.User.Email != "nadia2@example.com" || !me.User.EmailVerified || me.User.PendingEmail != nil {
		t.Fatalf("me after confirm = %+v", me.User)
	}
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "nadia@example.com", "password": "password123"}).expect(401)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "nadia2@example.com", "password": "password123"}).expect(200)

	// The old address is told, with the new one masked, and can undo.
	deliver(t, s)
	notice := m.takeSubject(changedSubject)
	if len(notice) != 1 || notice[0].To != "nadia@example.com" || !strings.Contains(notice[0].Text, "n***@example.com") ||
		strings.Contains(notice[0].Text, "nadia2@") {
		t.Fatalf("notice = %+v", notice)
	}
	undo := linkToken(t, revertChangeRE, notice)

	*clock = clock.Add(5 * time.Second)
	var r authResponse
	do(t, h, "POST", "/api/v1/auth/email-change/revert", map[string]string{"token": undo}).expect(200).json(&r)
	if r.User.Email != "nadia@example.com" {
		t.Fatalf("revert = %+v", r)
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(c.AccessToken)).expect(401)
	do(t, h, "POST", "/api/v1/auth/email-change/revert", map[string]string{"token": undo}).expect(400)
	deliver(t, s)
	if n := len(m.takeSubject(changedSubject)); n != 0 {
		t.Errorf("revert sent %d change notices", n)
	}
}

func TestEmailChangeStaleLinks(t *testing.T) {
	s, h, m, clock := mailServer(t)
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "omar@example.com", "password": "password123", "username": "omar"}).expect(201).json(&a)
	tok := bearer(a.AccessToken)
	body := func(e string) map[string]string { return map[string]string{"new_email": e, "password": "password123"} }
	relogin := func() { // access tokens outlive none of the clock jumps below
		var l authResponse
		do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "omar@example.com", "password": "password123"}).expect(200).json(&l)
		tok = bearer(l.AccessToken)
	}

	// Expired.
	do(t, h, "POST", "/api/v1/me/email", body("omar1@example.com"), tok).expect(202)
	deliver(t, s)
	expired := linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))
	*clock = clock.Add(emailChangeTTL + time.Second)
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": expired}).expect(400)
	relogin()

	// Cancelled.
	do(t, h, "POST", "/api/v1/me/email", body("omar2@example.com"), tok).expect(202)
	deliver(t, s)
	cancelled := linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))
	do(t, h, "DELETE", "/api/v1/me/email/pending", nil, tok).expect(204)
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": cancelled}).expect(400)

	// The address got taken while pending.
	do(t, h, "POST", "/api/v1/me/email", body("omar3@example.com"), tok).expect(202)
	deliver(t, s)
	raced := linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "omar3@example.com", "password": "password123", "username": "omarthree"}).expect(201)
	if c := do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": raced}).expect(409).errCode(); c != "email_taken" {
		t.Errorf("code = %s", c)
	}

	// Hourly cap: omar2, omar3 and this one fill the hour; a 4th is refused.
	*clock = clock.Add(time.Minute)
	do(t, h, "POST", "/api/v1/me/email", body("omar4@example.com"), tok).expect(202)
	if c := do(t, h, "POST", "/api/v1/me/email", body("omar5@example.com"), tok).expect(429).errCode(); c != "rate_limited" {
		t.Errorf("code = %s", c)
	}

	// An undo link is dead once the account moved on to yet another email.
	*clock = clock.Add(2 * time.Hour)
	relogin()
	deliver(t, s)
	m.takeSubject(changeSubject)
	do(t, h, "POST", "/api/v1/me/email", body("omar8@example.com"), tok).expect(202)
	deliver(t, s)
	var c authResponse
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))}).expect(200).json(&c)
	deliver(t, s)
	undo := linkToken(t, revertChangeRE, m.takeSubject(changedSubject))
	*clock = clock.Add(5 * time.Second)
	do(t, h, "POST", "/api/v1/me/email", body("omar9@example.com"), bearer(c.AccessToken)).expect(202)
	deliver(t, s)
	do(t, h, "POST", "/api/v1/auth/email-change/confirm", map[string]string{"token": linkToken(t, confirmChangeRE, m.takeSubject(changeSubject))}).expect(200)
	do(t, h, "POST", "/api/v1/auth/email-change/revert", map[string]string{"token": undo}).expect(400)
}

func TestEmailChangeGoogleOnlyNeedsNoPassword(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	s.mailer = &captureMailer{}
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-ec", "email": "gec@gmail.com"})
	var g googleResp
	googleCallback(t, h, c, code, state).expect(201).json(&g)
	do(t, h, "POST", "/api/v1/me/email", map[string]string{"new_email": "gec@work.example"}, bearer(g.AccessToken)).expect(202)
}
