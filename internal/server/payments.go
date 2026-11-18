package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/payments"
)

// Payments (a): the creator connects their own Xendit, Midtrans or Stripe
// account. Keys are sealed with PAYMENTS_ENCRYPTION_KEY and never leave the
// server; the dashboard sees the last 4 characters only.

type providerTest struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type providerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type providerSettings struct {
	Provider          string         `json:"provider"`
	Configured        bool           `json:"configured"`
	Enabled           bool           `json:"enabled"`
	Mode              *string        `json:"mode"`
	SecretKeyHint     *string        `json:"secret_key_hint"`
	WebhookSecretHint *string        `json:"webhook_secret_hint"`
	NeedsWebhookSec   bool           `json:"needs_webhook_secret"`
	WebhookURL        string         `json:"webhook_url"`
	Currencies        []string       `json:"currencies"`
	VerifiedAt        *time.Time     `json:"verified_at"`
	LastError         *providerError `json:"last_error"`
	UpdatedAt         *time.Time     `json:"updated_at"`
}

type providerRow struct {
	mode       string
	enabled    bool
	sealed     []byte
	hints      map[string]string
	verifiedAt *time.Time
	lastError  *providerError
	updatedAt  time.Time
}

func (s *Server) webhookURL(provider, profileID string) string {
	return s.cfg.PublicBaseURL + "/webhooks/" + provider + "/" + profileID
}

func credAAD(profileID, provider, mode string) []byte {
	return []byte(profileID + "|" + provider + "|" + mode)
}

func (s *Server) loadProviderRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, profileID, provider string, forUpdate bool) (*providerRow, error) {
	sql := `SELECT mode, enabled, credentials, hints, verified_at, last_error, updated_at
		FROM payment_providers WHERE profile_id = $1 AND provider = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var row providerRow
	err := q.QueryRow(ctx, sql, profileID, provider).Scan(
		&row.mode, &row.enabled, &row.sealed, &row.hints, &row.verifiedAt, &row.lastError, &row.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Server) providerView(profileID, provider string, row *providerRow) providerSettings {
	v := providerSettings{
		Provider:        provider,
		NeedsWebhookSec: payments.NeedsWebhookSecret(provider),
		WebhookURL:      s.webhookURL(provider, profileID),
		Currencies:      payments.Currencies(provider),
	}
	if row == nil {
		return v
	}
	mode := row.mode
	v.Configured, v.Enabled, v.Mode = true, row.enabled, &mode
	if h := row.hints["secret_key"]; h != "" {
		v.SecretKeyHint = &h
	}
	if h := row.hints["webhook_secret"]; h != "" {
		v.WebhookSecretHint = &h
	}
	v.VerifiedAt, v.LastError = row.verifiedAt, row.lastError
	u := row.updatedAt
	v.UpdatedAt = &u
	return v
}

// openCreds decrypts a row's credentials.
func (s *Server) openCreds(profileID, provider string, row *providerRow) (payments.Credentials, error) {
	var c payments.Credentials
	pt, _, err := s.sealer.Open(row.sealed, credAAD(profileID, provider, row.mode))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(pt, &c)
	return c, err
}

func (s *Server) provider(name, mode string, c payments.Credentials) (payments.Provider, error) {
	return payments.New(name, mode, c, s.PaymentOptions)
}

// requirePayments answers 503 when PAYMENTS_ENCRYPTION_KEY isn't set.
func (s *Server) requirePayments(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sealer == nil {
			writeError(w, http.StatusServiceUnavailable, "payments_unavailable",
				"payments are not set up on this server (PAYMENTS_ENCRYPTION_KEY is missing)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pathProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := chi.URLParam(r, "provider")
	if !payments.Valid(name) {
		writeError(w, http.StatusNotFound, "not_found", "unknown payment provider")
		return "", false
	}
	return name, true
}

// GET /payments/providers
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	pid := profileID(r)
	rows := map[string]*providerRow{}
	if s.sealer != nil {
		for _, name := range payments.Names {
			row, err := s.loadProviderRow(r.Context(), s.db, pid, name, false)
			if err != nil {
				s.internalError(w, err)
				return
			}
			rows[name] = row
		}
	}
	out := make([]providerSettings, 0, len(payments.Names))
	for _, name := range payments.Names {
		out = append(out, s.providerView(pid, name, rows[name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": s.sealer != nil, "providers": out})
}

type putProviderReq struct {
	Mode          *string `json:"mode"`
	SecretKey     *string `json:"secret_key"`
	WebhookSecret *string `json:"webhook_secret"`
	Enabled       *bool   `json:"enabled"`
}

// PUT /payments/providers/{provider}
//
// Creates or updates a connection. secret_key and webhook_secret are
// write-only: omit them to keep the stored values (only while the mode stays
// the same), send "" for webhook_secret to clear it. Whenever the key or mode
// changes the connection is tested; a failed test saves the keys but leaves
// the provider disabled.
func (s *Server) handlePutProvider(w http.ResponseWriter, r *http.Request) {
	name, ok := pathProvider(w, r)
	if !ok {
		return
	}
	var req putProviderReq
	if !decodeJSON(w, r, &req) {
		return
	}
	pid := profileID(r)
	ctx := r.Context()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	row, err := s.loadProviderRow(ctx, tx, pid, name, true)
	if err != nil {
		s.internalError(w, err)
		return
	}

	details := map[string]string{}
	var mode string
	switch {
	case req.Mode != nil:
		mode = *req.Mode
	case row != nil:
		mode = row.mode
	}
	if mode != payments.Sandbox && mode != payments.Live {
		details["mode"] = "must be sandbox or live"
	}
	var cur payments.Credentials
	if row != nil && row.mode == mode {
		if cur, err = s.openCreds(pid, name, row); err != nil {
			// The stored keys can't be read (key rotated without listing the
			// old one): the creator must re-enter them.
			cur = payments.Credentials{}
		}
	}
	next := cur
	if req.SecretKey != nil {
		next.SecretKey = *req.SecretKey
	}
	if req.WebhookSecret != nil {
		next.WebhookSecret = *req.WebhookSecret
	}
	if len(details) == 0 {
		for k, v := range payments.ValidateCredentials(name, mode, next) {
			if k == "secret_key" && next.SecretKey == "" && row != nil && row.mode != mode {
				v = "enter the " + mode + " key when switching mode"
			}
			details[k] = v
		}
	}
	if len(details) > 0 {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_data", "check the highlighted fields", details)
		return
	}

	keyChanged := row == nil || row.mode != mode || next.SecretKey != cur.SecretKey
	enabled := row != nil && row.enabled && !keyChanged
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if enabled && payments.NeedsWebhookSecret(name) && next.WebhookSecret == "" {
		writeErrorDetails(w, http.StatusBadRequest, "webhook_secret_required",
			"add the webhook secret before turning this provider on", map[string]string{"webhook_secret": "required to turn the provider on"})
		return
	}

	var verifiedAt *time.Time
	var lastError *providerError
	if row != nil && !keyChanged {
		verifiedAt, lastError = row.verifiedAt, row.lastError
	}
	var test *providerTest
	if keyChanged || verifiedAt == nil {
		t := s.testConnection(ctx, name, mode, next)
		test = &t
		if t.OK {
			now := s.now().UTC()
			verifiedAt, lastError = &now, nil
		} else {
			verifiedAt, lastError = nil, &providerError{Code: t.Code, Message: t.Message}
			enabled = false
		}
	}

	plain, _ := json.Marshal(next)
	sealed, err := s.sealer.Seal(plain, credAAD(pid, name, mode))
	if err != nil {
		s.internalError(w, err)
		return
	}
	hints := map[string]string{"secret_key": payments.Hint(next.SecretKey)}
	if next.WebhookSecret != "" {
		hints["webhook_secret"] = payments.Hint(next.WebhookSecret)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO payment_providers (profile_id, provider, mode, enabled, credentials, hints, verified_at, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (profile_id, provider) DO UPDATE SET
			mode = EXCLUDED.mode, enabled = EXCLUDED.enabled, credentials = EXCLUDED.credentials,
			hints = EXCLUDED.hints, verified_at = EXCLUDED.verified_at, last_error = EXCLUDED.last_error,
			updated_at = now()`,
		pid, name, mode, enabled, sealed, hints, verifiedAt, lastError)
	if err != nil {
		s.internalError(w, err)
		return
	}
	saved, err := s.loadProviderRow(ctx, tx, pid, name, false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": s.providerView(pid, name, saved), "test": test})
}

