package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Phase 3-C "Engage" cards: poll, ask_me, lead_magnet, giveaway.
// See "P3-C/D: implemented (BE)" in SPEC.md.

type (
	PollOption struct {
		ID    string `json:"id"` // assigned by the server when empty; keep it on edits
		Label string `json:"label"`
	}
	PollData struct {
		Question    string       `json:"question"`
		Options     []PollOption `json:"options"`
		ShowResults string       `json:"show_results"` // after_vote | always | never
		EndsAt      *time.Time   `json:"ends_at,omitempty"`
		Schedule    *Schedule    `json:"schedule,omitempty"`
	}
	AskMeData struct {
		Prompt        string    `json:"prompt"`
		PublicAnswers bool      `json:"public_answers"`
		Placeholder   string    `json:"placeholder,omitempty"`
		ButtonLabel   string    `json:"button_label,omitempty"`
		Schedule      *Schedule `json:"schedule,omitempty"`
	}
	LeadMagnetData struct {
		Title          string    `json:"title"`
		Description    string    `json:"description,omitempty"`
		FileURL        string    `json:"file_url"` // never sent in public reads
		CoverURL       string    `json:"cover_url,omitempty"`
		ButtonLabel    string    `json:"button_label,omitempty"`
		SuccessMessage string    `json:"success_message,omitempty"`
		Subscribe      string    `json:"subscribe"` // everyone | ask | none
		Schedule       *Schedule `json:"schedule,omitempty"`
	}
	GiveawayStep struct {
		Label string `json:"label"`
		URL   string `json:"url"`
	}
	GiveawayData struct {
		Title     string         `json:"title"`
		Prize     string         `json:"prize"`
		EndsAt    time.Time      `json:"ends_at"`
		Steps     []GiveawayStep `json:"steps"`
		RulesURL  string         `json:"rules_url,omitempty"`
		Subscribe string         `json:"subscribe"` // everyone | ask | none
		Schedule  *Schedule      `json:"schedule,omitempty"`
	}
)

var (
	engageTypes      = []string{"poll", "ask_me", "lead_magnet", "giveaway"}
	showResultsModes = set("after_vote", "always", "never")
	optionIDRE       = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
)

func init() {
	for _, t := range engageTypes {
		blockTypes[t] = true
	}
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func checkPoll(d *PollData) error {
	if d.Options == nil {
		d.Options = make([]PollOption, 0)
	}
	if d.ShowResults == "" {
		d.ShowResults = "after_vote"
	}
	if err := firstErr(checkText("question", d.Question, 200), checkCount("options", len(d.Options), 2, 5),
		oneOf("show_results", d.ShowResults, showResultsModes)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range d.Options {
		o := &d.Options[i]
		f := fmt.Sprintf("options[%d]", i)
		if o.ID == "" {
			o.ID = shortID()
		}
		if !optionIDRE.MatchString(o.ID) || seen[o.ID] {
			return ferr(f+".id", "%s.id must be a unique id of a-z, 0-9, _ or -", f)
		}
		seen[o.ID] = true
		if err := checkText(f+".label", o.Label, 80); err != nil {
			return err
		}
	}
	return nil
}

func checkAskMe(d *AskMeData) error {
	return firstErr(checkText("prompt", d.Prompt, 200), maxLen("placeholder", d.Placeholder, 100), maxLen("button_label", d.ButtonLabel, 40))
}

func checkLeadMagnet(d *LeadMagnetData) error {
	if d.Subscribe == "" {
		d.Subscribe = "ask"
	}
	return firstErr(checkText("title", d.Title, 100), maxLen("description", d.Description, 300),
		checkImageURL("file_url", d.FileURL, true), checkImageURL("cover_url", d.CoverURL, false),
		maxLen("button_label", d.ButtonLabel, 40), maxLen("success_message", d.SuccessMessage, 200),
		oneOf("subscribe", d.Subscribe, subscribeModes))
}

func checkGiveaway(d *GiveawayData) error {
	if d.Steps == nil {
		d.Steps = make([]GiveawayStep, 0)
	}
	if d.Subscribe == "" {
		d.Subscribe = "ask"
	}
	if d.EndsAt.IsZero() {
		return ferr("ends_at", "ends_at is required (RFC 3339)")
	}
	if err := firstErr(checkText("title", d.Title, 100), checkText("prize", d.Prize, 200),
		checkCount("steps", len(d.Steps), 0, 5), checkWebURL("rules_url", d.RulesURL, false),
		oneOf("subscribe", d.Subscribe, subscribeModes)); err != nil {
		return err
	}
	for i, st := range d.Steps {
		f := fmt.Sprintf("steps[%d]", i)
		if err := firstErr(checkText(f+".label", st.Label, 80), checkWebURL(f+".url", st.URL, true)); err != nil {
			return err
		}
	}
	return nil
}

func engageBlock(typ string) (v any, check func() error, sched func() *Schedule, ok bool) {
	switch typ {
	case "poll":
		d := &PollData{}
		return d, func() error { return checkPoll(d) }, func() *Schedule { return d.Schedule }, true
	case "ask_me":
		d := &AskMeData{}
		return d, func() error { return checkAskMe(d) }, func() *Schedule { return d.Schedule }, true
	case "lead_magnet":
		d := &LeadMagnetData{}
		return d, func() error { return checkLeadMagnet(d) }, func() *Schedule { return d.Schedule }, true
	case "giveaway":
		d := &GiveawayData{}
		return d, func() error { return checkGiveaway(d) }, func() *Schedule { return d.Schedule }, true
	}
	return nil, nil, nil, false
}

// ---------- public endpoints ----------

// publicCard resolves {username}/{id} to a live block of typ, writing 404s.
func (s *Server) publicCard(w http.ResponseWriter, r *http.Request, typ string) (string, Block, bool) {
	pid, b, err := s.publicBlock(r.Context(), chi.URLParam(r, "username"), chi.URLParam(r, "id"), typ)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "card not found on this page")
		return "", b, false
	}
	if err != nil {
		s.internalError(w, err)
		return "", b, false
	}
	if !s.gateOK(w, r, b) {
		return "", b, false
	}
	return pid, b, true
}

