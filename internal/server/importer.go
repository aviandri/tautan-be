package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"
)

const maxImportBytes = 2 << 20

// Hosts whose profile links become header socials rather than link blocks.
var socialPlatforms = map[string]string{
	"instagram.com": "instagram", "tiktok.com": "tiktok", "x.com": "x", "twitter.com": "x", "youtube.com": "youtube",
	"facebook.com": "facebook", "linkedin.com": "linkedin", "threads.net": "threads", "github.com": "github",
	"pinterest.com": "pinterest", "snapchat.com": "snapchat", "t.me": "telegram", "wa.me": "whatsapp",
	"discord.gg": "discord", "twitch.tv": "twitch", "bsky.app": "bluesky",
}

// Link-page hosts whose own navigation links should be ignored.
var linkPageHosts = set("linktr.ee", "linktree.com", "beacons.ai", "stan.store", "bio.link", "lnk.bio", "carrd.co", "tap.bio")

var skipLinkWords = []string{"privacy", "terms", "cookie", "login", "log in", "sign up", "signup", "report", "help center", "get started"}

type importLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type importPreview struct {
	SourceURL string `json:"source_url"`
	Profile   struct {
		DisplayName string `json:"display_name"`
		Bio         string `json:"bio"`
		AvatarURL   string `json:"avatar_url"`
	} `json:"profile"`
	Links   []importLink `json:"links"`
	Socials []SocialIcon `json:"socials"`
}

func (p *importPreview) addLink(title, raw string, page *url.URL) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return
	}
	if page != nil {
		u = page.ResolveReference(u)
	}
	if u.Scheme == "mailto" {
		if len(p.Socials) < 30 && !hasPlatform(p.Socials, "email") {
			p.Socials = append(p.Socials, SocialIcon{Platform: "email", URL: u.String()})
		}
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return
	}
	host := normalizeHost(strings.ToLower(u.Hostname()))
	if page != nil && (strings.EqualFold(u.Hostname(), page.Hostname()) || linkPageHosts[host]) {
		return // the link page's own navigation
	}
	title = clip(title, 100)
	lt := strings.ToLower(title)
	for _, w := range skipLinkWords {
		if strings.Contains(lt, w) {
			return
		}
	}
	u.Fragment = ""
	clean := u.String()
	for _, l := range p.Links {
		if l.URL == clean {
			return
		}
	}
	for _, s := range p.Socials {
		if s.URL == clean {
			return
		}
	}
	if platform, ok := socialPlatforms[host]; ok && isProfilePath(u.Path) {
		if !hasPlatform(p.Socials, platform) && len(p.Socials) < 30 {
			p.Socials = append(p.Socials, SocialIcon{Platform: platform, URL: clean})
			return
		}
	}
	if title == "" {
		title = host
	}
	if len(p.Links) < 50 {
		p.Links = append(p.Links, importLink{Title: title, URL: clean})
	}
}

// Path segments that point at content (a post or video), not a profile.
var contentSegments = set("watch", "shorts", "reel", "reels", "p", "status", "video", "videos", "playlist", "posts", "share", "live", "events", "embed")

// isProfilePath reports whether a social URL path looks like a profile (e.g.
// /alice, /@alice, /in/alice, /c/alice), not a single post.
func isProfilePath(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if segs[0] == "" || len(segs) > 2 {
		return false
	}
	for _, seg := range segs {
		if contentSegments[strings.ToLower(seg)] {
			return false
		}
	}
	return true
}

func hasPlatform(items []SocialIcon, platform string) bool {
	for _, s := range items {
		if s.Platform == platform {
			return true
		}
	}
	return false
}

// parseLinkPage extracts profile info, links and socials from a link-in-bio
// page: Linktree's embedded __NEXT_DATA__ when present, plus OG tags and anchors.
func parseLinkPage(body []byte, page *url.URL) importPreview {
	var p importPreview
	p.SourceURL = page.String()
	meta := parseMeta(body, page)
	p.Profile.DisplayName, p.Profile.Bio, p.Profile.AvatarURL = meta.Title, meta.Description, meta.ImageURL

	z := html.NewTokenizer(bytes.NewReader(body))
	var href, text string
	inA, inNext := false, false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "a":
				inA, href, text = true, "", ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = string(v)
					}
				}
			case "script":
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "id" && string(v) == "__NEXT_DATA__" {
						inNext = true
					}
				}
			}
		case html.TextToken:
			if inNext {
				applyNextData(&p, z.Text(), page)
				inNext = false
			} else if inA {
				text += " " + string(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" && inA {
				inA = false
				if href != "" {
					p.addLink(strings.TrimSpace(text), href, page)
				}
			}
		}
	}
	p.Profile.DisplayName = clip(strings.TrimSuffix(strings.TrimSuffix(p.Profile.DisplayName, " | Linktree"), " | Instagram, Facebook, TikTok | Linktree"), 80)
	p.Profile.Bio = clip(p.Profile.Bio, 500)
	if p.Links == nil {
		p.Links = []importLink{}
	}
	if p.Socials == nil {
		p.Socials = []SocialIcon{}
	}
	return p
}

