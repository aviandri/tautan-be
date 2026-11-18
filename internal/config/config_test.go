package config

import (
	"strings"
	"testing"
)

func prodConfig() Config {
	return Config{
		Env:           "production",
		DatabaseURL:   "postgres://x",
		JWTSecret:     strings.Repeat("k", 32),
		PublicBaseURL: "https://api.tautan.id",
		AppBaseURL:    "https://tautan.id",
		CORSOrigins:   []string{"https://tautan.id"},
		CookieSecure:  true,
		LogFormat:     "json",
	}
}

func TestValidateProductionOK(t *testing.T) {
	if err := prodConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProductionRejects(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"dev jwt":      func(c *Config) { c.JWTSecret = devJWTSecret },
		"short jwt":    func(c *Config) { c.JWTSecret = "short" },
		"http base":    func(c *Config) { c.PublicBaseURL = "http://api.tautan.id" },
		"origin path":  func(c *Config) { c.CORSOrigins = []string{"https://tautan.id/"} },
		"no secure":    func(c *Config) { c.CookieSecure = false },
		"no database":  func(c *Config) { c.DatabaseURL = "" },
		"local origin": func(c *Config) { c.CORSOrigins = []string{"http://localhost:3000"} },
	} {
		c := prodConfig()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
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