func (s *Server) testConnection(ctx context.Context, name, mode string, c payments.Credentials) providerTest {
	p, err := s.provider(name, mode, c)
	if err != nil {
		return providerTest{Code: "provider_error", Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.TestConnection(ctx); err != nil {
		var pe *payments.Error
		if errors.As(err, &pe) {
			return providerTest{Code: pe.Code, Message: pe.Message}
		}
		return providerTest{Code: "provider_error", Message: "the test failed"}
	}
	return providerTest{OK: true}
}

// POST /payments/providers/{provider}/test re-tests the stored keys. A failed
// test is recorded but doesn't turn the provider off (outages happen).
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	name, ok := pathProvider(w, r)
	if !ok {
		return
	}
	pid := profileID(r)
	if !s.providerTestLimiter.Allow(pid) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many tests; try again in a few minutes")
		return
	}
	ctx := r.Context()
	row, err := s.loadProviderRow(ctx, s.db, pid, name, false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "not_configured", "connect this provider first")
		return
	}
	creds, err := s.openCreds(pid, name, row)
	if err != nil {
		writeError(w, http.StatusConflict, "credentials_unreadable", "the saved keys can't be read any more; enter them again")
		return
	}
	t := s.testConnection(ctx, name, row.mode, creds)
	if t.OK {
		_, err = s.db.Exec(ctx, `UPDATE payment_providers SET verified_at = now(), last_error = NULL WHERE profile_id = $1 AND provider = $2`, pid, name)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE payment_providers SET last_error = $3 WHERE profile_id = $1 AND provider = $2`,
			pid, name, providerError{Code: t.Code, Message: t.Message})
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	row, err = s.loadProviderRow(ctx, s.db, pid, name, false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": s.providerView(pid, name, row), "test": t})
}

// DELETE /payments/providers/{provider} forgets the keys.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name, ok := pathProvider(w, r)
	if !ok {
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM payment_providers WHERE profile_id = $1 AND provider = $2`, profileID(r), name); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
