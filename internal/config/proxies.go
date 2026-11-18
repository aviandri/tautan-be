package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// Cloudflare's published edge ranges (https://www.cloudflare.com/ips/, checked
// 2026-11). They change rarely; if Cloudflare adds a range, list it explicitly
// in TRUSTED_PROXIES until this list is updated.
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// Loopback, RFC 1918, CGNAT (used by some PaaS internal networks), IPv6 ULA
// (Fly's 6PN is fdaa::/16) and link-local: where a same-host or same-network
// reverse proxy (Caddy, Fly's proxy, a Docker network) connects from.
var privateRanges = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"::1/128", "fc00::/7", "fe80::/10",
}

// ParseTrustedProxies turns TRUSTED_PROXIES into prefixes. Entries are IPs,
// CIDRs or the keywords "private", "cloudflare" and "*" (trust every peer;
// only when the app is unreachable except through your proxy).
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	add := func(list []string) {
		for _, s := range list {
			out = append(out, netip.MustParsePrefix(s))
		}
	}
	for _, e := range entries {
		switch strings.ToLower(e) {
		case "private":
			add(privateRanges)
		case "cloudflare":
			add(cloudflareRanges)
		case "*":
			add([]string{"0.0.0.0/0", "::/0"})
		default:
			if p, err := netip.ParsePrefix(e); err == nil {
				out = append(out, p.Masked())
			} else if a, err := netip.ParseAddr(e); err == nil {
				out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			} else {
				return nil, fmt.Errorf("TRUSTED_PROXIES: %q is not an IP, CIDR, private, cloudflare or *", e)
			}
		}
	}
	return out, nil
}
