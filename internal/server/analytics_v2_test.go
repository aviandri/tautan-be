package server

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestParseUAAndChannel(t *testing.T) {
	cases := map[string][2]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1": {"Safari", "iOS"},
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 300.0.0.0":       {"Instagram", "iOS"},
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36":                       {"Chrome", "Android"},
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36 Edg/120.0":                   {"Edge", "Windows"},
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.0; rv:121.0) Gecko/20100101 Firefox/121.0":                                                     {"Firefox", "macOS"},
		"Mozilla/5.0 (Linux; Android 13; SM-S901B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0 Mobile Safari/537.36":  {"Samsung Internet", "Android"},
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 [FBAN/FBIOS;FBAV/440.0]":   {"Facebook", "iOS"},
		"curl/8.0": {"Other", "Other"},
		"":         {"", ""},
	}
	for ua, want := range cases {
		if b, o := parseUA(ua); b != want[0] || o != want[1] {
			t.Errorf("%.50q => %s/%s, want %v", ua, b, o, want)
		}
	}
	ch := []struct{ host, src, med, browser, want string }{
		{"instagram.com", "", "", "", "social"},
		{"google.co.id", "", "", "", "search"},
		{"www.bing.com", "", "", "", "search"},
		{"blog.example", "", "", "", "referral"},
		{"", "", "", "", "direct"},
		{"", "", "", "Instagram", "social"},
		{"", "newsletter", "email", "", "email"},
		{"", "qr", "", "", "qr"},
		{"", "ig", "", "", "social"},
		{"", "google", "cpc", "", "paid"},
		{"", "partner", "", "", "referral"},
	}
	for _, c := range ch {
		if got := channelFor(c.host, c.src, c.med, c.browser); got != c.want {
			t.Errorf("channel(%v) = %s, want %s", c, got, c.want)
		}
	}
	if normalizeHost("l.instagram.com") != "instagram.com" || normalizeHost("m.facebook.com") != "facebook.com" || normalizeHost("t.co") != "t.co" {
		t.Error("normalizeHost")
	}
}

func TestEventCaptureV2(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	ig := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 300.0"
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "view", "referrer": "https://l.instagram.com/?u=x", "page_url": "http://localhost:3000/alice?utm_source=IG&utm_campaign=Launch"},
		withHeader("User-Agent", ig)).expect(204)
	do(t, h, "POST", "/api/v1/public/alice/events", map[string]any{"kind": "click", "block_id": f.link.ID, "utm_source": "newsletter", "utm_medium": "email"},
		withHeader("User-Agent", ig)).expect(204)
	rows, _ := testPool.Query(t.Context(), `SELECT referrer, browser, os, utm_source, utm_medium, utm_campaign, channel, visitor_hash FROM events ORDER BY id`)
	var got [][]string
	for rows.Next() {
		v := make([]string, 8)
		rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7])
		got = append(got, v)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d", len(got))
	}
	if strings.Join(got[0][:7], "|") != "instagram.com|Instagram|iOS|ig||launch|social" {
		t.Errorf("view = %v", got[0])
	}
	if strings.Join(got[1][:7], "|") != "|Instagram|iOS|newsletter|email||email" {
		t.Errorf("click = %v", got[1])
	}
	if len(got[0][7]) != 32 || got[0][7] != got[1][7] {
		t.Errorf("visitor hash should be stable within a day: %q %q", got[0][7], got[1][7])
	}
}

