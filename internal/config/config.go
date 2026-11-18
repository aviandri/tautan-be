// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Addr          string        // HTTP listen address, e.g. ":8080"
	DatabaseURL   string        // postgres://...
	JWTSecret     string        // HMAC secret for access tokens
	AccessTTL     time.Duration // access token lifetime
	RefreshTTL    time.Duration // refresh token lifetime
	CORSOrigins   []string      // allowed browser origins (credentials enabled)
	UploadDir     string        // local directory for uploaded files
	PublicBaseURL string        // base URL used to build absolute upload URLs
	CookieSecure  bool          // set Secure on the refresh cookie
	CountryHeader string        // request header carrying the visitor country code
	TrustProxy    bool          // honour X-Forwarded-For / X-Real-IP (only behind a proxy you control)
	AppBaseURL    string        // public FE origin, used for page URLs (vCard)
	// Revalidate webhook (perf 2c): POSTed to RevalidateURL, signed with
	// RevalidateSecret. Disabled while the secret is empty.
	RevalidateURL    string
	RevalidateSecret string
	// Google sign-in (P2-8). The feature is off unless all three are set.
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string // the FE callback page, e.g. http://localhost:3000/auth/google/callback
	GoogleIssuer       string // OIDC issuer used for discovery; tests point it at a fake server

	// Operations.
	Env             string        // APP_ENV: "development" (default) or "production" (strict Validate)
	LogFormat       string        // "json" (default in production) or "text"
	LogLevel        string        // debug | info | warn | error
	MigrateOnStart  bool          // apply pending migrations before serving (default true)
	ShutdownDelay   time.Duration // after SIGTERM: report not-ready for this long before closing listeners
	ShutdownTimeout time.Duration // max time to drain in-flight requests
}

// GoogleEnabled reports whether Google sign-in is configured.
func (c Config) GoogleEnabled() bool {
	return c.GoogleClientID != "" && c.GoogleClientSecret != "" && c.GoogleRedirectURL != ""
}

// Production reports whether APP_ENV=production.
func (c Config) Production() bool { return c.Env == "production" }

func Load() Config {
	c := Config{
		Addr:          env("ADDR", ":8080"),
		DatabaseURL:   env("DATABASE_URL", ""),
		JWTSecret:     env("JWT_SECRET", devJWTSecret),
		AccessTTL:     dur("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:    dur("REFRESH_TTL", 30*24*time.Hour),
		CORSOrigins:   list(env("CORS_ORIGINS", "http://localhost:3000")),
		UploadDir:     env("UPLOAD_DIR", "./uploads"),
		PublicBaseURL: strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		CookieSecure:  env("COOKIE_SECURE", "false") == "true",
		CountryHeader: env("COUNTRY_HEADER", "CF-IPCountry"),
		TrustProxy:    env("TRUST_PROXY", "false") == "true",
		AppBaseURL:    strings.TrimRight(env("APP_BASE_URL", "http://localhost:3000"), "/"),
	}
	c.RevalidateURL = env("REVALIDATE_URL", env("FRONTEND_REVALIDATE_URL", c.AppBaseURL+"/api/revalidate"))
	c.RevalidateSecret = env("REVALIDATE_SECRET", "")
	c.GoogleClientID = env("GOOGLE_CLIENT_ID", "")
	c.GoogleClientSecret = env("GOOGLE_CLIENT_SECRET", "")
	c.GoogleRedirectURL = env("GOOGLE_REDIRECT_URL", "")
	c.GoogleIssuer = strings.TrimRight(env("GOOGLE_ISSUER", "https://accounts.google.com"), "/")
	c.Env = strings.ToLower(env("APP_ENV", "development"))
	defFormat := "text"
	if c.Production() {
		defFormat = "json"
	}
	c.LogFormat = strings.ToLower(env("LOG_FORMAT", defFormat))
	c.LogLevel = strings.ToLower(env("LOG_LEVEL", "info"))
	c.MigrateOnStart = env("MIGRATE_ON_START", "true") == "true"
	c.ShutdownDelay = dur("SHUTDOWN_DELAY", 0)
	c.ShutdownTimeout = dur("SHUTDOWN_TIMEOUT", 15*time.Second)
	return c
}

const devJWTSecret = "dev-secret-change-me"

// Validate rejects settings that are unsafe or broken in production. In
// development it accepts everything, so `make dev` keeps working with no env.
func (c Config) Validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if !c.Production() {
		return errors.Join(errs...)
	}
	if c.JWTSecret == devJWTSecret || len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET must be set to a random value of at least 32 characters"))
	}
	for k, v := range map[string]string{"PUBLIC_BASE_URL": c.PublicBaseURL, "APP_BASE_URL": c.AppBaseURL} {
		if u, err := url.Parse(v); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an https:// URL (got %q)", k, v))
		}
	}
	for _, o := range c.CORSOrigins {
		if u, err := url.Parse(strings.TrimSpace(o)); err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS entries must be https origins without a path (got %q)", o))
		}
	}
	if !c.CookieSecure {
		errs = append(errs, errors.New("COOKIE_SECURE must be true in production"))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text (got %q)", c.LogFormat))
	}
	return errors.Join(errs...)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return d
	}
	return def
}

// list splits a comma-separated value, trimming spaces and dropping empties.
func list(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
