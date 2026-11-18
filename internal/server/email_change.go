package server

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	tmail "github.com/aviandri/tautan-be/internal/mail"
)

const (
	emailChangeTTL = 24 * time.Hour
	emailRevertTTL = 7 * 24 * time.Hour
	// Change requests per account per hour; more get 429.
	emailChangePerHour = 3
)

// maskEmail keeps the first letter of the local part and the domain:
// "dewi@example.com" → "d***@example.com".
func maskEmail(e string) string {
	at := strings.LastIndexByte(e, '@')
	if at < 1 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}

// POST /me/email {new_email, password} → 202 {pending_email}. Accounts with
// a password must give it (a stolen session alone can't move the account);
// Google-only accounts have none to give and rely on the old-address notice
// and its undo link. Nothing changes until the link sent to the new address
// is used.
func (s *Server) handleRequestEmailChange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NewEmail string `json:"new_email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.NewEmail))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		writeError(w, http.StatusBadRequest, "invalid_email", "email address is invalid")
		return
	}
	ctx, uid, now := r.Context(), userID(r), s.now()
	var code string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var cur string
		var hash *string
		var recent int
		if err := tx.QueryRow(ctx, `SELECT email, password_hash,
			(SELECT count(*) FROM email_change_tokens t WHERE t.user_id = users.id AND t.kind = 'confirm' AND t.created_at > $2)
			FROM users WHERE id = $1 FOR UPDATE`, uid, now.Add(-time.Hour)).Scan(&cur, &hash, &recent); err != nil {
			return err
		}
		switch {
		case hash != nil && bcrypt.CompareHashAndPassword([]byte(*hash), []byte(in.Password)) != nil:
			code = "invalid_password"
			return nil
		case strings.EqualFold(cur, email):
			code = "same_email"
			return nil
		case recent >= emailChangePerHour:
			code = "rate_limited"
			return nil
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&taken); err != nil {
			return err
		}
		if taken {
			code = "email_taken"
			return nil
		}
		// One pending change at a time: a new request replaces the old link.
		if _, err := tx.Exec(ctx, `UPDATE email_change_tokens SET used_at = $2 WHERE user_id = $1 AND kind = 'confirm' AND used_at IS NULL`, uid, now); err != nil {
			return err
		}
		raw := randomToken()
		if _, err := tx.Exec(ctx, `INSERT INTO email_change_tokens (token_hash, user_id, kind, old_email, new_email, expires_at, created_at)
			VALUES ($1, $2, 'confirm', $3, $4, $5, $6)`, hashToken(raw), uid, cur, email, now.Add(emailChangeTTL), now); err != nil {
			return err
		}
		link := s.cfg.AppBaseURL + "/confirm-email-change?token=" + url.QueryEscape(raw)
		return s.enqueueEmail(ctx, tx, "email_change", tmail.EmailChangeConfirm(email, link))
	})
	switch {
	case err != nil:
		s.internalError(w, err)
	case code == "invalid_password":
		writeError(w, http.StatusForbidden, "invalid_password", "your current password is incorrect")
	case code == "same_email":
		writeError(w, http.StatusBadRequest, "same_email", "that's already your email")
	case code == "rate_limited":
		writeError(w, http.StatusTooManyRequests, "rate_limited", "we've sent a few links already; check your inbox or try again in an hour")
	case code == "email_taken":
		writeError(w, http.StatusConflict, "email_taken", "an account with this email already exists")
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"pending_email": email})
	}
}

// DELETE /me/email/pending → 204: drops an unconfirmed change.
func (s *Server) handleCancelEmailChange(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(r.Context(), `UPDATE email_change_tokens SET used_at = $2 WHERE user_id = $1 AND kind = 'confirm' AND used_at IS NULL`,
		userID(r), s.now()); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /auth/email-change/confirm {token} → the /auth/login shape. No auth
// (the link may be opened on another device). Switches the email, marks it
// verified, ends every session and refresh token, and queues the notice
// (with an undo link) to the old address.
func (s *Server) handleConfirmEmailChange(w http.ResponseWriter, r *http.Request) {
	s.useEmailChangeToken(w, r, "confirm")
}

// POST /auth/email-change/revert {token} → the /auth/login shape. The undo
// link from the old-address notice: puts the old email back (only if the
// account still has the email it was changed to), ends every session and
// cancels any pending change. The password is unchanged; the FE sends the
// user to reset it.
func (s *Server) handleRevertEmailChange(w http.ResponseWriter, r *http.Request) {
	s.useEmailChangeToken(w, r, "revert")
}

func (s *Server) useEmailChangeToken(w http.ResponseWriter, r *http.Request, kind string) {
	var in struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.resetLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; try again later")
		return
	}
	ctx, now := r.Context(), s.now()
	var u User
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid, oldEmail, newEmail string
		if err := tx.QueryRow(ctx, `UPDATE email_change_tokens SET used_at = $3
			WHERE token_hash = $1 AND kind = $2 AND used_at IS NULL AND expires_at > $3
			RETURNING user_id, old_email, new_email`, hashToken(in.Token), kind, now).Scan(&uid, &oldEmail, &newEmail); err != nil {
			return err
		}
		from, to := oldEmail, newEmail
		if kind == "revert" {
			from, to = newEmail, oldEmail
		}
		// The account must still have the email this link was made for: a
		// confirm link from before another change, or an undo after a later
		// change, is stale.
		if err := tx.QueryRow(ctx, `UPDATE users SET email = $3, email_verified_at = $4, sessions_revoked_at = $4
			WHERE id = $1 AND email = $2 RETURNING id, email, created_at`, uid, from, to, now).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE email_change_tokens SET used_at = $2 WHERE user_id = $1 AND kind = 'confirm' AND used_at IS NULL`, uid, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, uid, now); err != nil {
			return err
		}
		if kind == "revert" {
			return nil
		}
		raw := randomToken()
		if _, err := tx.Exec(ctx, `INSERT INTO email_change_tokens (token_hash, user_id, kind, old_email, new_email, expires_at, created_at)
			VALUES ($1, $2, 'revert', $3, $4, $5, $6)`, hashToken(raw), uid, oldEmail, newEmail, now.Add(emailRevertTTL), now); err != nil {
			return err
		}
		link := s.cfg.AppBaseURL + "/revert-email-change?token=" + url.QueryEscape(raw)
		return s.enqueueEmail(ctx, tx, "email_changed", tmail.EmailChanged(oldEmail, maskEmail(newEmail), link))
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusBadRequest, "invalid_token", "this link is invalid, used or expired")
		return
	case isUniqueViolation(err, "users_email_key"):
		// Someone signed up with the address while the link was pending.
		writeError(w, http.StatusConflict, "email_taken", "an account with this email already exists")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	p, err := s.profileForUser(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.respondWithSession(ctx, w, http.StatusOK, u, p)
}
