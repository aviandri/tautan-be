package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
)

// Owner notifications for form responses and contact messages: on by
// default, instant, through the outbox. They carry visitors' personal data,
// so they only go to a confirmed address, and at most notifyPerHour per
// owner (a flooded form keeps every submission in the inbox but stops
// emailing; this also protects the provider's sending quota).
const notifyPerHour = 20

type submissionNote struct {
	kind       string // "form" | "contact"
	blockTitle string
	replyTo    string // the visitor's email, if any
	fields     []mail.Field
}

// notifySubmission enqueues the owner's email inside the submission's tx.
func (s *Server) notifySubmission(ctx context.Context, tx pgx.Tx, profileID string, n submissionNote) error {
	var to, username, display string
	var on, verified bool
	err := tx.QueryRow(ctx, `SELECT u.email, u.notify_submissions, u.email_verified_at IS NOT NULL, p.username, p.display_name
		FROM profiles p JOIN users u ON u.id = p.user_id WHERE p.id = $1`, profileID).Scan(&to, &on, &verified, &username, &display)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!on || !verified)) {
		return nil
	}
	if err != nil {
		return err
	}
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM email_outbox WHERE kind = 'submission' AND to_email = $1 AND created_at > $2`,
		to, s.now().Add(-time.Hour)).Scan(&recent); err != nil {
		return err
	}
	if recent >= notifyPerHour {
		return nil
	}
	page := display
	if page == "" {
		page = "@" + username
	}
	link := s.cfg.AppBaseURL + "/dashboard/forms"
	if n.kind == "contact" {
		link = s.cfg.AppBaseURL + "/dashboard/inbox"
	}
	m := mail.SubmissionNotice(to, n.replyTo, page, n.blockTitle, n.fields, link, s.cfg.AppBaseURL+"/dashboard/settings#account")
	return s.enqueueEmail(ctx, tx, "submission", m)
}

// answerFields turns stored answers into label/value rows.
func answerFields(answers []Answer) []mail.Field {
	out := make([]mail.Field, 0, len(answers))
	for _, a := range answers {
		v := ""
		switch x := a.Value.(type) {
		case nil:
			continue
		case bool:
			v = map[bool]string{true: "Yes", false: "No"}[x]
		case string:
			v = x
		default:
			v = fmt.Sprint(x)
		}
		if v == "" {
			continue
		}
		label := a.Label
		if label == "" {
			label = a.FieldID
		}
		out = append(out, mail.Field{Label: label, Value: v})
	}
	return out
}

// PATCH /me/notifications {submissions: bool} → {submissions}
func (s *Server) handlePatchNotifications(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Submissions *bool `json:"submissions"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Submissions == nil {
		writeError(w, http.StatusBadRequest, "invalid_notifications", "submissions must be a boolean")
		return
	}
	var v bool
	if err := s.db.QueryRow(r.Context(), `UPDATE users SET notify_submissions = $2 WHERE id = $1 RETURNING notify_submissions`,
		userID(r), *in.Submissions).Scan(&v); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"submissions": v})
}
