package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// ---------- CSV ----------

// csvCell neutralises spreadsheet formula injection (=, +, -, @, tab, CR prefixes).
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func writeCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	cw := csv.NewWriter(w)
	_ = cw.Write(header)
	for _, r := range rows {
		for i := range r {
			r[i] = csvCell(r[i])
		}
		_ = cw.Write(r)
	}
	cw.Flush()
}

func answerString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---------- forms ----------

type formSummary struct {
	BlockID        string     `json:"block_id"`
	Type           string     `json:"type"`
	Title          string     `json:"title"`
	TabID          string     `json:"tab_id"`
	IsVisible      bool       `json:"is_visible"`
	Responses      int        `json:"responses"`
	LastResponseAt *time.Time `json:"last_response_at"`
}

func (s *Server) handleListForms(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `
		SELECT b.id, b.type, COALESCE(b.data->>'title', ''), b.tab_id, b.is_visible, count(f.id), max(f.created_at)
		FROM blocks b LEFT JOIN form_submissions f ON f.block_id = b.id
		WHERE b.profile_id = $1 AND b.type IN ('form', 'email_signup') AND b.deleted_at IS NULL
		GROUP BY b.id ORDER BY b.created_at`, profileID(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (formSummary, error) {
		var f formSummary
		err := r.Scan(&f.BlockID, &f.Type, &f.Title, &f.TabID, &f.IsVisible, &f.Responses, &f.LastResponseAt)
		return f, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type FormSubmission struct {
	ID        string          `json:"id"`
	BlockID   *string         `json:"block_id"`
	ContactID *string         `json:"contact_id"`
	Answers   json.RawMessage `json:"answers"`
	CreatedAt time.Time       `json:"created_at"`
}

func scanFormSubmission(r rowScanner) (FormSubmission, error) {
	var f FormSubmission
	err := r.Scan(&f.ID, &f.BlockID, &f.ContactID, &f.Answers, &f.CreatedAt)
	return f, err
}

// ownedFormBlock loads a form or email_signup block of the owner (deleted ones included).
func (s *Server) ownedFormBlock(r *http.Request) (Block, error) {
	b, err := scanBlock(s.db.QueryRow(r.Context(), `SELECT `+blockCols+` FROM blocks b
		WHERE b.id = $1 AND b.profile_id = $2 AND b.type IN ('form', 'email_signup')`, chi.URLParam(r, "id"), profileID(r)))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return b, notFound("form")
	}
	return b, err
}

func (s *Server) handleFormSubmissions(w http.ResponseWriter, r *http.Request) {
	b, err := s.ownedFormBlock(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	args := []any{b.ID, contactPageSize + 1}
	where := `block_id = $1`
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		args = append(args, t, id)
		where += ` AND (created_at, id) < ($3, $4)`
	}
	rows, err := s.db.Query(r.Context(), `SELECT id, block_id, contact_id, answers, created_at FROM form_submissions
		WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FormSubmission, error) { return scanFormSubmission(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	var next *string
	if len(items) > contactPageSize {
		items = items[:contactPageSize]
		c := encodeCursor(items[len(items)-1].CreatedAt, items[len(items)-1].ID)
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// handleFormSubmissionsCSV exports every submission: one column per current
// field (in form order), then any fields that only exist in older answers.
func (s *Server) handleFormSubmissionsCSV(w http.ResponseWriter, r *http.Request) {
	b, err := s.ownedFormBlock(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT id, block_id, contact_id, answers, created_at FROM form_submissions
		WHERE block_id = $1 ORDER BY created_at DESC, id DESC LIMIT 50000`, b.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FormSubmission, error) { return scanFormSubmission(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	var cols []string
	labels := map[string]string{}
	add := func(id, label string) {
		if _, ok := labels[id]; !ok {
			labels[id] = label
			cols = append(cols, id)
		}
	}
	if b.Type == "form" {
		var fd FormData
		_ = json.Unmarshal(b.Data, &fd)
		for _, f := range fd.Fields {
			add(f.ID, f.Label)
		}
	}
	parsed := make([]map[string]any, len(subs))
	for i, sub := range subs {
		var answers []Answer
		_ = json.Unmarshal(sub.Answers, &answers)
		parsed[i] = map[string]any{}
		for _, a := range answers {
			add(a.FieldID, a.Label)
			parsed[i][a.FieldID] = a.Value
		}
	}
	header := []string{"Submitted at"}
	for _, c := range cols {
		header = append(header, labels[c])
	}
	out := make([][]string, len(subs))
	for i, sub := range subs {
		row := []string{ts(sub.CreatedAt)}
		for _, c := range cols {
			row = append(row, answerString(parsed[i][c]))
		}
		out[i] = row
	}
	writeCSV(w, "form-"+b.ID[:8]+".csv", header, out)
}

func (s *Server) handleContactSubmissionsCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT created_at, name, email, message FROM contact_submissions
		WHERE profile_id = $1 ORDER BY created_at DESC, id DESC LIMIT 50000`, profileID(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]string, error) {
		var t time.Time
		var n, e, m string
		err := r.Scan(&t, &n, &e, &m)
		return []string{ts(t), n, e, m}, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeCSV(w, "contact-submissions.csv", []string{"Submitted at", "Name", "Email", "Message"}, out)
}

// ---------- audience ----------

type contactSource struct {
	BlockID string `json:"block_id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
}

type Contact struct {
	ID             string          `json:"id"`
	Email          string          `json:"email"`
	Name           string          `json:"name"`
	Subscribed     bool            `json:"subscribed"`
	SubscribedAt   *time.Time      `json:"subscribed_at"`
	Sources        []contactSource `json:"sources"`
	Responses      int             `json:"responses"`
	FirstSeenAt    time.Time       `json:"first_seen_at"`
	LastActivityAt time.Time       `json:"last_activity_at"`
}

const contactSelect = `SELECT c.id, c.email, c.name, c.subscribed, c.subscribed_at,
	COALESCE((SELECT json_agg(json_build_object('block_id', b.id, 'type', b.type, 'title', COALESCE(b.data->>'title', '')) ORDER BY b.created_at)
	          FROM blocks b WHERE b.id = ANY(c.source_block_ids)), '[]'),
	c.responses, c.first_seen_at, c.last_activity_at FROM contacts c`

func scanContact(r rowScanner) (Contact, error) {
	var c Contact
	var src []byte
	err := r.Scan(&c.ID, &c.Email, &c.Name, &c.Subscribed, &c.SubscribedAt, &src, &c.Responses, &c.FirstSeenAt, &c.LastActivityAt)
	if err == nil {
		err = json.Unmarshal(src, &c.Sources)
	}
	return c, err
}

// audienceFilter builds the WHERE clause shared by the list and CSV export.
func audienceFilter(r *http.Request, pid string) (string, []any, error) {
	where, args := `c.profile_id = $1`, []any{pid}
	switch r.URL.Query().Get("status") {
	case "", "all":
	case "subscribed":
		where += ` AND c.subscribed`
	case "unsubscribed":
		where += ` AND NOT c.subscribed`
	default:
		return "", nil, badReq("invalid_status", "status must be all, subscribed or unsubscribed")
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)+"%")
		where += fmt.Sprintf(` AND (c.email ILIKE $%d OR c.name ILIKE $%d)`, len(args), len(args))
	}
	return where, args, nil
}

func (s *Server) handleAudience(w http.ResponseWriter, r *http.Request) {
	ctx, pid := r.Context(), profileID(r)
	where, args, err := audienceFilter(r, pid)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		args = append(args, t, id)
		where += fmt.Sprintf(` AND (c.last_activity_at, c.id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, contactPageSize+1)
	rows, err := s.db.Query(ctx, contactSelect+` WHERE `+where+
		fmt.Sprintf(` ORDER BY c.last_activity_at DESC, c.id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contact, error) { return scanContact(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	var next *string
	if len(items) > contactPageSize {
		items = items[:contactPageSize]
		c := encodeCursor(items[len(items)-1].LastActivityAt, items[len(items)-1].ID)
		next = &c
	}
	var all, subscribed int
	if err := s.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE subscribed) FROM contacts WHERE profile_id = $1`, pid).
		Scan(&all, &subscribed); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "next_cursor": next,
		"counts": map[string]int{"all": all, "subscribed": subscribed, "unsubscribed": all - subscribed},
	})
}

func (s *Server) handleAudienceCSV(w http.ResponseWriter, r *http.Request) {
	where, args, err := audienceFilter(r, profileID(r))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rows, err := s.db.Query(r.Context(), contactSelect+` WHERE `+where+` ORDER BY c.last_activity_at DESC, c.id DESC LIMIT 100000`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]string, error) {
		c, err := scanContact(r)
		status, subAt := "Not subscribed", ""
		if c.Subscribed {
			status = "Subscribed"
		}
		if c.SubscribedAt != nil {
			subAt = ts(*c.SubscribedAt)
		}
		titles := make([]string, len(c.Sources))
		for i, s := range c.Sources {
			titles[i] = s.Title
			if titles[i] == "" {
				titles[i] = s.Type
			}
		}
		return []string{c.Email, c.Name, status, subAt, strings.Join(titles, "; "), strconv.Itoa(c.Responses), ts(c.FirstSeenAt), ts(c.LastActivityAt)}, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeCSV(w, "audience.csv", []string{"Email", "Name", "Status", "Subscribed at", "Sources", "Responses", "First seen", "Last activity"}, out)
}

type activity struct {
	Kind      string          `json:"kind"` // form | contact
	BlockID   *string         `json:"block_id"`
	Answers   json.RawMessage `json:"answers,omitempty"`
	Message   string          `json:"message,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// handleContactDetail returns a contact and its latest 50 submissions.
func (s *Server) handleContactDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := scanContact(s.db.QueryRow(ctx, contactSelect+` WHERE c.id = $1 AND c.profile_id = $2`, chi.URLParam(r, "id"), profileID(r)))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		writeError(w, http.StatusNotFound, "not_found", "contact not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT 'form', block_id, answers, '', created_at FROM form_submissions WHERE contact_id = $1
		UNION ALL
		SELECT 'contact', block_id, NULL, message, created_at FROM contact_submissions WHERE contact_id = $1
		ORDER BY 5 DESC LIMIT 50`, c.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	acts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (activity, error) {
		var a activity
		err := r.Scan(&a.Kind, &a.BlockID, &a.Answers, &a.Message, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact": c, "activity": acts})
}
