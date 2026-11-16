package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9_.]{3,30}$`)

var reservedUsernames = map[string]bool{
	"admin": true, "dashboard": true, "login": true, "signup": true,
	"api": true, "settings": true, "uploads": true,
}

// validUsername normalises u to lowercase and reports whether it is allowed.
func validUsername(u string) (string, bool) {
	u = strings.ToLower(strings.TrimSpace(u))
	return u, usernameRE.MatchString(u) && !reservedUsernames[u] && !strings.HasPrefix(u, tempUsernamePrefix)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

type authResponse struct {
	AccessToken string  `json:"access_token"`
	User        User    `json:"user"`
	Profile     Profile `json:"profile"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password, Username string }
	if !decodeJSON(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		writeError(w, http.StatusBadRequest, "invalid_email", "email address is invalid")
		return
	}
	if len(in.Password) < 8 || len(in.Password) > 72 {
		writeError(w, http.StatusBadRequest, "invalid_password", "password must be 8 to 72 characters")
		return
	}
	// The username is optional: without one the account gets a temporary
	// username and claims a real one at the end of onboarding.
	onboarding := Onboarding{UsernameClaimed: true}
	username, ok := validUsername(in.Username)
	if strings.TrimSpace(in.Username) == "" {
		username, ok = tempUsername(), true
		onboarding = Onboarding{UsernameClaimed: false, SuggestedUsername: s.suggestUsername(r.Context(), email)}
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_username", "username must be 3-30 chars of a-z, 0-9, _ or . and not reserved")
		return
	}
	onboardingJSON, _ := json.Marshal(onboarding)
	if onboarding.UsernameClaimed {
		if taken, err := s.usernameTaken(r.Context(), username, ""); err != nil {
			s.internalError(w, err)
			return
		} else if taken {
			writeError(w, http.StatusConflict, "username_taken", "this username is taken")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		s.internalError(w, err)
		return
	}

	ctx := r.Context()
	var u User
	var p Profile
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, email, created_at`,
			email, string(hash)).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return err
		}
		p, err = scanProfile(tx.QueryRow(ctx,
			`INSERT INTO profiles (user_id, username, display_name, theme, onboarding) VALUES ($1, $2, $3, $4, $5) RETURNING `+profileCols,
			u.ID, username, displayNameFor(username), DefaultTheme, onboardingJSON))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO tabs (profile_id, title, slug, position) VALUES ($1, 'Links', 'links', 0)`, p.ID)
		return err
	})
	switch {
	case isUniqueViolation(err, "users_email_key"):
		writeError(w, http.StatusConflict, "email_taken", "an account with this email already exists")
		return
	case isUniqueViolation(err, "profiles_username_key"):
		writeError(w, http.StatusConflict, "username_taken", "this username is taken")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.respondWithSession(ctx, w, http.StatusCreated, u, p)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !decodeJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	var u User
	var hash string
	err := s.db.QueryRow(ctx, `SELECT id, email, created_at, password_hash FROM users WHERE email = $1`,
		strings.TrimSpace(in.Email)).Scan(&u.ID, &u.Email, &u.CreatedAt, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.internalError(w, err)
		return
	}
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	p, err := s.profileForUser(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.respondWithSession(ctx, w, http.StatusOK, u, p)
}

func (s *Server) respondWithSession(ctx context.Context, w http.ResponseWriter, status int, u User, p Profile) {
	tok, err := s.issueAccessToken(u.ID)
	if err == nil {
		err = s.issueRefreshToken(ctx, w, u.ID)
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, status, authResponse{AccessToken: tok, User: u, Profile: p})
}

// handleRefresh rotates the refresh token: the presented one is revoked and a new one is set.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(refreshCookie)
	if err != nil || c.Value == "" {
		writeError(w, http.StatusUnauthorized, "no_refresh_token", "refresh token missing")
		return
	}
	ctx := r.Context()
	var uid string
	err = s.db.QueryRow(ctx,
		`UPDATE refresh_tokens SET revoked_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2
		 RETURNING user_id`, hashToken(c.Value), s.now()).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		s.setRefreshCookie(w, "", s.now())
		writeError(w, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	tok, err := s.issueAccessToken(uid)
	if err == nil {
		err = s.issueRefreshToken(ctx, w, uid)
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": tok})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(refreshCookie); err == nil && c.Value != "" {
		if _, err := s.db.Exec(r.Context(),
			`UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
			hashToken(c.Value)); err != nil {
			s.internalError(w, err)
			return
		}
	}
	s.setRefreshCookie(w, "", s.now())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsernameAvailable(w http.ResponseWriter, r *http.Request) {
	u, ok := validUsername(r.URL.Query().Get("u"))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]bool{"available": false})
		return
	}
	exists, err := s.usernameTaken(r.Context(), u, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": !exists})
}

// usernameTaken reports whether u is used by a profile other than exceptProfile.
func (s *Server) usernameTaken(ctx context.Context, u, exceptProfile string) (bool, error) {
	var taken bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM profiles WHERE username = $1 AND id::text <> $2)
		OR EXISTS (SELECT 1 FROM username_redirects WHERE old_username = $1 AND profile_id::text <> $2)`, u, exceptProfile).Scan(&taken)
	return taken, err
}

// displayNameFor is the initial display name: the username, or empty for a temporary one.
func displayNameFor(username string) string {
	if strings.HasPrefix(username, tempUsernamePrefix) {
		return ""
	}
	return username
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var u User
	err := s.db.QueryRow(ctx, `SELECT id, email, created_at FROM users WHERE id = $1`, userID(r)).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "user no longer exists")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	p, err := s.profileForUser(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "profile": p})
}

func (s *Server) profileForUser(ctx context.Context, uid string) (Profile, error) {
	return scanProfile(s.db.QueryRow(ctx, `SELECT `+profileCols+` FROM profiles WHERE user_id = $1 ORDER BY created_at LIMIT 1`, uid))
}
