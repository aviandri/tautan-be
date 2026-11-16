package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThemeV2Validation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	full := `{"theme":{"preset":"dusk","bg":{"type":"gradient","value":"linear-gradient(135deg, #ff7e5f 0%, rgba(254,180,123,0.8) 100%)"},
	  "bg_desktop":"#111111","button":{"style":"outline","radius":22,"color":"#000","text_color":"white"},
	  "font":"Inter","fonts":{"title":"Poppins","body":"DM Sans"},"text_color":"hsl(210, 20%, 10%)",
	  "header":{"layout":"banner","banner_image_url":"https://cdn.example/b.jpg"},
	  "block":{"color":"#ffffff","text_color":"#111"},"label":{"color":"#ff0","text_color":"#000"},
	  "border":{"width":2,"color":"#000000"},"shadow":{"amount":20,"style":"soft"},"spacing":26,
	  "future_field":{"anything":true}}}`
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", full, tok).expect(200).json(&p)
	var th map[string]any
	json.Unmarshal(p.Theme, &th)
	if th["future_field"] == nil || th["header"].(map[string]any)["layout"] != "banner" {
		t.Errorf("theme not stored as sent: %s", p.Theme)
	}
	// The MVP default theme stays valid.
	do(t, h, "PATCH", "/api/v1/profile", `{"theme":`+string(DefaultTheme)+`}`, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/profile", `{"theme":{"bg":{"type":"split","value":"linear-gradient(#000 50%, #fff 50%)"}}}`, tok).expect(200)
	do(t, h, "PATCH", "/api/v1/profile", `{"theme":{"bg":{"type":"none","value":""}}}`, tok).expect(200)

	for _, bad := range []string{
		`{"bg":{"type":"color","value":"red; background:url(https://evil)"}}`,
		`{"bg":{"type":"gradient","value":"url(https://evil.example/x.png)"}}`,
		`{"bg":{"type":"gradient","value":"linear-gradient(red, blue); }"}}`,
		`{"bg":{"type":"image","value":"javascript:alert(1)"}}`,
		`{"bg":{"type":"video","value":""}}`,
		`{"button":{"style":"glow"}}`,
		`{"button":{"radius":500}}`,
		`{"text_color":"#12"}`,
		`{"fonts":{"title":"Inter;}body{display:none"}}`,
		`{"header":{"layout":"sidebar"}}`,
		`{"header":{"banner_image_url":"data:image/png;base64,AAAA"}}`,
		`{"border":{"width":-1}}`,
		`{"shadow":{"style":"hard"}}`,
		`{"spacing":"wide"}`,
		`{"preset":"Has Spaces"}`,
	} {
		r := do(t, h, "PATCH", "/api/v1/profile", `{"theme":`+bad+`}`, tok)
		if r.Code != 400 || r.errCode() != "invalid_theme" {
			t.Errorf("%s: %d %s", bad, r.Code, r.Body)
		}
	}
}

func TestProfileSocials(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	if string(a.Profile.Socials) != "[]" || a.Profile.SocialsPos != "top" {
		t.Fatalf("defaults: %s %s", a.Profile.Socials, a.Profile.SocialsPos)
	}
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", `{"socials":[{"platform":"Instagram","url":"https://instagram.com/alice"},{"platform":"divider","url":"x"},{"platform":"email","url":"mailto:a@b.co"}],"socials_position":"bottom"}`, tok).
		expect(200).json(&p)
	if !jsonEq(p.Socials, `[{"platform":"instagram","url":"https://instagram.com/alice"},{"platform":"divider","url":""},{"platform":"email","url":"mailto:a@b.co"}]`) || p.SocialsPos != "bottom" {
		t.Errorf("socials = %s %s", p.Socials, p.SocialsPos)
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, tok).expect(200)
	var pub pageResponse
	do(t, h, "GET", "/api/v1/public/alice", nil).expect(200).json(&pub)
	if !strings.Contains(string(pub.Profile.Socials), "instagram") {
		t.Error("socials missing from public page")
	}
	for body, code := range map[string]string{
		`{"socials":{"platform":"x"}}`:                               "invalid_socials",
		`{"socials":[{"platform":"x","url":"javascript:alert(1)"}]}`: "invalid_socials",
		`{"socials":[{"platform":"","url":"https://x.com/a"}]}`:      "invalid_socials",
		`{"socials":[{"platform":"x y","url":"https://x.com/a"}]}`:   "invalid_socials",
		`{"socials_position":"left"}`:                                "invalid_socials_position",
		`{"socials":[` + strings.Repeat(`{"platform":"x","url":"https://x.com"},`, 30) + `{"platform":"x","url":"https://x.com"}]}`: "invalid_socials",
	} {
		if r := do(t, h, "PATCH", "/api/v1/profile", body, tok); r.errCode() != code {
			t.Errorf("%.60s: %d %s", body, r.Code, r.Body)
		}
	}
	do(t, h, "PATCH", "/api/v1/profile", `{"socials":[]}`, tok).expect(200).json(&p)
	if string(p.Socials) != "[]" {
		t.Errorf("clear socials = %s", p.Socials)
	}
}

func TestThemePalette(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "palette")
	tok := bearer(a.AccessToken)
	good := `{"theme":{"preset":"pelangi","palette_mode":"cycle","palette":{
	  "surfaces":["#ffe1d6","#d9f2e6","#e5e0ff","#fff3c4","rgb(214, 236, 255)"],
	  "accents":["#ff6b4a","rgba(30,30,30,0.9)"],"ink":"#2a1208","ink_on_accent":"#fff"}}}`
	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", good, tok).expect(200).json(&p)
	if !strings.Contains(string(p.Theme), `"palette_mode":"cycle"`) || !strings.Contains(string(p.Theme), `rgb(214, 236, 255)`) {
		t.Errorf("palette not stored as sent: %s", p.Theme)
	}
	for _, mode := range []string{"by_type", "mono"} {
		do(t, h, "PATCH", "/api/v1/profile", `{"theme":{"palette_mode":"`+mode+`"}}`, tok).expect(200)
	}
	surf := func(n int) string {
		c := make([]string, n)
		for i := range c {
			c[i] = `"#abcdef"`
		}
		return "[" + strings.Join(c, ",") + "]"
	}
	pal := func(surfaces, accents, ink, onAccent string) string {
		return `{"palette":{"surfaces":` + surfaces + `,"accents":` + accents + `,"ink":` + ink + `,"ink_on_accent":` + onAccent + `}}`
	}
	for _, bad := range []string{
		pal(surf(3), `["#000"]`, `"#111"`, `"#fff"`), // too few surfaces
		pal(surf(7), `["#000"]`, `"#111"`, `"#fff"`), // too many surfaces
		pal(surf(4), `[]`, `"#111"`, `"#fff"`),
		pal(surf(4), `["#000","#111","#222"]`, `"#111"`, `"#fff"`),
		pal(surf(4), `["red"]`, `"#111"`, `"#fff"`),               // named colours not allowed in a palette
		pal(surf(4), `["hsl(10, 50%, 50%)"]`, `"#111"`, `"#fff"`), // hsl not allowed
		pal(surf(4), `["#000"]`, `"#11"`, `"#fff"`),               // bad hex
		pal(surf(4), `["#000"]`, `"#111"`, `""`),                  // ink_on_accent required
		pal(surf(4), `["#000"]`, `"#111; background:url(x)"`, `"#fff"`),
		pal(`["#fff","#fff","#fff","rgb(1,2,3); x:y"]`, `["#000"]`, `"#111"`, `"#fff"`),
		`{"palette_mode":"rainbow"}`,
		`{"palette":"#fff"}`,
	} {
		if r := do(t, h, "PATCH", "/api/v1/profile", `{"theme":`+bad+`}`, tok); r.Code != 400 {
			t.Errorf("%s: got %d", bad, r.Code)
		}
	}
	// palette: null clears it
	do(t, h, "PATCH", "/api/v1/profile", `{"theme":{"palette":null}}`, tok).expect(200)
}
