package ttl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// DefaultReaperMaxAge duplicates a value owned by the ttl-reaper, which is a
// separate Go module. This gate reads the reaper's source and fails when the two
// drift.
//
// Without it the constant is a second source of truth, which is exactly #790:
// pkg/infrastructure reconstructed a Lambda name from a convention instead of
// reading it from the deploy mechanism, and the CLI then asked AWS for a function
// that had never existed. A duplicated constant is fine; a duplicated constant
// nothing compares is not.
func TestDefaultReaperMaxAgeMatchesTheReaper(t *testing.T) {
	root := repoRoot(t)

	// 1. The Go default in the reaper.
	src := readFile(t, filepath.Join(root, "lambda", "ttl-reaper", "main.go"))
	m := regexp.MustCompile(`const defaultMaxAge = ([0-9]+) \* ([0-9]+) \* time\.Hour`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find `const defaultMaxAge = N * M * time.Hour` in the reaper — " +
			"the gate is reading the wrong file or the declaration changed shape")
	}
	days, hours := atoi(t, m[1]), atoi(t, m[2])
	got := time.Duration(days) * time.Duration(hours) * time.Hour
	if got != DefaultReaperMaxAge {
		t.Errorf("reaper defaultMaxAge is %s but pkg/ttl.DefaultReaperMaxAge is %s — "+
			"the CLI would warn at the wrong threshold", got, DefaultReaperMaxAge)
	}

	// 2. The CloudFormation parameter default, which is what is actually
	// deployed. The Go constant only applies when the env var is unset.
	tmpl := readFile(t, filepath.Join(root, "lambda", "ttl-reaper", "template.yaml"))
	pm := regexp.MustCompile(`(?s)MaxAge:.*?Default:\s*'([0-9]+)h'`).FindStringSubmatch(tmpl)
	if pm == nil {
		t.Fatal("could not find the MaxAge parameter Default in the reaper template")
	}
	if tmplAge := time.Duration(atoi(t, pm[1])) * time.Hour; tmplAge != DefaultReaperMaxAge {
		t.Errorf("template MaxAge default is %s but pkg/ttl.DefaultReaperMaxAge is %s", tmplAge, DefaultReaperMaxAge)
	}
}

// The reaper's two descriptions of what the ceiling is FOR must agree.
//
// They did not, and the disagreement is why a security control was inert for
// months. main.go's header said the ceiling "fires even for --no-timeout /
// missing / far-future deadlines — defense against escape hatches and tag
// tampering"; template.yaml's MaxAge Description said it "reaps even
// --no-timeout/missing-deadline instances", omitting far-future entirely — and
// the code implemented the narrower claim, exempting any instance inside its
// deadline. Since the deadline tag is writable from the instance, that exemption
// defeated the control.
//
// Asserting on prose is normally a poor gate, so this checks the one word that
// carries the scope, in both places, rather than matching whole sentences.
func TestReaperDocsAgreeThatTheCeilingBeatsAFarFutureDeadline(t *testing.T) {
	root := repoRoot(t)
	// The README was not in this list, and said the opposite — "Within-deadline
	// instances are always spared" — for as long as the code did. It is the page a
	// reader reaches first, so it is the one most worth gating.
	for _, f := range []string{
		filepath.Join(root, "lambda", "ttl-reaper", "main.go"),
		filepath.Join(root, "lambda", "ttl-reaper", "template.yaml"),
		filepath.Join(root, "lambda", "ttl-reaper", "README.md"),
	} {
		body := readFile(t, f)
		if !strings.Contains(strings.ToLower(body), "far-future") {
			t.Errorf("%s does not mention a far-future deadline.\n"+
				"Both the reaper's header comment and its MaxAge parameter Description must say "+
				"the ceiling applies to a far-future deadline, because that is the tamper case it "+
				"exists for. The two drifting apart is how the exemption survived.",
				filepath.Base(f))
		}
	}
}

// The warning only fires above the ceiling, and says the number it is comparing
// against — a warning that omits the threshold cannot be acted on.
func TestCeilingWarning(t *testing.T) {
	if w := CeilingWarning(DefaultReaperMaxAge); w != "" {
		t.Errorf("a TTL exactly at the ceiling must not warn, got: %s", w)
	}
	if w := CeilingWarning(DefaultReaperMaxAge - time.Hour); w != "" {
		t.Errorf("a TTL under the ceiling must not warn, got: %s", w)
	}
	w := CeilingWarning(DefaultReaperMaxAge + time.Hour)
	if w == "" {
		t.Fatal("a TTL over the ceiling must warn")
	}
	for _, want := range []string{DefaultReaperMaxAge.String(), "REAPER_MAX_AGE"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning does not mention %q, so the reader cannot act on it:\n%s", want, w)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/spore-host/spawn\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root go.mod")
	return ""
}
