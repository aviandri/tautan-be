package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/mail"
)

const (
	verifyTTL = 72 * time.Hour
	// Resend requests per account per hour; more get 429.
	verifyPerHour = 3
)

// queueVerification creates a verification token for uid and enqueues the
// email, inside tx.
func (s *Server) queueVerification(ctx context.Context, tx pgx.Tx, uid, email string) error {
	raw := randomToken()
	if _, err := tx.Exec(ctx, `INSERT INTO email_verifications (token_hash, user_id, expires_at, created_at) VALUES ($1, $2, $3, $4)`,
		hashToken(raw), uid, s.now().Add(verifyTTL), s.now()); err != nil {
		return err
	}
	link := s.cfg.AppBaseURL + "/verify-email?token=" + url.QueryEscape(raw)
	return s.enqueueEmail(ctx, tx, "verify_email", mail.VerifyEmail(email, link))
}

// markVerified stamps email_verified_at once (Google, a reset link or a
// verification link all prove the address).
func markVerified(ctx context.Context, q execer, uid string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE users SET email_verified_at = COALESCE(email_verified_at, $2) WHERE id = $1`, uid, at)
	return err
}

// POST /auth/verify-email {token} → {verified: true}. No auth: the link may
// be opened on another device.
func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
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
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid string
		if err := tx.QueryRow(ctx, `UPDATE email_verifications SET used_at = $2
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING user_id`, hashToken(in.Token), now).Scan(&uid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE email_verifications SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, uid, now); err != nil {
			return err
		}
		return markVerified(ctx, tx, uid, now)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, "invalid_token", "this confirmation link is invalid, used or expired")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"verified": true})
}

// POST /me/verification-email → 202 (sent) or 200 {already_verified: true}.
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var already, limited bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var email string
		var verified bool
		var recent int
		if err := tx.QueryRow(ctx, `SELECT email, email_verified_at IS NOT NULL,
			(SELECT count(*) FROM email_verifications v WHERE v.user_id = users.id AND v.created_at > $2)
			FROM users WHERE id = $1 FOR UPDATE`, userID(r), s.now().Add(-time.Hour)).Scan(&email, &verified, &recent); err != nil {
			return err
		}
		if verified {
			already = true
			return nil
		}
		if recent >= verifyPerHour {
			limited = true
			return nil
		}
		return s.queueVerification(ctx, tx, userID(r), email)
	})
	switch {
	case err != nil:
		s.internalError(w, err)
	case already:
		writeJSON(w, http.StatusOK, map[string]bool{"already_verified": true})
	case limited:
		writeError(w, http.StatusTooManyRequests, "rate_limited", "we've sent a few links already; check your inbox or try again in an hour")
	default:
		writeJSON(w, http.StatusAccepted, map[string]bool{"sent": true})
	}
}
