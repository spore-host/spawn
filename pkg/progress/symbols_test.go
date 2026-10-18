package progress

import (
	"strings"
	"testing"

	"github.com/spore-host/libs/i18n"
)

// Every status getSymbol renders must map to a real symbol in every display
// mode. "running" and "skipped" asked the catalog for keys it doesn't have, so
// each running step printed the "?" fallback ("[?]" with --no-emoji or
// --accessibility) (#1124). Checking for a non-empty result, as TestGetSymbol
// does, can't see that: "?" isn't empty.
func TestGetSymbolNeverFallsBackToUnknown(t *testing.T) {
	old := i18n.Global
	t.Cleanup(func() { i18n.Global = old })
	for _, mode := range []struct {
		name string
		cfg  i18n.Config
	}{
		{"emoji", i18n.Config{Language: "en"}},
		{"no-emoji", i18n.Config{Language: "en", NoEmoji: true}},
		{"accessibility", i18n.Config{Language: "en", AccessibilityMode: true}},
	} {
		if err := i18n.Init(mode.cfg); err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{"pending", "running", "complete", "error", "skipped"} {
			if got := getSymbol(status); strings.Contains(got, "?") {
				t.Errorf("%s mode: getSymbol(%q) = %q, the catalog's unknown-key fallback", mode.name, status, got)
			}
		}
	}
}

// A step this launch doesn't do (Skip) isn't listed, in the TTY box or the
// plain log: "⏭ FSx Lustre filesystem" on a launch that never mentioned FSx
// read as something requested, or as a problem (#1124).
func TestSkippedStepsAreNotListed(t *testing.T) {
	for _, tty := range []bool{true, false} {
		out := captureStdout(t, func() {
			p := NewProgress()
			p.tty = tty
			p.Start("Launching instance")
			p.Skip("FSx Lustre filesystem")
			p.Complete("Launching instance")
		})
		if strings.Contains(out, "FSx Lustre filesystem") {
			t.Errorf("tty=%v: a skipped step was listed:\n%s", tty, out)
		}
		if !strings.Contains(out, "Launching instance") {
			t.Errorf("tty=%v: the step that ran wasn't listed:\n%s", tty, out)
		}
	}
}
