package cmd

import (
	"errors"
	"strings"
	"testing"
)

// TestServiceFlagErrorNamesTheFix is spawn#621. The raw cobra error —
// "unknown shorthand flag: 'm' in -m" — names a flag the user never passed to
// spawn, so it reads as a spawn bug rather than a missing "--". It bites the two
// most ordinary ways to start a service: `python3 -m app.serve` and
// `node --enable-source-maps server.js`.
func TestServiceFlagErrorNamesTheFix(t *testing.T) {
	// Exercise the hook the command actually installs, rather than a copy of it.
	raw := errors.New("unknown shorthand flag: 'm' in -m")
	got := serviceCmd.FlagErrorFunc()(serviceCmd, raw)

	if got == nil {
		t.Fatal("expected an error")
	}
	msg := got.Error()

	if !strings.Contains(msg, "unknown shorthand flag") {
		t.Errorf("the original cobra error must survive — it says which flag:\n%s", msg)
	}
	if !errors.Is(got, raw) {
		t.Error("the original error must stay wrapped, so callers inspecting it still can")
	}
	for _, want := range []string{
		`"--"`, // the separator
		"spawn's flags first",
		"-- python3 -m", // a concrete, copyable example
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the hint must contain %q:\n%s", want, msg)
		}
	}
}

// TestServiceUseStringDocumentsTheSeparator: the Use line is what `--help` and the
// generated docs show first. It previously advertised `service <command> [args...]`,
// a form that works right up until the command has a flag of its own — so the
// documented shape was correct only by luck.
func TestServiceUseStringDocumentsTheSeparator(t *testing.T) {
	if !strings.Contains(serviceCmd.Use, "--") {
		t.Errorf("Use must show the \"--\" separator (cmd/connect.go already does), got %q", serviceCmd.Use)
	}
}

// TestServiceExamplesAlwaysWork is the gate on the actual regression: every example
// in the long help must put spawn's flags before the command. The old examples were
// flags-after-command, which is why a user following them hit the bug the moment
// their command took a flag.
func TestServiceExamplesAlwaysWork(t *testing.T) {
	for _, line := range strings.Split(serviceCmd.Long, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "spawn service") {
			continue
		}
		// An example is safe if it separates the command with "--".
		if !strings.Contains(trimmed, " -- ") {
			t.Errorf("example does not use the \"--\" separator, so it breaks as soon as the "+
				"command takes a flag of its own:\n    %s", trimmed)
		}
	}
}

// TestServiceExamplesCoverAFlaggedCommand: the docs should demonstrate the case that
// actually fails, not only dash-free commands that worked either way.
func TestServiceExamplesCoverAFlaggedCommand(t *testing.T) {
	if !strings.Contains(serviceCmd.Long, "python3 -m") {
		t.Error("the help should show a command carrying its own flag (e.g. python3 -m ...), " +
			"since that is the shape that used to fail")
	}
}
