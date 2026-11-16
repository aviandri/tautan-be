package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEmbedPosters(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir, s.cfg.PublicBaseURL = t.TempDir(), ""
	s.InlineImages, s.InlineRemote = true, true
	s.fetcher = newSafeFetcher(true)
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/vi/HASMAX1/maxresdefault.jpg", strings.HasPrefix(r.URL.Path, "/img/"):
			w.Write(demoJPEG(1280, 720, 30))
		case strings.HasSuffix(r.URL.Path, "/maxresdefault.jpg"):
			http.NotFound(w, r)
		case r.URL.Path == "/yt":
			fmt.Fprintf(w, `{"title":"Old video","author_name":"Chan","thumbnail_url":"%s/img/hq.jpg"}`, up.URL)
		case r.URL.Path == "/spotify":
			fmt.Fprintf(w, `{"title":"Song","thumbnail_url":"%s/img/cover.jpg"}`, up.URL)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	for _, kv := range []struct {
		p *string
		v string
	}{{&youtubeOEmbedURL, up.URL + "/yt"}, {&youtubeThumbBase, up.URL}, {&spotifyOEmbedURL, up.URL + "/spotify"}} {
		old := *kv.p
		*kv.p = kv.v
		t.Cleanup(func() { *kv.p = old })
	}
	h := s.Routes()
	f := setupPage(t, h, "embeds", true)
	mk := func(typ, u string) {
		createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": typ, "data": map[string]any{"url": u}})
	}
	mk("video", "https://www.youtube.com/watch?v=HASMAX1")
	mk("video", "https://youtu.be/NOMAX22")
	mk("music", "https://open.spotify.com/track/abc")
	mk("video", "https://www.dailymotion.com/video/x1")

	var page pageResponse
	do(t, h, "GET", "/api/v1/public/embeds", nil).expect(200).json(&page)
	var got []ResolvedMedia
	for _, b := range page.Blocks {
		var d struct {
			Resolved struct {
				Status string `json:"status"`
				ResolvedMedia
			} `json:"resolved"`
		}
		if b.Type == "video" || b.Type == "music" {
			json.Unmarshal(b.Data, &d)
			if d.Resolved.Status != "ok" {
				t.Errorf("%s status = %q (%s)", b.Type, d.Resolved.Status, b.Data)
			}
			got = append(got, d.Resolved.ResolvedMedia)
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %d embeds", len(got))
	}
	for i, want := range []struct{ provider, title string }{{"youtube", "Old video"}, {"youtube", "Old video"}, {"spotify", "Song"}, {"other", ""}} {
		g := got[i]
		if g.Provider != want.provider || g.Title != want.title || (want.provider != "other") != strings.HasPrefix(g.Poster, "/uploads/") {
			t.Errorf("embed %d = %+v", i, g)
		}
	}
	if got[0].VideoID != "HASMAX1" || page.Media[got[0].Poster].Width != 1280 {
		t.Errorf("maxres poster: %+v media %+v", got[0], page.Media[got[0].Poster])
	}
	if m := page.Media[got[1].Poster]; m.Width == 0 || len(m.Widths) == 0 {
		t.Errorf("fallback poster not in media with variants: %+v", m)
	}
	if strings.Contains(toJSON(page), up.URL) {
		t.Error("public payload hotlinks an upstream image")
	}
}

func TestYouTubeIDsAndChannels(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ": "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                "dQw4w9WgXcQ",
		"https://youtube.com/shorts/tPEE9ZwTmy0":      "tPEE9ZwTmy0",
		"https://www.youtube.com/embed/abcdefg":       "abcdefg",
		"https://www.youtube.com/@chan":               "",
		"https://vimeo.com/123":                       "",
	} {
		u, _ := url.Parse(raw)
		if got := youtubeID(u); got != want {
			t.Errorf("youtubeID(%s) = %q, want %q", raw, got, want)
		}
	}
	id := "UC_x5XG1OV2P6uZZ5FSM9Ttw"
	u, _ := url.Parse("https://www.youtube.com/@GoogleDevelopers")
	for _, body := range []string{
		`<link rel="canonical" href="https://www.youtube.com/channel/` + id + `">`,
		`<meta itemprop="identifier" content="` + id + `">`,
		`{"externalId":"` + id + `"}`,
	} {
		if got := channelIDFrom(u, []byte(body)); got != id {
			t.Errorf("channelIDFrom(%q) = %q", body, got)
		}
	}
	if !isYouTubeChannelPage(u) || channelIDFrom(u, []byte("nothing")) != "" {
		t.Error("channel page detection")
	}
	c, _ := url.Parse("https://youtube.com/channel/" + id + "/videos")
	if channelIDFrom(c, nil) != id {
		t.Error("channel id from path")
	}
	w, _ := url.Parse("https://www.youtube.com/watch?v=x")
	if isYouTubeChannelPage(w) {
		t.Error("watch page treated as channel")
	}
}
