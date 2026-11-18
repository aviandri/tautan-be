package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.0.0.5": false, "172.16.3.4": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "0.1.2.3": false, "224.0.0.1": false, "255.255.255.255": false, "198.18.0.1": false,
		"::1": false, "::": false, "fe80::1": false, "fc00::1": false, "fd12::1": false, "::ffff:127.0.0.1": false,
		"::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false, "ff02::1": false,
		// audit L4: IPv4-compatible, SIIT, site-local, 6to4 relay
		"::127.0.0.1": false, "::ffff:0:7f00:1": false, "fec0::1": false, "192.88.99.1": false, "2a00:1450:4001::1": true,
	} {
		if got := publicIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestSafeFetcherBlocks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer ts.Close()
	f := newSafeFetcher(false)
	ctx := context.Background()
	port := ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	for _, u := range []string{
		ts.URL, // 127.0.0.1:<port>: blocked port and loopback
		"http://localhost/", "http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/", "http://0.0.0.0/", "http://example.com:" + port + "/",
		"ftp://example.com/", "file:///etc/passwd", "http://user:pass@example.com/", "gopher://127.0.0.1/",
	} {
		_, err := f.fetch(ctx, u, 1024, "*/*")
		if !errors.Is(err, errBlockedURL) {
			t.Errorf("%s: err = %v, want blocked", u, err)
		}
	}
	// Decimal-integer hosts aren't IP literals to Go's resolver: they must simply fail.
	if _, err := f.fetch(ctx, "http://2130706433/", 1024, "*/*"); err == nil {
		t.Error("decimal host should not be fetched")
	}
}

func TestSafeFetcherRedirectToInternal(t *testing.T) {
	// A loopback-allowed fetcher (as in tests) still refuses redirects to other schemes,
	// and a normal fetcher refuses the loopback hop at dial time.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "internal") }))
	defer internal.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
	}))
	defer redir.Close()
	lf := newSafeFetcher(true)
	if _, err := lf.fetch(context.Background(), redir.URL+"/?to="+url.QueryEscape("file:///etc/passwd"), 1024, "*/*"); err == nil {
		t.Error("redirect to file:// should fail")
	}
	// Redirect loop hits the cap.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/again", http.StatusFound) }))
	defer loop.Close()
	if _, err := lf.fetch(context.Background(), loop.URL, 1024, "*/*"); err == nil {
		t.Error("redirect loop should fail")
	}
	// Size cap truncates.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("a", 5000))) }))
	defer big.Close()
	res, err := lf.fetch(context.Background(), big.URL, 1000, "*/*")
	if err != nil || len(res.Body) != 1000 || !res.Truncated {
		t.Errorf("cap: err=%v len=%d", err, len(res.Body))
	}
	// Slow server hits the timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	start := time.Now()
	if _, err := lf.fetch(context.Background(), slow.URL, 1000, "*/*"); err == nil || time.Since(start) > 4*time.Second {
		t.Errorf("timeout: err=%v after %v", err, time.Since(start))
	}
}

func TestClassifyAndParseMeta(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.youtube.com/watch?v=x": "video", "https://youtu.be/x": "video", "https://vimeo.com/1": "video",
		"https://open.spotify.com/track/1": "music", "https://artist.bandcamp.com/album/a": "music", "https://soundcloud.com/a/b": "music",
		"https://calendly.com/a/30": "booking", "https://cdn.example/x.JPG": "image", "https://example.com/": "link",
	} {
		u, _ := url.Parse(raw)
		if got := classifyURL(u); got != want {
			t.Errorf("%s = %s, want %s", raw, got, want)
		}
	}
	base, _ := url.Parse("https://shop.example/p/1")
	m := parseMeta([]byte(`<html><head><title> Fallback  title </title>
		<meta name="description" content="plain desc">
		<meta property="og:title" content="OG Title">
		<meta property="og:image" content="/img/cover.png">
		<meta property="og:site_name" content="Shop">
		</head><body><meta property="og:title" content="ignored"></body></html>`), base)
	if m.Title != "OG Title" || m.Description != "plain desc" || m.ImageURL != "https://shop.example/img/cover.png" || m.SiteName != "Shop" {
		t.Errorf("meta = %+v", m)
	}
	m = parseMeta([]byte(`<title>Only title</title><meta name="twitter:image" content="javascript:alert(1)">`), base)
	if m.Title != "Only title" || m.ImageURL != "" {
		t.Errorf("fallback meta = %+v", m)
	}
}

