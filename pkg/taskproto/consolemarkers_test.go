package taskproto

import (
	"strings"
	"testing"
)

// The writer (pkg/agent) and the reader (cmd/) live in different packages that do
// not import each other, so the markers are shared here. A drift between them
// would be silent: the console would still contain a block and the reader would
// simply never find it.
func TestExtractConsoleLog(t *testing.T) {
	// Exactly what pkg/agent writes, including the line count and reason the
	// reader must not have to predict.
	block := "\n" + ConsoleLogStartPrefix + " (last 50 lines) — workload failed ===\n" +
		"error: package not found\nexit 1\n" + ConsoleLogEndMarker + "\n"

	for _, tc := range []struct {
		name   string
		input  string
		want   string
		wantOK bool
		why    string
	}{
		{
			"a block surrounded by boot noise",
			"UEFI firmware\n[  0.00] Linux version\ncloud-init running\n" + block +
				"[ 17.05] systemd-shutdown: Powering off.\n",
			"error: package not found\nexit 1", true,
			"the point: 50 useful lines out of 40 KB of boot output",
		},
		{
			"no block at all (a successful job writes none)",
			"UEFI firmware\ncloud-init running\nreboot: Power down\n",
			"", false,
			"must be distinguishable from an empty block",
		},
		{
			"empty console (capture has not populated)",
			"", "", false,
			"the ~5-minute window; the caller tells this apart by termination time",
		},
		{
			// The console is capped and a long boot can evict the tail, so a
			// start with no end is a real shape — and a partial log still answers
			// "why did it fail" more often than silence.
			"truncated mid-block",
			"boot\n" + ConsoleLogStartPrefix + " (last 50 lines) — workload failed ===\nline one\nline two",
			"line one\nline two", true,
			"return what survived rather than nothing",
		},
		{
			"block with no body",
			ConsoleLogStartPrefix + " (last 50 lines) — workload failed ===\n" + ConsoleLogEndMarker + "\n",
			"", true,
			"found but empty is not the same as not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractConsoleLog(tc.input)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v — %s", ok, tc.wantOK, tc.why)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}

// The framing must not leak into the body: a user reading a failure should see
// their log, not our delimiters.
func TestExtractConsoleLogStripsItsOwnFraming(t *testing.T) {
	in := ConsoleLogStartPrefix + " (last 50 lines) — workload failed ===\nreal content\n" +
		ConsoleLogEndMarker + "\n"
	got, ok := ExtractConsoleLog(in)
	if !ok {
		t.Fatal("block not found")
	}
	if strings.Contains(got, "===") {
		t.Errorf("framing leaked into the body: %q", got)
	}
	if got != "real content" {
		t.Errorf("got %q, want %q", got, "real content")
	}
}
