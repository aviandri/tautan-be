package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/aviandri/tautan-be/internal/testutil"
)

// fakeOIDC is a minimal OpenID provider: discovery, JWKS, and a token endpoint
// that enforces client auth, redirect_uri and PKCE like Google does.
type fakeOIDC struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	mu     sync.Mutex
	codes  map[string]fakeGrant
	tokens int // token endpoint hits
	// Overrides for negative tests.
	mutate func(jwt.MapClaims)
	signer *rsa.PrivateKey
	// Extra query for /auth/google/start, e.g. "?u=jane".
	startQuery string
}

type fakeGrant struct {
	challenge, nonce, redirect string
	claims                     jwt.MapClaims
}

const (
	testClientID     = "client-123.apps.googleusercontent.com"
	testClientSecret = "shh"
	testRedirect     = "http://localhost:3000/auth/google/callback"
)

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{t: t, key: key, kid: "k1", codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/o/oauth2/v2/auth",
			"token_endpoint":         f.srv.URL + "/token",
			"jwks_uri":               f.srv.URL + "/certs",
		})
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": f.kid,
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", f.handleToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOIDC) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens++
	_ = r.ParseForm()
	fail := func(code string) { writeJSON(w, 400, map[string]string{"error": code}) }
	if r.Form.Get("client_id") != testClientID || r.Form.Get("client_secret") != testClientSecret {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	g, ok := f.codes[r.Form.Get("code")]
	delete(f.codes, r.Form.Get("code")) // single use
	if !ok || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != g.redirect ||
		pkceChallenge(r.Form.Get("code_verifier")) != g.challenge {
		fail("invalid_grant")
		return
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": f.srv.URL, "aud": testClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": g.nonce, "email_verified": true,
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	if f.mutate != nil {
		f.mutate(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signer := f.key
	if f.signer != nil {
		signer = f.signer
	}
	idToken, err := tok.SignedString(signer)
	if err != nil {
		f.t.Fatal(err)
	}
	writeJSON(w, 200, map[string]string{"access_token": "ya29.x", "token_type": "Bearer", "id_token": idToken})
}

// googleServer returns a test server with Google sign-in pointed at f.
func googleServer(t *testing.T, f *fakeOIDC) (*Server, http.Handler) {
	t.Helper()
	cfg := testConfig()
	cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL = testClientID, testClientSecret, testRedirect
	cfg.GoogleIssuer = f.srv.URL
	testutil.Truncate(t, testPool)
	s := New(cfg, testPool)
	s.Quiet = true
	return s, s.Routes()
}

// start runs /auth/google/start and plays the user consenting at the
// provider: it returns the state cookie and the code Google would send back.
func (f *fakeOIDC) start(t *testing.T, h http.Handler, claims jwt.MapClaims) (cookie *http.Cookie, code, state string) {
	t.Helper()
	r := do(t, h, "GET", "/api/v1/auth/google/start"+f.startQuery, nil).expect(302)
	loc, err := url.Parse(r.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), f.srv.URL+"/o/oauth2/v2/auth?") {
		t.Fatalf("redirect = %q", r.Header().Get("Location"))
	}
	q := loc.Query()
	for k, want := range map[string]string{
		"client_id": testClientID, "redirect_uri": testRedirect, "response_type": "code",
		"scope": "openid email profile", "code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("missing state/nonce/challenge: %v", q)
	}
	for _, c := range r.Result().Cookies() {
		if c.Name == oauthCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.Path != oauthCookiePath || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad state cookie: %+v", cookie)
	}
	if strings.Contains(cookie.Value, q.Get("code_challenge")) {
		t.Error("cookie should hold the verifier, not the challenge")
	}
	code = randomToken()
	f.mu.Lock()
	f.codes[code] = fakeGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri"), claims: claims}
	f.mu.Unlock()
	return cookie, code, q.Get("state")
}

func googleCallback(t *testing.T, h http.Handler, c *http.Cookie, code, state string) resp {
	t.Helper()
	opts := []reqOpt{}
	if c != nil {
		opts = append(opts, withCookie(c))
	}
	return do(t, h, "POST", "/api/v1/auth/google/callback", map[string]string{"code": code, "state": state}, opts...)
}

type googleResp struct {
	authResponse
	IsNewUser       bool `json:"is_new_user"`
	PasswordRemoved bool `json:"password_removed"`
}

func TestAuthProvidersDisabled(t *testing.T) {
	_, h := newTestServer(t)
	var got map[string]bool
	do(t, h, "GET", "/api/v1/auth/providers", nil).expect(200).json(&got)
	if got["google"] {
		t.Fatal("google should be off without credentials")
	}
	if c := do(t, h, "GET", "/api/v1/auth/google/start", nil).expect(404).errCode(); c != "provider_disabled" {
		t.Errorf("code = %s", c)
	}
	do(t, h, "POST", "/api/v1/auth/google/callback", map[string]string{"code": "x", "state": "y"}).expect(404)
}

func TestGoogleNewUserGoesThroughOnboarding(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)

	var p map[string]bool
	do(t, h, "GET", "/api/v1/auth/providers", nil).expect(200).json(&p)
	if !p["google"] {
		t.Fatal("google should be on")
	}

	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-1", "email": "Jane.Doe@Gmail.com", "name": "Jane"})
	r := googleCallback(t, h, c, code, state).expect(201)
	var a googleResp
	r.json(&a)
	if !a.IsNewUser || a.AccessToken == "" || a.User.Email != "jane.doe@gmail.com" {
		t.Fatalf("response = %+v", a)
	}
	if !strings.HasPrefix(a.Profile.Username, tempUsernamePrefix) {
		t.Errorf("username = %q, want a temporary one", a.Profile.Username)
	}
	var ob Onboarding
	_ = json.Unmarshal(a.Profile.Onboarding, &ob)
	if ob.Completed || ob.UsernameClaimed || ob.SuggestedUsername != "jane.doe" {
		t.Errorf("onboarding = %+v", ob)
	}
	// Same session cookie as /auth/login, and the state cookie is cleared.
	rc := refreshCookieFrom(t, r)
	if !rc.HttpOnly || rc.Path != "/api/v1/auth" {
		t.Errorf("refresh cookie = %+v", rc)
	}
	cleared := false
	for _, ck := range r.Result().Cookies() {
		cleared = cleared || (ck.Name == oauthCookie && ck.MaxAge < 0)
	}
	if !cleared {
		t.Error("state cookie not cleared")
	}
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(rc)).expect(200)
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(200)

	// A password login to a Google-only account fails cleanly.
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "jane.doe@gmail.com", "password": ""}).expect(401)

	// Signing in again finds the same user through the identity link.
	c, code, state = f.start(t, h, jwt.MapClaims{"sub": "g-1", "email": "jane.doe@gmail.com"})
	var again googleResp
	googleCallback(t, h, c, code, state).expect(200).json(&again)
	if again.IsNewUser || again.User.ID != a.User.ID {
		t.Fatalf("second sign-in = %+v", again)
	}

	// The email is now taken for password signups.
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "jane.doe@gmail.com", "password": "password123"}).expect(409)
}

