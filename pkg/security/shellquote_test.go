package security

import "testing"

// TestShellQuoteSuppressesExpansion is the primitive #660 needed and the repo
// did not have: ShellEscape claimed to be shell-safe and was not.
func TestShellQuoteSuppressesExpansion(t *testing.T) {
	cases := map[string]string{
		"/efs":  `'/efs'`,
		"$HOME": `'$HOME'`,
		"$(id)": `'$(id)'`,
		"a b":   `'a b'`,
		`it's`:  `'it'\''s'`,
		`"dq"`:  `'"dq"'`,
		"":      `''`,
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestShellEscapeIsNotShellSafe pins the behaviour that motivated ShellQuote, so
// nobody "fixes" the doc comment back to claiming safety. strconv.Quote leaves
// $VAR and command substitution live, because double quotes do not suppress them.
func TestShellEscapeIsNotShellSafe(t *testing.T) {
	got := ShellEscape("$(id)")
	if got != `"$(id)"` {
		t.Fatalf("ShellEscape(%q) = %s, expected Go double-quoting", "$(id)", got)
	}
	// Double-quoted: a POSIX shell WOULD run the substitution. That is the bug.
	if got[0] != '"' {
		t.Error("expected double quotes — the point of this test is that they are unsafe")
	}
}
