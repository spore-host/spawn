package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// truffleVersionOutput is what `truffle --version` printed in the #1140 report.
const truffleVersionOutput = "truffle 0.59.0\nGit Commit: c43db2a1a974fdd866857f58a5e923c5b195b27c\nBuild Date: 2026-10-15T22:04:44Z\n"

// multiLineProber returns multi-line text from every probe that has text to
// return, as real tools and AWS errors can.
type multiLineProber struct{ mockProber }

func (multiLineProber) TruffleAvailable() (string, error) { return truffleVersionOutput, nil }
func (multiLineProber) AWSCLIAvailable() (string, error) {
	return "aws-cli/2.32.0\nPython/3.13", nil
}
func (multiLineProber) SSHKeyPresent() (string, error) {
	return "", errors.New("no default SSH key\nchecked ~/.ssh")
}
func (multiLineProber) EC2Describe(context.Context) error {
	return errors.New("UnauthorizedOperation\n\tencoded message follows")
}
func (multiLineProber) Route53Available(context.Context) (string, error) {
	return "", errors.New("AccessDenied\r\nrequest id: x")
}

// statusSymbols discovers every status glyph RenderText can lead a line with,
// by walking the Status values until Symbol falls through to its default.
func statusSymbols(t *testing.T) []string {
	t.Helper()
	unknown := Status(-1).Symbol()
	var syms []string
	for s := Status(0); s.Symbol() != unknown; s++ {
		syms = append(syms, s.Symbol()+" ")
	}
	if len(syms) == 0 {
		t.Fatal("found no status symbols")
	}
	return syms
}

// TestRenderTextEveryLineIsACheckOrAFix is #1140's gate: in the text report,
// every line before the summary starts with a status symbol or the "    → "
// fix prefix. truffle's three-line --version broke it, leaving "Git Commit:"
// and "Build Date:" as lines that looked like unmarked checks.
func TestRenderTextEveryLineIsACheckOrAFix(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, Run(context.Background(), multiLineProber{mockProber{acct: "123456789012"}}))

	prefixes := append(statusSymbols(t), "    → ")
	body, _, found := strings.Cut(buf.String(), "\n\n")
	if !found {
		t.Fatalf("no blank line before the summary in:\n%s", buf.String())
	}
	lines := strings.Split(body, "\n")
	if len(lines) < 5 {
		t.Fatalf("report has only %d check lines:\n%s", len(lines), body)
	}
	for _, line := range lines {
		ok := false
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("report line %q starts with neither a status symbol nor the fix prefix", line)
		}
	}
	if !strings.Contains(body, "truffle 0.59.0") {
		t.Errorf("the truffle version itself is missing from the report:\n%s", body)
	}
}

// TestTruffleAvailableKeepsTheVersionLine runs the real prober against a fake
// truffle printing what truffle prints: the detail is its first line.
func TestTruffleAvailableKeepsTheVersionLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake truffle is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '" + strings.ReplaceAll(truffleVersionOutput, "\n", `\n`) + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "truffle"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := (&awsProber{}).TruffleAvailable()
	if err != nil {
		t.Fatalf("TruffleAvailable: %v", err)
	}
	if got != "truffle 0.59.0" {
		t.Errorf("TruffleAvailable = %q, want %q", got, "truffle 0.59.0")
	}
}
