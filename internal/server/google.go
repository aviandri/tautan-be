package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

// Google sign-in (P2-8): OIDC authorization code flow with PKCE, state and nonce.
//
//  1. The FE navigates the browser to GET /auth/google/start. The server stores
//     state, nonce and the PKCE verifier in a short-lived signed cookie and
//     redirects to Google.
//  2. Google redirects to GOOGLE_REDIRECT_URL (the FE callback page) with
//     ?code&state. The FE POSTs them to /auth/google/callback with credentials.
//  3. The server checks state against the cookie, redeems the code, verifies
//     the ID token and signs the user in exactly like /auth/login.

const (
	oauthCookie     = "tautan_oauth"
	oauthCookiePath = "/api/v1/auth/google"
	oauthStateTTL   = 10 * time.Minute
	oauthIssuer     = "tautan-oauth" // never accepted as an access token (issuer "tautan")
	providerGoogle  = "google"
)

type oauthStateClaims struct {
	jwt.RegisteredClaims
	State    string `json:"st"`
	Nonce    string `json:"nn"`
	Verifier string `json:"cv"`
	// Username typed on the landing page (signup?u=): becomes the new
	// account's onboarding suggestion.
	Username string `json:"u,omitempty"`
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// setOAuthCookie builds the state cookie with the deployment's cookie
// attributes (s.cookie): it is set on a top-level navigation to the API and
// read on a credentialed fetch from the FE, exactly like the refresh cookie.
func (s *Server) setOAuthCookie(w http.ResponseWriter, value string, exp time.Time) {
	http.SetCookie(w, s.cookie(oauthCookie, value, oauthCookiePath, exp))
}

// GET /auth/providers → {google: bool}
func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"google": s.google != nil})
}

// startError sends the browser back to the FE callback page with an error code,
// since /start is a navigation, not a fetch.
func (s *Server) startError(w http.ResponseWriter, r *http.Request, code string) {
	u, err := url.Parse(s.cfg.GoogleRedirectURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, code, "Google sign-in is unavailable")
		return
	}
	q := u.Query()
	q.Set("error", code)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// GET /auth/google/start → 302 to Google's consent screen.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if s.google == nil {
		writeError(w, http.StatusNotFound, "provider_disabled", "Google sign-in is not enabled")
		return
	}
	meta, err := s.google.discover(r.Context())
	if err != nil {
		log.Printf("google start: %v", err)
		s.startError(w, r, "provider_error")
		return
	}
	st := oauthStateClaims{State: randomToken(), Nonce: randomToken(), Verifier: randomToken()}
	if u, ok := validUsername(r.URL.Query().Get("u")); ok {
		st.Username = u
	}
	now := s.now()
	st.Issuer = oauthIssuer
	st.IssuedAt = jwt.NewNumericDate(now)
	st.ExpiresAt = jwt.NewNumericDate(now.Add(oauthStateTTL))
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, st).SignedString([]byte(s.cfg.JWTSecret))
	if err != nil {
		s.internalError(w, err)
		return
	}
	dest, err := s.google.authURL(meta, st.State, st.Nonce, pkceChallenge(st.Verifier))
	if err != nil {
		log.Printf("google start: %v", err)
		s.startError(w, r, "provider_error")
		return
	}
	s.setOAuthCookie(w, signed, now.Add(oauthStateTTL))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, dest, http.StatusFound)
}

