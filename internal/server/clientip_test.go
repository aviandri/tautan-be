package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aviandri/tautan-be/internal/config"
)

func resolver(t *testing.T, header string, trusted ...string) proxyResolver {
	t.Helper()
	p, err := config.ParseTrustedProxies(trusted)
	if err != nil {
		t.Fatal(err)
	}
	return proxyResolver{trusted: p, header: header}
}

func TestClientIPResolution(t *testing.T) {
	cases := []struct {
		name    string
		r       proxyResolver
		peer    string
		headers map[string]string
		want    string
		via     bool
	}{
		{"no proxies: XFF ignored", resolver(t, "X-Forwarded-For"), "203.0.113.9:5000",
			map[string]string{"X-Forwarded-For": "1.1.1.1"}, "203.0.113.9", false},
		{"untrusted peer: XFF ignored", resolver(t, "X-Forwarded-For", "private"), "203.0.113.9:5000",
			map[string]string{"X-Forwarded-For": "1.1.1.1"}, "203.0.113.9", false},
		{"caddy on loopback", resolver(t, "X-Forwarded-For", "private"), "127.0.0.1:5000",
			map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7", true},
		{"spoofed left entry skipped", resolver(t, "X-Forwarded-For", "private"), "10.0.0.2:5000",
			map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.7"}, "198.51.100.7", true},
		{"cloudflare then caddy", resolver(t, "X-Forwarded-For", "private", "cloudflare"), "172.18.0.3:5000",
			map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.7, 162.158.1.2"}, "198.51.100.7", true},
		{"ipv6 client via fly 6pn", resolver(t, "X-Forwarded-For", "private"), "[fdaa:0:1::3]:5000",
			map[string]string{"X-Forwarded-For": "2001:db8::1"}, "2001:db8::1", true},
		{"single header from trusted peer", resolver(t, "CF-Connecting-IP", "private"), "127.0.0.1:5000",
			map[string]string{"CF-Connecting-IP": "198.51.100.7"}, "198.51.100.7", true},
		{"single header from untrusted peer", resolver(t, "CF-Connecting-IP", "private"), "203.0.113.9:5000",
			map[string]string{"CF-Connecting-IP": "6.6.6.6"}, "203.0.113.9", false},
		{"garbage header falls back to peer", resolver(t, "CF-Connecting-IP", "private"), "127.0.0.1:5000",
			map[string]string{"CF-Connecting-IP": "nope"}, "127.0.0.1", true},
		{"legacy star trusts all", resolver(t, "X-Forwarded-For", "*"), "203.0.113.9:5000",
			map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7", true},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.peer
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		got, via := c.r.resolve(req)
		if got != c.want || via != c.via {
			t.Errorf("%s: got %s via=%v, want %s via=%v", c.name, got, via, c.want, c.via)
		}
	}
}

func TestParseTrustedProxiesRejectsJunk(t *testing.T) {
	if _, err := config.ParseTrustedProxies([]string{"10.0.0.0/8", "not-an-ip"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestVisitorCountryOnlyFromTrustedProxy(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.CountryHeader = "CF-IPCountry"
	s.proxy = resolver(t, "X-Forwarded-For", "private")
	var got string
	h := s.proxy.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = s.visitorCountry(r) }))
	for _, c := range []struct{ peer, hdr, want string }{
		{"127.0.0.1:1", "id", "ID"},
		{"203.0.113.9:1", "ID", ""}, // direct visitor spoofing the header
		{"127.0.0.1:1", "XX", ""},
		{"127.0.0.1:1", "T1", ""},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.peer
		req.Header.Set("CF-IPCountry", c.hdr)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != c.want {
			t.Errorf("peer %s header %s: got %q, want %q", c.peer, c.hdr, got, c.want)
		}
	}
}

func TestCookieAttributes(t *testing.T) {
	s, _ := newTestServer(t)
	exp := time.Now().Add(time.Hour)
	c := s.cookie("x", "v", "/p", exp)
	if c.SameSite != http.SameSiteLaxMode || c.Domain != "" || !c.HttpOnly {
		t.Fatalf("default cookie %+v", c)
	}
	s.cfg.CookieSecure, s.cfg.CookieDomain = true, ".tautan.id"
	if c = s.cookie("x", "v", "/p", exp); !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Domain != ".tautan.id" {
		t.Fatalf("secure cookie must stay Lax with the domain set: %+v", c)
	}
	s.cfg.CookieSecure, s.cfg.CookieSameSite = false, "none"
	if c = s.cookie("x", "v", "/p", exp); c.SameSite != http.SameSiteNoneMode || !c.Secure {
		t.Fatalf("SameSite=None must force Secure: %+v", c)
	}
	rec := httptest.NewRecorder()
	s.setOAuthCookie(rec, "state", exp)
	if oc := rec.Result().Cookies(); len(oc) != 1 || oc[0].Name != oauthCookie || oc[0].SameSite != http.SameSiteNoneMode || !oc[0].Secure {
		t.Fatalf("OAuth state cookie must follow COOKIE_SAMESITE: %+v", oc)
	}
	if c = s.cookie("x", "", "/p", exp); c.MaxAge != -1 {
		t.Fatalf("empty value must delete: %+v", c)
	}
}
