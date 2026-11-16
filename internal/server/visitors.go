package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
)

// ---------- user agent ----------

// parseUA returns coarse browser and OS names. In-app browsers are reported as
// their app, since that's what owners care about.
func parseUA(ua string) (browser, os string) {
	l := strings.ToLower(ua)
	switch {
	case ua == "":
		browser = ""
	case strings.Contains(l, "instagram"):
		browser = "Instagram"
	case strings.Contains(l, "fban") || strings.Contains(l, "fbav") || strings.Contains(l, "fb_iab"):
		browser = "Facebook"
	case strings.Contains(l, "bytedancewebview") || strings.Contains(l, "musical_ly") || strings.Contains(l, "tiktok"):
		browser = "TikTok"
	case strings.Contains(l, "edg/") || strings.Contains(l, "edga/") || strings.Contains(l, "edgios/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/") && strings.Contains(l, "version/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	switch {
	case ua == "":
		os = ""
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		os = "iOS"
	case strings.Contains(l, "android"):
		os = "Android"
	case strings.Contains(l, "windows"):
		os = "Windows"
	case strings.Contains(l, "cros"):
		os = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		os = "macOS"
	case strings.Contains(l, "linux"):
		os = "Linux"
	default:
		os = "Other"
	}
	return
}

// ---------- referrer / channel ----------

var (
	socialHosts = set("instagram.com", "facebook.com", "fb.com", "t.co", "x.com", "twitter.com", "tiktok.com",
		"youtube.com", "youtu.be", "linkedin.com", "lnkd.in", "pinterest.com", "pin.it", "reddit.com", "threads.net",
		"snapchat.com", "whatsapp.com", "wa.me", "telegram.org", "t.me", "discord.com", "bsky.app", "tumblr.com")
	socialSources = set("instagram", "ig", "facebook", "fb", "twitter", "x", "tiktok", "youtube", "linkedin",
		"pinterest", "reddit", "threads", "snapchat", "whatsapp", "telegram", "discord", "bluesky")
	searchHosts = []string{"google.", "bing.com", "duckduckgo.com", "yahoo.", "yandex.", "baidu.com", "ecosia.org", "search.brave.com"}
)

// normalizeHost folds mobile/link-shim subdomains into the main site
// (l.instagram.com → instagram.com).
func normalizeHost(h string) string {
	for _, p := range []string{"www.", "m.", "l.", "lm.", "mobile.", "out."} {
		if strings.HasPrefix(h, p) && strings.Count(h, ".") >= 2 {
			h = strings.TrimPrefix(h, p)
		}
	}
	return h
}

// channelFor classifies a visit as social, search, email, qr, paid, referral or direct.
func channelFor(refHost, utmSource, utmMedium, browser string) string {
	src, med := strings.ToLower(utmSource), strings.ToLower(utmMedium)
	switch {
	case src == "qr" || med == "qr":
		return "qr"
	case med == "email" || med == "newsletter" || src == "newsletter":
		return "email"
	case med == "cpc" || med == "ppc" || med == "paid" || med == "paid_social":
		return "paid"
	case socialSources[src] || med == "social":
		return "social"
	}
	if refHost != "" {
		if socialHosts[refHost] {
			return "social"
		}
		for _, s := range searchHosts {
			if strings.HasPrefix(refHost, s) || strings.Contains(refHost, "."+s) {
				return "search"
			}
		}
		return "referral"
	}
	if browser == "Instagram" || browser == "Facebook" || browser == "TikTok" {
		return "social"
	}
	if src != "" {
		return "referral"
	}
	return "direct"
}

// utmFrom reads utm_* from the visitor's page URL; explicit values win.
func utmFrom(pageURL string, explicit [3]string) [3]string {
	out := explicit
	if u, err := url.Parse(pageURL); err == nil && pageURL != "" {
		q := u.Query()
		for i, k := range []string{"utm_source", "utm_medium", "utm_campaign"} {
			if out[i] == "" {
				out[i] = q.Get(k)
			}
		}
	}
	for i := range out {
		out[i] = strings.ToLower(strings.TrimSpace(out[i]))
		if len(out[i]) > 100 {
			out[i] = out[i][:100]
		}
	}
	return out
}

// ---------- visitor hash ----------

type saltCache struct {
	mu   sync.Mutex
	day  string
	salt []byte
}

// visitorHash is sha256(daily salt, profile, ip, ua), truncated. Salts rotate
// every UTC day and old ones are deleted, so visitors can't be tracked across days.
func (s *Server) visitorHash(ctx context.Context, pid, ip, ua string) (string, error) {
	salt, err := s.dailySalt(ctx)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(salt)
	for _, p := range []string{pid, ip, ua} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

func (s *Server) dailySalt(ctx context.Context) ([]byte, error) {
	day := s.now().UTC().Format("2006-01-02")
	s.salts.mu.Lock()
	defer s.salts.mu.Unlock()
	if s.salts.day == day {
		return s.salts.salt, nil
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO analytics_salts (day, salt) VALUES ($1, $2) ON CONFLICT (day) DO NOTHING`, day, fresh); err != nil {
		return nil, err
	}
	var salt []byte
	if err := s.db.QueryRow(ctx, `SELECT salt FROM analytics_salts WHERE day = $1`, day).Scan(&salt); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM analytics_salts WHERE day < $1::date - 1`, day); err != nil {
		return nil, err
	}
	s.salts.day, s.salts.salt = day, salt
	return salt, nil
}
