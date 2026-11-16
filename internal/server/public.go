package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// publishedProfileID returns the id of a published profile, or ErrNoRows.
func (s *Server) publishedProfileID(ctx context.Context, username string) (string, error) {
	var pid string
	err := s.db.QueryRow(ctx, `SELECT id FROM profiles WHERE username = $1 AND is_published`, strings.ToLower(username)).Scan(&pid)
	return pid, err
}

func (s *Server) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	pid, err := s.publishedProfileID(r.Context(), chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	page, err := s.loadPage(r.Context(), pid, true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, page)
}

var (
	botRE     = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|facebookexternalhit|preview|headless`)
	tabletRE  = regexp.MustCompile(`(?i)ipad|tablet|kindle|silk`)
	mobileRE  = regexp.MustCompile(`(?i)mobi|iphone|ipod|android|windows phone`)
	countryRE = regexp.MustCompile(`^[A-Z]{2}$`)
)

func deviceFromUA(ua string) string {
	switch {
	case ua == "":
		return "unknown"
	case tabletRE.MatchString(ua), strings.Contains(strings.ToLower(ua), "android") && !strings.Contains(strings.ToLower(ua), "mobile"):
		return "tablet"
	case mobileRE.MatchString(ua):
		return "mobile"
	default:
		return "desktop"
	}
}

// referrerHost reduces a referrer URL to its host ("" means direct). Referrers
// from our own front end or API count as direct.
func (s *Server) referrerHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	for _, o := range append([]string{s.cfg.PublicBaseURL}, s.cfg.CORSOrigins...) {
		if ou, err := url.Parse(o); err == nil && ou.Host != "" && strings.EqualFold(ou.Host, u.Host) {
			return ""
		}
	}
	if len(host) > 255 {
		host = host[:255]
	}
	return host
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handlePublicEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind        string  `json:"kind"`
		BlockID     *string `json:"block_id"`
		Referrer    *string `json:"referrer"`
		PageURL     string  `json:"page_url"`
		UTMSource   string  `json:"utm_source"`
		UTMMedium   string  `json:"utm_medium"`
		UTMCampaign string  `json:"utm_campaign"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Kind != "view" && in.Kind != "click" {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be view or click")
		return
	}
	if in.Kind == "click" && (in.BlockID == nil || *in.BlockID == "") {
		writeError(w, http.StatusBadRequest, "invalid_block", "click events need a block_id")
		return
	}
	ctx := r.Context()
	pid, err := s.publishedProfileID(ctx, chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	ua := r.UserAgent()
	// Bots and floods are accepted but not recorded, so clients never retry.
	if botRE.MatchString(ua) || !s.eventLimiter.Allow(clientIP(r)+"|"+pid) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var blockID *string
	if in.Kind == "click" {
		var ok bool
		err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE id = $1 AND profile_id = $2 AND deleted_at IS NULL)`, *in.BlockID, pid).Scan(&ok)
		if isInvalidUUID(err) || (err == nil && !ok) {
			writeError(w, http.StatusBadRequest, "invalid_block", "block not found on this page")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		blockID = in.BlockID
	}
	ref := r.Referer()
	if in.Referrer != nil {
		ref = *in.Referrer // the browser page's document.referrer: the visitor's real source
	}
	country := strings.ToUpper(strings.TrimSpace(r.Header.Get(s.cfg.CountryHeader)))
	if !countryRE.MatchString(country) || country == "XX" {
		country = ""
	}
	browser, osName := parseUA(ua)
	host := normalizeHost(s.referrerHost(ref))
	utm := utmFrom(in.PageURL, [3]string{in.UTMSource, in.UTMMedium, in.UTMCampaign})
	vh, err := s.visitorHash(ctx, pid, clientIP(r), ua)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if _, err := s.db.Exec(ctx,
		`INSERT INTO events (profile_id, block_id, kind, referrer, country, device, visitor_hash, browser, os, utm_source, utm_medium, utm_campaign, channel, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		pid, blockID, in.Kind, host, country, deviceFromUA(ua), vh, browser, osName,
		utm[0], utm[1], utm[2], channelFor(host, utm[0], utm[1], browser), s.now()); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePublicContact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BlockID string `json:"block_id"`
		Name    string `json:"name"`
		Email   string `json:"email"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name, in.Email, in.Message = strings.TrimSpace(in.Name), strings.TrimSpace(in.Email), strings.TrimSpace(in.Message)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 100:
		writeError(w, http.StatusBadRequest, "invalid_name", "name is required (max 100 chars)")
		return
	case !validEmail(in.Email):
		writeError(w, http.StatusBadRequest, "invalid_email", "email address is invalid")
		return
	case in.Message == "" || len([]rune(in.Message)) > 2000:
		writeError(w, http.StatusBadRequest, "invalid_message", "message is required (max 2000 chars)")
		return
	}
	ctx := r.Context()
	pid, err := s.publishedProfileID(ctx, chi.URLParam(r, "username"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !s.contactLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many messages, try again later")
		return
	}
	var ok bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blocks WHERE id = $1 AND profile_id = $2 AND type = 'contact_form' AND is_visible AND deleted_at IS NULL)`,
		in.BlockID, pid).Scan(&ok)
	if isInvalidUUID(err) || (err == nil && !ok) {
		writeError(w, http.StatusBadRequest, "invalid_block", "contact form not found on this page")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	var id string
	var created time.Time
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cid, err := upsertContact(ctx, tx, pid, in.Email, in.Name, false, in.BlockID, s.now())
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO contact_submissions (profile_id, block_id, name, email, message, contact_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
			pid, in.BlockID, in.Name, in.Email, in.Message, cid).Scan(&id, &created)
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "created_at": created})
}

func validEmail(e string) bool {
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e && len(e) <= 254
}

type ContactSubmission struct {
	ID        string    `json:"id"`
	ProfileID string    `json:"profile_id"`
	BlockID   *string   `json:"block_id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

const contactPageSize = 20

// handleListContacts pages newest first with an opaque (created_at, id) cursor.
func (s *Server) handleListContacts(w http.ResponseWriter, r *http.Request) {
	ctx, pid := r.Context(), profileID(r)
	args := []any{pid, contactPageSize + 1}
	where := `profile_id = $1`
	if c := r.URL.Query().Get("cursor"); c != "" {
		ts, id, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		args = append(args, ts, id)
		where += ` AND (created_at, id) < ($3, $4)`
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, profile_id, block_id, name, email, message, created_at FROM contact_submissions
		 WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ContactSubmission, error) {
		var c ContactSubmission
		err := r.Scan(&c.ID, &c.ProfileID, &c.BlockID, &c.Name, &c.Email, &c.Message, &c.CreatedAt)
		return c, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	var next *string
	if len(items) > contactPageSize {
		items = items[:contactPageSize]
		last := items[len(items)-1]
		c := encodeCursor(last.CreatedAt, last.ID)
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, ok := strings.Cut(string(b), "|")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil || len(id) != 36 {
		return time.Time{}, "", false
	}
	return t, id, true
}