type pollResults struct {
	Counts map[string]int `json:"counts"`
	Total  int            `json:"total"`
}

func (s *Server) pollResults(ctx context.Context, blockID string, opts []PollOption) (pollResults, error) {
	res := pollResults{Counts: make(map[string]int, len(opts))}
	for _, o := range opts {
		res.Counts[o.ID] = 0
	}
	rows, err := s.db.Query(ctx, `SELECT option_id, count(*) FROM poll_votes WHERE block_id = $1 GROUP BY 1`, blockID)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return res, err
		}
		if _, ok := res.Counts[id]; ok { // votes for removed options don't count
			res.Counts[id] = n
			res.Total += n
		}
	}
	return res, rows.Err()
}

func (s *Server) handlePollVote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OptionID  string `json:"option_id"`
		VisitorID string `json:"visitor_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	pid, b, ok := s.publicCard(w, r, "poll")
	if !ok {
		return
	}
	var d PollData
	_ = json.Unmarshal(b.Data, &d)
	ctx, now := r.Context(), s.now()
	if d.EndsAt != nil && !now.Before(*d.EndsAt) {
		writeError(w, http.StatusConflict, "poll_ended", "this poll has ended")
		return
	}
	valid := false
	for _, o := range d.Options {
		valid = valid || o.ID == in.OptionID
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_option", "option_id is not an option of this poll")
		return
	}
	vid := visitorID(in.VisitorID)
	if !s.engageLimiter.Allow(clientIP(r)+"|"+vid+"|"+b.ID) || !s.engageIPLimiter.Allow(clientIP(r)+"|"+b.ID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
		return
	}
	vh, err := s.pollVoter(ctx, pid, r, vid)
	if err != nil {
		s.internalError(w, err)
		return
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO poll_votes (block_id, visitor_hash, day, option_id, created_at)
		VALUES ($1, $2, ($3::timestamptz AT TIME ZONE 'UTC')::date, $4, $3) ON CONFLICT DO NOTHING`, b.ID, vh, now, in.OptionID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := map[string]any{"voted": in.OptionID}
	if d.ShowResults != "never" {
		res, err := s.pollResults(ctx, b.ID, d.Options)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out["results"] = res
	}
	if tag.RowsAffected() == 0 {
		out["error"] = apiError{Code: "already_voted", Message: "you already voted today"}
		writeJSON(w, http.StatusConflict, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Question  string `json:"question"`
		VisitorID string `json:"visitor_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	pid, b, ok := s.publicCard(w, r, "ask_me")
	if !ok {
		return
	}
	q := strings.TrimSpace(in.Question)
	if q == "" || len([]rune(q)) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_question", "question is required (max 500 characters)")
		return
	}
	if !s.askLimiter.Allow(clientIP(r)+"|"+visitorID(in.VisitorID)+"|"+pid) || !s.askIPLimiter.Allow(clientIP(r)+"|"+pid) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many questions, try again later")
		return
	}
	if _, err := s.db.Exec(r.Context(), `INSERT INTO questions (profile_id, block_id, question, created_at) VALUES ($1, $2, $3, $4)`,
		pid, b.ID, q, s.now()); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// contactIn is the body of the email-collecting public endpoints.
type contactIn struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Subscribe bool   `json:"subscribe"`
}

func (in *contactIn) valid(w http.ResponseWriter) bool {
	in.Email, in.Name = strings.ToLower(strings.TrimSpace(in.Email)), strings.TrimSpace(in.Name)
	if !validEmail(in.Email) {
		writeError(w, http.StatusBadRequest, "invalid_email", "email address is invalid")
		return false
	}
	if len([]rune(in.Name)) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must be at most 100 characters")
		return false
	}
	return true
}

func (s *Server) handleLead(w http.ResponseWriter, r *http.Request) {
	var in contactIn
	if !decodeJSON(w, r, &in) || !in.valid(w) {
		return
	}
	pid, b, ok := s.publicCard(w, r, "lead_magnet")
	if !ok {
		return
	}
	if !s.formLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many submissions, try again later")
		return
	}
	var d LeadMagnetData
	_ = json.Unmarshal(b.Data, &d)
	sub := d.Subscribe == "everyone" || (d.Subscribe == "ask" && in.Subscribe)
	now := s.now()
	if _, err := upsertContact(r.Context(), s.db, pid, in.Email, in.Name, sub, b.ID, now); err != nil {
		s.internalError(w, err)
		return
	}
	exp := now.Add(leadTokenTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"download_url": "/api/v1/public/" + chi.URLParam(r, "username") + "/download/" + s.leadToken(b.ID, exp),
		"expires_at":   exp.UTC().Truncate(time.Second),
	})
}

const leadTokenTTL = 15 * time.Minute

// leadToken is <exp unix>.<block id>.<base64url HMAC>.
func (s *Server) leadToken(blockID string, exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10) + "." + blockID
	return payload + "." + s.sign("lead", payload)
}

func (s *Server) sign(purpose, payload string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.JWTSecret))
	m.Write([]byte(purpose + "|" + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) handleLeadDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	parts := strings.Split(chi.URLParam(r, "token"), ".")
	if len(parts) != 3 || !hmac.Equal([]byte(parts[2]), []byte(s.sign("lead", parts[0]+"."+parts[1]))) {
		writeError(w, http.StatusForbidden, "invalid_token", "download link is invalid")
		return
	}
	if exp, err := strconv.ParseInt(parts[0], 10, 64); err != nil || s.now().Unix() > exp {
		writeError(w, http.StatusGone, "expired", "download link has expired, submit the form again")
		return
	}
	_, b, err := s.publicBlock(r.Context(), chi.URLParam(r, "username"), parts[1], "lead_magnet")
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	var d LeadMagnetData
	_ = json.Unmarshal(b.Data, &d)
	local := strings.TrimPrefix(d.FileURL, s.cfg.PublicBaseURL)
	if strings.HasPrefix(local, "/uploads/") {
		name := path.Base(local)
		f, err := s.uploads().Open(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "file not found")
			return
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, f.ModTime(), f)
		return
	}
	http.Redirect(w, r, d.FileURL, http.StatusFound)
}

func (s *Server) handleGiveawayEnter(w http.ResponseWriter, r *http.Request) {
	var in contactIn
	if !decodeJSON(w, r, &in) || !in.valid(w) {
		return
	}
	pid, b, ok := s.publicCard(w, r, "giveaway")
	if !ok {
		return
	}
	var d GiveawayData
	_ = json.Unmarshal(b.Data, &d)
	now := s.now()
	if !now.Before(d.EndsAt) {
		writeError(w, http.StatusConflict, "giveaway_ended", "this giveaway has ended")
		return
	}
	if !s.formLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many submissions, try again later")
		return
	}
	sub := d.Subscribe == "everyone" || (d.Subscribe == "ask" && in.Subscribe)
	var inserted bool
	err := pgx.BeginFunc(r.Context(), s.db, func(tx pgx.Tx) error {
		cid, err := upsertContact(r.Context(), tx, pid, in.Email, in.Name, sub, b.ID, now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(r.Context(), `INSERT INTO giveaway_entries (block_id, contact_id, email, name, created_at)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (block_id, email) DO NOTHING`, b.ID, cid, in.Email, in.Name, now)
		inserted = tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"entered": true, "already_entered": !inserted})
}

// ---------- owner endpoints ----------

// ownCard loads one of the signed-in owner's blocks of typ.
func (s *Server) ownCard(r *http.Request, typ string) (Block, error) {
	b, err := scanBlock(s.db.QueryRow(r.Context(), `SELECT `+blockCols+` FROM blocks b
		WHERE b.id = $1 AND b.profile_id = $2 AND b.type = $3`, chi.URLParam(r, "id"), profileID(r), typ))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return b, notFound(strings.ReplaceAll(typ, "_", " "))
	}
	return b, err
}

func (s *Server) handlePollResults(w http.ResponseWriter, r *http.Request) {
	b, err := s.ownCard(r, "poll")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var d PollData
	_ = json.Unmarshal(b.Data, &d)
	res, err := s.pollResults(r.Context(), b.ID, d.Options)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type Question struct {
	ID         string     `json:"id"`
	BlockID    string     `json:"block_id"`
	Question   string     `json:"question"`
	Answer     string     `json:"answer"`
	AnsweredAt *time.Time `json:"answered_at"`
	Pinned     bool       `json:"pinned"`
	CreatedAt  time.Time  `json:"created_at"`
}

const questionCols = `id, block_id, question, answer, answered_at, pinned, created_at`

func scanQuestion(row pgx.Row) (Question, error) {
	var q Question
	err := row.Scan(&q.ID, &q.BlockID, &q.Question, &q.Answer, &q.AnsweredAt, &q.Pinned, &q.CreatedAt)
	return q, err
}

// handleListQuestions: ?status=unanswered|answered|all (default all), ?block_id=, newest first, max 500.
func (s *Server) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where, args := "profile_id = $1", []any{profileID(r)}
	switch q.Get("status") {
	case "unanswered":
		where += " AND answered_at IS NULL"
	case "answered":
		where += " AND answered_at IS NOT NULL"
	case "", "all":
	default:
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be unanswered, answered or all")
		return
	}
	if id := q.Get("block_id"); id != "" {
		if !uuidRE.MatchString(id) {
			writeError(w, http.StatusBadRequest, "invalid_block", "block_id must be a block id")
			return
		}
		args = append(args, id)
		where += " AND block_id = $2"
	}
	rows, err := s.db.Query(r.Context(), `SELECT `+questionCols+` FROM questions WHERE `+where+` ORDER BY created_at DESC LIMIT 500`, args...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Question, error) { return scanQuestion(r) })
	if err != nil {
		s.internalError(w, err)
		return
	}
	if items == nil {
		items = []Question{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePatchQuestion sets answer (empty clears it, which also unpins) and/or pinned.
func (s *Server) handlePatchQuestion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answer *string `json:"answer"`
		Pinned *bool   `json:"pinned"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx, id, pid := r.Context(), chi.URLParam(r, "id"), profileID(r)
	var out Question
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		cur, err := scanQuestion(tx.QueryRow(ctx, `SELECT `+questionCols+` FROM questions WHERE id = $1 AND profile_id = $2 FOR UPDATE`, id, pid))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return notFound("question")
		}
		if err != nil {
			return err
		}
		if in.Answer != nil {
			a := strings.TrimSpace(*in.Answer)
			if len([]rune(a)) > 2000 {
				return badReq("invalid_answer", "answer must be at most 2000 characters")
			}
			if a == "" {
				cur.Answer, cur.AnsweredAt, cur.Pinned = "", nil, false
			} else {
				if cur.AnsweredAt == nil || a != cur.Answer {
					now := s.now()
					cur.AnsweredAt = &now
				}
				cur.Answer = a
			}
		}
		if in.Pinned != nil {
			if *in.Pinned && cur.AnsweredAt == nil {
				return badReq("not_answered", "answer the question before pinning it")
			}
			cur.Pinned = *in.Pinned
		}
		out, err = scanQuestion(tx.QueryRow(ctx, `UPDATE questions SET answer = $2, answered_at = $3, pinned = $4 WHERE id = $1 RETURNING `+questionCols,
			id, cur.Answer, cur.AnsweredAt, cur.Pinned))
		return err
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	tag, err := s.db.Exec(r.Context(), `DELETE FROM questions WHERE id = $1 AND profile_id = $2`, chi.URLParam(r, "id"), profileID(r))
	if isInvalidUUID(err) || (err == nil && tag.RowsAffected() == 0) {
		writeError(w, http.StatusNotFound, "not_found", "question not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type giveawayWinner struct {
	Email   string    `json:"email"` // owner endpoints only
	Name    string    `json:"name"`
	Display string    `json:"display"` // shown on the public card
	DrawnAt time.Time `json:"drawn_at"`
}

func (s *Server) giveawayState(ctx context.Context, blockID string) (int, *giveawayWinner, error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM giveaway_entries WHERE block_id = $1`, blockID).Scan(&n); err != nil {
		return 0, nil, err
	}
	var gw giveawayWinner
	err := s.db.QueryRow(ctx, `SELECT email, name, display, drawn_at FROM giveaway_winners WHERE block_id = $1`, blockID).
		Scan(&gw.Email, &gw.Name, &gw.Display, &gw.DrawnAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, nil, nil
	}
	return n, &gw, err
}

func (s *Server) handleGiveaway(w http.ResponseWriter, r *http.Request) {
	b, err := s.ownCard(r, "giveaway")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	n, winner, err := s.giveawayState(r.Context(), b.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": n, "winner": winner})
}

// handleGiveawayDraw picks a uniformly random entry once the giveaway has
// ended. Drawing again replaces the winner.
func (s *Server) handleGiveawayDraw(w http.ResponseWriter, r *http.Request) {
	b, err := s.ownCard(r, "giveaway")
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var d GiveawayData
	_ = json.Unmarshal(b.Data, &d)
	ctx, now := r.Context(), s.now()
	if now.Before(d.EndsAt) {
		writeError(w, http.StatusConflict, "not_ended", "you can draw a winner after the giveaway ends")
		return
	}
	var n int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM giveaway_entries WHERE block_id = $1`, b.ID).Scan(&n); err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusConflict, "no_entries", "nobody entered this giveaway")
		return
	}
	k, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		s.internalError(w, err)
		return
	}
	gw := giveawayWinner{DrawnAt: now}
	if err := s.db.QueryRow(ctx, `SELECT email, name FROM giveaway_entries WHERE block_id = $1 ORDER BY created_at, email OFFSET $2 LIMIT 1`,
		b.ID, k.Int64()).Scan(&gw.Email, &gw.Name); err != nil {
		s.internalError(w, err)
		return
	}
	gw.Display = winnerDisplay(gw.Name, gw.Email)
	if _, err := s.db.Exec(ctx, `INSERT INTO giveaway_winners (block_id, email, name, display, drawn_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (block_id) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name, display = EXCLUDED.display, drawn_at = EXCLUDED.drawn_at`,
		b.ID, gw.Email, gw.Name, gw.Display, gw.DrawnAt); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": n, "winner": gw})
}

