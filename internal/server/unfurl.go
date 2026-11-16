package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const maxHTMLBytes = 1 << 20

type pageMeta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	ImageURL    string `json:"image_url"`
	SiteName    string `json:"site_name"`
}

// parseMeta reads OpenGraph/Twitter/HTML metadata from the document head.
func parseMeta(body []byte, base *url.URL) pageMeta {
	var m pageMeta
	var htmlTitle, desc, twTitle, twDesc, twImage string
	z := html.NewTokenizer(bytes.NewReader(body))
	inTitle := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			goto done
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "title":
				inTitle = tt == html.StartTagToken
			case "body":
				goto done
			case "meta":
				var key, content string
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch strings.ToLower(string(k)) {
					case "property", "name":
						if key == "" {
							key = strings.ToLower(string(v))
						}
					case "content":
						content = strings.TrimSpace(string(v))
					}
				}
				switch key {
				case "og:title":
					m.Title = content
				case "og:description":
					m.Description = content
				case "og:image", "og:image:url", "og:image:secure_url":
					if m.ImageURL == "" {
						m.ImageURL = content
					}
				case "og:site_name":
					m.SiteName = content
				case "twitter:title":
					twTitle = content
				case "twitter:description":
					twDesc = content
				case "twitter:image", "twitter:image:src":
					twImage = content
				case "description":
					desc = content
				}
			}
		case html.TextToken:
			if inTitle && htmlTitle == "" {
				htmlTitle = strings.TrimSpace(string(z.Text()))
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = false
			} else if string(name) == "head" {
				goto done
			}
		}
	}
done:
	m.Title = clip(firstNonEmpty(m.Title, twTitle, htmlTitle), 200)
	m.Description = clip(firstNonEmpty(m.Description, twDesc, desc), 300)
	m.SiteName = clip(m.SiteName, 100)
	m.ImageURL = absHTTP(base, firstNonEmpty(m.ImageURL, twImage))
	return m
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

// absHTTP resolves ref against base and returns it only if it is http(s).
func absHTTP(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

var (
	videoHosts = set("youtube.com", "youtu.be", "vimeo.com", "tiktok.com", "twitch.tv", "dailymotion.com")
	musicHosts = set("open.spotify.com", "spotify.com", "soundcloud.com", "music.apple.com", "deezer.com", "tidal.com", "audiomack.com")
	imageExts  = set(".png", ".jpg", ".jpeg", ".gif", ".webp")
)

// classifyURL suggests a block type from the URL alone.
func classifyURL(u *url.URL) string {
	host := normalizeHost(strings.ToLower(u.Hostname()))
	switch {
	case videoHosts[host]:
		return "video"
	case musicHosts[host] || strings.HasSuffix(host, ".bandcamp.com"):
		return "music"
	case bookingHosts[host]:
		return "booking"
	case imageExts[strings.ToLower(path.Ext(u.Path))]:
		return "image"
	}
	return "link"
}

type unfurlResult struct {
	URL           string `json:"url"`
	SuggestedType string `json:"suggested_type"`
	DestKind      string `json:"dest_kind,omitempty"`
	Fetched       bool   `json:"fetched"`
	pageMeta
}

// handleUnfurl turns pasted text into block suggestions: emails become mailto
// links, media/booking URLs map to embed types, other URLs are scraped for OG data.
func (s *Server) handleUnfurl(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	raw := strings.TrimSpace(in.URL)
	if raw == "" || len(raw) > 2048 {
		writeError(w, http.StatusBadRequest, "invalid_url", "url is required")
		return
	}
	if !s.unfurlLimiter.Allow(userID(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
		return
	}
	if addr := strings.TrimPrefix(raw, "mailto:"); !strings.Contains(addr, "/") && strings.Contains(addr, "@") {
		if !validEmail(addr) {
			writeError(w, http.StatusBadRequest, "invalid_url", "not a valid URL or email address")
			return
		}
		writeJSON(w, http.StatusOK, unfurlResult{URL: "mailto:" + addr, SuggestedType: "link", DestKind: "email", pageMeta: pageMeta{Title: addr}})
		return
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || !strings.Contains(u.Hostname(), ".") && !s.fetcher.allowLoopback {
		writeError(w, http.StatusBadRequest, "invalid_url", "not a valid http(s) URL")
		return
	}
	out := unfurlResult{URL: u.String(), SuggestedType: classifyURL(u)}
	if out.SuggestedType == "image" {
		out.ImageURL = out.URL
	}
	res, err := s.fetcher.fetch(r.Context(), u.String(), maxHTMLBytes, "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	switch {
	case errors.Is(err, errBlockedURL):
		writeError(w, http.StatusBadRequest, "blocked_url", "that address can't be fetched")
		return
	case err != nil:
		// Unreachable or slow sites still get a suggestion, just without metadata.
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Fetched = true
	switch {
	case strings.HasPrefix(res.ContentType, "image/"):
		out.SuggestedType, out.ImageURL = "image", res.URL.String()
	case strings.Contains(res.ContentType, "html") || res.ContentType == "":
		out.pageMeta = parseMeta(res.Body, res.URL)
	}
	writeJSON(w, http.StatusOK, out)
}
