package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Phase 3-B cards whose content comes from elsewhere: post_grid (oEmbed),
// latest_video (YouTube channel RSS) and podcast (podcast RSS). Visitors'
// browsers never call third parties: the server fetches through the SSRF-safe
// fetcher, re-hosts thumbnails as uploads (so they get AVIF/WebP variants),
// caches results in remote_cache, refreshes them on a schedule, and page reads
// merge the cache into data.resolved.

type (
	PostRef struct {
		URL      string `json:"url"`
		Platform string `json:"platform"` // set by the server from url
		Caption  string `json:"caption,omitempty"`
	}
	PostGridData struct {
		Title    string    `json:"title,omitempty"`
		Posts    []PostRef `json:"posts"`
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	LatestVideoShow struct {
		Count int `json:"count"` // 1-3
	}
	LatestVideoData struct {
		Title     string          `json:"title,omitempty"`
		ChannelID string          `json:"channel_id"`
		Show      LatestVideoShow `json:"show"`
		Schedule  *Schedule       `json:"schedule,omitempty"`
	}
	PodcastData struct {
		Title    string    `json:"title,omitempty"`
		FeedURL  string    `json:"feed_url"`
		Count    int       `json:"count"` // 1-10
		Schedule *Schedule `json:"schedule,omitempty"`
	}
)

var (
	remoteTypes = []string{"post_grid", "latest_video", "podcast"}
	channelRE   = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)

	// Upstream endpoints; tests point them at local servers.
	youtubeOEmbedURL   = "https://www.youtube.com/oembed"
	tiktokOEmbedURL    = "https://www.tiktok.com/oembed"
	instagramOEmbedURL = "https://graph.facebook.com/v21.0/instagram_oembed"
	youtubeFeedURL     = "https://www.youtube.com/feeds/videos.xml"
)

const (
	maxFeedBytes  = 2 << 20
	remoteTimeout = 20 * time.Second
)

func init() {
	for _, t := range remoteTypes {
		blockTypes[t] = true
	}
}

// postPlatform recognises Instagram, TikTok and YouTube post URLs.
func postPlatform(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	p := u.Path
	switch {
	case host == "instagram.com" && regexp.MustCompile(`^/(p|reel|reels|tv)/[A-Za-z0-9_-]+/?$`).MatchString(p):
		return "instagram"
	case host == "tiktok.com" && regexp.MustCompile(`^/@[A-Za-z0-9_.]+/(video|photo)/[0-9]+/?$`).MatchString(p),
		(host == "vm.tiktok.com" || host == "vt.tiktok.com") && len(p) > 1:
		return "tiktok"
	case host == "youtube.com" && (p == "/watch" && u.Query().Get("v") != "" || strings.HasPrefix(p, "/shorts/")),
		host == "youtu.be" && len(p) > 1:
		return "youtube"
	}
	return ""
}

