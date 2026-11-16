// Package config loads runtime configuration from environment variables.
package config

import (
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
}

func Load() Config {
	c := Config{
		Addr:          env("ADDR", ":8080"),
		DatabaseURL:   env("DATABASE_URL", ""),
		JWTSecret:     env("JWT_SECRET", "dev-secret-change-me"),
		AccessTTL:     dur("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:    dur("REFRESH_TTL", 30*24*time.Hour),
		CORSOrigins:   strings.Split(env("CORS_ORIGINS", "http://localhost:3000"), ","),
		UploadDir:     env("UPLOAD_DIR", "./uploads"),
		PublicBaseURL: strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		CookieSecure:  env("COOKIE_SECURE", "false") == "true",
		CountryHeader: env("COUNTRY_HEADER", "CF-IPCountry"),
		TrustProxy:    env("TRUST_PROXY", "false") == "true",
		AppBaseURL:    strings.TrimRight(env("APP_BASE_URL", "http://localhost:3000"), "/"),
	}
	c.RevalidateURL = env("REVALIDATE_URL", c.AppBaseURL+"/api/revalidate")
	c.RevalidateSecret = env("REVALIDATE_SECRET", "")
	return c
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
