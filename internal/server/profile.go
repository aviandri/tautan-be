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
			if !isJSONObject(raw) {
				bad("invalid_theme", "theme must be an object")
				return
			}
			add("theme", raw)
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
		case "username":
			var v string
			_ = json.Unmarshal(raw, &v)
			u, ok := validUsername(v)
			if !ok {
				bad("invalid_username", "username must be 3-30 chars of a-z, 0-9, _ or . and not reserved")
				return
			}
			add("username", u)
		}
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, profileID(r))
	q := fmt.Sprintf(`UPDATE profiles SET %s WHERE id = $%d RETURNING %s`, strings.Join(sets, ", "), len(args), profileCols)
	p, err := scanProfile(s.db.QueryRow(r.Context(), q, args...))
	if isUniqueViolation(err, "profiles_username_key") {
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
	}
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
