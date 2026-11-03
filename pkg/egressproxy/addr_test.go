package egressproxy

import (
	"net/netip"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.255.255.254", "::1", // loopback
		"169.254.169.254", "169.254.0.1", "169.254.170.2", "fe80::1", // link-local, IMDS, ECS
		"fd00:ec2::254", "fd00:ec2::23", // EC2 IPv6 IMDS and DNS
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", // private
		"100.64.0.1", "100.127.255.255", // CGNAT
		"0.0.0.0", "::", "0.1.2.3", // unspecified, this-network
		"224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1", // multicast, broadcast, reserved
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1", // IPv4-mapped
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", // NAT64 of IMDS and loopback
		"2002:a9fe:a9fe::1", "2001:0:a9fe:a9fe::1", // 6to4, Teredo
		"198.18.0.1", "192.0.0.8",
	}
	for _, s := range refused {
		if publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("publicAddr(%s) = true, want false", s)
		}
	}
	for _, s := range []string{"52.94.233.129", "91.189.91.81", "185.125.190.36", "2600:1f18::1", "2620:2d:4000:1::17", "64:ff9b::345e:e981"} {
		if !publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("publicAddr(%s) = false, want true", s)
		}
	}
	if publicAddr(netip.Addr{}) {
		t.Error("the zero Addr is public")
	}
	if publicAddr(netip.MustParseAddr("2600:1f18::1%eth0")) {
		t.Error("a zoned address is public")
	}
}
