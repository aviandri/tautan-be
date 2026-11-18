package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// proxyResolver works out the visitor's IP and whether proxy-set headers
// (the client IP header, the country header) can be believed.
//
// A header is believed only when the TCP peer is a trusted proxy, so a
// visitor talking to the API directly can't spoof their IP (rate limits,
// unique-visitor hashes) or country (analytics).
//
// With X-Forwarded-For the list is walked from the right, skipping trusted
// hops; the first untrusted address is the client. Left-most entries are
// whatever the client sent and are never used while an untrusted hop is to
// their right. Single-IP headers (CF-Connecting-IP, Fly-Client-IP,
// X-Real-IP) are taken as is from a trusted peer.
type proxyResolver struct {
	trusted []netip.Prefix
	header  string
}

type proxyCtxKey struct{}

func (p proxyResolver) isTrusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, pr := range p.trusted {
		if pr.Contains(a) {
			return true
		}
	}
	return false
}

// resolve returns the client IP and whether the peer was a trusted proxy.
func (p proxyResolver) resolve(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || len(p.trusted) == 0 || !p.isTrusted(peer) {
		return host, false
	}
	if !strings.EqualFold(p.header, "X-Forwarded-For") {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(p.header))); err == nil {
			return a.Unmap().String(), true
		}
		return peer.Unmap().String(), true
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer.Unmap()
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // garbage: stop at the last address we could vouch for
		}
		client = a.Unmap()
		if !p.isTrusted(client) {
			break
		}
	}
	return client.String(), true
}

// middleware rewrites RemoteAddr to the resolved client IP (so clientIP and
// the access log see the visitor) and records whether the peer was trusted.
func (p proxyResolver) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, viaProxy := p.resolve(r)
		r.RemoteAddr = net.JoinHostPort(ip, "0")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyCtxKey{}, viaProxy)))
	})
}

func viaTrustedProxy(r *http.Request) bool {
	v, _ := r.Context().Value(proxyCtxKey{}).(bool)
	return v
}

// visitorCountry is the ISO country code from COUNTRY_HEADER (CF-IPCountry,
// X-Vercel-IP-Country, ...), or "" when unknown or not from a trusted proxy.
func (s *Server) visitorCountry(r *http.Request) string {
	if !viaTrustedProxy(r) {
		return ""
	}
	c := strings.ToUpper(strings.TrimSpace(r.Header.Get(s.cfg.CountryHeader)))
	if !countryRE.MatchString(c) || c == "XX" || c == "T1" {
		return "" // XX unknown, T1 Tor (Cloudflare)
	}
	return c
}
