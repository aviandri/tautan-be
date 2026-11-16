package server

import (
	"strings"
	"testing"
)

func TestLayoutBlockValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "layouts")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	post := func(typ, data string) resp {
		return do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok)
	}
	ok := func(typ, data string) string {
		t.Helper()
		var b Block
		post(typ, data).expect(201).json(&b)
		return string(b.Data)
	}
	bad := func(typ, data, path string) {
		t.Helper()
		r := post(typ, data)
		var e struct {
			Error apiError `json:"error"`
		}
		r.json(&e)
		if r.Code != 400 || (path != "" && e.Error.Details[path] == "") {
			t.Errorf("%s %s: %d %s (want 400 at %q)", typ, data, r.Code, r.Body, path)
		}
	}

	if d := ok("spotlight", `{"image_url":"/uploads/drop.jpg","eyebrow":"New","headline":"The Fall drop","text":"Out Friday","button":{"label":"Shop","url":"https://x.example"}}`); !strings.Contains(d, `"align":"left"`) {
		t.Errorf("spotlight default align: %s", d)
	}
	bad("spotlight", `{"headline":""}`, "headline")
	bad("spotlight", `{"headline":"x","button":{"label":"Go","url":"javascript:x"}}`, "button.url")

	if d := ok("marquee", `{"text":"FREE SHIPPING"}`); !strings.Contains(d, `"speed":"normal"`) || !strings.Contains(d, `"style":"ribbon"`) {
		t.Errorf("marquee defaults: %s", d)
	}
	bad("marquee", `{"text":"x","speed":"warp"}`, "speed")

	ok("timeline", `{"style":"dotted","items":[{"title":"Order"},{"title":"Pay","highlight":true,"badge":"Step 2"}]}`)
	bad("timeline", `{"items":[{"title":"a","highlight":true},{"title":"b","highlight":true}]}`, "items.1.highlight")
	bad("timeline", `{"items":[]}`, "items")

	if d := ok("resume", `{"items":[{"role":"Designer","company":"Acme","start":"2022-03","end":"2024-01","current":true}]}`); strings.Contains(d, `"end"`) {
		t.Errorf("current job keeps end: %s", d)
	}
	bad("resume", `{"items":[{"role":"x","company":"y","start":"2024-05","end":"2023-01"}]}`, "items.0.end")
	bad("resume", `{"items":[{"role":"x","company":"y","start":"May 2024"}]}`, "items.0.start")

	ok("before_after", `{"before_url":"/uploads/a.jpg","after_url":"/uploads/b.jpg","before_label":"Before","start_pct":40}`)
	bad("before_after", `{"before_url":"/uploads/a.jpg"}`, "after_url")
	bad("before_after", `{"before_url":"/uploads/a.jpg","after_url":"/uploads/b.jpg","start_pct":101}`, "start_pct")

	d := ok("map", `{"label":"Toko","address":"Jl. Braga 10","lat":-6.9174639,"lng":107.6098111,"links":{"google":"https://evil.example"}}`)
	if !strings.Contains(d, `"lat":-6.91746`) || !strings.Contains(d, `"zoom":15`) || strings.Contains(d, "evil") ||
		!strings.Contains(d, `"google":"https://www.google.com/maps/dir/?api=1`) || !strings.Contains(d, `destination=-6.91746,107.60981"`) {
		t.Errorf("map = %s", d)
	}
	bad("map", `{"lat":91,"lng":0}`, "lat")
	bad("map", `{"lat":0,"lng":0}`, "lat")
	bad("map", `{"lat":1,"lng":1,"zoom":25}`, "zoom")

	// Additions to existing types.
	ok("text", `{"content":"hi","style":"sticky_note"}`)
	bad("text", `{"content":"hi","style":"neon"}`, "style")
	ok("discount_code", `{"code":"A10","codes":[{"code":"A10","discount":"10%"},{"code":"B20","url":"https://x.example"}]}`)
	bad("discount_code", `{"codes":[{"code":""}]}`, "codes.0.code")
	ok("social_icons", `{"variant":"donate","items":[{"platform":"saweria","url":"https://saweria.co/x"}]}`)
	if d := ok("social_icons", `{"variant":"icons","items":[{"platform":"instagram","url":"https://instagram.com/x"}]}`); strings.Contains(d, "variant") {
		t.Errorf("default variant should be omitted: %s", d)
	}
	bad("social_icons", `{"variant":"donate","items":[{"platform":"instagram","url":"https://instagram.com/x"}]}`, "items.0.platform")
	bad("social_icons", `{"variant":"big","items":[]}`, "variant")
}
