package aws

import "testing"

// TestInstanceMemoryFromSuffix covers the offline fallback. It matters because
// the warning it feeds (#682) must work when DescribeInstanceTypes is
// unavailable — the launch still happens, and an unenforceable TTL is no less
// unenforceable for being offline.
func TestInstanceMemoryFromSuffix(t *testing.T) {
	tests := []struct {
		typ     string
		wantMiB int64
		wantOK  bool
	}{
		{"t4g.nano", 512, true},   // the #682 instance
		{"t3.nano", 512, true},    // size suffix, not family — works across families
		{"t4g.micro", 1024, true}, // exactly the floor
		{"t4g.small", 2048, true},
		{"c8g.48xlarge", 0, false}, // not listed: assumed above the floor, so silent
		{"nonsense", 0, false},     // no dot at all
		{"t4g.", 0, false},         // trailing dot
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := instanceMemoryFromSuffix(tt.typ)
		if got != tt.wantMiB || ok != tt.wantOK {
			t.Errorf("instanceMemoryFromSuffix(%q) = (%d, %v), want (%d, %v)",
				tt.typ, got, ok, tt.wantMiB, tt.wantOK)
		}
	}
}

// TestSporedMemoryFloorMatchesTheEvidence: the floor is 1 GiB because 0.5 GiB
// demonstrably failed and 4 GiB demonstrably worked, and nothing in between was
// tested. A higher floor would be inventing evidence; a lower one would not
// cover the observed failure.
func TestSporedMemoryFloorMatchesTheEvidence(t *testing.T) {
	if SporedMemoryFloorMiB <= 512 {
		t.Errorf("floor %d MiB does not cover the 512 MiB instance that outlived its TTL (#682)",
			SporedMemoryFloorMiB)
	}
	if SporedMemoryFloorMiB > 4096 {
		t.Errorf("floor %d MiB would warn about the 4 GiB size that behaved correctly in #682 — "+
			"no evidence supports warning there", SporedMemoryFloorMiB)
	}
}