func TestAnalyticsV2(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	tok := bearer(f.tok)
	var pid string
	testPool.QueryRow(t.Context(), `SELECT id FROM profiles WHERE username='alice'`).Scan(&pid)
	jkt, _ := time.LoadLocation("Asia/Jakarta") // UTC+7
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, jkt)
	s.now = func() time.Time { return now }

	type ev struct {
		kind, visitor, country, device, browser, ref, channel string
		block                                                 bool
		at                                                    time.Time
	}
	day := func(d, hour int) time.Time { return time.Date(2026, 3, 10-d, hour, 0, 0, 0, jkt) }
	evs := []ev{
		{"view", "v1", "ID", "mobile", "Instagram", "instagram.com", "social", false, day(0, 8)},
		{"view", "v1", "ID", "mobile", "Instagram", "instagram.com", "social", false, day(0, 8)},
		{"click", "v1", "ID", "mobile", "Instagram", "instagram.com", "social", true, day(0, 8)},
		{"view", "v2", "SG", "desktop", "Chrome", "", "direct", false, day(0, 1)}, // 18:00 UTC the day before: local today
		{"view", "v3", "ID", "mobile", "Safari", "google.com", "search", false, day(1, 12)},
		{"view", "v4", "US", "desktop", "Chrome", "", "direct", false, day(9, 12)},  // previous period for 7d
		{"view", "v5", "US", "desktop", "Chrome", "", "direct", false, day(10, 12)}, // previous period for 7d
		{"click", "v5", "US", "desktop", "Chrome", "", "direct", true, day(10, 12)},
	}
	for _, e := range evs {
		var blk any
		if e.block {
			blk = f.link.ID
		}
		if _, err := testPool.Exec(t.Context(), `INSERT INTO events (profile_id, block_id, kind, visitor_hash, country, device, browser, referrer, channel, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, pid, blk, e.kind, e.visitor, e.country, e.device, e.browser, e.ref, e.channel, e.at); err != nil {
			t.Fatal(err)
		}
	}

	var ov struct {
		Range    map[string]string
		Totals   totals
		Previous totals
		Change   map[string]*float64
		Series   []seriesRow
		TopLink  *struct {
			BlockID string `json:"block_id"`
			Clicks  int
		} `json:"top_link"`
	}
	do(t, h, "GET", "/api/v1/analytics/overview?range=7d&tz=Asia/Jakarta", nil, tok).expect(200).json(&ov)
	if ov.Totals.Views != 4 || ov.Totals.Clicks != 1 || ov.Totals.Visitors != 3 || ov.Totals.CTR != 0.25 || ov.Totals.Engagement != 0.3333 {
		t.Errorf("totals = %+v", ov.Totals)
	}
	if ov.Previous.Views != 2 || ov.Previous.Visitors != 2 || ov.Previous.Clicks != 1 {
		t.Errorf("previous = %+v", ov.Previous)
	}
	if ov.Change["views"] == nil || *ov.Change["views"] != 100 || *ov.Change["clicks"] != 0 {
		t.Errorf("change = %v", ov.Change)
	}
	if ov.Range["interval"] != "day" || len(ov.Series) != 7 || ov.Series[6].T != "2026-03-10" || ov.Series[6].Views != 3 || ov.Series[6].Visitors != 2 || ov.Series[5].Views != 1 {
		t.Errorf("series = %+v range=%v", ov.Series, ov.Range)
	}
	if ov.TopLink == nil || ov.TopLink.BlockID != f.link.ID {
		t.Errorf("top link = %+v", ov.TopLink)
	}

	// Today in Jakarta, hourly.
	do(t, h, "GET", "/api/v1/analytics/overview?range=today&tz=Asia/Jakarta", nil, tok).expect(200).json(&ov)
	if len(ov.Series) != 24 || ov.Series[8].T != "2026-03-10T08:00" || ov.Series[8].Views != 2 || ov.Series[1].Views != 1 || ov.Totals.Views != 3 {
		t.Errorf("today hourly: len=%d s8=%+v s1=%+v totals=%+v", len(ov.Series), ov.Series[8], ov.Series[1], ov.Totals)
	}
	// Same "today" in UTC excludes the 01:00 Jakarta view (it was yesterday in UTC).
	do(t, h, "GET", "/api/v1/analytics/overview?range=today", nil, tok).expect(200).json(&ov)
	if ov.Totals.Views != 2 {
		t.Errorf("utc today views = %d", ov.Totals.Views)
	}

	// Cross-filter: mobile only.
	do(t, h, "GET", "/api/v1/analytics/overview?range=7d&tz=Asia/Jakarta&device=mobile", nil, tok).expect(200).json(&ov)
	if ov.Totals.Views != 3 || ov.Totals.Visitors != 2 {
		t.Errorf("filtered = %+v", ov.Totals)
	}
	// referrer= (empty) filters to direct.
	do(t, h, "GET", "/api/v1/analytics/overview?range=7d&tz=Asia/Jakarta&referrer=", nil, tok).expect(200).json(&ov)
	if ov.Totals.Views != 1 {
		t.Errorf("direct-only views = %d", ov.Totals.Views)
	}

	// Custom range with weekly buckets.
	do(t, h, "GET", "/api/v1/analytics/overview?range=custom&from=2026-02-23&to=2026-03-10&interval=week&tz=Asia/Jakarta", nil, tok).expect(200).json(&ov)
	if len(ov.Series) != 3 || ov.Series[0].T != "2026-02-23" || ov.Totals.Views != 6 {
		t.Errorf("weekly = %+v totals=%+v", ov.Series, ov.Totals)
	}

	// Breakdown.
	var bd struct{ Items []breakdownRow }
	do(t, h, "GET", "/api/v1/analytics/breakdown?dimension=country&range=7d&tz=Asia/Jakarta", nil, tok).expect(200).json(&bd)
	if len(bd.Items) != 2 || bd.Items[0] != (breakdownRow{"ID", 3, 1, 2}) || bd.Items[1] != (breakdownRow{"SG", 1, 0, 1}) {
		t.Errorf("country = %+v", bd.Items)
	}
	do(t, h, "GET", "/api/v1/analytics/breakdown?dimension=channel&range=30d&tz=Asia/Jakarta&country=ID", nil, tok).expect(200).json(&bd)
	if len(bd.Items) != 2 || bd.Items[0].Value != "social" {
		t.Errorf("channel|ID = %+v", bd.Items)
	}

	// Blocks + per-block series.
	var blocks struct{ Items []blockStat }
	do(t, h, "GET", "/api/v1/analytics/blocks?range=30d&tz=Asia/Jakarta", nil, tok).expect(200).json(&blocks)
	if len(blocks.Items) < 3 || blocks.Items[0].BlockID != f.link.ID || blocks.Items[0].Clicks != 2 || blocks.Items[0].Title != "Site" {
		t.Errorf("blocks = %+v", blocks.Items)
	}
	var bs struct {
		Clicks int
		Series []struct {
			T      string
			Clicks int
		}
	}
	do(t, h, "GET", "/api/v1/analytics/blocks/"+f.link.ID+"/series?range=7d&tz=Asia/Jakarta", nil, tok).expect(200).json(&bs)
	if bs.Clicks != 1 || len(bs.Series) != 7 || bs.Series[6].Clicks != 1 {
		t.Errorf("block series = %+v", bs)
	}
	do(t, h, "GET", "/api/v1/analytics/blocks/00000000-0000-0000-0000-000000000000/series", nil, tok).expect(404)

	// CSV.
	r := do(t, h, "GET", "/api/v1/analytics/export.csv?range=7d&tz=Asia/Jakarta", nil, tok).expect(200)
	recs, _ := csv.NewReader(strings.NewReader(r.Body.String())).ReadAll()
	if len(recs) != 8 || recs[7][0] != "2026-03-10" || recs[7][1] != "3" {
		t.Errorf("series csv = %v", recs)
	}
	r = do(t, h, "GET", "/api/v1/analytics/export.csv?type=breakdown&dimension=browser&range=7d", nil, tok).expect(200)
	if !strings.HasPrefix(r.Body.String(), "browser,Views,Clicks,Visitors") {
		t.Errorf("breakdown csv = %s", r.Body)
	}
	do(t, h, "GET", "/api/v1/analytics/export.csv?type=blocks&range=7d", nil, tok).expect(200)

	// Validation.
	for _, q := range []string{"range=1y", "range=custom&from=2026-03-10&to=2026-03-01", "range=custom&from=2020-01-01&to=2026-01-01",
		"range=90d&interval=hour", "tz=Mars/Base", "interval=minute"} {
		if r := do(t, h, "GET", "/api/v1/analytics/overview?"+q, nil, tok); r.Code != 400 {
			t.Errorf("%s: %d", q, r.Code)
		}
	}
	do(t, h, "GET", "/api/v1/analytics/breakdown?dimension=password", nil, tok).expect(400)
	do(t, h, "GET", "/api/v1/analytics/overview", nil).expect(401)
}
