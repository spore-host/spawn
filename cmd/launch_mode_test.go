package cmd

import "testing"

// TestLaunchMode covers the input-mode selection, especially the #34 fix:
// explicit --instance-type must use flags mode even when stdin is a pipe
// (non-TTY), so spawn doesn't try to parse an empty stdin as truffle JSON.
func TestLaunchMode(t *testing.T) {
	cases := []struct {
		name         string
		interactive  bool
		instanceType string
		stdinIsTTY   bool
		want         launchInputMode
	}{
		// #34: explicit instance type + piped stdin → flags (NOT pipe).
		{"flags via pipe (Java subprocess)", false, "t4g.nano", false, modeFlags},
		{"flags via TTY", false, "t4g.nano", true, modeFlags},
		// Genuine truffle pipe: no instance type, piped stdin.
		{"pipe from truffle", false, "", false, modePipe},
		// Interactive wizard: TTY, no instance type.
		{"wizard on TTY", false, "", true, modeWizard},
		// --interactive always wins, even with an instance type.
		{"explicit interactive", true, "t4g.nano", true, modeWizard},
		{"explicit interactive over pipe", true, "", false, modeWizard},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := launchMode(c.interactive, c.instanceType, c.stdinIsTTY); got != c.want {
				t.Errorf("launchMode(interactive=%v, type=%q, tty=%v) = %d, want %d",
					c.interactive, c.instanceType, c.stdinIsTTY, got, c.want)
			}
		})
	}
}
