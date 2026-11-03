package egressproxy

import "net/netip"

// refusedPrefixes are address ranges the proxy never dials, beyond what the
// netip predicates in publicAddr cover. The point is the instance's own
// network: the VPC (RFC 1918 or CGNAT space), the instance (loopback) and the
// metadata service (169.254.169.254 and fd00:ec2::254).
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"; 0.0.0.0 reaches the local host
	netip.MustParsePrefix("100.64.0.0/10"),      // CGNAT, RFC 6598: usable as VPC space
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking, RFC 2544: never a public origin
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved, and 255.255.255.255
	netip.MustParsePrefix("fec0::/10"),          // deprecated site-local
	netip.MustParsePrefix("2001::/32"),          // Teredo: tunnels to an embedded IPv4
	netip.MustParsePrefix("2002::/16"),          // 6to4: tunnels to an embedded IPv4
	netip.MustParsePrefix("100::/64"),           // discard-only
	netip.MustParsePrefix("2001:db8::/32"),      // documentation
	netip.MustParsePrefix("64:ff9b:1::/48"),     // local-use NAT64, RFC 8215
	netip.MustParsePrefix("::ffff:0:0:0/96"),    // SIIT, RFC 6145
	netip.MustParsePrefix("fd00:ec2::/32"),      // EC2's IPv6 IMDS, DNS and NTP (also inside fc00::/7)
	netip.MustParsePrefix("169.254.169.254/32"), // IMDS (also inside 169.254.0.0/16)
}

// nat64 is the well-known NAT64 prefix (RFC 6052): an address in it reaches
// the IPv4 address in its last 32 bits, so that address is what is vetted.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// publicAddr reports whether a is a public unicast address the proxy may dial.
// It refuses loopback, link-local (169.254.0.0/16, including IMDS, and
// fe80::/10), private (RFC 1918 and fc00::/7), CGNAT, unspecified, multicast
// and the ranges in refusedPrefixes. An IPv4-mapped IPv6 address is judged as
// the IPv4 address it is.
//
// This is deliberately not a seam: tests reach a loopback upstream by
// replacing the dialer, never by relaxing this check.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	if nat64.Contains(a) {
		b := a.As16()
		return publicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsPrivate() ||
		a.IsMulticast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range refusedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
