package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const ytFeed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
 <title>Perf Channel</title>
 <entry><yt:videoId>vid1</yt:videoId><title>Newest &amp; best</title><published>2026-11-15T10:00:00+00:00</published>
  <media:group><media:thumbnail url="%[1]s/thumb/vid1.jpg" width="480" height="360"/></media:group></entry>
 <entry><yt:videoId>vid2</yt:videoId><title>Older</title><published>2026-11-01T10:00:00+00:00</published>
  <media:group><media:thumbnail url="%[1]s/thumb/vid2.jpg" width="480" height="360"/></media:group></entry>
</feed>`

const podFeed = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel>
 <title>Ngobrol Santai</title>
 <image><url>%[1]s/thumb/rss.jpg</url></image>
 <itunes:image href="%[1]s/thumb/cover.jpg"/>
 <item><title>Ep 2</title><pubDate>Sat, 14 Nov 2026 08:00:00 +0700</pubDate><itunes:duration>1:02:03</itunes:duration>
  <enclosure url="https://cdn.example/ep2.mp3" type="audio/mpeg" length="1"/></item>
 <item><title>Ep 1</title><pubDate>Sat, 07 Nov 2026 08:00:00 GMT</pubDate><itunes:duration>754</itunes:duration>
  <enclosure url="https://cdn.example/ep1.mp3" type="audio/mpeg"/></item>
 <item><title>Bad</title><enclosure url="javascript:alert(1)"/></item>
 <item><title>Ep 0, cut off mid-way`

