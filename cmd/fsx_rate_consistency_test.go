package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// There must be exactly one FSx storage rate in spawn, and it must be the
// PERSISTENT_2 one (#619).
//
// `fsx info` carried its own `0.22` multiplied by capacity, commented "$0.22/GB-
// month for SSD" — a PERSISTENT_1 rate applied to filesystems spawn creates as
// PERSISTENT_2, whose rate at the default 125 MB/s/TiB tier is $0.145. It
// overstated a 1200 GiB filesystem by ~52% ($264 vs $174), and once
// `--estimate-only` started quoting storage cost (#613/#617) the two spawn
// commands disagreed about the same filesystem. A wrong number in the one place
// that quotes storage cost is worse than the previous silence, because a user
// reconciling it against a real bill concludes the tool is guessing.

// TestFSxStorageRateHasASingleSource fails if a second hardcoded per-GiB rate
// appears anywhere in cmd/, which is how the drift happened the first time.
func TestFSxStorageRateHasASingleSource(t *testing.T) {
	// Rates that have appeared, or plausibly would: PERSISTENT_1 SSD and HDD
	// figures that are not what spawn provisions.
	banned := regexp.MustCompile(`\b0\.22\b|\b0\.145\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}

	// fsx_cost.go is the one place allowed to name the rate; this test names it too.
	allowed := map[string]bool{
		"fsx_cost.go":                  true,
		"fsx_rate_consistency_test.go": true,
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || allowed[name] {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			// Comments are stripped before matching: the risk this guards is a rate
			// used in a computation, and prose explaining why the old rate was wrong
			// (including the comment above) must stay allowed.
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if banned.MatchString(line) {
				t.Errorf("%s:%d names an FSx storage rate directly:\n    %s\n"+
					"Use fsxLustreUSDPerGiBMonth/fsxLustreMonthlyUSD in cmd/fsx_cost.go so the\n"+
					"rate has one source and spawn's surfaces cannot quote different costs (#619).",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestFSxRateIsPersistent2NotPersistent1 pins the actual values, so "one source"
// cannot be satisfied by centralising the *wrong* rate.
func TestFSxRateIsPersistent2NotPersistent1(t *testing.T) {
	// The default --fsx-throughput is 125 MB/s/TiB.
	if got := fsxLustreUSDPerGiBMonth(125); got != 0.145 {
		t.Errorf("rate at 125 MB/s/TiB = %.4f, want 0.1450 (PERSISTENT_2 SSD, us-east-1)", got)
	}
	if got := fsxLustreUSDPerGiBMonth(125); got == 0.22 {
		t.Error("rate is back to the PERSISTENT_1 $0.22/GiB-month figure (#619)")
	}

	// 1200 GiB is the FSx Lustre minimum, so this is the floor cost of any
	// --fsx-create, and the number #613 and #619 both quote.
	const minCapacity int32 = 1200
	got := fsxLustreMonthlyUSD(minCapacity, 125)
	if got < 173.5 || got > 174.5 {
		t.Errorf("1200 GiB at 125 MB/s/TiB = $%.2f/month, want ~$174 (the old 0.22 rate gave $264)", got)
	}
}