// winnerDisplay is "Budi S." from a name, else a masked email "bu***@gmail.com".
func winnerDisplay(name, email string) string {
	if f := strings.Fields(name); len(f) > 0 {
		out := f[0]
		if len(f) > 1 {
			out += " " + string([]rune(f[len(f)-1])[:1]) + "."
		}
		return clip(out, 40)
	}
	local, domain, _ := strings.Cut(email, "@")
	r := []rune(local)
	keep := min(2, len(r))
	return string(r[:keep]) + "***@" + domain
}

// ---------- reads ----------

// attachEngage adds server state to engage cards. Public reads: poll results
// (show_results=always) and closed flag, pinned answers (public_answers),
// lead_magnet without file_url, giveaway entry count / ended / winner display.
// The editor additionally always sees poll results.
func (s *Server) attachEngage(ctx context.Context, blocks []Block, public bool) error {
	now := s.now()
	for i := range blocks {
		b := &blocks[i]
		var m map[string]any
		switch b.Type {
		case "poll", "ask_me", "lead_magnet", "giveaway":
			if json.Unmarshal(b.Data, &m) != nil {
				continue
			}
		default:
			continue
		}
		switch b.Type {
		case "poll":
			var d PollData
			_ = json.Unmarshal(b.Data, &d)
			m["closed"] = d.EndsAt != nil && !now.Before(*d.EndsAt)
			if !public || d.ShowResults == "always" {
				res, err := s.pollResults(ctx, b.ID, d.Options)
				if err != nil {
					return err
				}
				m["results"] = res
			}
		case "ask_me":
			var d AskMeData
			_ = json.Unmarshal(b.Data, &d)
			if public && !d.PublicAnswers {
				break
			}
			rows, err := s.db.Query(ctx, `SELECT question, answer, answered_at FROM questions
				WHERE block_id = $1 AND pinned AND answered_at IS NOT NULL ORDER BY answered_at DESC LIMIT 20`, b.ID)
			if err != nil {
				return err
			}
			type qa struct {
				Question   string    `json:"question"`
				Answer     string    `json:"answer"`
				AnsweredAt time.Time `json:"answered_at"`
			}
			list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (qa, error) {
				var x qa
				err := r.Scan(&x.Question, &x.Answer, &x.AnsweredAt)
				return x, err
			})
			if err != nil {
				return err
			}
			if list == nil {
				list = []qa{}
			}
			m["answers"] = list
		case "lead_magnet":
			if public {
				delete(m, "file_url")
			}
		case "giveaway":
			var d GiveawayData
			_ = json.Unmarshal(b.Data, &d)
			n, gw, err := s.giveawayState(ctx, b.ID)
			if err != nil {
				return err
			}
			ended := !now.Before(d.EndsAt)
			m["ended"], m["entry_count"] = ended, n
			if gw != nil && ended {
				m["winner"] = map[string]any{"display": gw.Display, "drawn_at": gw.DrawnAt}
			}
		}
		if out, err := json.Marshal(m); err == nil {
			b.Data = out
		}
	}
	return nil
}

