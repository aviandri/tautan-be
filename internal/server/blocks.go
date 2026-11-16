package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const blockCols = `b.id, b.profile_id, b.tab_id, b.parent_id, b.type, b.position, b.is_visible, b.data, b.created_at, b.updated_at, b.deleted_at`

func scanBlock(r rowScanner) (Block, error) {
	var b Block
	err := r.Scan(&b.ID, &b.ProfileID, &b.TabID, &b.ParentID, &b.Type, &b.Position, &b.IsVisible, &b.Data, &b.CreatedAt, &b.UpdatedAt, &b.DeletedAt)
	return b, err
}

// isInvalidUUID reports a malformed uuid in a path/body param (treated as not found).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

type httpErr struct {
	status        int
	code, message string
	details       map[string]string
}

func (e *httpErr) Error() string { return e.message }

func badReq(code, format string, a ...any) *httpErr {
	return &httpErr{status: http.StatusBadRequest, code: code, message: fmt.Sprintf(format, a...)}
}

func notFound(what string) *httpErr {
	return &httpErr{status: http.StatusNotFound, code: "not_found", message: what + " not found"}
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var he *httpErr
	if errors.As(err, &he) {
		writeErrorDetails(w, he.status, he.code, he.message, he.details)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	s.internalError(w, err)
}

// resolveParent validates a folder parent and returns its tab id.
func resolveParent(ctx context.Context, tx pgx.Tx, pid, parentID, childType string) (string, error) {
	if childType != "link" {
		return "", badReq("invalid_parent", "only link blocks can be placed in a folder")
	}
	var typ, tabID string
	err := tx.QueryRow(ctx, `SELECT type, tab_id FROM blocks WHERE id = $1 AND profile_id = $2 AND deleted_at IS NULL`, parentID, pid).Scan(&typ, &tabID)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return "", badReq("invalid_parent", "parent block not found")
	}
	if err != nil {
		return "", err
	}
	if typ != "folder" {
		return "", badReq("invalid_parent", "parent must be a folder block")
	}
	return tabID, nil
}