func TestUnfurlEndpoint(t *testing.T) {
	s, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes())
		default:
			if r.Header.Get("User-Agent") == "" || !strings.Contains(r.Header.Get("User-Agent"), "TautanBot") {
				http.Error(w, "no ua", 400)
				return
			}
			fmt.Fprint(w, `<html><head><meta property="og:title" content="My Shop"><meta property="og:description" content="Best stuff"><meta property="og:image" content="/c.jpg"></head></html>`)
		}
	}))
	defer site.Close()

	// Default fetcher: the loopback test server is refused.
	if c := do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": site.URL}, tok).expect(400).errCode(); c != "blocked_url" {
		t.Errorf("code = %s", c)
	}
	s.fetcher = newSafeFetcher(true)
	var out unfurlResult
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": site.URL + "/page"}, tok).expect(200).json(&out)
	if !out.Fetched || out.Title != "My Shop" || out.Description != "Best stuff" || out.ImageURL != site.URL+"/c.jpg" || out.SuggestedType != "link" {
		t.Errorf("unfurl = %+v", out)
	}
	out = unfurlResult{}
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": site.URL + "/img.png"}, tok).expect(200).json(&out)
	if out.SuggestedType != "image" || out.ImageURL == "" {
		t.Errorf("image = %+v", out)
	}
	out = unfurlResult{}
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "Hello@Example.com"}, tok).expect(200).json(&out)
	if out.URL != "mailto:Hello@Example.com" || out.DestKind != "email" || out.Fetched {
		t.Errorf("email = %+v", out)
	}
	// Unreachable host still returns a suggestion.
	site.Close()
	out = unfurlResult{}
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": site.URL}, tok).expect(200).json(&out)
	if out.Fetched || out.SuggestedType != "link" {
		t.Errorf("unreachable = %+v", out)
	}
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": ""}, tok).expect(400)
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "javascript:alert(1)"}, tok).expect(400)
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "not@@email"}, tok).expect(400)
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "https://example.com"}).expect(401)

	s.unfurlLimiter = newRateLimiter(1, time.Minute)
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "a@b.co"}, tok).expect(200)
	do(t, h, "POST", "/api/v1/unfurl", map[string]string{"url": "a@b.co"}, tok).expect(429)
}

func TestMarketplaceUnfurlHelpers(t *testing.T) {
	cases := map[string]string{
		"https://shopee.co.id/kaos-i.1.2":          "shopee",
		"https://shp.ee/abc":                       "shopee",
		"https://www.tokopedia.com/toko/kaos":      "tokopedia",
		"https://tk.tokopedia.com/ZSabc/":          "tokopedia",
		"https://shop.tiktok.com/view/product/123": "tiktok_shop",
		"https://www.tiktok.com/view/product/123":  "tiktok_shop",
		"https://www.tiktok.com/@me/video/1":       "",
		"https://s.lazada.co.id/abc":               "lazada",
		"https://www.blibli.com/p/x":               "blibli",
		"https://www.bukalapak.com/p/x":            "bukalapak",
		"https://www.etsy.com/listing/1":           "etsy",
		"https://amzn.to/x":                        "amazon",
		"https://www.amazon.co.jp/dp/x":            "amazon",
		"https://me.gumroad.com/l/x":               "gumroad",
		"https://shopee-fake.example.com/x":        "",
		"https://example.com/shopee":               "",
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := marketplacePlatform(u); got != want {
			t.Errorf("%s => %q, want %q", raw, got, want)
		}
	}
	for in, want := range map[string]int64{"85000": 8500000, "85000.00": 8500000, "85,000.5": 8500050, "0.99": 99} {
		if got := parsePriceMinor(in); got == nil || *got != want {
			t.Errorf("parsePriceMinor(%q) = %v, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "abc", "1.234", "-5", "1e5"} {
		if parsePriceMinor(in) != nil {
			t.Errorf("parsePriceMinor(%q) should fail", in)
		}
	}
	m := parseMeta([]byte(`<head><meta property="og:type" content="product"><meta property="product:price:amount" content="85000"><meta property="product:price:currency" content="idr"></head>`), nil)
	if m.Price == nil || *m.Price != 8500000 || m.Currency != "IDR" || m.ogType != "product" {
		t.Errorf("meta = %+v", m)
	}
}