func TestRemoteCards(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir, s.cfg.PublicBaseURL = t.TempDir(), ""
	s.InlineImages, s.InlineRemote = true, true
	s.fetcher = newSafeFetcher(true)
	var hits atomic.Int32
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasPrefix(r.URL.Path, "/thumb/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(demoJPEG(480, 360, 10))
		case r.URL.Path == "/yt-oembed":
			fmt.Fprintf(w, `{"title":"My short","author_name":"Me","thumbnail_url":"%s/thumb/short.jpg"}`, up.URL)
		case r.URL.Path == "/tt-oembed":
			fmt.Fprintf(w, `{"title":"Dance","author_name":"tt user","thumbnail_url":"%s/thumb/tt.jpg"}`, up.URL)
		case r.URL.Path == "/ig-oembed":
			http.Error(w, "token expired", 400)
		case r.URL.Path == "/feed":
			fmt.Fprintf(w, ytFeed, up.URL)
		case r.URL.Path == "/pod.xml":
			fmt.Fprintf(w, podFeed, up.URL)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	for _, kv := range []struct {
		p *string
		v string
	}{{&youtubeOEmbedURL, up.URL + "/yt-oembed"}, {&tiktokOEmbedURL, up.URL + "/tt-oembed"},
		{&instagramOEmbedURL, up.URL + "/ig-oembed"}, {&youtubeFeedURL, up.URL + "/feed"}} {
		old := *kv.p
		*kv.p = kv.v
		t.Cleanup(func() { *kv.p = old })
	}
	t.Setenv("INSTAGRAM_OEMBED_TOKEN", "tok")
	h := s.Routes()
	f := setupPage(t, h, "creatr", true)
	tok := bearer(f.tok)

	post := func(typ string, data any) resp {
		return do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": f.tab.ID, "type": typ, "data": data}, tok)
	}
	// Validation.
	for _, bad := range []map[string]any{
		{"posts": []map[string]any{{"url": "https://example.com/p/1"}}},
		{"posts": []map[string]any{{"url": "https://instagram.com/someone"}}},
		{"posts": []map[string]any{}},
	} {
		post("post_grid", bad).expect(400)
	}
	post("latest_video", map[string]any{"channel_id": "@handle"}).expect(400)
	post("latest_video", map[string]any{"channel_id": "UCabcdefghijklmnopqrstuv", "show": map[string]any{"count": 5}}).expect(400)
	post("podcast", map[string]any{"feed_url": "file:///etc/passwd"}).expect(400)

	var grid Block
	post("post_grid", map[string]any{"posts": []map[string]any{
		{"url": "https://www.youtube.com/shorts/abc123"},
		{"url": "https://www.tiktok.com/@me/video/7300000000000000000", "caption": "My dance"},
		{"url": "https://www.instagram.com/reel/Cxyz/"},
	}}).expect(201).json(&grid)
	if !strings.Contains(string(grid.Data), `"platform":"tiktok"`) {
		t.Errorf("platform not set: %s", grid.Data)
	}
	post("latest_video", map[string]any{"channel_id": "UCabcdefghijklmnopqrstuv", "show": map[string]any{"count": 1}}).expect(201)
	post("podcast", map[string]any{"feed_url": up.URL + "/pod.xml", "count": 5}).expect(201)

	var page struct {
		Blocks []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"blocks"`
		Media map[string]imageInfo `json:"media"`
	}
	do(t, h, "GET", "/api/v1/public/creatr", nil).expect(200).json(&page)
	got := map[string]json.RawMessage{}
	for _, b := range page.Blocks {
		got[b.Type] = b.Data
	}

	var pg struct {
		Resolved struct {
			Posts []ResolvedPost `json:"posts"`
		} `json:"resolved"`
	}
	json.Unmarshal(got["post_grid"], &pg)
	ps := pg.Resolved.Posts
	if len(ps) != 3 || ps[0].Status != "ok" || ps[0].Title != "My short" || !strings.HasPrefix(ps[0].ThumbURL, "/uploads/") ||
		ps[1].Status != "ok" || ps[1].Author != "tt user" || ps[2].Status != "link_only" || ps[2].Platform != "instagram" {
		t.Fatalf("posts = %+v", ps)
	}
	if m := page.Media[ps[0].ThumbURL]; m.Width != 480 || len(m.Widths) == 0 {
		t.Errorf("thumbnail not in media map with variants: %+v", m)
	}

	var lv struct {
		Resolved struct {
			Status       string          `json:"status"`
			ChannelTitle string          `json:"channel_title"`
			Videos       []ResolvedVideo `json:"videos"`
		} `json:"resolved"`
	}
	json.Unmarshal(got["latest_video"], &lv)
	if r := lv.Resolved; r.Status != "ok" || r.ChannelTitle != "Perf Channel" || len(r.Videos) != 1 || r.Videos[0].VideoID != "vid1" ||
		r.Videos[0].Title != "Newest & best" || !strings.HasPrefix(r.Videos[0].ThumbURL, "/uploads/") || r.Videos[0].PublishedAt.IsZero() {
		t.Errorf("latest_video = %+v", r)
	}

	var pc struct {
		Resolved struct {
			Status   string            `json:"status"`
			Title    string            `json:"title"`
			ImageURL string            `json:"image_url"`
			Episodes []ResolvedEpisode `json:"episodes"`
		} `json:"resolved"`
	}
	json.Unmarshal(got["podcast"], &pc)
	r := pc.Resolved
	if r.Status != "ok" || r.Title != "Ngobrol Santai" || !strings.HasPrefix(r.ImageURL, "/uploads/") || len(r.Episodes) != 2 ||
		r.Episodes[0].DurationSec != 3723 || r.Episodes[1].DurationSec != 754 || r.Episodes[0].PublishedAt == nil ||
		r.Episodes[0].AudioURL != "https://cdn.example/ep2.mp3" {
		t.Errorf("podcast = %+v", r)
	}
	// No third-party URLs for images in the public payload.
	do(t, h, "GET", "/api/v1/public/creatr", nil).expect(200)
	raw := do(t, h, "GET", "/api/v1/public/creatr", nil).Body.String()
	if strings.Contains(raw, "/thumb/") {
		t.Errorf("public page hotlinks an upstream image")
	}

	// Refresh: nothing due right after fetching; due again after the TTL.
	before := hits.Load()
	if n, err := s.RefreshRemoteDue(t.Context()); err != nil || n != 0 || hits.Load() != before {
		t.Errorf("immediate refresh: n=%d err=%v hits=%d", n, err, hits.Load()-before)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, _ := s.RefreshRemoteDue(t.Context()); n != 2 { // the two feeds; posts last 7 days
		t.Errorf("after an hour %d keys were due, want 2", n)
	}
	// Thumbnails are re-hosted once (img: cache), not on every refresh.
	var imgs int
	testPool.QueryRow(t.Context(), `SELECT count(*) FROM images`).Scan(&imgs)
	if imgs != 5 { // short, tt, vid1, vid2, cover
		t.Errorf("%d images stored, want 5", imgs)
	}
	// A failing upstream keeps the last good result.
	youtubeFeedURL = up.URL + "/missing"
	s.now = func() time.Time { return time.Now().Add(4 * time.Hour) }
	s.RefreshRemoteDue(t.Context())
	s.now = time.Now
	do(t, h, "GET", "/api/v1/public/creatr", nil).expect(200).json(&page)
	for _, b := range page.Blocks {
		if b.Type == "latest_video" && !strings.Contains(string(b.Data), "Perf Channel") {
			t.Errorf("lost cached feed after a failure: %s", b.Data)
		}
	}
}

func TestPostPlatformAndDuration(t *testing.T) {
	for u, want := range map[string]string{
		"https://www.instagram.com/p/C1abc/":    "instagram",
		"https://instagram.com/reel/C1abc":      "instagram",
		"https://www.instagram.com/someone/":    "",
		"https://www.tiktok.com/@a.b/video/123": "tiktok",
		"https://vm.tiktok.com/ZSabc/":          "tiktok",
		"https://www.tiktok.com/@a":             "",
		"https://youtu.be/abc":                  "youtube",
		"https://www.youtube.com/watch?v=abc":   "youtube",
		"https://m.youtube.com/shorts/abc":      "youtube",
		"https://www.youtube.com/@chan":         "",
		"ftp://youtu.be/abc":                    "",
	} {
		if got := postPlatform(u); got != want {
			t.Errorf("%s => %q, want %q", u, got, want)
		}
	}
	for in, want := range map[string]int{"754": 754, "12:34": 754, "1:02:03": 3723, "x": 0, "": 0, "1:2:3:4": 0} {
		if got := parseDuration(in); got != want {
			t.Errorf("duration %q = %d, want %d", in, got, want)
		}
	}
}
