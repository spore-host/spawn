// Package shellcheckgate runs shellcheck over the shell scripts spawn
// generates (#689 part C).
//
// spawn renders a lot of bash: the spored bootstrap, MPI and storage user-data,
// the task wrapper, the app templates, the pool worker, the pipeline stage
// runner. Its tests mostly assert the TEXT of that shell, which is how the
// quoting class (#660: a command collapsed to one argv word) and an unbounded
// wait loop (#684) reached hardware. shellcheck reads the script as a shell
// would, so it catches classes a text assertion cannot.
//
// Every generator's test calls Check with the generator's id, and
// TestEveryShellGeneratorIsShellchecked (cmd/shellcheck_test.go) discovers the
// generators from source and fails for one no Check call names.
package shellcheckgate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Severity is the shellcheck level the gate enforces: errors and warnings.
// info and style findings (mostly SC2086-style quoting advice on values spawn
// controls) are not enforced.
const Severity = "warning"

// Binary returns the shellcheck path, or skips/fails the test when it is
// missing: a skip on a developer machine, a FAILURE under CI=true, because in
// CI a skipped linter reads as a passing one.
//
// The one CI exception is Windows. The scripts are Linux user-data, the
// windows-latest job runs the unit tests for the Windows spored, and the
// shellcheck verdict for a given script text does not depend on the GOOS the
// test ran on; the Linux Test job installs shellcheck and enforces it.
func Binary(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("shellcheck")
	if err == nil {
		return path
	}
	if os.Getenv("CI") == "true" && runtime.GOOS != "windows" {
		t.Fatalf("shellcheck is not installed, and CI=true: the shellcheck gate (#689) must run in CI. " +
			"Install it in the workflow (pinned version + sha256), as the Test job does.")
	}
	t.Skip("shellcheck not installed; skipping the generated-script shellcheck gate (#689)")
	return ""
}

// Check runs `shellcheck -S warning` on script, and on every nested script it
// writes through a quoted heredoc (see NestedScripts), failing t with each
// finding and the offending lines. id names the generator, as
// "<package dir>.<func>" (e.g. "pkg/launcher.BuildLinuxBootstrap"); it is how
// the discovery gate knows the generator is covered, so it must be a literal.
func Check(t testing.TB, id, script string) {
	t.Helper()
	bin := Binary(t)
	if strings.TrimSpace(script) == "" {
		t.Fatalf("%s: rendered an empty script; nothing to check", id)
	}
	run(t, bin, id, script)
	for i, nested := range NestedScripts(script) {
		run(t, bin, fmt.Sprintf("%s (nested script %d)", id, i+1), nested)
	}
}

func run(t testing.TB, bin, id, script string) {
	t.Helper()
	findings, err := Findings(bin, script)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	if len(findings) == 0 {
		return
	}
	t.Errorf("%s: shellcheck -S %s found problems in the generated script.\n"+
		"Fix the generator, or, for an intentional pattern, add a scoped\n"+
		"`# shellcheck disable=SCxxxx  # reason` on the line above it:\n%s",
		id, Severity, strings.Join(findings, ""))
}

// Findings runs shellcheck on script and returns one entry per finding, each
// with the offending line quoted. An error means shellcheck did not run.
func Findings(bin, script string) ([]string, error) {
	// --shell=bash: fragments (no shebang) are bash, as every one of them is
	// concatenated into a #!/bin/bash user-data script.
	cmd := exec.Command(bin, "-S", Severity, "--shell=bash", "--format=gcc", "-") // nosemgrep
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return nil, nil
	}
	if _, ok := err.(*exec.ExitError); !ok || out.Len() == 0 {
		return nil, fmt.Errorf("shellcheck did not run: %v\n%s", err, errb.String())
	}
	lines := strings.Split(script, "\n")
	var findings []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		f := "  " + sc.Text() + "\n"
		if n := findingLine(sc.Text()); n > 0 && n <= len(lines) {
			f += fmt.Sprintf("      | %s\n", lines[n-1])
		}
		findings = append(findings, f)
	}
	return findings, nil
}

var gccLine = regexp.MustCompile(`^-:(\d+):`)

func findingLine(f string) int {
	m := gccLine.FindStringSubmatch(f)
	if m == nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(m[1], "%d", &n)
	return n
}

// heredocStart matches a heredoc whose delimiter is quoted (<<'EOF', <<"EOF",
// <<-'EOF'). A quoted delimiter means the body is written verbatim, so it is
// a script in its own right; an unquoted one is expanded by the OUTER shell
// and is not checkable on its own.
var heredocStart = regexp.MustCompile(`<<-?\s*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")`)

// NestedScripts returns the bodies of quoted-delimiter heredocs whose first
// line is a shebang: scripts the generated script writes to disk and runs
// later (e.g. /tmp/spawn-command.sh in the bootstrap). shellcheck treats a
// heredoc body as data, so without this they would never be checked.
func NestedScripts(script string) []string {
	var out []string
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		m := heredocStart.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		delim := m[1] + m[2]
		stripTabs := strings.HasPrefix(strings.TrimSpace(m[0]), "<<-")
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			line := lines[j]
			if stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == delim {
				break
			}
			body = append(body, line)
		}
		if len(body) > 0 && strings.HasPrefix(body[0], "#!") && isShellShebang(body[0]) {
			out = append(out, strings.Join(body, "\n")+"\n")
		}
		i = j
	}
	return out
}

func isShellShebang(line string) bool {
	return regexp.MustCompile(`^#!\s*/(usr/)?bin/(env\s+)?(ba)?sh\b`).MatchString(line)
}
