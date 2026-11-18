package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func onboardingOf(t *testing.T, p Profile) Onboarding {
	t.Helper()
	var o Onboarding
	if err := json.Unmarshal(p.Onboarding, &o); err != nil {
		t.Fatalf("onboarding %s: %v", p.Onboarding, err)
	}
	return o
}

func TestSignupWithoutUsernameAndClaim(t *testing.T) {
	_, h := newTestServer(t)
	signup(t, h, "jane.doe") // takes the obvious suggestion
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "Jane.Doe+promo@example.com", "password": "password123"}).expect(201).json(&a)
	o := onboardingOf(t, a.Profile)
	if !strings.HasPrefix(a.Profile.Username, "tmp.") || o.UsernameClaimed || o.Completed || a.Profile.DisplayName != "" {
		t.Fatalf("temp profile = %+v %+v", a.Profile, o)
	}
	if o.SuggestedUsername != "jane.doe.promo" {
		t.Errorf("suggested = %q", o.SuggestedUsername)
	}
	tok := bearer(a.AccessToken)
	// tmp.* names can't be claimed or checked as available.
	var av struct{ Available bool }
	do(t, h, "GET", "/api/v1/auth/username-available?u="+a.Profile.Username, nil).expect(200).json(&av)
	if av.Available {
		t.Error("temp username reported available")
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "tmp.abcdefghij"}, tok).expect(400)
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "x@example.com", "password": "password123", "username": "tmp.zzzzzzzzzz"}).expect(400)

	// Can't publish before claiming.
	if c := do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(400).errCode(); c != "username_not_claimed" {
		t.Errorf("code = %s", c)
	}
	// Onboarding progress.
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"onboarding": map[string]any{"step": "profile", "intent": "audience", "source": "scratch", "username_claimed": true}}, tok).
		expect(200).json(&p)
	o = onboardingOf(t, p)
	if o.Step != "profile" || o.Intent != "audience" || o.Source != "scratch" || o.UsernameClaimed {
		t.Errorf("progress = %+v (username_claimed must stay server-managed)", o)
	}
	for _, bad := range []string{`{"onboarding":{"intent":"fame"}}`, `{"onboarding":{"step":"Bad Step"}}`, `{"onboarding":"done"}`} {
		do(t, h, "PATCH", "/api/v1/profile", bad, tok).expect(400)
	}
	// Publishing needs a confirmed email (verify_test.go covers the gate).
	if _, err := testPool.Exec(t.Context(), `UPDATE users SET email_verified_at = now()`); err != nil {
		t.Fatal(err)
	}
	// Claim + publish + complete in one request.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "jane", "is_published": true, "onboarding": map[string]any{"completed": true}}, tok).
		expect(200).json(&p)
	o = onboardingOf(t, p)
	if p.Username != "jane" || !p.IsPublished || !o.UsernameClaimed || !o.Completed || o.CompletedAt == nil || o.SuggestedUsername != "" || o.Intent != "audience" {
		t.Errorf("claimed = %s %v %+v", p.Username, p.IsPublished, o)
	}
}

func TestExistingSignupOnboarding(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	o := onboardingOf(t, a.Profile)
	if !o.UsernameClaimed || o.Completed {
		t.Errorf("onboarding = %+v", o)
	}
}

const linktreeHTML = `<!doctype html><html><head>
<title>Ana | Linktree</title>
<meta property="og:title" content="Ana | Linktree"><meta property="og:description" content="Baker in Bandung">
<meta property="og:image" content="/avatar.png">
</head><body>
<a href="/">Linktree home</a>
<a href="https://linktr.ee/s/about">About Linktree</a>
<a href="https://shop.example/cakes">  Order cakes  </a>
<a href="https://instagram.com/ana.bakes">Instagram</a>
<a href="https://linktr.ee/privacy">Privacy</a>
<a href="javascript:alert(1)">bad</a>
<a href="mailto:ana@example.com">Email me</a>
<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{
 "account":{"pageTitle":"Ana Bakes","description":"Fresh bread daily","profilePictureUrl":"/avatar.png"},
 "links":[{"title":"Order cakes","url":"https://shop.example/cakes"},{"title":"Menu PDF","url":"https://cdn.example/menu.pdf"},{"title":"Evil","url":"javascript:alert(1)"}],
 "socialLinks":[{"type":"TIKTOK","url":"https://tiktok.com/@ana"},{"type":"INSTAGRAM","url":"https://instagram.com/ana.bakes"}]}}}</script>
</body></html>`

