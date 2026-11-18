package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

const refreshCookie = "tautan_refresh"

type ctxKey int

const userIDKey ctxKey = iota

func (s *Server) issueAccessToken(userID string) (string, error) {
	now := s.now()
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.AccessTTL)),
		Issuer:    "tautan",
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
}

// parseAccessToken returns the user id and issue time of a valid access token.
func (s *Server) parseAccessToken(tok string) (string, time.Time, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("tautan"), jwt.WithTimeFunc(s.now))
	if err != nil {
		return "", time.Time{}, err
	}
	if claims.Subject == "" || claims.IssuedAt == nil {
		return "", time.Time{}, errors.New("missing subject or iat")
	}
	return claims.Subject, claims.IssuedAt.Time, nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// issueRefreshToken stores a new refresh token and sets it as an httpOnly
// cookie. family is the rotation chain it continues; "" starts a new one
// (a fresh sign-in).
func (s *Server) issueRefreshToken(ctx context.Context, w http.ResponseWriter, userID, family string) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	exp := s.now().Add(s.cfg.RefreshTTL)
	if _, err := s.db.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_id) VALUES ($1, $2, $3, COALESCE(NULLIF($4, '')::uuid, gen_random_uuid()))`,
		userID, hashToken(raw), exp, family); err != nil {
		return err
	}
	s.setRefreshCookie(w, raw, exp)
	return nil
}

func (s *Server) setRefreshCookie(w http.ResponseWriter, value string, exp time.Time) {
	http.SetCookie(w, s.cookie(refreshCookie, value, "/api/v1/auth", exp))
}

// cookie builds an API cookie with the deployment's attributes: httpOnly,
// Secure from COOKIE_SECURE, SameSite from COOKIE_SAMESITE and Domain from
// COOKIE_DOMAIN. An empty value builds a deletion cookie. Every cookie the API
// sets goes through here (refresh token, OAuth state), so they all work in the
// same FE/API layout.
//
// Lax suits the recommended layout (FE on tautan.id, API on api.tautan.id):
// those are the same site, so credentialed fetches from the FE carry Lax
// cookies. Use none only when the FE and API are on different registrable
// domains; Safari and other browsers that block third-party cookies may then
// drop them anyway.
func (s *Server) cookie(name, value, path string, exp time.Time) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	}
	switch s.cfg.CookieSameSite {
	case "strict":
		c.SameSite = http.SameSiteStrictMode
	case "none":
		c.SameSite = http.SameSiteNoneMode
		c.Secure = true // browsers reject SameSite=None without Secure
	}
	if value == "" {
		c.MaxAge = -1
		c.Expires = time.Unix(0, 0)
	}
	return c
}

// requireAuth validates the Bearer token and stores the user id in the context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		uid, iat, err := s.parseAccessToken(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_token", "access token is invalid or expired")
			return
		}
		// Tokens issued before the account's sessions were revoked are dead.
		// iat has second precision, so compare against the revocation second.
		var revoked *time.Time
		err = s.db.QueryRow(r.Context(), `SELECT sessions_revoked_at FROM users WHERE id = $1`, uid).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && revoked != nil && iat.Before(revoked.Truncate(time.Second))) {
			writeError(w, http.StatusUnauthorized, "invalid_token", "access token is invalid or expired")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	})
}

func userID(r *http.Request) string {
	v, _ := r.Context().Value(userIDKey).(string)
	return v
}
