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
	// Product price from og:price / product:price tags, in minor units (P3-A).
	Price    *int64 `json:"price,omitempty"`
	Currency string `json:"currency,omitempty"`
	ogType   string
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
				case "og:type":
					m.ogType = strings.ToLower(content)
				case "product:price:amount", "og:price:amount":
					if m.Price == nil {
						m.Price = parsePriceMinor(content)
					}
				case "product:price:currency", "og:price:currency":
					if m.Currency == "" {
						m.Currency = strings.ToUpper(content)
					}
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

// marketplacePlatform maps a store or product URL host to a marketplace platform id.
func marketplacePlatform(u *url.URL) string {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	labels := strings.Split(host, ".")
	first := labels[0]
	if len(labels) > 2 && (first == "s" || first == "m" || first == "tk" || first == "vt") {
		first = labels[1] // s.lazada.co.id, m.tokopedia.com, tk.tokopedia.com
	}
	switch {
	case first == "shopee" || host == "shp.ee":
		return "shopee"
	case first == "tokopedia":
		return "tokopedia"
	case host == "shop.tiktok.com" || host == "seller-id.tiktok.com" ||
		(host == "tiktok.com" && (strings.Contains(u.Path, "/product/") || strings.HasPrefix(u.Path, "/view/product"))):
		return "tiktok_shop"
	case first == "lazada":
		return "lazada"
	case host == "blibli.com":
		return "blibli"
	case host == "bukalapak.com":
		return "bukalapak"
	case host == "etsy.com" || host == "etsy.me":
		return "etsy"
	case first == "amazon" || host == "amzn.to" || host == "amzn.asia" || host == "amzn.eu":
		return "amazon"
	case host == "gumroad.com" || strings.HasSuffix(host, ".gumroad.com") || host == "gum.co":
		return "gumroad"
	}
	return ""
}

// parsePriceMinor reads "85000", "85000.00" or "85,000.5" as minor units (2 decimals).
func parsePriceMinor(s string) *int64 {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(whole) > 13 || len(frac) > 2 {
		return nil
	}
	frac += strings.Repeat("0", 2-len(frac))
	var n int64
	for _, c := range whole + frac {
		if c < '0' || c > '9' {
			return nil
		}
		n = n*10 + int64(c-'0')
	}
	return &n
}

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
	Kind          string `json:"kind,omitempty"`     // "marketplace" for store/product URLs
	Platform      string `json:"platform,omitempty"` // marketplace platform id
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
	if p := marketplacePlatform(u); p != "" {
		out.Kind, out.Platform, out.SuggestedType = "marketplace", p, "marketplace"
	}
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
	if !currencies[out.Currency] {
		out.Price, out.Currency = nil, ""
	}
	if out.Kind == "marketplace" && (out.Price != nil || out.ogType == "product" || out.ogType == "og:product") {
		out.SuggestedType = "product"
	}
	writeJSON(w, http.StatusOK, out)
}
