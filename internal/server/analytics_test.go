package server

import (
	"testing"
	"time"
)

func TestAnalyticsSummary(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "alice", true)
	other := setupPage(t, h, "bob", true)
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	var pid, otherPID string
	testPool.QueryRow(t.Context(), `SELECT id FROM profiles WHERE username = 'alice'`).Scan(&pid)
	testPool.QueryRow(t.Context(), `SELECT id FROM profiles WHERE username = 'bob'`).Scan(&otherPID)
	second := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "link", "data": map[string]any{"title": "Shop", "url": "https://shop.example"}})

	ins := func(profile, kind string, block *string, ref string, at time.Time) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO events (profile_id, block_id, kind, referrer, created_at) VALUES ($1, $2, $3, $4, $5)`,
			profile, block, kind, ref, at); err != nil {
			t.Fatal(err)
		}
	}
	day := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	for i := 0; i < 4; i++ {
		ins(pid, "view", nil, "instagram.com", day(0))
	}
	ins(pid, "view", nil, "", day(0))
	ins(pid, "view", nil, "", day(6))
	ins(pid, "view", nil, "t.co", day(7))   // outside 7d, inside 30d
	ins(pid, "view", nil, "x.com", day(40)) // only in 90d
	ins(pid, "click", &f.link.ID, "", day(0))
	ins(pid, "click", &second.ID, "", day(1))
	ins(pid, "click", &second.ID, "", day(2))
	ins(otherPID, "view", nil, "", day(0)) // other profile ignored
	_ = other

	var sum analyticsSummary
	do(t, h, "GET", "/api/v1/analytics/summary", nil, bearer(f.tok)).expect(200).json(&sum)
	if sum.Views != 6 || sum.Clicks != 3 || sum.CTR != 0.5 {
		t.Fatalf("totals: views=%d clicks=%d ctr=%v", sum.Views, sum.Clicks, sum.CTR)
	}
	if len(sum.Series) != 7 || sum.Series[0].Date != "2026-03-04" || sum.Series[6].Date != "2026-03-10" {
		t.Fatalf("series = %+v", sum.Series)
	}
	if sum.Series[6].Views != 5 || sum.Series[6].Clicks != 1 || sum.Series[0].Views != 1 || sum.Series[3].Views != 0 {
		t.Errorf("series values = %+v", sum.Series)
	}
	if len(sum.TopLinks) != 2 || sum.TopLinks[0].BlockID != second.ID || sum.TopLinks[0].Title != "Shop" || sum.TopLinks[0].Clicks != 2 {
		t.Errorf("top links = %+v", sum.TopLinks)
	}
	if len(sum.TopReferrers) != 2 || sum.TopReferrers[0] != (topReferrer{"instagram.com", 4}) || sum.TopReferrers[1] != (topReferrer{"", 2}) {
		t.Errorf("top referrers = %+v", sum.TopReferrers)
	}

	do(t, h, "GET", "/api/v1/analytics/summary?range=30d", nil, bearer(f.tok)).expect(200).json(&sum)
	if sum.Views != 7 || len(sum.Series) != 30 {
		t.Errorf("30d: views=%d len=%d", sum.Views, len(sum.Series))
	}
	do(t, h, "GET", "/api/v1/analytics/summary?range=90d", nil, bearer(f.tok)).expect(200).json(&sum)
	if sum.Views != 8 || len(sum.Series) != 90 {
		t.Errorf("90d: views=%d len=%d", sum.Views, len(sum.Series))
	}
	do(t, h, "GET", "/api/v1/analytics/summary?range=1y", nil, bearer(f.tok)).expect(400)
	do(t, h, "GET", "/api/v1/analytics/summary", nil).expect(401)

	// Empty profile: zero-filled series, empty arrays (not null), ctr 0.
	b := signup(t, h, "carol")
	r := do(t, h, "GET", "/api/v1/analytics/summary", nil, bearer(b.AccessToken)).expect(200)
	var empty map[string]any
	r.json(&empty)
	if empty["ctr"].(float64) != 0 || empty["top_links"] == nil || empty["top_referrers"] == nil || len(empty["series"].([]any)) != 7 {
		t.Errorf("empty summary = %s", r.Body)
	}
}
