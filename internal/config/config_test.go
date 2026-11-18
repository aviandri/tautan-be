package config

import (
	"strings"
	"testing"
)

func prodConfig() Config {
	return Config{
		Env:            "production",
		DatabaseURL:    "postgres://x",
		JWTSecret:      strings.Repeat("k", 32),
		PublicBaseURL:  "https://api.tautan.id",
		AppBaseURL:     "https://tautan.id",
		CORSOrigins:    []string{"https://tautan.id"},
		CookieSecure:   true,
		LogFormat:      "json",
		CookieSameSite: "lax",

		PaymentsEncryptionKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
	}
}

func TestValidateProductionOK(t *testing.T) {
	if err := prodConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProductionRejects(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"dev jwt":         func(c *Config) { c.JWTSecret = devJWTSecret },
		"short jwt":       func(c *Config) { c.JWTSecret = "short" },
		"http base":       func(c *Config) { c.PublicBaseURL = "http://api.tautan.id" },
		"origin path":     func(c *Config) { c.CORSOrigins = []string{"https://tautan.id/"} },
		"no secure":       func(c *Config) { c.CookieSecure = false },
		"no database":     func(c *Config) { c.DatabaseURL = "" },
		"local origin":    func(c *Config) { c.CORSOrigins = []string{"http://localhost:3000"} },
		"bad samesite":    func(c *Config) { c.CookieSameSite = "lox" },
		"no payments key": func(c *Config) { c.PaymentsEncryptionKey = "" },
		"fake payments":   func(c *Config) { c.PaymentsFake = true },
	} {
		c := prodConfig()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFakePayments(t *testing.T) {
	for _, tc := range []struct {
		env  string
		flag bool
		want bool
	}{{"development", false, false}, {"development", true, true}, {"production", true, false}, {"staging", true, true}} {
		if got := (Config{Env: tc.env, PaymentsFake: tc.flag}).FakePayments(); got != tc.want {
			t.Errorf("env=%s flag=%v: FakePayments() = %v", tc.env, tc.flag, got)
		}
	}
}

func TestValidateDevelopmentLenient(t *testing.T) {
	c := Config{Env: "development", DatabaseURL: "postgres://x", JWTSecret: devJWTSecret}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestListTrims(t *testing.T) {
	got := list(" https://a.id , ,https://b.id")
	if len(got) != 2 || got[0] != "https://a.id" || got[1] != "https://b.id" {
		t.Fatalf("got %q", got)
	}
}