func TestGoogleLinksExistingAccountByVerifiedEmail(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)
	existing := signup(t, h, "alice")

	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-alice", "email": existing.User.Email})
	var a googleResp
	googleCallback(t, h, c, code, state).expect(200).json(&a)
	if a.IsNewUser || a.User.ID != existing.User.ID || a.Profile.Username != "alice" {
		t.Fatalf("link = %+v", a)
	}
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM user_identities WHERE user_id = $1`, existing.User.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("identities = %d, %v", n, err)
	}
	// The account's email was already confirmed (signup helper), so the link
	// keeps its password; unverified accounts lose it (TestGoogleLinkUndoesPreHijack).
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": existing.User.Email, "password": "password123"}).expect(200)

	// Even if the Google email changes later, the subject keeps the link.
	c, code, state = f.start(t, h, jwt.MapClaims{"sub": "g-alice", "email": "alice.new@example.com"})
	googleCallback(t, h, c, code, state).expect(200).json(&a)
	if a.User.ID != existing.User.ID {
		t.Fatal("subject link not used")
	}
}

func TestGoogleUnverifiedEmailIsRejected(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)
	existing := signup(t, h, "bob")

	for _, claims := range []jwt.MapClaims{
		{"sub": "g-bob", "email": existing.User.Email, "email_verified": false},
		{"sub": "g-bob", "email": existing.User.Email, "email_verified": "false"},
		{"sub": "g-x"}, // no email scope granted
	} {
		c, code, state := f.start(t, h, claims)
		if got := googleCallback(t, h, c, code, state).expect(403).errCode(); got != "email_not_verified" {
			t.Errorf("%v: code = %s", claims, got)
		}
	}
	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM user_identities`).Scan(&n)
	if n != 0 {
		t.Fatal("unverified email must not be linked")
	}
	// String "true" (some providers) is accepted.
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-bob", "email": existing.User.Email, "email_verified": "true"})
	googleCallback(t, h, c, code, state).expect(200)
}

