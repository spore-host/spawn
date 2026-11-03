package egressproxy

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAllowedMatchesTheListedHosts(t *testing.T) {
	for _, h := range []string{
		"efa-installer.amazonaws.com",
		"awscli.amazonaws.com",
		"us-east-1.ec2.archive.ubuntu.com",
		"ap-southeast-2.ec2.archive.ubuntu.com",
		"us-west-2.ec2.ports.ubuntu.com",
		"archive.ubuntu.com",
		"ports.ubuntu.com",
		"security.ubuntu.com",
		// Case and one trailing dot (the absolute form) are normalised.
		"EFA-Installer.AmazonAWS.com",
		"efa-installer.amazonaws.com.",
		"US-EAST-1.EC2.ARCHIVE.UBUNTU.COM.",
	} {
		if !Allowed(h) {
			t.Errorf("Allowed(%q) = false, want true", h)
		}
	}
}

// The suffix rules must not be bypassable: an entry matches only by naming it.
func TestAllowedCannotBeBypassed(t *testing.T) {
	for _, h := range []string{
		// Prefix and suffix tricks on an exact entry.
		"evilefa-installer.amazonaws.com",
		"efa-installer.amazonaws.com.evil.com",
		"efa-installer.amazonaws.com.evil.com.",
		"x.efa-installer.amazonaws.com",
		"amazonaws.com",
		"evil-awscli.amazonaws.com",
		// The wildcard is exactly one label.
		"ec2.archive.ubuntu.com",
		".ec2.archive.ubuntu.com",
		"a.b.ec2.archive.ubuntu.com",
		"evilec2.archive.ubuntu.com",
		"us-east-1.ec2.archive.ubuntu.com.evil.com",
		"us-east-1-ec2.archive.ubuntu.com",
		"*.ec2.archive.ubuntu.com",
		"evilarchive.ubuntu.com",
		"archive.ubuntu.com.evil.com",
		"x.security.ubuntu.com",
		// Trailing dots beyond one, empty labels, whitespace.
		"efa-installer.amazonaws.com..",
		"efa-installer..amazonaws.com",
		" efa-installer.amazonaws.com",
		"efa-installer.amazonaws.com ",
		"efa-installer.amazonaws.com\x00",
		"", ".",
		// Carrying a port, user info, or a path.
		"efa-installer.amazonaws.com:443",
		"user@efa-installer.amazonaws.com",
		"efa-installer.amazonaws.com/x",
		// IP literals, in every spelling.
		"52.94.233.129", "127.0.0.1", "169.254.169.254", "127.1", "0x7f.1", "2130706433",
		"::1", "[::1]", "fd00:ec2::254", "[fd00:ec2::254]",
		// IDN: Unicode look-alikes and punycode never match an ASCII entry.
		"efa-installer。amazonaws.com",  // ideographic full stop
		"еfa-installer.amazonaws.com",  // Cyrillic е
		"efa-installer.amazonaws.com​", // zero-width space
		"xn--fa-installer-9ve.amazonaws.com",
		"xn--efa-installer.amazonaws.com",
		"us-east-1.ec2.archive.ubuntu.xn--com-9ka",
		// Leading or trailing hyphen.
		"-.ec2.archive.ubuntu.com",
	} {
		if Allowed(h) {
			t.Errorf("Allowed(%q) = true, want false", h)
		}
	}
}

// Every entry says why it is there, is in normal form, and is a name, not an
// address; a wildcard is exactly "*." on a valid name.
func TestAllowlistEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Allowlist {
		if strings.TrimSpace(r.Why) == "" {
			t.Errorf("%q has no Why", r.Pattern)
		}
		if seen[r.Pattern] {
			t.Errorf("%q listed twice", r.Pattern)
		}
		seen[r.Pattern] = true
		base := strings.TrimPrefix(r.Pattern, "*.")
		if strings.Contains(base, "*") {
			t.Errorf("%q: only a leading \"*.\" wildcard is supported", r.Pattern)
		}
		if n, ok := normalizeHost(base); !ok || n != base {
			t.Errorf("%q is not a normalised DNS name", r.Pattern)
		}
		if _, err := netip.ParseAddr(base); err == nil {
			t.Errorf("%q is an address", r.Pattern)
		}
	}
}
