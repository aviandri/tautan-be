package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const profileIDKey ctxKey = 100

// withProfile resolves the owner's profile id (one profile per user in phase 1).
func (s *Server) withProfile(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pid string
		err := s.db.QueryRow(r.Context(), `SELECT id FROM profiles WHERE user_id = $1 ORDER BY created_at LIMIT 1`, userID(r)).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "no profile for this user")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), profileIDKey, pid)))
	})
}

func profileID(r *http.Request) string {
	v, _ := r.Context().Value(profileIDKey).(string)
	return v
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	var m map[string]any
	return json.Unmarshal(t, &m) == nil
}

func (s *Server) handlePatchProfile(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if !decodeJSON(w, r, &in) {
		return
	}
	sets := []string{}
	args := []any{}
	onboardingPatch := map[string]any{}
	publishing := false
	newUsername := ""
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	bad := func(code, msg string) { writeError(w, http.StatusBadRequest, code, msg) }

	for k, raw := range in {
		switch k {
		case "display_name", "bio":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				bad("invalid_"+k, k+" must be a string")
				return
			}
			max := 80
			if k == "bio" {
				max = 500
			}
			if len([]rune(v)) > max {
				bad("invalid_"+k, fmt.Sprintf("%s must be at most %d characters", k, max))
				return
			}
			add(k, strings.TrimSpace(v))
		case "avatar_url":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				bad("invalid_avatar_url", "avatar_url must be a string or null")
				return
			}
			val := ""
			if v != nil {
				val = strings.TrimSpace(*v)
			}
			if err := checkURL("avatar_url", val, false); err != nil {
				bad("invalid_avatar_url", err.Error())
				return
			}
			add("avatar_url", val)
		case "theme":
			if err := validateTheme(raw); err != nil {
				bad("invalid_theme", err.Error())
				return
			}
			add("theme", raw)
		case "socials":
			norm, err := normalizeSocials(raw)
			if err != nil {
				bad("invalid_socials", err.Error())
				return
			}
			add("socials", norm)
		case "socials_position":
			var v string
			if json.Unmarshal(raw, &v) != nil || (v != "top" && v != "bottom") {
				bad("invalid_socials_position", "socials_position must be top or bottom")
				return
			}
			add("socials_position", v)
		case "seo":
			var seo struct {
				Title       *string `json:"title,omitempty"`
				Description *string `json:"description,omitempty"`
				ImageURL    *string `json:"image_url,omitempty"`
			}
			if !isJSONObject(raw) || json.Unmarshal(raw, &seo) != nil {
				bad("invalid_seo", "seo must be {title?, description?, image_url?}")
				return
			}
			if seo.ImageURL != nil {
				if err := checkURL("seo.image_url", *seo.ImageURL, false); err != nil {
					bad("invalid_seo", err.Error())
					return
				}
			}
			norm, _ := json.Marshal(seo)
			add("seo", norm)
		case "is_published":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				bad("invalid_is_published", "is_published must be a boolean")
				return
			}
			add("is_published", v)
			publishing = v
		case "username":
			var v string
			_ = json.Unmarshal(raw, &v)
			u, ok := validUsername(v)
			if !ok {
				bad("invalid_username", "username must be 3-30 chars of a-z, 0-9, _ or . and not reserved")
				return
			}
			add("username", u)
			newUsername = u
			onboardingPatch["username_claimed"] = true
			onboardingPatch["suggested_username"] = nil
		case "announcement":
			var prevID string
			if err := s.db.QueryRow(r.Context(), `SELECT COALESCE(announcement->>'id', '') FROM profiles WHERE id = $1`, profileID(r)).Scan(&prevID); err != nil {
				s.internalError(w, err)
				return
			}
			norm, err := normalizeAnnouncement(raw, prevID)
			if err != nil {
				bad("invalid_announcement", err.Error())
				return
			}
			if norm == nil {
				sets = append(sets, "announcement = NULL")
			} else {
				add("announcement", norm)
			}
		case "settings":
			norm, err := normalizeSettings(raw)
			if err != nil {
				bad("invalid_settings", err.Error())
				return
			}
			add("settings", norm)
		case "onboarding":
			patch, err := parseOnboardingPatch(raw, s.now())
			if err != nil {
				bad("invalid_onboarding", err.Error())
				return
			}
			for k, v := range patch {
				onboardingPatch[k] = v
			}
		}
	}
	if publishing && onboardingPatch["username_claimed"] == nil {
		var claimed bool
		if err := s.db.QueryRow(r.Context(), `SELECT COALESCE((onboarding->>'username_claimed')::boolean, true) FROM profiles WHERE id = $1`,
			profileID(r)).Scan(&claimed); err != nil {
			s.internalError(w, err)
			return
		}
		if !claimed {
			bad("username_not_claimed", "choose a username before publishing")
			return
		}
	}
	if publishing {
		// Unverified accounts can't take an unpublished page live. Pages that
		// are already published stay published (grandfathered).
		var wasPublished, verified bool
		if err := s.db.QueryRow(r.Context(), `SELECT p.is_published, u.email_verified_at IS NOT NULL
			FROM profiles p JOIN users u ON u.id = p.user_id WHERE p.id = $1`, profileID(r)).Scan(&wasPublished, &verified); err != nil {
			s.internalError(w, err)
			return
		}
		if !wasPublished && !verified {
			writeError(w, http.StatusForbidden, "email_unverified", "confirm your email address before publishing your page")
			return
		}
	}
	if len(onboardingPatch) > 0 {
		enc, _ := json.Marshal(onboardingPatch)
		args = append(args, enc)
		// jsonb_strip_nulls drops keys patched to null (e.g. suggested_username once claimed).
		sets = append(sets, fmt.Sprintf("onboarding = jsonb_strip_nulls(onboarding || $%d::jsonb)", len(args)))
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, profileID(r))
	q := fmt.Sprintf(`UPDATE profiles SET %s WHERE id = $%d RETURNING %s`, strings.Join(sets, ", "), len(args), profileCols)
	ctx, pid := r.Context(), profileID(r)
	var p Profile
	var taken bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var oldUsername string
		if newUsername != "" {
			if err := tx.QueryRow(ctx, `SELECT username FROM profiles WHERE id = $1 FOR UPDATE`, pid).Scan(&oldUsername); err != nil {
				return err
			}
			// Another profile's retired username keeps redirecting there, so it can't be claimed.
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM username_redirects WHERE old_username = $1 AND profile_id <> $2)`,
				newUsername, pid).Scan(&taken); err != nil || taken {
				return err
			}
		}
		var err error
		if p, err = scanProfile(tx.QueryRow(ctx, q, args...)); err != nil {
			return err
		}
		if newUsername != "" {
			return recordRename(ctx, tx, pid, oldUsername, newUsername)
		}
		return nil
	})
	if taken || isUniqueViolation(err, "profiles_username_key") {
		writeError(w, http.StatusConflict, "username_taken", "this username is taken")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type pageResponse struct {
	Profile Profile `json:"profile"`
	Tabs    []Tab   `json:"tabs"`
	Blocks  []Block `json:"blocks"`
	// Media maps each referenced "/uploads/<name>" image to its size and ready
	// variant widths (perf 2b).
	Media map[string]imageInfo `json:"media"`
	// Payments (b): providers buyers can pay with; null when none is on.
	Payments *pagePayments `json:"payments"`
}

func (s *Server) handleEditor(w http.ResponseWriter, r *http.Request) {
	page, err := s.loadPage(r.Context(), profileID(r), false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// loadPage returns the profile, its tabs and live (not deleted) blocks. With
// visibleOnly, blocks a visitor shouldn't see are omitted (see publicBlocks).
func (s *Server) loadPage(ctx context.Context, pid string, visibleOnly bool) (pageResponse, error) {
	if visibleOnly {
		return s.loadPageOpen(ctx, pid, nil)
	}
	return s.loadPageWith(ctx, pid, false, nil)
}

// loadPageOpen is the public read with the gated cards in open unlocked.
func (s *Server) loadPageOpen(ctx context.Context, pid string, open map[string]bool) (pageResponse, error) {
	return s.loadPageWith(ctx, pid, true, open)
}

func (s *Server) loadPageWith(ctx context.Context, pid string, visibleOnly bool, open map[string]bool) (pageResponse, error) {
	var out pageResponse
	p, err := scanProfile(s.db.QueryRow(ctx, `SELECT `+profileCols+` FROM profiles WHERE id = $1`, pid))
	if err != nil {
		return out, err
	}
	out.Profile = p
	if out.Tabs, err = s.listTabs(ctx, pid); err != nil {
		return out, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+blockCols+` FROM blocks b
		WHERE b.profile_id = $1 AND b.deleted_at IS NULL ORDER BY b.position, b.created_at`, pid)
	if err != nil {
		return out, err
	}
	out.Blocks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Block, error) { return scanBlock(r) })
	if err == nil && visibleOnly {
		out.Blocks = publicBlocks(out.Blocks, s.now())
		for i := range out.Blocks {
			publicBlockData(&out.Blocks[i], s.now())
			stripPaidContent(&out.Blocks[i])
		}
		out.Profile.Announcement = publicAnnouncement(out.Profile.Announcement, s.now())
	}
	if err != nil {
		return out, err
	}
	if err = s.attachRemote(ctx, out.Blocks); err != nil {
		return out, err
	}
	if err = s.attachEngage(ctx, out.Blocks, visibleOnly); err != nil {
		return out, err
	}
	if visibleOnly {
		lockBlocks(out.Blocks, open) // after every attach, before media: nothing gated leaks
	}
	if out.Media, err = s.pageMedia(ctx, out); err != nil {
		return out, err
	}
	out.Payments, err = s.pagePayments(ctx, pid)
	return out, err
}

// publicBlocks drops hidden blocks, blocks outside their schedule window, and
// children of folders that were dropped.
func publicBlocks(blocks []Block, now time.Time) []Block {
	shown := make(map[string]bool, len(blocks))
	for _, b := range blocks {
		if b.IsVisible && blockSchedule(b.Data).activeAt(now) {
			shown[b.ID] = true
		}
	}
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if shown[b.ID] && (b.ParentID == nil || shown[*b.ParentID]) {
			out = append(out, b)
		}
	}
	return out
}

// publicAnnouncement hides an announcement outside its schedule window.
func publicAnnouncement(raw json.RawMessage, now time.Time) json.RawMessage {
	var a struct {
		Schedule *Schedule `json:"schedule"`
	}
	if json.Unmarshal(raw, &a) == nil && !a.Schedule.activeAt(now) {
		return json.RawMessage("null")
	}
	return raw
}
