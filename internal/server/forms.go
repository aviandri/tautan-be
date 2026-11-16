package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// ---------- form block data ----------

type FormField struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Options     []string `json:"options,omitempty"` // select
}

type FormData struct {
	Title          string      `json:"title,omitempty"`
	Description    string      `json:"description,omitempty"`
	ButtonLabel    string      `json:"button_label,omitempty"`
	SuccessMessage string      `json:"success_message,omitempty"`
	HideLabels     bool        `json:"hide_labels,omitempty"`
	CreateContacts *bool       `json:"create_contacts,omitempty"` // default true
	Subscribe      string      `json:"subscribe,omitempty"`       // everyone | ask | none (default none)
	Fields         []FormField `json:"fields"`
	Schedule       *Schedule   `json:"schedule,omitempty"`
}

var (
	fieldTypes     = set("text", "email", "phone", "textarea", "select", "checkbox", "date", "number")
	subscribeModes = set("everyone", "ask", "none")
	fieldIDRE      = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	phoneRE        = regexp.MustCompile(`^[0-9+()\-.\s]{4,32}$`)
	nameLabelRE    = regexp.MustCompile(`(?i)\bname\b`)
)

func checkForm(d *FormData) error {
	if d.Fields == nil {
		d.Fields = []FormField{}
	}
	if len(d.Fields) == 0 || len(d.Fields) > 30 {
		return fmt.Errorf("a form needs 1 to 30 fields")
	}
	if err := firstErr(oneOf("subscribe", d.Subscribe, subscribeModes), maxLen("title", d.Title, 120),
		maxLen("description", d.Description, 500), maxLen("success_message", d.SuccessMessage, 300)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, f := range d.Fields {
		p := fmt.Sprintf("fields[%d]", i)
		if !fieldIDRE.MatchString(f.ID) || seen[f.ID] {
			return fmt.Errorf("%s.id must be a unique lowercase id", p)
		}
		seen[f.ID] = true
		if !fieldTypes[f.Type] {
			return fmt.Errorf("%s.type %q is not supported", p, f.Type)
		}
		if err := firstErr(required(p+".label", f.Label), maxLen(p+".label", f.Label, 200), maxLen(p+".placeholder", f.Placeholder, 200)); err != nil {
			return err
		}
		if f.Type == "select" {
			if len(f.Options) == 0 || len(f.Options) > 50 {
				return fmt.Errorf("%s needs 1 to 50 options", p)
			}
			for _, o := range f.Options {
				if strings.TrimSpace(o) == "" || len([]rune(o)) > 100 {
					return fmt.Errorf("%s options must be 1-100 characters", p)
				}
			}
		} else {
			d.Fields[i].Options = nil
		}
	}
	return nil
}

// Answer is one stored answer; label and type are snapshotted at submit time.
type Answer struct {
	FieldID string `json:"field_id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Value   any    `json:"value"`
}

// validateAnswers checks submitted values against the form's fields and
// returns normalised answers, or per-field error messages.
func validateAnswers(fields []FormField, in map[string]json.RawMessage) ([]Answer, map[string]string) {
	errs := map[string]string{}
	out := []Answer{}
	for _, f := range fields {
		raw, present := in[f.ID]
		var val any
		empty := !present || string(raw) == "null"
		if !empty {
			switch f.Type {
			case "checkbox":
				var b bool
				if json.Unmarshal(raw, &b) != nil {
					errs[f.ID] = "must be true or false"
					continue
				}
				empty, val = !b, b
			case "number":
				var n float64
				if json.Unmarshal(raw, &n) != nil {
					var sv string
					if json.Unmarshal(raw, &sv) != nil {
						errs[f.ID] = "must be a number"
						continue
					}
					sv = strings.TrimSpace(sv)
					if sv == "" {
						empty = true
						break
					}
					var err error
					if n, err = strconv.ParseFloat(sv, 64); err != nil {
						errs[f.ID] = "must be a number"
						continue
					}
				}
				if math.IsInf(n, 0) || math.IsNaN(n) {
					errs[f.ID] = "must be a number"
					continue
				}
				val = n
			default:
				var sv string
				if json.Unmarshal(raw, &sv) != nil {
					errs[f.ID] = "must be text"
					continue
				}
				sv = strings.TrimSpace(sv)
				if sv == "" {
					empty = true
					break
				}
				if msg := checkAnswerText(f, sv); msg != "" {
					errs[f.ID] = msg
					continue
				}
				val = sv
			}
		}
		if empty {
			if f.Required {
				errs[f.ID] = "is required"
			}
			continue
		}
		out = append(out, Answer{FieldID: f.ID, Label: f.Label, Type: f.Type, Value: val})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func checkAnswerText(f FormField, v string) string {
	switch f.Type {
	case "text":
		if len([]rune(v)) > 500 {
			return "must be at most 500 characters"
		}
	case "textarea":
		if len([]rune(v)) > 5000 {
			return "must be at most 5000 characters"
		}
	case "email":
		if !validEmail(v) {
			return "must be a valid email address"
		}
	case "phone":
		if !phoneRE.MatchString(v) {
			return "must be a phone number"
		}
	case "date":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return "must be a date (YYYY-MM-DD)"
		}
	case "select":
		for _, o := range f.Options {
			if o == v {
				return ""
			}
		}
		return "must be one of the options"
	}
	return ""
}

// ---------- contacts ----------

// upsertContact records activity for email on a profile and returns the contact id.
// Subscription is sticky: once subscribed, a contact stays subscribed.
func upsertContact(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, pid, email, name string, subscribed bool, blockID string, at time.Time) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO contacts (profile_id, email, name, subscribed, subscribed_at, source_block_ids, responses, first_seen_at, last_activity_at)
		VALUES ($1, $2, $3, $4, CASE WHEN $4 THEN $6::timestamptz END, ARRAY[$5::uuid], 1, $6, $6)
		ON CONFLICT (profile_id, email) DO UPDATE SET
			name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE contacts.name END,
			subscribed = contacts.subscribed OR EXCLUDED.subscribed,
			subscribed_at = COALESCE(contacts.subscribed_at, EXCLUDED.subscribed_at),
			source_block_ids = CASE WHEN $5::uuid = ANY(contacts.source_block_ids) THEN contacts.source_block_ids
			                        ELSE contacts.source_block_ids || $5::uuid END,
			responses = contacts.responses + 1,
			last_activity_at = EXCLUDED.last_activity_at
		RETURNING id`, pid, strings.ToLower(email), name, subscribed, blockID, at).Scan(&id)
	return id, err
}