func checkPostGrid(d *PostGridData) error {
	if d.Posts == nil {
		d.Posts = make([]PostRef, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("posts", len(d.Posts), 1, 12)); err != nil {
		return err
	}
	for i := range d.Posts {
		p := &d.Posts[i]
		f := fmt.Sprintf("posts[%d]", i)
		p.URL = strings.TrimSpace(p.URL)
		if p.Platform = postPlatform(p.URL); p.Platform == "" {
			return ferr(f+".url", "%s.url must be an Instagram, TikTok or YouTube post link", f)
		}
		if err := maxLen(f+".caption", p.Caption, 200); err != nil {
			return err
		}
	}
	return nil
}

func checkLatestVideo(d *LatestVideoData) error {
	if d.Show.Count == 0 {
		d.Show.Count = 1
	}
	if !channelRE.MatchString(d.ChannelID) {
		return ferr("channel_id", "channel_id must be a YouTube channel id (UC…, 24 characters)")
	}
	if d.Show.Count < 1 || d.Show.Count > 3 {
		return ferr("show.count", "show.count must be 1 to 3")
	}
	return maxLen("title", d.Title, 60)
}

func checkPodcast(d *PodcastData) error {
	if d.Count == 0 {
		d.Count = 3
	}
	if d.Count < 1 || d.Count > 10 {
		return ferr("count", "count must be 1 to 10")
	}
	return firstErr(maxLen("title", d.Title, 60), checkWebURL("feed_url", d.FeedURL, true))
}

func remoteBlock(typ string) (v any, check func() error, sched func() *Schedule, ok bool) {
	switch typ {
	case "post_grid":
		d := &PostGridData{}
		return d, func() error { return checkPostGrid(d) }, func() *Schedule { return d.Schedule }, true
	case "latest_video":
		d := &LatestVideoData{}
		return d, func() error { return checkLatestVideo(d) }, func() *Schedule { return d.Schedule }, true
	case "podcast":
		d := &PodcastData{}
		return d, func() error { return checkPodcast(d) }, func() *Schedule { return d.Schedule }, true
	}
	return nil, nil, nil, false
}

// remoteKeys lists the cache keys a block's content depends on.
func remoteKeys(typ string, data json.RawMessage) []string {
	switch typ {
	case "post_grid":
		var d PostGridData
		_ = json.Unmarshal(data, &d)
		keys := make([]string, 0, len(d.Posts))
		for _, p := range d.Posts {
			keys = append(keys, "post:"+p.URL)
		}
		return keys
	case "latest_video":
		var d LatestVideoData
		_ = json.Unmarshal(data, &d)
		return []string{"yt:" + d.ChannelID}
	case "podcast":
		var d PodcastData
		_ = json.Unmarshal(data, &d)
		return []string{"rss:" + d.FeedURL}
	}
	return nil
}

// ttl is how long a successful result is kept before refreshing; failures
// are retried after retry.
func remoteTTL(key string) (ttl, retry time.Duration) {
	if strings.HasPrefix(key, "post:") {
		return 7 * 24 * time.Hour, 6 * time.Hour
	}
	return time.Hour, 15 * time.Minute
}

// ---- resolvers ----

type ResolvedPost struct {
	Status   string `json:"status"` // ok | link_only | pending
	URL      string `json:"url"`
	Platform string `json:"platform"`
	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
	ThumbURL string `json:"thumb_url,omitempty"` // re-hosted under /uploads/
}

type ResolvedVideo struct {
	VideoID     string    `json:"video_id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	ThumbURL    string    `json:"thumb_url,omitempty"`
}

type ResolvedChannel struct {
	ChannelTitle string          `json:"channel_title"`
	Videos       []ResolvedVideo `json:"videos"`
}

type ResolvedEpisode struct {
	Title       string     `json:"title"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	AudioURL    string     `json:"audio_url"`
	AudioType   string     `json:"audio_type,omitempty"`
	DurationSec int        `json:"duration_sec,omitempty"`
	Link        string     `json:"link,omitempty"`
}

type ResolvedPodcast struct {
	Title    string            `json:"title"`
	ImageURL string            `json:"image_url,omitempty"` // re-hosted under /uploads/
	Episodes []ResolvedEpisode `json:"episodes"`
}

func (s *Server) resolveRemote(ctx context.Context, key string) (any, error) {
	kind, id, _ := strings.Cut(key, ":")
	switch kind {
	case "post":
		return s.resolvePost(ctx, id)
	case "yt":
		return s.resolveChannel(ctx, id)
	case "rss":
		return s.resolvePodcast(ctx, id)
	}
	return nil, fmt.Errorf("unknown remote key %q", key)
}

func (s *Server) resolvePost(ctx context.Context, postURL string) (ResolvedPost, error) {
	out := ResolvedPost{Status: "link_only", URL: postURL, Platform: postPlatform(postURL)}
	var endpoint string
	switch out.Platform {
	case "youtube":
		endpoint = youtubeOEmbedURL + "?format=json&url=" + url.QueryEscape(postURL)
	case "tiktok":
		endpoint = tiktokOEmbedURL + "?url=" + url.QueryEscape(postURL)
	case "instagram":
		if tok := os.Getenv("INSTAGRAM_OEMBED_TOKEN"); tok != "" {
			endpoint = instagramOEmbedURL + "?omitscript=true&url=" + url.QueryEscape(postURL) + "&access_token=" + url.QueryEscape(tok)
		}
	}
	var meta struct {
		Title        string `json:"title"`
		AuthorName   string `json:"author_name"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if endpoint != "" {
		res, err := s.fetcher.fetch(ctx, endpoint, 256<<10, "application/json")
		if err == nil && json.Unmarshal(res.Body, &meta) != nil {
			err = errors.New("oembed: invalid JSON")
		}
		if err != nil {
			return out, err
		}
	} else {
		// Instagram without an app token: try the page's OpenGraph tags, which
		// often fail (login wall); then the card degrades to a link.
		res, err := s.fetcher.fetch(ctx, postURL, maxHTMLBytes, "text/html")
		if err != nil {
			return out, nil
		}
		m := parseMeta(res.Body, res.URL)
		meta.Title, meta.ThumbnailURL = m.Title, m.ImageURL
	}
	out.Title, out.Author = clip(meta.Title, 200), clip(meta.AuthorName, 80)
	if meta.ThumbnailURL != "" {
		if thumb, err := s.cachedRehost(ctx, meta.ThumbnailURL); err == nil {
			out.ThumbURL, out.Status = thumb, "ok"
		}
	}
	return out, nil
}

// feedDecoder walks an RSS/Atom document element by element, so a feed cut at
// maxFeedBytes still yields the items before the cut.
func feedDecoder(body []byte) *xml.Decoder {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return dec
}

func (s *Server) resolveChannel(ctx context.Context, channelID string) (ResolvedChannel, error) {
	out := ResolvedChannel{Videos: []ResolvedVideo{}}
	res, err := s.fetcher.fetch(ctx, youtubeFeedURL+"?channel_id="+url.QueryEscape(channelID), maxFeedBytes, "application/atom+xml, application/xml")
	if err != nil {
		return out, err
	}
	type entry struct {
		VideoID   string `xml:"videoId"`
		Title     string `xml:"title"`
		Published string `xml:"published"`
		Group     struct {
			Thumbnail struct {
				URL string `xml:"url,attr"`
			} `xml:"thumbnail"`
		} `xml:"group"`
	}
	dec := feedDecoder(res.Body)
	depth := 0
	for len(out.Videos) < 3 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "title" && depth == 2 && out.ChannelTitle == "":
				var title string
				if dec.DecodeElement(&title, &t) == nil {
					out.ChannelTitle = clip(title, 120)
				}
				depth--
			case t.Name.Local == "entry":
				var e entry
				if dec.DecodeElement(&e, &t) != nil || e.VideoID == "" {
					depth--
					continue
				}
				depth--
				v := ResolvedVideo{VideoID: e.VideoID, Title: clip(e.Title, 200), URL: "https://www.youtube.com/watch?v=" + url.QueryEscape(e.VideoID)}
				v.PublishedAt, _ = time.Parse(time.RFC3339, e.Published)
				if e.Group.Thumbnail.URL != "" {
					if thumb, err := s.cachedRehost(ctx, e.Group.Thumbnail.URL); err == nil {
						v.ThumbURL = thumb
					}
				}
				out.Videos = append(out.Videos, v)
			}
		case xml.EndElement:
			depth--
		}
	}
	if len(out.Videos) == 0 && out.ChannelTitle == "" {
		return out, errors.New("youtube feed: no entries")
	}
	return out, nil
}

var pubDateLayouts = []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700", time.RFC3339}

func parsePubDate(v string) *time.Time {
	v = strings.TrimSpace(v)
	for _, l := range pubDateLayouts {
		if t, err := time.Parse(l, v); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// parseDuration reads itunes:duration: seconds, MM:SS or HH:MM:SS.
func parseDuration(v string) int {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) > 3 {
		return 0
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.SplitN(p, ".", 2)[0])
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

func (s *Server) resolvePodcast(ctx context.Context, feedURL string) (ResolvedPodcast, error) {
	out := ResolvedPodcast{Episodes: []ResolvedEpisode{}}
	res, err := s.fetcher.fetch(ctx, feedURL, maxFeedBytes, "application/rss+xml, application/xml, text/xml")
	if err != nil {
		return out, err
	}
	type item struct {
		Title     string `xml:"title"`
		PubDate   string `xml:"pubDate"`
		Link      string `xml:"link"`
		Duration  string `xml:"duration"`
		Enclosure struct {
			URL  string `xml:"url,attr"`
			Type string `xml:"type,attr"`
		} `xml:"enclosure"`
	}
	var imageSrc string
	dec := feedDecoder(res.Body)
	depth := 0
	for len(out.Episodes) < 10 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 3 && t.Name.Local == "title" && out.Title == "": // rss > channel > title
				var title string
				if dec.DecodeElement(&title, &t) == nil {
					out.Title = clip(title, 120)
				}
				depth--
			case depth == 3 && t.Name.Local == "image":
				var img struct {
					URL  string `xml:"url"`
					Href string `xml:"href,attr"`
				}
				if dec.DecodeElement(&img, &t) == nil {
					if img.Href != "" { // itunes:image wins over the RSS <image>
						imageSrc = strings.TrimSpace(img.Href)
					} else if imageSrc == "" {
						imageSrc = strings.TrimSpace(img.URL)
					}
				}
				depth--
			case t.Name.Local == "item":
				var it item
				err := dec.DecodeElement(&it, &t)
				depth--
				if err != nil || checkWebURL("audio", it.Enclosure.URL, true) != nil {
					continue
				}
				ep := ResolvedEpisode{Title: clip(it.Title, 200), AudioURL: strings.TrimSpace(it.Enclosure.URL),
					AudioType: clip(it.Enclosure.Type, 40), DurationSec: parseDuration(it.Duration), PublishedAt: parsePubDate(it.PubDate)}
				if checkWebURL("link", it.Link, true) == nil {
					ep.Link = strings.TrimSpace(it.Link)
				}
				out.Episodes = append(out.Episodes, ep)
			}
		case xml.EndElement:
			depth--
		}
	}
	if out.Title == "" && len(out.Episodes) == 0 {
		return out, errors.New("podcast feed: no channel or episodes")
	}
	if imageSrc != "" {
		if img, err := s.cachedRehost(ctx, imageSrc); err == nil {
			out.ImageURL = img
		}
	}
	return out, nil
}

// cachedRehost re-hosts a remote image once and remembers the upload URL.
func (s *Server) cachedRehost(ctx context.Context, src string) (string, error) {
	key := "img:" + src
	var local string
	if err := s.db.QueryRow(ctx, `SELECT data->>'url' FROM remote_cache WHERE key = $1 AND ok`, key).Scan(&local); err == nil && local != "" {
		return local, nil
	}
	local, err := s.rehostImage(ctx, src)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO remote_cache (key, data, ok, fetched_at, next_at) VALUES ($1, $2, true, now(), 'infinity')
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, ok = true, fetched_at = now()`, key, map[string]string{"url": local})
	return local, err
}

// ---- cache refresh ----

type remoteWorker struct {
	once  sync.Once
	queue chan string
}

// queueMissingRemote queues a saved block's keys that have never been fetched.
func (s *Server) queueMissingRemote(ctx context.Context, typ string, data json.RawMessage) {
	keys := remoteKeys(typ, data)
	if len(keys) == 0 {
		return
	}
	rows, err := s.db.Query(ctx, `SELECT k FROM unnest($1::text[]) k WHERE NOT EXISTS (SELECT 1 FROM remote_cache c WHERE c.key = k)`, keys)
	if err != nil {
		return
	}
	if missing, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
		s.enqueueRemote(missing...)
	}
}

