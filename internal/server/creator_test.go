package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCreatorBlockValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "creator")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	post := func(typ, data string) resp {
		return do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok)
	}
	ok := func(typ, data string) Block {
		t.Helper()
		var b Block
		post(typ, data).expect(201).json(&b)
		return b
	}
	bad := func(typ, data, path string) {
		t.Helper()
		r := post(typ, data)
		var e struct {
			Error apiError `json:"error"`
		}
		r.json(&e)
		if r.Code != 400 || (path != "" && e.Error.Details[path] == "") {
			t.Errorf("%s %s: got %d %s (want 400 at %q)", typ, data, r.Code, r.Body, path)
		}
	}

	// media_kit
	ok("media_kit", `{"platforms":[{"platform":"tiktok","handle":"@me","followers":120000,"avg_views":40000,"engagement":7.5}],
		"audience":{"age":[{"label":"18-24","pct":60},{"label":"25-34","pct":30}],"gender":[{"label":"Female","pct":70}],"cities":[{"label":"Jakarta","pct":25}]},
		"updated_at":"2026-11-01","pdf_url":"/uploads/kit.pdf"}`)
	bad("media_kit", `{"platforms":[],"updated_at":"2026-11-01"}`, "platforms")
	bad("media_kit", `{"platforms":[{"platform":"myspace","handle":"x","followers":1}],"updated_at":"2026-11-01"}`, "platforms.0.platform")
	bad("media_kit", `{"platforms":[{"platform":"tiktok","handle":"x","followers":1}],"updated_at":"Nov 1"}`, "updated_at")
	bad("media_kit", `{"platforms":[{"platform":"tiktok","handle":"x","followers":1,"engagement":120}],"updated_at":"2026-11-01"}`, "platforms.0.engagement")
	bad("media_kit", `{"platforms":[{"platform":"tiktok","handle":"x","followers":1}],"updated_at":"2026-11-01","audience":{"age":[{"label":"a","pct":70},{"label":"b","pct":40}]}}`, "audience.age")

	// rate_card + form ownership
	var form Block
	post("form", `{"title":"Collab","fields":[{"id":"brand","type":"text","label":"Brand"}]}`).expect(201).json(&form)
	b := ok("rate_card", `{"packages":[{"name":"1 TikTok video","price_from":500000000,"deliverables":["60s video","2 revisions"],"turnaround":"7 days"}],"form_block_id":"`+form.ID+`"}`)
	if !strings.Contains(string(b.Data), `"currency":"IDR"`) {
		t.Errorf("rate_card = %s", b.Data)
	}
	var text Block
	post("text", `{"content":"x"}`).expect(201).json(&text)
	bad("rate_card", `{"packages":[{"name":"x","deliverables":["y"]}],"form_block_id":"`+text.ID+`"}`, "form_block_id")
	bad("rate_card", `{"packages":[{"name":"x","deliverables":["y"]}],"form_block_id":"`+tab.ID+`"}`, "form_block_id")
	bad("rate_card", `{"packages":[{"name":"x","deliverables":[]}]}`, "packages.0.deliverables")
	other := signup(t, h, "other")
	var foreign Block
	do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": editor(t, h, other.AccessToken).Tabs[0].ID, "type": "form",
		"data": map[string]any{"fields": []map[string]any{{"id": "a", "type": "text", "label": "A"}}}}, bearer(other.AccessToken)).expect(201).json(&foreign)
	bad("rate_card", `{"packages":[{"name":"x","deliverables":["y"]}],"form_block_id":"`+foreign.ID+`"}`, "form_block_id")
	do(t, h, "PATCH", "/api/v1/blocks/"+b.ID, map[string]any{"data": map[string]any{"packages": []map[string]any{{"name": "x", "deliverables": []string{"y"}}},
		"form_block_id": foreign.ID}}, tok).expect(400)
	// payment confirm via form uses the same check
	bad("payment", `{"qris_image_url":"/uploads/q.png","confirm":{"kind":"form","target":"`+foreign.ID+`"}}`, "confirm.target")
	ok("payment", `{"qris_image_url":"/uploads/q.png","confirm":{"kind":"form","target":"`+form.ID+`"}}`)

	// brands, shop_the_look, gallery
	ok("brands", `{"title":"Worked with","logos":[{"image_url":"/uploads/a.png","name":"Acme","url":"https://acme.example"}]}`)
	bad("brands", `{"logos":[{"image_url":"javascript:x","name":"A"}]}`, "logos.0.image_url")
	ok("shop_the_look", `{"image_url":"/uploads/look.jpg","spots":[{"x":20.5,"y":80,"title":"Jacket","price":45000000,"url":"https://shop.example/j","code":"LOOK10"}]}`)
	bad("shop_the_look", `{"image_url":"/uploads/look.jpg","spots":[{"x":120,"y":5,"title":"J","url":"https://x.example"}]}`, "spots.0.x")
	bad("shop_the_look", `{"image_url":"/uploads/look.jpg","spots":[{"x":1,"y":5,"title":"J"}]}`, "spots.0.url")
	b = ok("gallery", `{"images":[{"url":"/uploads/1.jpg","alt":"one"},{"url":"https://cdn.example/2.jpg","link_url":"https://x.example"}]}`)
	if !strings.Contains(string(b.Data), `"layout":"grid"`) {
		t.Errorf("gallery default layout: %s", b.Data)
	}
	bad("gallery", `{"images":[{"url":"/uploads/1.jpg"}],"layout":"masonry"}`, "layout")

	// community
	ok("community", `{"platform":"whatsapp_group","url":"https://chat.whatsapp.com/AbC123","title":"Customers group","members_note":"1.2k members"}`)
	ok("community", `{"platform":"whatsapp_channel","url":"https://www.whatsapp.com/channel/0029Va","title":"Drops"}`)
	ok("community", `{"platform":"telegram","url":"https://t.me/mychannel","title":"Telegram"}`)
	bad("community", `{"platform":"telegram","url":"https://evil.example/t.me","title":"x"}`, "url")
	bad("community", `{"platform":"whatsapp_channel","url":"https://whatsapp.com/foo","title":"x"}`, "url")
	bad("community", `{"platform":"discord","url":"http://discord.gg/x","title":"x"}`, "url")
	bad("community", `{"platform":"slack","url":"https://slack.com","title":"x"}`, "platform")

	// events
	b = ok("events", `{"mode":"dates","tz":"Asia/Jakarta","dates":[{"date":"2030-01-05","time":"19:30","city":"Bandung","venue":"Hall","url":"https://tix.example","status":"few_left"}],"weekly":[{"days":["mon"],"start":"10:00","duration_min":60}]}`)
	if strings.Contains(string(b.Data), "weekly") {
		t.Errorf("dates mode keeps weekly: %s", b.Data)
	}
	ok("events", `{"mode":"weekly","tz":"Asia/Jakarta","weekly":[{"days":["fri","sat"],"start":"20:00","duration_min":120,"platform":"tiktok","title":"Live sale"}]}`)
	bad("events", `{"mode":"weekly","tz":"Asia/Jakarta","weekly":[{"days":["fri","fri"],"start":"20:00","duration_min":60}]}`, "weekly.0.days.1")
	bad("events", `{"mode":"weekly","tz":"Asia/Jakarta","weekly":[{"days":["fri"],"start":"20:00","duration_min":1000}]}`, "weekly.0.duration_min")
	bad("events", `{"mode":"dates","tz":"Asia/Jakarta","dates":[{"date":"2030-13-01"}]}`, "dates.0.date")
	bad("events", `{"mode":"dates","dates":[{"date":"2030-01-01"}]}`, "tz")
	bad("events", `{"mode":"monthly","tz":"UTC"}`, "mode")
}