// readOAuthState validates the state cookie against the state echoed by Google.
func (s *Server) readOAuthState(r *http.Request, state string) (*oauthStateClaims, bool) {
	c, err := r.Cookie(oauthCookie)
	if err != nil || c.Value == "" || state == "" {
		return nil, false
	}
	claims := &oauthStateClaims{}
	_, err = jwt.ParseWithClaims(c.Value, claims, func(*jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(oauthIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	if err != nil || subtle.ConstantTimeCompare([]byte(claims.State), []byte(state)) != 1 {
		return nil, false
	}
	return claims, true
}

type googleAuthResponse struct {
	authResponse
	IsNewUser bool `json:"is_new_user"`
	// The account was linked by email and its old password was removed.
	PasswordRemoved bool `json:"password_removed"`
}

// POST /auth/google/callback {code, state} → {access_token, user, profile, is_new_user} + refresh cookie.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if s.google == nil {
		writeError(w, http.StatusNotFound, "provider_disabled", "Google sign-in is not enabled")
		return
	}
	var in struct{ Code, State string }
	if !decodeJSON(w, r, &in) {
		return
	}
	st, ok := s.readOAuthState(r, in.State)
	// The state is single-use whatever happens next.
	s.setOAuthCookie(w, "", s.now())
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_state", "sign-in session expired or did not match; please try again")
		return
	}
	if strings.TrimSpace(in.Code) == "" {
		writeError(w, http.StatusBadRequest, "invalid_code", "authorization code is missing")
		return
	}
	ctx := r.Context()
	claims, err := s.google.exchange(ctx, in.Code, st.Verifier, st.Nonce)
	switch {
	case errors.Is(err, errOIDCBadCode):
		writeError(w, http.StatusBadRequest, "invalid_code", "authorization code is invalid or expired; please try again")
		return
	case err != nil:
		log.Printf("google callback: %v", err)
		writeError(w, http.StatusBadGateway, "provider_error", "could not complete Google sign-in")
		return
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !claims.emailVerified() {
		writeError(w, http.StatusForbidden, "email_not_verified", "your Google account's email address is not verified")
		return
	}

	res, err := s.resolveGoogleUser(ctx, claims.Subject, email, st.Username)
	if isUniqueViolation(err, "") {
		// A concurrent callback created the same user or link; the retry finds it.
		res, err = s.resolveGoogleUser(ctx, claims.Subject, email, st.Username)
	}
	u := res.user
	if err != nil {
		s.internalError(w, err)
		return
	}
	p, err := s.profileForUser(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	tok, err := s.issueSession(ctx, w, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	status := http.StatusOK
	if res.isNew {
		status = http.StatusCreated
	}
	writeJSON(w, status, googleAuthResponse{authResponse{AccessToken: tok, User: u, Profile: p}, res.isNew, res.passwordRemoved})
}

type googleResolution struct {
	user            User
	isNew           bool
	passwordRemoved bool
}

// resolveGoogleUser finds the user linked to the Google subject, links an
// existing account with the same (verified) email, or creates a new account
// that goes through onboarding with a temporary username, like a signup
// without a username.
//
// Whoever set an unverified account's password may not own its email
// (account pre-hijacking). When Google is the first to prove ownership (no
// identity yet and email_verified_at is null), the takeover is undone: the
// password is cleared and every existing session is revoked. Verified
// accounts keep their password and sessions.
func (s *Server) resolveGoogleUser(ctx context.Context, sub, email, wantUsername string) (res googleResolution, err error) {
	u := &res.user
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		res = googleResolution{} // a retried transaction starts clean
		err := tx.QueryRow(ctx,
			`SELECT u.id, u.email, u.created_at FROM user_identities i JOIN users u ON u.id = i.user_id
			 WHERE i.provider = $1 AND i.subject = $2`, providerGoogle, sub).Scan(&u.ID, &u.Email, &u.CreatedAt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var hasPassword, verified bool
		err = tx.QueryRow(ctx,
			`SELECT id, email, created_at, password_hash IS NOT NULL,
			        email_verified_at IS NOT NULL OR EXISTS (SELECT 1 FROM user_identities WHERE user_id = users.id)
			 FROM users WHERE email = $1 FOR UPDATE`, email).
			Scan(&u.ID, &u.Email, &u.CreatedAt, &hasPassword, &verified)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			onboarding := Onboarding{UsernameClaimed: false, SuggestedUsername: s.suggestUsername(ctx, email)}
			if wantUsername != "" {
				if taken, err := s.usernameTaken(ctx, wantUsername, ""); err == nil && !taken {
					onboarding.SuggestedUsername = wantUsername
				}
			}
			if *u, _, err = createAccount(ctx, tx, email, nil, tempUsername(), onboarding); err != nil {
				return err
			}
			res.isNew = true
		case err != nil:
			return err
		case !verified:
			now := s.now()
			if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = NULL, sessions_revoked_at = $2 WHERE id = $1`, u.ID, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, u.ID, now); err != nil {
				return err
			}
			res.passwordRemoved = hasPassword
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_identities (provider, subject, user_id, email) VALUES ($1, $2, $3, $4)`,
			providerGoogle, sub, u.ID, email); err != nil {
			return err
		}
		return markVerified(ctx, tx, u.ID, s.now())
	})
	return res, err
}