func (s *Server) enqueueRemote(keys ...string) {
	for _, k := range keys {
		if s.InlineRemote {
			if err := s.refreshRemote(context.Background(), k); err != nil {
				log.Printf("remote %s: %v", k, err)
			}
			continue
		}
		s.remote.once.Do(func() {
			s.remote.queue = make(chan string, 1024)
			go func() {
				for k := range s.remote.queue {
					if err := s.refreshRemote(context.Background(), k); err != nil {
						log.Printf("remote %s: %v", k, err)
					}
				}
			}()
		})
		select {
		case s.remote.queue <- k:
		default: // the scheduled refresher will pick it up
		}
	}
}

// refreshRemote fetches one key. Successful results replace the cache; on a
// failure the last good result is kept and retried later.
func (s *Server) refreshRemote(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	ttl, retry := remoteTTL(key)
	v, err := s.resolveRemote(ctx, key)
	now := s.now()
	if err != nil {
		// Posts degrade to a link card at once; feeds stay pending. A last good
		// result is never overwritten by a failure.
		fallback := json.RawMessage("null")
		if strings.HasPrefix(key, "post:") && v != nil {
			fallback, _ = json.Marshal(v)
		}
		_, dbErr := s.db.Exec(ctx, `INSERT INTO remote_cache (key, data, ok, error, fetched_at, next_at) VALUES ($1, $2, false, $3, $4, $5)
			ON CONFLICT (key) DO UPDATE SET error = EXCLUDED.error, next_at = EXCLUDED.next_at,
				data = CASE WHEN remote_cache.ok THEN remote_cache.data ELSE EXCLUDED.data END`,
			key, fallback, clip(err.Error(), 300), now, now.Add(retry))
		return errors.Join(err, dbErr)
	}
	data, _ := json.Marshal(v)
	var changed bool
	if err := s.db.QueryRow(ctx, `INSERT INTO remote_cache (key, data, ok, error, fetched_at, next_at) VALUES ($1, $2, true, '', $3, $4)
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, ok = true, error = '', fetched_at = EXCLUDED.fetched_at, next_at = EXCLUDED.next_at
		RETURNING (xmax = 0) OR remote_cache.data IS DISTINCT FROM $2::jsonb`, key, data, now, now.Add(ttl)).Scan(&changed); err != nil {
		return err
	}
	if changed {
		_, id, _ := strings.Cut(key, ":")
		s.revalidateReferencing(ctx, id)
	}
	return nil
}