// ---------- public submit endpoints ----------

// publicBlock loads a visible, live, in-schedule block of the given type on a published page.
func (s *Server) publicBlock(ctx context.Context, username, blockID, typ string) (pid string, b Block, err error) {
	if pid, err = s.publishedProfileID(ctx, username); err != nil {
		return
	}
	b, err = scanBlock(s.db.QueryRow(ctx, `SELECT `+blockCols+` FROM blocks b
		WHERE b.id = $1 AND b.profile_id = $2 AND b.type = $3 AND b.is_visible AND b.deleted_at IS NULL`, blockID, pid, typ))
	if err == nil && !blockSchedule(b.Data).activeAt(s.now()) {
		err = pgx.ErrNoRows
	}
	if isInvalidUUID(err) {
		err = pgx.ErrNoRows
	}
	return
}

func (s *Server) handlePublicForm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BlockID   string                     `json:"block_id"`
		Answers   map[string]json.RawMessage `json:"answers"`
		Subscribe bool                       `json:"subscribe"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	if _, err := s.publishedProfileID(ctx, chi.URLParam(r, "username")); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	pid, b, err := s.publicBlock(ctx, chi.URLParam(r, "username"), in.BlockID, "form")
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "invalid_block", "form not found on this page")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !s.gateOK(w, r, b) {
		return
	}
	var form FormData
	_ = json.Unmarshal(b.Data, &form)
	answers, fieldErrs := validateAnswers(form.Fields, in.Answers)
	if fieldErrs != nil {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_answers", "some answers are missing or invalid", fieldErrs)
		return
	}
	if !s.formLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many submissions, try again later")
		return
	}
	email, name := "", ""
	for _, a := range answers {
		if a.Type == "email" && email == "" {
			email, _ = a.Value.(string)
		}
		if a.Type == "text" && name == "" && (a.FieldID == "name" || nameLabelRE.MatchString(a.Label)) {
			name, _ = a.Value.(string)
		}
	}
	subscribed := form.Subscribe == "everyone" || (form.Subscribe == "ask" && in.Subscribe)
	createContacts := form.CreateContacts == nil || *form.CreateContacts
	now := s.now()
	var id string
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var contactID *string
		if createContacts && email != "" {
			cid, err := upsertContact(ctx, tx, pid, email, name, subscribed, b.ID, now)
			if err != nil {
				return err
			}
			contactID = &cid
		}
		enc, _ := json.Marshal(answers)
		return tx.QueryRow(ctx, `INSERT INTO form_submissions (profile_id, block_id, contact_id, answers, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, pid, b.ID, contactID, enc, now).Scan(&id)
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "success_message": form.SuccessMessage})
}

func (s *Server) handlePublicSubscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BlockID string `json:"block_id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Email, in.Name = strings.TrimSpace(in.Email), strings.TrimSpace(in.Name)
	if !validEmail(in.Email) {
		writeError(w, http.StatusBadRequest, "invalid_email", "email address is invalid")
		return
	}
	if len([]rune(in.Name)) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must be at most 100 characters")
		return
	}
	ctx := r.Context()
	if _, err := s.publishedProfileID(ctx, chi.URLParam(r, "username")); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	pid, b, err := s.publicBlock(ctx, chi.URLParam(r, "username"), in.BlockID, "email_signup")
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "invalid_block", "signup block not found on this page")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !s.gateOK(w, r, b) {
		return
	}
	if !s.formLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many submissions, try again later")
		return
	}
	var data EmailSignupData
	_ = json.Unmarshal(b.Data, &data)
	now := s.now()
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cid, err := upsertContact(ctx, tx, pid, in.Email, in.Name, true, b.ID, now)
		if err != nil {
			return err
		}
		answers := []Answer{{FieldID: "email", Label: "Email", Type: "email", Value: strings.ToLower(in.Email)}}
		if in.Name != "" {
			answers = append(answers, Answer{FieldID: "name", Label: "Name", Type: "text", Value: in.Name})
		}
		enc, _ := json.Marshal(answers)
		_, err = tx.Exec(ctx, `INSERT INTO form_submissions (profile_id, block_id, contact_id, answers, created_at) VALUES ($1, $2, $3, $4, $5)`,
			pid, b.ID, cid, enc, now)
		return err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"success_message": data.SuccessMessage})
}