// visitorIDRE matches the first-party visitor id the public page keeps in
// localStorage (random, base64url). Anything else is ignored.
var visitorIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func visitorID(raw string) string {
	if visitorIDRE.MatchString(raw) {
		return raw
	}
	return ""
}

// pollVoter is the poll dedupe key for one day. With a visitor id it is that id
// (hashed with the daily salt), so different people behind one carrier-NAT IP
// with the same phone model can each vote. Clients that send none fall back to
// IP + User-Agent, the old behaviour.
func (s *Server) pollVoter(ctx context.Context, pid string, r *http.Request, vid string) (string, error) {
	if vid != "" {
		return s.visitorHash(ctx, pid, "vid:"+vid, "")
	}
	return s.visitorHash(ctx, pid, clientIP(r), r.UserAgent())
}

// handlePollStatus tells a returning visitor whether they voted today, so the
// card can show its voted state on load: GET .../vote?visitor_id=...
// → {voted: option_id|null, results?}. Results follow show_results: always, or
// after a vote unless "never". Never cached (it is per visitor).
func (s *Server) handlePollStatus(w http.ResponseWriter, r *http.Request) {
	pid, b, ok := s.publicCard(w, r, "poll")
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	var d PollData
	_ = json.Unmarshal(b.Data, &d)
	ctx := r.Context()
	vid := visitorID(r.URL.Query().Get("visitor_id"))
	var voted *string
	if vid != "" {
		vh, err := s.pollVoter(ctx, pid, r, vid)
		if err != nil {
			s.internalError(w, err)
			return
		}
		var opt string
		err = s.db.QueryRow(ctx, `SELECT option_id FROM poll_votes WHERE block_id = $1 AND visitor_hash = $2
			AND day = ($3::timestamptz AT TIME ZONE 'UTC')::date`, b.ID, vh, s.now()).Scan(&opt)
		if err == nil {
			voted = &opt
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internalError(w, err)
			return
		}
	}
	out := map[string]any{"voted": voted}
	show := d.ShowResults
	if show == "always" || (voted != nil && show != "never") {
		res, err := s.pollResults(ctx, b.ID, d.Options)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out["results"] = res
	}
	writeJSON(w, http.StatusOK, out)
}
