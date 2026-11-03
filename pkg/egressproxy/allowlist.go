// Package egressproxy is the forward proxy `spored egress-proxy` runs (#1515):
// it lets a --no-public-ip instance fetch the few things spawn's own bootstrap
// needs from outside AWS, and nothing else.
//
// It speaks two things, both limited to the hosts in [Allowlist]:
//
//   - HTTPS CONNECT to port 443, tunnelled byte for byte. TLS is never
//     terminated or inspected; the client verifies the origin's certificate.
//   - Plain-HTTP forwarding of an absolute-URI GET or HEAD to port 80, which
//     Ubuntu's EC2 apt mirrors need: they are http:// by default.
//
// Every destination is resolved once, refused if ANY of its addresses is not a
// public unicast address (loopback, link-local and so IMDS, RFC 1918, CGNAT,
// unspecified and the like, see [publicAddr]), and then dialled by the vetted
// address itself, never by name, so a DNS answer that changes between the check
// and the dial (rebinding) cannot reach the instance network.
//
// Standard library only.
package egressproxy

import (
	"net/netip"
	"strings"
)

// Rule is one allowlist entry.
type Rule struct {
	// Pattern is either an exact host ("awscli.amazonaws.com") or a one-label
	// wildcard ("*.ec2.archive.ubuntu.com"), which matches exactly one
	// non-empty DNS label in place of the "*" and nothing else: not the bare
	// suffix, not two labels, and never a host that merely ends in the same
	// characters.
	Pattern string
	// Why is the reason the host is on the list: what fetches from it.
	Why string
}

// Allowlist is every host the egress proxy forwards to, and the one list the
// member bootstrap's proxied fetches are checked against (#1515 phase 4): a
// fetch spawn adds to the bootstrap through the proxy must add its host here,
// with a reason. Tests in pkg/userdata and pkg/launcher hold the EFA installer
// and AWS CLI origins those packages fetch from to this list.
//
// Nothing else is reachable: no other AWS service (those go through VPC
// endpoints, which --no-public-ip already requires), no package repository
// beyond Ubuntu's, and no user-supplied URL. The user's own job is never
// proxied.
var Allowlist = []Rule{
	{Pattern: "efa-installer.amazonaws.com",
		Why: "the pinned EFA installer tarball --efa installs (pkg/userdata efaInstallerOrigin), over HTTPS"},
	{Pattern: "awscli.amazonaws.com",
		Why: "the pinned AWS CLI v2 zip the Linux bootstrap installs on an image with no aws CLI, such as stock Ubuntu (pkg/launcher spawn_need_aws), over HTTPS"},
	{Pattern: "*.ec2.archive.ubuntu.com",
		Why: "the regional amd64 apt mirror cloud-init writes on Ubuntu EC2 images (http://<region>.ec2.archive.ubuntu.com/ubuntu): the MPI and EFA installs run apt-get against it, over HTTP"},
	{Pattern: "*.ec2.ports.ubuntu.com",
		Why: "the regional arm64 apt mirror on Ubuntu EC2 images (http://<region>.ec2.ports.ubuntu.com/ubuntu-ports), over HTTP"},
	{Pattern: "archive.ubuntu.com",
		Why: "Ubuntu's primary amd64 archive, which an image whose regional mirror is not configured uses, over HTTP"},
	{Pattern: "ports.ubuntu.com",
		Why: "Ubuntu's primary arm64 archive, which also serves the arm64 -security pocket on EC2 images, over HTTP"},
	{Pattern: "security.ubuntu.com",
		Why: "the amd64 -security pocket every Ubuntu image's sources list names, over HTTP"},
}

// Allowed reports whether host, as a client names it, is on [Allowlist]. host
// carries no port. Case and one trailing dot are normalised away; anything
// that is not a plain ASCII DNS name (an IP literal, a Unicode name, a name
// with an empty label or a character DNS does not allow) is refused, so the
// only way to match an entry is to name it.
func Allowed(host string) bool {
	h, ok := normalizeHost(host)
	if !ok {
		return false
	}
	for _, r := range Allowlist {
		if r.matches(h) {
			return true
		}
	}
	return false
}

// matches reports whether the normalised host h matches r.
func (r Rule) matches(h string) bool {
	base, wild := strings.CutPrefix(r.Pattern, "*.")
	if !wild {
		return h == r.Pattern
	}
	label, rest, ok := strings.Cut(h, ".")
	return ok && label != "" && rest == base
}

// normalizeHost lower-cases host and strips one trailing dot, and reports
// whether the result is a syntactically valid ASCII DNS name that is not an
// IP address literal.
func normalizeHost(host string) (string, bool) {
	h := strings.ToLower(host)
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return "", false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	// An all-numeric last label is never a real TLD, and keeps forms such as
	// "127.1" or "0x7f.1" (which some resolvers read as addresses) off the list.
	last := h[strings.LastIndexByte(h, '.')+1:]
	if strings.Trim(last, "0123456789") == "" {
		return "", false
	}
	return h, true
}
