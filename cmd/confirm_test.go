package cmd

import "testing"

// TestConfirmYes covers the shared destructive-action confirmation helper
// (spawn#40): the --yes bypass, explicit yes/no, and the EOF/non-interactive
// case which must read as "no" so unattended invocations don't proceed.
func TestConfirmYes(t *testing.T) {
	t.Run("skip flag bypasses prompt without reading stdin", func(t *testing.T) {
		if !confirmYes(true, "delete?") {
			t.Error("confirmYes(skip=true) should return true without prompting")
		}
	})

	cases := map[string]bool{
		"y\n":       true,
		"yes\n":     true,
		"YES\n":     true,
		"  y \n":    true,
		"n\n":       false,
		"no\n":      false,
		"\n":        false, // bare enter → default no
		"":          false, // EOF / closed pipe → no
		"garbage\n": false,
	}
	for input, want := range cases {
		t.Run("stdin="+input, func(t *testing.T) {
			restore := withStdin(t, input)
			defer restore()
			if got := confirmYes(false, "delete?"); got != want {
				t.Errorf("confirmYes(false, stdin=%q) = %v, want %v", input, got, want)
			}
		})
	}
}