// revalidateReferencing queues published pages whose blocks mention needle.
func (s *Server) revalidateReferencing(ctx context.Context, needle string) {
	if s.cfg.RevalidateSecret == "" {
		return
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT p.username::text FROM profiles p JOIN blocks b ON b.profile_id = p.id
		WHERE p.is_published AND b.deleted_at IS NULL AND strpos(b.data::text, $1) > 0`, needle)
	if err != nil {
		return
	}
	if names, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
		s.revalidate(names...)
	}
}

// RefreshRemoteDue refreshes every key used by a live block that is missing
// or past its next_at. Run hourly-ish by RunRemoteRefresher.
func (s *Server) RefreshRemoteDue(ctx context.Context) (int, error) {
	rows, err := s.db.Query(ctx, `SELECT type, data FROM blocks WHERE type = ANY($1) AND deleted_at IS NULL`, remoteTypes)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for rows.Next() {
		var typ string
		var data json.RawMessage
		if err := rows.Scan(&typ, &data); err != nil {
			rows.Close()
			return 0, err
		}
		for _, k := range remoteKeys(typ, data) {
			want[k] = true
		}
	}
	rows.Close()
	if len(want) == 0 {
		return 0, nil
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	rows, err = s.db.Query(ctx, `SELECT k FROM unnest($1::text[]) k LEFT JOIN remote_cache c ON c.key = k
		WHERE c.key IS NULL OR c.next_at <= $2`, keys, s.now())
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, k := range due {
		if ctx.Err() != nil {
			break
		}
		if err := s.refreshRemote(ctx, k); err != nil {
			log.Printf("remote %s: %v", k, err)
		}
	}
	return len(due), nil
}

// RunRemoteRefresher checks for due remote content every 10 minutes until ctx ends.
func (s *Server) RunRemoteRefresher(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if _, err := s.RefreshRemoteDue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("remote refresh: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- reads ----

// attachRemote adds data.resolved to remote cards from the cache.
func (s *Server) attachRemote(ctx context.Context, blocks []Block) error {
	var keys []string
	for _, b := range blocks {
		keys = append(keys, remoteKeys(b.Type, b.Data)...)
	}
	if len(keys) == 0 {
		return nil
	}
	cache := map[string]json.RawMessage{}
	rows, err := s.db.Query(ctx, `SELECT key, data, fetched_at FROM remote_cache WHERE key = ANY($1) AND data <> 'null'`, keys)
	if err != nil {
		return err
	}
	fetched := map[string]time.Time{}
	for rows.Next() {
		var k string
		var d json.RawMessage
		var at time.Time
		if err := rows.Scan(&k, &d, &at); err != nil {
			rows.Close()
			return err
		}
		cache[k], fetched[k] = d, at
	}
	rows.Close()
	for i := range blocks {
		b := &blocks[i]
		var resolved any
		switch b.Type {
		case "post_grid":
			var d PostGridData
			_ = json.Unmarshal(b.Data, &d)
			posts := make([]json.RawMessage, len(d.Posts))
			for j, p := range d.Posts {
				if raw, ok := cache["post:"+p.URL]; ok {
					posts[j] = raw
				} else {
					posts[j], _ = json.Marshal(ResolvedPost{Status: "pending", URL: p.URL, Platform: p.Platform})
				}
			}
			resolved = map[string]any{"posts": posts}
		case "latest_video":
			var d LatestVideoData
			_ = json.Unmarshal(b.Data, &d)
			resolved = resolvedOrPending(cache, fetched, "yt:"+d.ChannelID, func(raw json.RawMessage) any {
				var c ResolvedChannel
				_ = json.Unmarshal(raw, &c)
				c.Videos = c.Videos[:min(len(c.Videos), d.Show.Count)]
				return c
			})
		case "podcast":
			var d PodcastData
			_ = json.Unmarshal(b.Data, &d)
			resolved = resolvedOrPending(cache, fetched, "rss:"+d.FeedURL, func(raw json.RawMessage) any {
				var p ResolvedPodcast
				_ = json.Unmarshal(raw, &p)
				p.Episodes = p.Episodes[:min(len(p.Episodes), d.Count)]
				return p
			})
		default:
			continue
		}
		var m map[string]any
		if json.Unmarshal(b.Data, &m) != nil {
			continue
		}
		m["resolved"] = resolved
		if out, err := json.Marshal(m); err == nil {
			b.Data = out
		}
	}
	return nil
}

func resolvedOrPending(cache map[string]json.RawMessage, fetched map[string]time.Time, key string, trim func(json.RawMessage) any) any {
	raw, ok := cache[key]
	if !ok {
		return map[string]any{"status": "pending"}
	}
	b, _ := json.Marshal(trim(raw))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["status"], m["fetched_at"] = "ok", fetched[key].UTC()
	return m
}
