package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

var (
	slugRE      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)
)

func slugify(s string) string {
	s = nonSlugChar.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "tab"
	}
	return s
}

const tabCols = `id, profile_id, title, slug, position`

func scanTab(r rowScanner) (Tab, error) {
	var t Tab
	err := r.Scan(&t.ID, &t.ProfileID, &t.Title, &t.Slug, &t.Position)
	return t, err
}

func (s *Server) listTabs(ctx context.Context, pid string) ([]Tab, error) {
	rows, err := s.db.Query(ctx, `SELECT `+tabCols+` FROM tabs WHERE profile_id = $1 ORDER BY position, title`, pid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tab, error) { return scanTab(r) })
}

func validTitle(t string) (string, bool) {
	t = strings.TrimSpace(t)
	return t, t != "" && len([]rune(t)) <= 60
}

func (s *Server) handleCreateTab(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string  `json:"title"`
		Slug  *string `json:"slug"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	title, ok := validTitle(in.Title)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_title", "title is required (max 60 chars)")
		return
	}
	ctx, pid := r.Context(), profileID(r)
	explicit := in.Slug != nil && *in.Slug != ""
	base := slugify(title)
	if explicit {
		base = strings.ToLower(strings.TrimSpace(*in.Slug))
		if !slugRE.MatchString(base) || len(base) > 40 {
			writeError(w, http.StatusBadRequest, "invalid_slug", "slug must be lowercase letters, digits and dashes")
			return
		}
	}
	for i := 1; i <= 50; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		t, err := scanTab(s.db.QueryRow(ctx,
			`INSERT INTO tabs (profile_id, title, slug, position)
			 VALUES ($1, $2, $3, (SELECT COALESCE(MAX(position) + 1, 0) FROM tabs WHERE profile_id = $1))
			 RETURNING `+tabCols, pid, title, slug))
		if isUniqueViolation(err, "") {
			if explicit {
				writeError(w, http.StatusConflict, "slug_taken", "another tab already uses this slug")
				return
			}
			continue
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, t)
		return
	}
	writeError(w, http.StatusConflict, "slug_taken", "could not find a free slug")
}

func (s *Server) handlePatchTab(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title *string `json:"title"`
		Slug  *string `json:"slug"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var title, slug *string
	if in.Title != nil {
		t, ok := validTitle(*in.Title)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_title", "title is required (max 60 chars)")
			return
		}
		title = &t
	}
	if in.Slug != nil {
		sl := strings.ToLower(strings.TrimSpace(*in.Slug))
		if !slugRE.MatchString(sl) || len(sl) > 40 {
			writeError(w, http.StatusBadRequest, "invalid_slug", "slug must be lowercase letters, digits and dashes")
			return
		}
		slug = &sl
	}
	t, err := scanTab(s.db.QueryRow(r.Context(),
		`UPDATE tabs SET title = COALESCE($3, title), slug = COALESCE($4, slug)
		 WHERE id = $1 AND profile_id = $2 RETURNING `+tabCols,
		chi.URLParam(r, "id"), profileID(r), title, slug))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err):
		writeError(w, http.StatusNotFound, "not_found", "tab not found")
	case isUniqueViolation(err, ""):
		writeError(w, http.StatusConflict, "slug_taken", "another tab already uses this slug")
	case err != nil:
		s.internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) handleDeleteTab(w http.ResponseWriter, r *http.Request) {
	ctx, pid, id := r.Context(), profileID(r), chi.URLParam(r, "id")
	var status int
	var code, msg string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Lock the profile's tabs so two concurrent deletes can't remove the last one.
		var n int
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT count(*), bool_or(id::text = $2) FROM (SELECT id FROM tabs WHERE profile_id = $1 FOR UPDATE) t`,
			pid, id).Scan(&n, &exists); err != nil {
			return err
		}
		if !exists {
			status, code, msg = 404, "not_found", "tab not found"
			return nil
		}
		if n <= 1 {
			status, code, msg = 400, "last_tab", "a page needs at least one tab"
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM tabs WHERE id = $1 AND profile_id = $2`, id, pid)
		return err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if status != 0 {
		writeError(w, status, code, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOrderTabs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx, pid := r.Context(), profileID(r)
	var mismatch bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(array_agg(id::text ORDER BY id::text), '{}') = (SELECT COALESCE(array_agg(x ORDER BY x), '{}') FROM unnest($2::text[]) x)
			 FROM (SELECT id FROM tabs WHERE profile_id = $1 FOR UPDATE) t`, pid, in.IDs).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			mismatch = true
			return nil
		}
		_, err := tx.Exec(ctx,
			`UPDATE tabs SET position = o.ord - 1 FROM unnest($2::uuid[]) WITH ORDINALITY o(id, ord)
			 WHERE tabs.id = o.id AND tabs.profile_id = $1`, pid, in.IDs)
		return err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if mismatch {
		writeError(w, http.StatusBadRequest, "order_mismatch", "ids must list every tab of the page exactly once")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
