package aws

import "testing"

func TestPickFreeDevice(t *testing.T) {
	// Root only → first data device is /dev/sdf.
	if got := pickFreeDevice(map[string]bool{"/dev/xvda": true}); got != "/dev/sdf" {
		t.Errorf("got %q, want /dev/sdf", got)
	}
	// /dev/sdf taken → /dev/sdg.
	if got := pickFreeDevice(map[string]bool{"/dev/xvda": true, "/dev/sdf": true}); got != "/dev/sdg" {
		t.Errorf("got %q, want /dev/sdg", got)
	}
	// xvd alias counts as used.
	if got := pickFreeDevice(map[string]bool{"/dev/xvdf": true}); got != "/dev/sdg" {
		t.Errorf("xvd alias should block /dev/sdf; got %q, want /dev/sdg", got)
	}
	// All f..p taken → "".
	all := map[string]bool{}
	for c := byte('f'); c <= 'p'; c++ {
		all["/dev/sd"+string(rune(c))] = true
	}
	if got := pickFreeDevice(all); got != "" {
		t.Errorf("expected empty when all taken, got %q", got)
	}
}