func TestGoogleStateChecks(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	claims := jwt.MapClaims{"sub": "g-1", "email": "a@example.com"}

	// No cookie (login CSRF: someone else's code pushed into this browser).
	_, code, state := f.start(t, h, claims)
	if got := googleCallback(t, h, nil, code, state).expect(400).errCode(); got != "invalid_state" {
		t.Errorf("no cookie: %s", got)
	}
	// State from another flow.
	c1, _, _ := f.start(t, h, claims)
	_, code2, state2 := f.start(t, h, claims)
	googleCallback(t, h, c1, code2, state2).expect(400)
	// Tampered cookie.
	c, code, state := f.start(t, h, claims)
	bad := *c
	bad.Value = c.Value[:len(c.Value)-2] + "xx"
	googleCallback(t, h, &bad, code, state).expect(400)
	// Expired cookie.
	c, code, state = f.start(t, h, claims)
	s.now = func() time.Time { return time.Now().Add(oauthStateTTL + time.Minute) }
	googleCallback(t, h, c, code, state).expect(400)
	s.now = time.Now
	if f.tokens != 0 {
		t.Errorf("token endpoint hit %d times on bad state", f.tokens)
	}
	// Valid state but the code was already used / is wrong.
	c, code, state = f.start(t, h, claims)
	googleCallback(t, h, c, code, state).expect(201)
	if got := googleCallback(t, h, c, code, state).expect(400).errCode(); got != "invalid_code" {
		t.Errorf("replayed code: %s", got)
	}
	c, _, state = f.start(t, h, claims)
	if got := googleCallback(t, h, c, "", state).expect(400).errCode(); got != "invalid_code" {
		t.Errorf("empty code: %s", got)
	}
}

func TestGoogleIDTokenVerification(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)

	cases := map[string]func(){
		"wrong audience": func() { f.mutate = func(c jwt.MapClaims) { c["aud"] = "someone-else" } },
		"wrong issuer":   func() { f.mutate = func(c jwt.MapClaims) { c["iss"] = "https://evil.example" } },
		"wrong nonce":    func() { f.mutate = func(c jwt.MapClaims) { c["nonce"] = "replayed" } },
		"expired":        func() { f.mutate = func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() } },
		"bad signature":  func() { f.signer = other },
	}
	for name, setup := range cases {
		f.mutate, f.signer = nil, nil
		setup()
		c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-1", "email": "a@example.com"})
		if got := googleCallback(t, h, c, code, state).expect(502).errCode(); got != "provider_error" {
			t.Errorf("%s: code = %s", name, got)
		}
	}
	var n int
	_ = testPool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n)
	if n != 0 {
		t.Fatalf("users created from bad tokens: %d", n)
	}

	// A good token still works after the failures.
	f.mutate, f.signer = nil, nil
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-1", "email": "a@example.com"})
	googleCallback(t, h, c, code, state).expect(201)
}

func TestGoogleStartRedirectsToFEWhenProviderDown(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)
	f.srv.Close()
	r := do(t, h, "GET", "/api/v1/auth/google/start"+f.startQuery, nil).expect(302)
	if loc := r.Header().Get("Location"); loc != testRedirect+"?error=provider_error" {
		t.Fatalf("location = %q", loc)
	}
}

