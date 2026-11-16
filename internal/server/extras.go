package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// ---------- announcement ----------

type AnnouncementCTA struct {
	Label       string `json:"label"`
	URL         string `json:"url,omitempty"`
	FormBlockID string `json:"form_block_id,omitempty"` // opens a form block instead of a link
}

type Announcement struct {
	ID          string           `json:"id"` // server-assigned; changes only when re-created, so dismissals stick across edits
	Layout      string           `json:"layout"`
	Title       string           `json:"title"`
	Description string           `json:"description,omitempty"`
	ImageURL    string           `json:"image_url,omitempty"`
	CTA         *AnnouncementCTA `json:"cta,omitempty"`
	BgColor     string           `json:"bg_color,omitempty"`
	TextColor   string           `json:"text_color,omitempty"`
	Schedule    *Schedule        `json:"schedule,omitempty"`
}

var announcementLayouts = set("banner", "popup", "card")

// normalizeAnnouncement validates an announcement; prevID keeps the id stable on edits.
func normalizeAnnouncement(raw json.RawMessage, prevID string) (json.RawMessage, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var a Announcement
	if !isJSONObject(raw) || json.Unmarshal(raw, &a) != nil {
		return nil, fmt.Errorf("announcement must be an object or null")
	}
	if a.Layout == "" {
		a.Layout = "banner"
	}
	errs := []error{
		oneOf("layout", a.Layout, announcementLayouts), required("title", a.Title), maxLen("title", a.Title, 100),
		maxLen("description", a.Description, 204), checkURL("image_url", a.ImageURL, false),
		checkColor("bg_color", a.BgColor), checkColor("text_color", a.TextColor), checkSchedule(a.Schedule),
	}
	if c := a.CTA; c != nil {
		errs = append(errs, required("cta.label", c.Label), maxLen("cta.label", c.Label, 40))
		switch {
		case c.FormBlockID != "":
			if !uuidRE.MatchString(c.FormBlockID) {
				errs = append(errs, fmt.Errorf("cta.form_block_id must be a block id"))
			}
			c.URL = ""
		default:
			errs = append(errs, checkURL("cta.url", c.URL, true))
		}
	}
	if err := firstErr(errs...); err != nil {
		return nil, err
	}
	a.ID = prevID
	if a.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		a.ID = hex.EncodeToString(b)
	}
	return json.Marshal(a)
}

// ---------- settings ----------

type PageSettings struct {
	GA4ID           string `json:"ga4_id,omitempty"`
	MetaPixelID     string `json:"meta_pixel_id,omitempty"`
	Language        string `json:"language,omitempty"`
	ShowShare       bool   `json:"show_share,omitempty"`
	ShowSaveContact bool   `json:"show_save_contact,omitempty"`
	ContactEmail    string `json:"contact_email,omitempty"` // used in the vCard
	ContactPhone    string `json:"contact_phone,omitempty"`
}

var (
	ga4RE   = regexp.MustCompile(`^G-[A-Z0-9]{4,15}$`)
	pixelRE = regexp.MustCompile(`^[0-9]{6,20}$`)
	langRE  = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
)

// normalizeSettings validates the full settings object (unknown keys dropped:
// these values end up inside analytics scripts, so only known shapes are kept).
func normalizeSettings(raw json.RawMessage) (json.RawMessage, error) {
	var st PageSettings
	if !isJSONObject(raw) || json.Unmarshal(raw, &st) != nil {
		return nil, fmt.Errorf("settings must be an object")
	}
	st.GA4ID = strings.ToUpper(strings.TrimSpace(st.GA4ID))
	st.MetaPixelID = strings.TrimSpace(st.MetaPixelID)
	st.ContactEmail = strings.TrimSpace(st.ContactEmail)
	st.ContactPhone = strings.TrimSpace(st.ContactPhone)
	switch {
	case st.GA4ID != "" && !ga4RE.MatchString(st.GA4ID):
		return nil, fmt.Errorf("ga4_id must look like G-XXXXXXX")
	case st.MetaPixelID != "" && !pixelRE.MatchString(st.MetaPixelID):
		return nil, fmt.Errorf("meta_pixel_id must be digits")
	case st.Language != "" && !langRE.MatchString(st.Language):
		return nil, fmt.Errorf("language must be a code like id, en or pt-BR")
	case st.ContactEmail != "" && !validEmail(st.ContactEmail):
		return nil, fmt.Errorf("contact_email is invalid")
	case st.ContactPhone != "" && !phoneRE.MatchString(st.ContactPhone):
		return nil, fmt.Errorf("contact_phone is invalid")
	}
	return json.Marshal(st)
}

// ---------- username redirects ----------

const maxRedirectsPerProfile = 5

