package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/security"
)

// completionWriteRe matches every place the --command wrapper WRITES its
// completion record: the `cat > <target> <<JSONEOF` heredoc. Anchored on the
// write itself, so a comment naming /tmp/SPAWN_COMPLETE cannot satisfy it.
var completionWriteRe = regexp.MustCompile(`(?m)^\s*cat > (.+) <<JSONEOF$`)

// completionAssignRe matches the one assignment of the variable the
// completion writes target (#1274): the body is published once per release, so
// the path is a variable set ahead of it rather than text rendered into it.
var completionAssignRe = regexp.MustCompile(`(?m)^` + completionFileVar + `=(.+)$`)

// completionWriteTargets discovers the write targets rather than listing the
// sites by hand, and fails if discovery finds none: a wrapper with no
// completion write has broken, not proven itself clean. A target that is the
// completion variable is resolved to the value the script assigns it, so the
// result is the path each write actually lands at.
func completionWriteTargets(t *testing.T, cfg BootstrapConfig) []string {
	t.Helper()
	s, err := BuildLinuxBootstrap(cfg)
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	assigns := completionAssignRe.FindAllStringSubmatch(s, -1)
	var out []string
	for _, m := range completionWriteRe.FindAllStringSubmatch(s, -1) {
		target := m[1]
		if target == `"$`+completionFileVar+`"` {
			if len(assigns) != 1 {
				t.Fatalf("the completion write targets $%s, which the script assigns %d times, want exactly once", completionFileVar, len(assigns))
			}
			target = assigns[0][1]
		}
		out = append(out, target)
	}
	if len(out) == 0 {
		t.Fatal("found no `cat > ... <<JSONEOF` completion write in the --command wrapper")
	}
	return out
}

// TestCommandWritesTheConfiguredCompletionFile is the #1131 gate. spored watches
// --completion-file; the --command wrapper wrote a hardcoded /tmp/SPAWN_COMPLETE.
// With a custom path, --on-complete never fired and the instance billed to TTL.
// Every completion write (the failed-prerequisite path AND the normal exit path)
// must target exactly the configured path, and none the default.
func TestCommandWritesTheConfiguredCompletionFile(t *testing.T) {
	custom := "/data/run/done"
	targets := completionWriteTargets(t, BootstrapConfig{
		Username: "tester", Command: "true", CompletionFile: custom,
	})
	// Both the prerequisite-failure write and the exit write (#668, #614).
	if len(targets) != 2 {
		t.Errorf("found %d completion writes, want 2 (prerequisite failure + command exit): %q", len(targets), targets)
	}
	for _, got := range targets {
		if got != security.ShellQuote(custom) {
			t.Errorf("completion record written to %s, want exactly %s (the configured --completion-file)", got, security.ShellQuote(custom))
		}
		if strings.Contains(got, defaultCompletionFile) {
			t.Errorf("completion record still written to the default %s despite --completion-file %s", defaultCompletionFile, custom)
		}
	}
}

// The default stays what it was: no --completion-file means /tmp/SPAWN_COMPLETE,
// which is the flag default and spored's default.
func TestCommandCompletionFileDefault(t *testing.T) {
	for _, got := range completionWriteTargets(t, BootstrapConfig{Username: "tester", Command: "true"}) {
		if got != security.ShellQuote("/tmp/SPAWN_COMPLETE") {
			t.Errorf("default completion write targets %s, want '/tmp/SPAWN_COMPLETE'", got)
		}
	}
}

// TestCommandCompletionFileIsQuoted runs the rendered write under bash with a
// path holding a space, a single quote and a $: the record must land at exactly
// that path (in a directory that does not exist yet), and nothing may expand.
func TestCommandCompletionFileIsQuoted(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "new dir", "it's $HOME done")
	cfg := BootstrapConfig{Username: "tester", Command: "true", CompletionFile: path}

	s, err := BuildLinuxBootstrap(cfg)
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	syntax := exec.Command(bash, "-n")
	syntax.Stdin = strings.NewReader(s)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap with a quoted --completion-file is not valid bash: %v\n%s", err, out)
	}

	for _, target := range completionWriteTargets(t, cfg) {
		_ = os.RemoveAll(filepath.Join(dir, "new dir"))
		// The assignment and the write exactly as the script renders them.
		frag := "set -e\n" + completionFileVar + "=" + target + "\nmkdir -p \"$(dirname -- \"$" + completionFileVar + "\")\"\ncat > \"$" + completionFileVar + "\" <<JSONEOF\n{\"status\": \"completed\"}\nJSONEOF\n"
		if !strings.Contains(s, "mkdir -p \"$(dirname -- \"$"+completionFileVar+"\")\" || true\n") {
			t.Fatal("the script's completion mkdir is not the one this fragment runs")
		}
		c := exec.Command(bash, "-c", frag)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("running the rendered write failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("completion record did not land at the configured path %q: %v", path, err)
		}
		if !strings.Contains(string(got), `"completed"`) {
			t.Errorf("unexpected record at %q: %q", path, got)
		}
	}
}