func TestGoogleLinkUndoesPreHijack(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	// The attacker registers the victim's address with their own password.
	attacker := do(t, h, "POST", "/api/v1/auth/signup", map[string]string{
		"email": "victim@gmail.com", "password": "attacker-pw", "username": "victim",
	}).expect(201)
	var a authResponse
	attacker.json(&a)
	attackerRefresh := refreshCookieFrom(t, attacker)

	// Later, the real owner signs in with Google.
	clock = clock.Add(5 * time.Second)
	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-victim", "email": "victim@gmail.com"})
	var v googleResp
	r := googleCallback(t, h, c, code, state).expect(200)
	r.json(&v)
	if v.User.ID != a.User.ID || !v.PasswordRemoved {
		t.Fatalf("link = %+v", v)
	}

	// The attacker is locked out: refresh, access token and password all fail.
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(attackerRefresh)).expect(401)
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(401)
	if code := do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "victim@gmail.com", "password": "attacker-pw"}).
		expect(401).errCode(); code != "password_not_set" {
		t.Errorf("login code = %s", code)
	}

	// The owner's new session works, and refreshes.
	var me struct{ User meUser }
	do(t, h, "GET", "/api/v1/me", nil, bearer(v.AccessToken)).expect(200).json(&me)
	if me.User.HasPassword || len(me.User.Identities) != 1 || me.User.Identities[0] != "google" {
		t.Errorf("me = %+v", me.User)
	}
	do(t, h, "POST", "/api/v1/auth/refresh", nil, withCookie(refreshCookieFrom(t, r))).expect(200)

	// A later Google sign-in (already linked) revokes nothing.
	clock = clock.Add(5 * time.Second)
	c, code, state = f.start(t, h, jwt.MapClaims{"sub": "g-victim", "email": "victim@gmail.com"})
	var again googleResp
	googleCallback(t, h, c, code, state).expect(200).json(&again)
	if again.PasswordRemoved {
		t.Error("second sign-in should not remove anything")
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(v.AccessToken)).expect(200)

	// The owner adds a password of their own.
	if code := do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "short"}, bearer(v.AccessToken)).
		expect(400).errCode(); code != "invalid_password" {
		t.Errorf("code = %s", code)
	}
	do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "owner-password"}, bearer(v.AccessToken)).expect(204)
	if code := do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "another-one"}, bearer(v.AccessToken)).
		expect(409).errCode(); code != "password_already_set" {
		t.Errorf("code = %s", code)
	}
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "victim@gmail.com", "password": "owner-password"}).expect(200)
	do(t, h, "GET", "/api/v1/me", nil, bearer(v.AccessToken)).expect(200).json(&me)
	if !me.User.HasPassword {
		t.Error("has_password should be true")
	}
}

func TestGoogleSecondIdentityDoesNotRevoke(t *testing.T) {
	f := newFakeOIDC(t)
	s, h := googleServer(t, f)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-1", "email": "x@gmail.com"})
	var first googleResp
	googleCallback(t, h, c, code, state).expect(201).json(&first)
	do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "my-password"}, bearer(first.AccessToken)).expect(204)

	// Another Google account with the same email: the email was already
	// verified through Google, so the password and sessions stay.
	clock = clock.Add(5 * time.Second)
	c, code, state = f.start(t, h, jwt.MapClaims{"sub": "g-2", "email": "x@gmail.com"})
	var second googleResp
	googleCallback(t, h, c, code, state).expect(200).json(&second)
	if second.PasswordRemoved || second.User.ID != first.User.ID {
		t.Fatalf("second = %+v", second)
	}
	do(t, h, "GET", "/api/v1/me", nil, bearer(first.AccessToken)).expect(200)
	do(t, h, "POST", "/api/v1/auth/login", map[string]string{"email": "x@gmail.com", "password": "my-password"}).expect(200)
}

func TestSetPasswordWhenAlreadySet(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "carol")
	do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "whatever-1"}, bearer(a.AccessToken)).expect(409)
	do(t, h, "PATCH", "/api/v1/me/password", map[string]string{"new_password": "whatever-1"}).expect(401)
	var me struct{ User meUser }
	do(t, h, "GET", "/api/v1/me", nil, bearer(a.AccessToken)).expect(200).json(&me)
	if !me.User.HasPassword || len(me.User.Identities) != 0 {
		t.Errorf("me = %+v", me.User)
	}
}

func TestGoogleCarriesLandingPageUsername(t *testing.T) {
	f := newFakeOIDC(t)
	_, h := googleServer(t, f)
	signup(t, h, "taken.name")

	for _, tc := range []struct{ q, email, want string }{
		{"?u=Jane.Picks", "j1@gmail.com", "jane.picks"},
		{"?u=taken.name", "jay2@gmail.com", "jay2"},   // taken → falls back to the email
		{"?u=tmp.sneaky", "jay3@gmail.com", "jay3"},   // never claimable
		{"?u=%3Cscript%3E", "jay4@gmail.com", "jay4"}, // invalid
	} {
		f.startQuery = tc.q
		c, code, state := f.start(t, h, jwt.MapClaims{"sub": "g-" + tc.email, "email": tc.email})
		var a googleResp
		googleCallback(t, h, c, code, state).expect(201).json(&a)
		var ob Onboarding
		_ = json.Unmarshal(a.Profile.Onboarding, &ob)
		if ob.SuggestedUsername != tc.want || ob.UsernameClaimed {
			t.Errorf("%s: onboarding = %+v, want suggestion %q", tc.q, ob, tc.want)
		}
	}
}