// recordRename keeps old → profile as a redirect (unless old was temporary) and
// frees any redirect the profile is now reclaiming.
func recordRename(ctx context.Context, tx pgx.Tx, pid, oldName, newName string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM username_redirects WHERE old_username = $1 AND profile_id = $2`, newName, pid); err != nil {
		return err
	}
	if oldName == newName || strings.HasPrefix(oldName, tempUsernamePrefix) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO username_redirects (old_username, profile_id) VALUES ($1, $2)
		ON CONFLICT (old_username) DO UPDATE SET profile_id = EXCLUDED.profile_id, created_at = now()`, oldName, pid); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM username_redirects WHERE profile_id = $1 AND old_username NOT IN (
		SELECT old_username FROM username_redirects WHERE profile_id = $1 ORDER BY created_at DESC LIMIT $2)`, pid, maxRedirectsPerProfile)
	return err
}

// redirectTarget returns the current username for a retired one, if its page is published.
func (s *Server) redirectTarget(ctx context.Context, old string) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `SELECT p.username FROM username_redirects r JOIN profiles p ON p.id = r.profile_id
		WHERE r.old_username = $1 AND p.is_published`, strings.ToLower(old)).Scan(&name)
	return name, err
}

// ---------- vCard ----------

func vcardEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ",", `\,`, ";", `\;`, "\r\n", `\n`, "\n", `\n`, "\r", "").Replace(s)
}

// vcardFold folds a content line at 75 octets (RFC 6350 §3.2) without splitting UTF-8 runes.
func vcardFold(line string) string {
	var b strings.Builder
	n := 0
	for _, r := range line {
		size := len(string(r))
		if n+size > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	b.WriteString("\r\n")
	return b.String()
}

func (s *Server) handleVCard(w http.ResponseWriter, r *http.Request) {
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
	var p Profile
	var settings json.RawMessage
	err = s.db.QueryRow(ctx, `SELECT username, display_name, bio, avatar_url, socials, settings FROM profiles WHERE id = $1`, pid).
		Scan(&p.Username, &p.DisplayName, &p.Bio, &p.AvatarURL, &p.Socials, &settings)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var st PageSettings
	_ = json.Unmarshal(settings, &st)
	name := p.DisplayName
	if name == "" {
		name = p.Username
	}
	lines := []string{"BEGIN:VCARD", "VERSION:3.0", "FN:" + vcardEscape(name), "N:;" + vcardEscape(name) + ";;;"}
	if p.Bio != "" {
		lines = append(lines, "NOTE:"+vcardEscape(p.Bio))
	}
	lines = append(lines, "URL:"+s.cfg.AppBaseURL+"/"+p.Username)
	if st.ContactEmail != "" {
		lines = append(lines, "EMAIL;TYPE=INTERNET:"+vcardEscape(st.ContactEmail))
	}
	if st.ContactPhone != "" {
		lines = append(lines, "TEL;TYPE=CELL:"+vcardEscape(st.ContactPhone))
	}
	if p.AvatarURL != nil && *p.AvatarURL != "" {
		lines = append(lines, s.vcardPhoto(*p.AvatarURL))
	}
	var socials []SocialIcon
	_ = json.Unmarshal(p.Socials, &socials)
	for _, so := range socials {
		if so.URL != "" && so.Platform != "email" {
			lines = append(lines, "X-SOCIALPROFILE;TYPE="+so.Platform+":"+vcardEscape(so.URL))
		}
	}
	lines = append(lines, "REV:"+s.now().UTC().Format("20060102T150405Z"), "END:VCARD")
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.vcf"`, p.Username))
	for _, l := range lines {
		_, _ = w.Write([]byte(vcardFold(l)))
	}
}

// vcardPhoto embeds a locally stored avatar (≤1 MB) so phones show it offline;
// anything else is referenced by URL.
func (s *Server) vcardPhoto(avatar string) string {
	prefix := s.cfg.PublicBaseURL + "/uploads/"
	if name, ok := strings.CutPrefix(avatar, prefix); ok && !strings.ContainsAny(name, `/\`) {
		path := filepath.Join(s.cfg.UploadDir, name)
		if fi, err := os.Stat(path); err == nil && fi.Size() <= 1<<20 {
			if data, err := os.ReadFile(path); err == nil {
				typ := map[string]string{".png": "PNG", ".jpg": "JPEG", ".gif": "GIF", ".webp": "WEBP"}[strings.ToLower(filepath.Ext(name))]
				if typ != "" {
					return "PHOTO;ENCODING=b;TYPE=" + typ + ":" + base64.StdEncoding.EncodeToString(data)
				}
			}
		}
	}
	return "PHOTO;VALUE=URI:" + avatar
}
