package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/aviandri/tautan-be/internal/mail"
)

const (
	resetTTL = 30 * time.Minute
	// At most this many reset emails per account per hour; extra requests
	// still answer 202 but send nothing.
	resetPerHour = 3
)

// POST /auth/password/forgot {email} → 202 whether or not the account exists.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.resetLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; try again later")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	accepted := func() { writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true}) }
	if email == "" {
		accepted()
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid string
		var recent int
		err := tx.QueryRow(ctx, `SELECT u.id, (SELECT count(*) FROM password_resets p WHERE p.user_id = u.id AND p.created_at > $2)
			FROM users u WHERE u.email = $1 FOR UPDATE`, email, s.now().Add(-time.Hour)).Scan(&uid, &recent)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && recent >= resetPerHour) {
			return nil
		}
		if err != nil {
			return err
		}
		raw := randomToken()
		if _, err := tx.Exec(ctx, `INSERT INTO password_resets (token_hash, user_id, expires_at, created_at) VALUES ($1, $2, $3, $4)`,
			hashToken(raw), uid, s.now().Add(resetTTL), s.now()); err != nil {
			return err
		}
		link := s.cfg.AppBaseURL + "/reset-password?token=" + url.QueryEscape(raw)
		return s.enqueueEmail(ctx, tx, "password_reset", mail.PasswordReset(email, link))
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	accepted()
}

// POST /auth/password/reset {token, new_password} → the /auth/login shape.
// Every other session ends, and every other outstanding reset link dies.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.resetLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; try again later")
		return
	}
	if len(in.NewPassword) < 8 || len(in.NewPassword) > 72 {
		writeError(w, http.StatusBadRequest, "invalid_password", "password must be 8 to 72 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.internalError(w, err)
		return
	}
	ctx := r.Context()
	now := s.now()
	var u User
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid string
		if err := tx.QueryRow(ctx, `UPDATE password_resets SET used_at = $2
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING user_id`,
			hashToken(in.Token), now).Scan(&uid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE users SET password_hash = $2, sessions_revoked_at = $3 WHERE id = $1
			RETURNING id, email, created_at`, uid, string(hash), now).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE password_resets SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, uid, now); err != nil {
			return err
		}
		if err := markVerified(ctx, tx, uid, now); err != nil { // the link proved the address
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, uid, now)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "invalid_token", "this reset link is invalid, used or expired")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	slog.Info("password reset", "user", u.ID)
	p, err := s.profileForUser(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.respondWithSession(ctx, w, http.StatusOK, u, p)
}
