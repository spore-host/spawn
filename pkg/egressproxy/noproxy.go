package egressproxy

import (
	"net/netip"
	"strings"
)

// NoProxyBypasses reports whether curl, given noProxy as its no_proxy, would
// fetch host directly instead of through the proxy (#1515 phase 4). The
// member bootstrap scopes http_proxy/https_proxy/no_proxy to a few commands of
// its own (curl, the EFA installer's dnf or apt), and every host they reach
// that a VPC endpoint serves (S3, above all: AL2023's package repositories
// are S3 buckets) must bypass the proxy, which would refuse it, while every
// host on [Allowlist] must not.
//
// It mirrors curl's rule (lib/noproxy.c, curl 7.86 and later, which AL2023's
// and Ubuntu 22.04/24.04's curl are), which libcurl-based dnf shares:
//
//   - "*" alone bypasses every host;
//   - entries are separated by commas or spaces, and one leading dot and one
//     trailing dot of each are ignored;
//   - for a host name, an entry matches the name itself and any name ending
//     in "." plus the entry, case-insensitively, and nothing else (a plain
//     suffix such as "amazonaws.com" does not match "notamazonaws.com");
//   - for an IP address, an entry matches as an address or a CIDR prefix, and
//     never as a name.
func NoProxyBypasses(noProxy, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return false
	}
	if strings.TrimSpace(noProxy) == "*" {
		return true
	}
	ip, ipErr := netip.ParseAddr(host)
	for _, tok := range strings.FieldsFunc(noProxy, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		tok = strings.ToLower(tok)
		if ipErr == nil {
			if p, err := netip.ParsePrefix(tok); err == nil {
				if p.Contains(ip) {
					return true
				}
			} else if a, err := netip.ParseAddr(tok); err == nil && a == ip {
				return true
			}
			continue
		}
		tok = strings.TrimSuffix(strings.TrimPrefix(tok, "."), ".")
		if tok == "" {
			continue
		}
		if host == tok || strings.HasSuffix(host, "."+tok) {
			return true
		}
	}
	return false
}