func checkTab(ctx context.Context, tx pgx.Tx, pid, tabID string) error {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tabs WHERE id = $1 AND profile_id = $2)`, tabID, pid).Scan(&ok)
	if isInvalidUUID(err) || (err == nil && !ok) {
		return badReq("invalid_tab", "tab not found")
	}
	return err
}

const nextPositionSQL = `(SELECT COALESCE(MAX(position) + 1, 0) FROM blocks
	WHERE profile_id = $1 AND tab_id = $2 AND parent_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL)`

func (s *Server) handleCreateBlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TabID    string          `json:"tab_id"`
		Type     string          `json:"type"`
		Data     json.RawMessage `json:"data"`
		ParentID *string         `json:"parent_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !blockTypes[in.Type] {
		writeError(w, http.StatusBadRequest, "invalid_type", "unknown block type")
		return
	}
	data, err := validateBlockData(in.Type, in.Data)
	if err != nil {
		s.writeErr(w, dataErr(err))
		return
	}
	ctx, pid := r.Context(), profileID(r)
	var b Block
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if in.ParentID != nil && *in.ParentID != "" {
			tab, err := resolveParent(ctx, tx, pid, *in.ParentID, in.Type)
			if err != nil {
				return err
			}
			in.TabID = tab
		} else {
			in.ParentID = nil
			if err := checkTab(ctx, tx, pid, in.TabID); err != nil {
				return err
			}
		}
		if err := checkBlockRefs(ctx, tx, pid, in.Type, data); err != nil {
			return err
		}
		var err error
		b, err = scanBlock(tx.QueryRow(ctx,
			`INSERT INTO blocks AS b (profile_id, tab_id, parent_id, type, data, position)
			 VALUES ($1, $2, $3, $4, $5, `+nextPositionSQL+`) RETURNING `+blockCols,
			pid, in.TabID, in.ParentID, in.Type, data))
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.queueMissingRemote(ctx, b.Type, b.Data)
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handlePatchBlock(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx, pid, id := r.Context(), profileID(r), chi.URLParam(r, "id")
	var out Block
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cur, err := scanBlock(tx.QueryRow(ctx, `SELECT `+blockCols+` FROM blocks b WHERE b.id = $1 AND b.profile_id = $2 AND b.deleted_at IS NULL FOR UPDATE`, id, pid))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return notFound("block")
		}
		if err != nil {
			return err
		}
		next := cur
		if raw, ok := in["data"]; ok {
			if next.Data, err = validateBlockData(cur.Type, raw); err != nil {
				return dataErr(err)
			}
			if err := checkBlockRefs(ctx, tx, pid, cur.Type, next.Data); err != nil {
				return err
			}
		}
		if raw, ok := in["is_visible"]; ok {
			if json.Unmarshal(raw, &next.IsVisible) != nil {
				return badReq("invalid_is_visible", "is_visible must be a boolean")
			}
		}
		if raw, ok := in["parent_id"]; ok {
			var p *string
			if json.Unmarshal(raw, &p) != nil {
				return badReq("invalid_parent", "parent_id must be a string or null")
			}
			if p != nil && *p == "" {
				p = nil
			}
			next.ParentID = p
		}
		if raw, ok := in["tab_id"]; ok {
			if json.Unmarshal(raw, &next.TabID) != nil {
				return badReq("invalid_tab", "tab_id must be a string")
			}
			// Moving a folder child to another tab without naming a parent takes it out of the folder.
			if _, hasParent := in["parent_id"]; !hasParent && next.TabID != cur.TabID {
				next.ParentID = nil
			}
		}
		if next.ParentID != nil {
			if *next.ParentID == cur.ID {
				return badReq("invalid_parent", "a block cannot contain itself")
			}
			if next.TabID, err = resolveParent(ctx, tx, pid, *next.ParentID, cur.Type); err != nil {
				return err
			}
		} else if next.TabID != cur.TabID {
			if err := checkTab(ctx, tx, pid, next.TabID); err != nil {
				return err
			}
		}
		moved := next.TabID != cur.TabID || !sameParent(next.ParentID, cur.ParentID)
		pos := cur.Position
		if moved {
			if err := tx.QueryRow(ctx, `SELECT `+nextPositionSQL, pid, next.TabID, next.ParentID).Scan(&pos); err != nil {
				return err
			}
		}
		out, err = scanBlock(tx.QueryRow(ctx,
			`UPDATE blocks b SET data = $3, is_visible = $4, tab_id = $5, parent_id = $6, position = $7, updated_at = now()
			 WHERE b.id = $1 AND b.profile_id = $2 RETURNING `+blockCols,
			id, pid, next.Data, next.IsVisible, next.TabID, next.ParentID, pos))
		if err != nil {
			return err
		}
		if cur.Type == "folder" && next.TabID != cur.TabID {
			_, err = tx.Exec(ctx, `UPDATE blocks SET tab_id = $2, updated_at = now() WHERE parent_id = $1`, id, next.TabID)
		}
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.queueMissingRemote(ctx, out.Type, out.Data)
	writeJSON(w, http.StatusOK, out)
}

func sameParent(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// handleDeleteBlock soft-deletes a block, plus a folder's live children with
// the same timestamp so they are restored together.
func (s *Server) handleDeleteBlock(w http.ResponseWriter, r *http.Request) {
	ctx, pid, id, ts := r.Context(), profileID(r), chi.URLParam(r, "id"), s.now()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE blocks SET deleted_at = $3 WHERE id = $1 AND profile_id = $2 AND deleted_at IS NULL`, id, pid, ts)
		if isInvalidUUID(err) || (err == nil && tag.RowsAffected() == 0) {
			return notFound("block")
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE blocks SET deleted_at = $2 WHERE parent_id = $1 AND deleted_at IS NULL`, id, ts)
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const deletedRetention = 30 * 24 * time.Hour

// handleListDeleted lists restorable blocks, newest first. Children deleted
// together with their folder are restored with it and so aren't listed.
// Blocks deleted more than 30 days ago are purged.
func (s *Server) handleListDeleted(w http.ResponseWriter, r *http.Request) {
	ctx, pid := r.Context(), profileID(r)
	if _, err := s.db.Exec(ctx, `DELETE FROM blocks WHERE profile_id = $1 AND deleted_at < $2`, pid, s.now().Add(-deletedRetention)); err != nil {
		s.internalError(w, err)
		return
	}
	rows, err := s.db.Query(ctx, `SELECT `+blockCols+` FROM blocks b
		LEFT JOIN blocks p ON p.id = b.parent_id
		WHERE b.profile_id = $1 AND b.deleted_at IS NOT NULL
		  AND NOT (p.id IS NOT NULL AND p.deleted_at = b.deleted_at)
		ORDER BY b.deleted_at DESC, b.id LIMIT 200`, pid)
	if err != nil {
		s.internalError(w, err)
		return
	}
	blocks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Block, error) { return scanBlock(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks})
}

