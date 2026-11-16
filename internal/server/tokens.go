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

func (s *Server) parseAccessToken(tok string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("tautan"), jwt.WithTimeFunc(s.now))
	if err != nil {
		return "", err
	}
	if claims.Subject == "" {
		return "", errors.New("missing subject")
	}
	return claims.Subject, nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// issueRefreshToken stores a new refresh token and sets it as an httpOnly cookie.
func (s *Server) issueRefreshToken(ctx context.Context, w http.ResponseWriter, userID string) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	exp := s.now().Add(s.cfg.RefreshTTL)
	if _, err := s.db.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, hashToken(raw), exp); err != nil {
		return err
	}
	s.setRefreshCookie(w, raw, exp)
	return nil
}

func (s *Server) setRefreshCookie(w http.ResponseWriter, value string, exp time.Time) {
	c := &http.Cookie{
		Name:     refreshCookie,
		Value:    value,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	}
	if s.cfg.CookieSecure {
		// Cross-site deployments (FE and API on different sites) need SameSite=None.
		c.SameSite = http.SameSiteNoneMode
	}
	if value == "" {
		c.MaxAge = -1
		c.Expires = time.Unix(0, 0)
	}
	http.SetCookie(w, c)
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
		uid, err := s.parseAccessToken(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_token", "access token is invalid or expired")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	})
}

func userID(r *http.Request) string {
	v, _ := r.Context().Value(userIDKey).(string)
	return v
}
