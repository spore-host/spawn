package cmd

import "testing"

// TestLaunchMode covers the input-mode selection, especially the #34 fix:
// explicit --instance-type must use flags mode even when stdin is a pipe
// (non-TTY), so spawn doesn't try to parse an empty stdin as truffle JSON.
func TestLaunchMode(t *testing.T) {
	cases := []struct {
		name         string
		interactive  bool
		paramSweep   bool
		instanceType string
		stdinIsTTY   bool
		want         launchInputMode
	}{
		// #34: explicit instance type + piped stdin → flags (NOT pipe).
		{"flags via pipe (Java subprocess)", false, false, "t4g.nano", false, modeFlags},
		{"flags via TTY", false, false, "t4g.nano", true, modeFlags},
		// Genuine truffle pipe: no instance type, piped stdin.
		{"pipe from truffle", false, false, "", false, modePipe},
		// Interactive wizard: TTY, no instance type.
		{"wizard on TTY", false, false, "", true, modeWizard},
		// --interactive wins over an instance type (but not over a sweep, below).
		{"explicit interactive", true, false, "t4g.nano", true, modeWizard},
		{"explicit interactive over pipe", true, false, "", false, modeWizard},
		// #1532: a parameter sweep is a flags launch whatever the TTY and
		// --instance-type: runLaunch takes the sweep path before the wizard,
		// and its type may live in the param file's defaults:.
		{"sweep on TTY without --instance-type", false, true, "", true, modeFlags},
		{"sweep via pipe without --instance-type", false, true, "", false, modeFlags},
		{"sweep with --instance-type", false, true, "t4g.nano", true, modeFlags},
		{"sweep with --interactive", true, true, "", true, modeFlags},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := launchMode(c.interactive, c.paramSweep, c.instanceType, c.stdinIsTTY); got != c.want {
				t.Errorf("launchMode(interactive=%v, sweep=%v, type=%q, tty=%v) = %d, want %d",
					c.interactive, c.paramSweep, c.instanceType, c.stdinIsTTY, got, c.want)
			}
		})
	}
}