// handleRestoreBlock undeletes a block at the end of its group. If its folder is
// gone or still deleted it returns to the top level of its tab.
func (s *Server) handleRestoreBlock(w http.ResponseWriter, r *http.Request) {
	ctx, pid, id := r.Context(), profileID(r), chi.URLParam(r, "id")
	var out Block
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cur, err := scanBlock(tx.QueryRow(ctx, `SELECT `+blockCols+` FROM blocks b WHERE b.id = $1 AND b.profile_id = $2 AND b.deleted_at IS NOT NULL FOR UPDATE`, id, pid))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return notFound("deleted block")
		}
		if err != nil {
			return err
		}
		parent := cur.ParentID
		if parent != nil {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE id = $1 AND deleted_at IS NULL)`, *parent).Scan(&live); err != nil {
				return err
			}
			if !live {
				parent = nil
			}
		}
		var pos int
		if err := tx.QueryRow(ctx, `SELECT `+nextPositionSQL, pid, cur.TabID, parent).Scan(&pos); err != nil {
			return err
		}
		out, err = scanBlock(tx.QueryRow(ctx,
			`UPDATE blocks b SET deleted_at = NULL, parent_id = $3, position = $4, updated_at = now()
			 WHERE b.id = $1 AND b.profile_id = $2 RETURNING `+blockCols, id, pid, parent, pos))
		if err != nil {
			return err
		}
		if cur.Type == "folder" {
			_, err = tx.Exec(ctx, `UPDATE blocks SET deleted_at = NULL WHERE parent_id = $1 AND deleted_at = $2`, id, *cur.DeletedAt)
		}
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOrderBlocks sets positions for one sibling group: the top-level blocks
// of a tab, or the children of one folder. ids must list the whole group.
func (s *Server) handleOrderBlocks(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TabID string   `json:"tab_id"`
		IDs   []string `json:"ids"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "order_mismatch", "ids must not be empty")
		return
	}
	ctx, pid := r.Context(), profileID(r)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := checkTab(ctx, tx, pid, in.TabID); err != nil {
			return err
		}
		// The group is defined by the first id's parent.
		var parent *string
		err := tx.QueryRow(ctx, `SELECT parent_id FROM blocks WHERE id = $1 AND profile_id = $2 AND tab_id = $3 AND deleted_at IS NULL`, in.IDs[0], pid, in.TabID).Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return badReq("order_mismatch", "ids must belong to the given tab")
		}
		if err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(array_agg(id::text ORDER BY id::text), '{}') = (SELECT COALESCE(array_agg(x ORDER BY x), '{}') FROM unnest($4::text[]) x)
			 FROM (SELECT id FROM blocks WHERE profile_id = $1 AND tab_id = $2 AND parent_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL FOR UPDATE) g`,
			pid, in.TabID, parent, in.IDs).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return badReq("order_mismatch", "ids must list every block of one group (a tab's top level or one folder) exactly once")
		}
		_, err = tx.Exec(ctx,
			`UPDATE blocks SET position = o.ord - 1, updated_at = now() FROM unnest($2::uuid[]) WITH ORDINALITY o(id, ord)
			 WHERE blocks.id = o.id AND blocks.profile_id = $1`, pid, in.IDs)
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
