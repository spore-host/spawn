package taskproto

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// NICTargetGbps is the shell table read in Go (#1324), so the two must agree on
// every type the table can classify. The types are discovered from the table
// itself: each size pattern (a glob made concrete) in a general-purpose and a
// network-optimized family, the documented types in nicPeaks, the CRT-optimized
// types and some the table cannot classify.
func TestNICTargetGbpsAgreesWithTheWrapperShell(t *testing.T) {
	skipOnWindows(t, "runs the emitted shell under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	types := []string{"", "r8g", "u7i-12tb.224xlarge"}
	for it := range nicPeaks {
		types = append(types, it)
	}
	for _, c := range nicSizeClasses {
		for _, s := range c.Sizes {
			s = strings.ReplaceAll(s, "*", "48xl")
			types = append(types, "m7i."+s, "c7gn."+s)
		}
	}
	if len(types) < 20 {
		t.Fatalf("discovered only %d types; the table walk is broken", len(types))
	}
	top := 0
	for _, c := range nicSizeClasses {
		top = max(top, c.NetOptGbps)
	}
	for _, it := range crtOptimizedTypes {
		// The shell writes no target (the CLI detects these itself); Go
		// reports them as at least as fast as anything in the table.
		if g := NICTargetGbps(it); g != top {
			t.Errorf("%s: NICTargetGbps = %d, want the table's top figure %d", it, g, top)
		}
	}
	for _, it := range types {
		want := 0
		if s := nicTarget(t, it); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("%s: shell target %q", it, s)
			}
			want = n
		}
		if got := NICTargetGbps(it); got != want {
			t.Errorf("%q: NICTargetGbps = %d, the wrapper shell targets %d", it, got, want)
		}
	}
}
