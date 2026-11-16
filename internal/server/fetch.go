package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// errBlockedURL means the target is not a public http(s) address.
var errBlockedURL = errors.New("url points to a disallowed address")

const (
	fetchTimeout = 3 * time.Second
	maxRedirects = 3
	fetchUA      = "Mozilla/5.0 (compatible; TautanBot/1.0; +https://github.com/aviandri/tautan-be)"
)

// Ranges that are never fetched, on top of private/loopback/link-local/multicast.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2002::/16", "100::/64", "2001::/32",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// publicIP reports whether ip is a globally routable unicast address.
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// safeFetcher fetches public web pages. The address check runs at dial time on
// the resolved IP (so DNS rebinding and redirects can't reach internal hosts),
// only default ports are allowed, proxies are ignored, and time and size are capped.
type safeFetcher struct {
	client        *http.Client
	allowLoopback bool // tests only: lets httptest servers on 127.0.0.1 through
}

func newSafeFetcher(allowLoopback bool) *safeFetcher {
	f := &safeFetcher{allowLoopback: allowLoopback}
	dialer := &net.Dialer{
		Timeout: fetchTimeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlockedURL
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return errBlockedURL
			}
			if !publicIP(ip) && !(f.allowLoopback && ip.Unmap().IsLoopback()) {
				return errBlockedURL
			}
			return nil
		},
	}
	f.client = &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    fetchTimeout,
			ResponseHeaderTimeout:  fetchTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			return f.checkURL(req.URL)
		},
	}
	return f
}

func (f *safeFetcher) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errBlockedURL
	}
	if u.User != nil || u.Hostname() == "" {
		return errBlockedURL
	}
	if p := u.Port(); p != "" && !f.allowLoopback && !(p == "80" && u.Scheme == "http") && !(p == "443" && u.Scheme == "https") {
		return errBlockedURL
	}
	return nil
}

type fetchResult struct {
	URL         *url.URL // after redirects
	ContentType string
	Body        []byte // at most maxBytes; longer bodies are truncated
	Truncated   bool
}

func (f *safeFetcher) fetch(ctx context.Context, raw string, maxBytes int64, accept string) (*fetchResult, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errBlockedURL
	}
	if err := f.checkURL(u); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errBlockedURL
	}
	req.Header.Set("User-Agent", fetchUA)
	req.Header.Set("Accept", accept)
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedURL) {
			return nil, errBlockedURL
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil && len(body) == 0 {
		return nil, err
	}
	res := &fetchResult{URL: resp.Request.URL, ContentType: strings.ToLower(resp.Header.Get("Content-Type"))}
	if int64(len(body)) > maxBytes {
		body, res.Truncated = body[:maxBytes], true
	}
	res.Body = body
	return res, nil
}