func TestComputeEvents(t *testing.T) {
	jkt, _ := time.LoadLocation("Asia/Jakarta") // UTC+7, no DST
	// Friday 2026-11-20 20:30 in Jakarta.
	now := time.Date(2026, 11, 20, 20, 30, 0, 0, jkt)
	d := EventsData{Mode: "weekly", TZ: "Asia/Jakarta", Weekly: []WeeklySlot{
		{Days: []string{"fri"}, Start: "20:00", DurationMin: 60},
		{Days: []string{"sat", "sun"}, Start: "09:00", DurationMin: 30},
		{Days: []string{"thu"}, Start: "23:00", DurationMin: 720}, // overnight from yesterday, ended 11:00
	}}
	c := computeEvents(&d, now)
	if c.Live == nil || c.Live.Index != 0 || !c.Live.EndsAt.Equal(time.Date(2026, 11, 20, 21, 0, 0, 0, jkt)) {
		t.Errorf("live = %+v", c.Live)
	}
	if c.Next == nil || c.Next.Index != 1 || !c.Next.StartsAt.Equal(time.Date(2026, 11, 21, 9, 0, 0, 0, jkt)) {
		t.Errorf("next = %+v", c.Next)
	}
	// An overnight slot that started yesterday is live after midnight.
	c = computeEvents(&d, time.Date(2026, 11, 20, 2, 0, 0, 0, jkt))
	if c.Live == nil || c.Live.Index != 2 {
		t.Errorf("overnight live = %+v", c.Live)
	}
	// DST: a 09:00 New York slot is 14:00 UTC in winter, 13:00 UTC in summer.
	ny := EventsData{Mode: "weekly", TZ: "America/New_York", Weekly: []WeeklySlot{{Days: []string{"mon"}, Start: "09:00", DurationMin: 60}}}
	if c := computeEvents(&ny, time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)); c.Next.StartsAt.Hour() != 13 { // next Monday is Mar 9, after DST
		t.Errorf("DST next = %v", c.Next.StartsAt)
	}
	if c := computeEvents(&ny, time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)); c.Next.StartsAt.Hour() != 14 {
		t.Errorf("winter next = %v", c.Next.StartsAt)
	}
	// Dates: past ones (in the card's tz) are hidden.
	dd := EventsData{Mode: "dates", TZ: "Asia/Jakarta", Dates: []EventDate{{Date: "2026-11-19"}, {Date: "2026-11-20"}, {Date: "2026-12-01"}}}
	c = computeEvents(&dd, now)
	if len(dd.Dates) != 2 || c.PastHidden != 1 || c.TodayLocal != "2026-11-20" {
		t.Errorf("dates = %+v computed %+v", dd.Dates, c)
	}
}

func TestEventsPublicComputed(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "liver", true)
	createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "events", "data": map[string]any{
		"mode": "weekly", "tz": "UTC", "weekly": []map[string]any{{"days": []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, "start": "00:00", "duration_min": 720}}}})
	s.now = func() time.Time { return time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC) }
	var page pageResponse
	do(t, h, "GET", "/api/v1/public/liver", nil).expect(200).json(&page)
	var ev struct {
		Computed EventsComputed `json:"computed"`
	}
	for _, b := range page.Blocks {
		if b.Type == "events" {
			json.Unmarshal(b.Data, &ev)
		}
	}
	if ev.Computed.Live == nil || ev.Computed.Next == nil || ev.Computed.ComputedAt.IsZero() {
		t.Errorf("computed = %+v", ev.Computed)
	}
	// The editor gets stored data only.
	s.now = time.Now
	var ed pageResponse
	do(t, h, "GET", "/api/v1/profile/editor", nil, bearer(f.tok)).expect(200).json(&ed)
	for _, b := range ed.Blocks {
		if b.Type == "events" && strings.Contains(string(b.Data), "computed") {
			t.Error("editor data has computed")
		}
	}
}