// applyNextData reads Linktree's page props, which list links that may not
// be rendered as anchors in the server HTML.
func applyNextData(p *importPreview, raw []byte, page *url.URL) {
	var d struct {
		Props struct {
			PageProps struct {
				Account struct {
					PageTitle         string `json:"pageTitle"`
					Description       string `json:"description"`
					ProfilePictureURL string `json:"profilePictureUrl"`
				} `json:"account"`
				Links []struct {
					Title string `json:"title"`
					URL   string `json:"url"`
				} `json:"links"`
				SocialLinks []struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"socialLinks"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return
	}
	pp := d.Props.PageProps
	if pp.Account.PageTitle != "" {
		p.Profile.DisplayName = pp.Account.PageTitle
	}
	if pp.Account.Description != "" {
		p.Profile.Bio = pp.Account.Description
	}
	if img := absHTTP(page, pp.Account.ProfilePictureURL); img != "" {
		p.Profile.AvatarURL = img
	}
	for _, sl := range pp.SocialLinks {
		platform := strings.ToLower(nonUsernameChars.ReplaceAllString(strings.ToLower(sl.Type), ""))
		if platform == "twitter" {
			platform = "x"
		}
		if platformRE.MatchString(platform) && checkURL("url", sl.URL, true) == nil && !hasPlatform(p.Socials, platform) {
			p.Socials = append(p.Socials, SocialIcon{Platform: platform, URL: sl.URL})
		}
	}
	for _, l := range pp.Links {
		p.addLink(l.Title, l.URL, page)
	}
}

// rehostImage downloads a public image through the safe fetcher into uploads.
func (s *Server) rehostImage(ctx context.Context, raw string) (string, error) {
	res, err := s.fetcher.fetch(ctx, raw, maxUploadBytes, "image/*")
	if err != nil {
		return "", err
	}
	if res.Truncated {
		return "", errors.New("image too large")
	}
	ext, ok := uploadTypes[http.DetectContentType(res.Body)]
	if !ok {
		return "", errors.New("not a supported image")
	}
	return s.storeUpload(bytes.NewReader(res.Body), ext)
}

type importApplied struct {
	Links       int  `json:"links"`
	Socials     int  `json:"socials"`
	DisplayName bool `json:"display_name"`
	Bio         bool `json:"bio"`
	Avatar      bool `json:"avatar"`
}

// handleImport scrapes a public link page. By default it only previews; with
// apply=true it fills empty profile fields, sets socials if none exist, and
// appends link blocks to the given (or first) tab.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL   string `json:"url"`
		Apply bool   `json:"apply"`
		TabID string `json:"tab_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	raw := strings.TrimSpace(in.URL)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		writeError(w, http.StatusBadRequest, "invalid_url", "url must be a public http(s) link page")
		return
	}
	if !s.unfurlLimiter.Allow(userID(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
		return
	}
	ctx, pid := r.Context(), profileID(r)
	res, err := s.fetcher.fetch(ctx, u.String(), maxImportBytes, "text/html,application/xhtml+xml")
	switch {
	case errors.Is(err, errBlockedURL):
		writeError(w, http.StatusBadRequest, "blocked_url", "that address can't be fetched")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "fetch_failed", "couldn't load that page")
		return
	}
	preview := parseLinkPage(res.Body, res.URL)
	if !in.Apply {
		writeJSON(w, http.StatusOK, map[string]any{"preview": preview})
		return
	}

	var applied importApplied
	var avatarURL string
	if preview.Profile.AvatarURL != "" {
		// Best effort; a failed download just skips the avatar.
		if hosted, err := s.rehostImage(ctx, preview.Profile.AvatarURL); err == nil {
			avatarURL = hosted
		}
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var cur struct{ display, username, bio, avatar, socials string }
		if err := tx.QueryRow(ctx, `SELECT display_name, username, bio, avatar_url, socials::text FROM profiles WHERE id = $1 FOR UPDATE`, pid).
			Scan(&cur.display, &cur.username, &cur.bio, &cur.avatar, &cur.socials); err != nil {
			return err
		}
		if preview.Profile.DisplayName != "" && (cur.display == "" || cur.display == cur.username) {
			applied.DisplayName = true
			cur.display = preview.Profile.DisplayName
		}
		if preview.Profile.Bio != "" && cur.bio == "" {
			applied.Bio, cur.bio = true, preview.Profile.Bio
		}
		if avatarURL != "" && cur.avatar == "" {
			applied.Avatar, cur.avatar = true, avatarURL
		}
		socials := cur.socials
		if cur.socials == "[]" && len(preview.Socials) > 0 {
			enc, err := normalizeSocials(mustJSON(preview.Socials))
			if err == nil {
				socials, applied.Socials = string(enc), len(preview.Socials)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE profiles SET display_name = $2, bio = $3, avatar_url = $4, socials = $5::jsonb,
			onboarding = onboarding || '{"source": "import"}'::jsonb, updated_at = now() WHERE id = $1`,
			pid, cur.display, cur.bio, cur.avatar, socials); err != nil {
			return err
		}
		tabID := in.TabID
		if tabID == "" {
			if err := tx.QueryRow(ctx, `SELECT id FROM tabs WHERE profile_id = $1 ORDER BY position LIMIT 1`, pid).Scan(&tabID); err != nil {
				return err
			}
		} else if err := checkTab(ctx, tx, pid, tabID); err != nil {
			return err
		}
		for _, l := range preview.Links {
			data, err := validateBlockData("link", mustJSON(LinkData{Title: l.Title, URL: l.URL}))
			if err != nil {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO blocks (profile_id, tab_id, type, data, position)
				VALUES ($1, $2, 'link', $4, `+nextPositionSQL+`)`, pid, tabID, nil, data); err != nil {
				return err
			}
			applied.Links++
		}
		return nil
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	p, err := scanProfile(s.db.QueryRow(ctx, `SELECT `+profileCols+` FROM profiles WHERE id = $1`, pid))
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": preview, "applied": applied, "profile": p})
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