func TestImport(t *testing.T) {
	s, h := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	s.fetcher = newSafeFetcher(true)
	h = s.Routes()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/avatar.png":
			w.Write(pngBytes())
		case "/generic":
			fmt.Fprint(w, `<html><head><meta property="og:title" content="Bo"></head><body>
				<a href="https://blog.example/post">My blog</a><a href="https://x.com/bo">X</a><a href="https://youtube.com/watch?v=1">Latest video</a></body></html>`)
		default:
			fmt.Fprint(w, linktreeHTML)
		}
	}))
	defer site.Close()
	var a authResponse
	do(t, h, "POST", "/api/v1/auth/signup", map[string]string{"email": "ana@example.com", "password": "password123"}).expect(201).json(&a)
	tok := bearer(a.AccessToken)

	var pv struct{ Preview importPreview }
	do(t, h, "POST", "/api/v1/import", map[string]any{"url": site.URL + "/ana"}, tok).expect(200).json(&pv)
	p := pv.Preview
	if p.Profile.DisplayName != "Ana Bakes" || p.Profile.Bio != "Fresh bread daily" || p.Profile.AvatarURL != site.URL+"/avatar.png" {
		t.Errorf("profile = %+v", p.Profile)
	}
	var links []string
	for _, l := range p.Links {
		links = append(links, l.Title+"="+l.URL)
	}
	if strings.Join(links, ",") != "Order cakes=https://shop.example/cakes,Menu PDF=https://cdn.example/menu.pdf" {
		t.Errorf("links = %v", links)
	}
	plats := []string{}
	for _, s := range p.Socials {
		plats = append(plats, s.Platform)
	}
	if strings.Join(plats, ",") != "instagram,email,tiktok" {
		t.Errorf("socials = %v", plats)
	}
	if n := len(editor(t, h, a.AccessToken).Blocks); n != 0 {
		t.Fatalf("preview must not write: %d blocks", n)
	}

	var ap struct {
		Applied importApplied
		Profile Profile
	}
	do(t, h, "POST", "/api/v1/import", map[string]any{"url": site.URL + "/ana", "apply": true}, tok).expect(200).json(&ap)
	if ap.Applied != (importApplied{Links: 2, Socials: 3, DisplayName: true, Bio: true, Avatar: true}) {
		t.Errorf("applied = %+v", ap.Applied)
	}
	if ap.Profile.DisplayName != "Ana Bakes" || ap.Profile.AvatarURL == nil || !strings.Contains(*ap.Profile.AvatarURL, "/uploads/") || onboardingOf(t, ap.Profile).Source != "import" {
		t.Errorf("profile after apply = %+v", ap.Profile)
	}
	if n := len(editor(t, h, a.AccessToken).Blocks); n != 2 {
		t.Errorf("blocks = %d", n)
	}
	// A second apply doesn't overwrite filled fields or socials, but appends links.
	do(t, h, "POST", "/api/v1/import", map[string]any{"url": site.URL + "/generic", "apply": true}, tok).expect(200).json(&ap)
	if ap.Applied.DisplayName || ap.Applied.Bio || ap.Applied.Socials != 0 || ap.Applied.Links != 2 || ap.Profile.DisplayName != "Ana Bakes" {
		t.Errorf("second apply = %+v", ap.Applied)
	}

	do(t, h, "POST", "/api/v1/import", map[string]any{"url": "ftp://x"}, tok).expect(400)
	do(t, h, "POST", "/api/v1/import", map[string]any{"url": site.URL, "apply": true, "tab_id": "00000000-0000-0000-0000-000000000000"}, tok).expect(400)
	s.fetcher = newSafeFetcher(false)
	if c := do(t, h, "POST", "/api/v1/import", map[string]any{"url": site.URL}, tok).expect(400).errCode(); c != "blocked_url" {
		t.Errorf("code = %s", c)
	}
}
