package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractCommandScript pulls the generated /tmp/spawn-command.sh heredoc body
// out of the bootstrap, so a test can run the real thing.
func extractCommandScript(t *testing.T, script string) string {
	t.Helper()
	const start = "cat > /tmp/spawn-command.sh <<'EOFCMD'\n"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("could not find the spawn-command.sh heredoc (marker moved?)")
	}
	rest := script[i+len(start):]
	j := strings.Index(rest, "\nEOFCMD\n")
	if j < 0 {
		t.Fatal("could not find the end of the spawn-command.sh heredoc")
	}
	return rest[:j]
}

// TestCommandStillRunsUnderErrexit is the measurement spawn#707's notice rests
// on: --command runs with errexit ALREADY enabled, and `set -uo pipefail` does
// not clear it. The failure-time notice (below) tells users exactly that; if
// this ever stops being true the notice is wrong and must change with it.
// EXECUTES the generated script because the whole bug is about what the shell
// actually does.
func TestCommandStillRunsUnderErrexit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	got := runCommandScript(t, "set -uo pipefail\necho AFTER=$-\n")
	if !strings.Contains(got, "AFTER=") {
		t.Fatalf("the user's command did not run:\n%s", got)
	}
	after := got[strings.Index(got, "AFTER=")+len("AFTER="):]
	if !strings.Contains(strings.SplitN(after, "\n", 2)[0], "e") {
		t.Errorf("errexit is NOT set after `set -uo pipefail` (%q) — the errexit notice "+
			"now tells users something untrue", strings.SplitN(after, "\n", 2)[0])
	}
}

// runCommandScript runs the generated /tmp/spawn-command.sh with userCmd
// appended, as the bootstrap assembles it, and returns its combined output —
// i.e. exactly what lands in /var/log/spawn-command.log from the script itself.
func runCommandScript(t *testing.T, userCmd string) string {
	t.Helper()
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: userCmd})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	body := extractCommandScript(t, script) + "\n" + userCmd
	cmd := exec.Command("bash", "-c", body) //nolint:gosec // nosemgrep
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated command script failed to run: %v\noutput:\n%s", err, out)
	}
	return string(out)
}

// TestSuccessfulCommandLogHasNoShellOptionPreamble is spawn#897 (1).
//
// The #707 notice used to open EVERY /var/log/spawn-command.log — the file users
// read (`spawn array logs arr --index 3`) — on every member, success or not:
//
//	spawn: shell options for --command: $-=ehB
//	spawn: errexit is ON. set -uo pipefail does NOT clear it; use set +e to opt out.
//	member 3 of 8 on ...
//
// A command that succeeded was not cut short by errexit, so the log should be
// the command's own output and nothing else.
func TestSuccessfulCommandLogHasNoShellOptionPreamble(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	got := runCommandScript(t, "echo member 3 of 8\n")
	if got != "member 3 of 8\n" {
		t.Errorf("a successful command's log carries more than its own output (#897):\n%q", got)
	}
}

// extractCommandOutcome pulls the root-side block that runs after the command
// exits (it decides completed/failed) out of the real bootstrap.
func extractCommandOutcome(t *testing.T, script string) string {
	t.Helper()
	const start = "        if [ \"$CMD_RC\" -eq 0 ]; then\n"
	const end = "\n        fi\n"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("could not find the --command outcome block (marker moved?)")
	}
	j := strings.Index(script[i:], end)
	if j < 0 {
		t.Fatal("could not find the end of the --command outcome block")
	}
	return script[i : i+j+len(end)]
}

// TestFailedCommandLogNamesErrexit keeps #707's safety intent after #897 moved
// the notice: when the command FAILS — the only time errexit can have ended a
// run early, before the line that would have said why — the command log must
// still say errexit was on, that `set -uo pipefail` does not clear it, and how
// to opt out. Runs the real outcome block with the log redirected.
func TestFailedCommandLogNamesErrexit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: "false"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	for _, rc := range []string{"0", "141"} {
		log := filepath.Join(t.TempDir(), "spawn-command.log")
		block := strings.ReplaceAll(extractCommandOutcome(t, script), "/var/log/spawn-command.log", log)
		cmd := exec.Command("bash", "-c", "set -e\nCMD_RC="+rc+"\n"+block) //nolint:gosec // nosemgrep
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("outcome block failed (rc=%s): %v\n%s", rc, err, out)
		}
		data, _ := os.ReadFile(log)
		got := string(data)
		if rc == "0" {
			if got != "" {
				t.Errorf("a successful command still gets the errexit notice in its log (#897):\n%s", got)
			}
			continue
		}
		for _, want := range []string{"exited 141", "errexit is ON", "set -uo pipefail does NOT clear it", "set +e"} {
			if !strings.Contains(got, want) {
				t.Errorf("failed command's log is missing %q (#707):\n%s", want, got)
			}
		}
	}
}

// TestCommandAnnouncementDoesNotTee guards the bug I nearly shipped in the fix.
//
// The first version piped both announcement lines to
// `tee -a /var/log/spawn-command.log`. But this script runs as $LOCAL_USERNAME
// via `su`, which cannot write that file — and under the `set -e` at the top of
// the very same script, a failed tee would have killed the command BEFORE it
// started. Every --command launch, broken by the line explaining why commands
// break.
//
// The caller already pipes this script's combined output through tee as root, so
// plain stdout reaches the log anyway. Caught by executing the script rather
// than reading it.
func TestCommandAnnouncementDoesNotTee(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: "true"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	body := extractCommandScript(t, script)

	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "spawn: ") {
			continue
		}
		if strings.Contains(line, "tee") || strings.Contains(line, ">> /var/log") {
			t.Errorf("an announcement line writes to /var/log from inside the command "+
				"script:\n  %s\n\nThis script runs as the instance user via su and cannot "+
				"write there; under the set -e above, the failure kills the command before "+
				"it starts (#707).", strings.TrimSpace(line))
		}
	}

	// The outer invocation must still capture it, or the announcement never
	// reaches the log the user actually reads.
	if !strings.Contains(script, `su - "$LOCAL_USERNAME" -c '/tmp/spawn-command.sh' 2>&1 | tee -a /var/log/spawn-command.log`) {
		t.Error("the caller no longer pipes the command script through tee, so the " +
			"announcement would not reach /var/log/spawn-command.log")
	}
}

// TestCommandScriptStaysValidBash: the announcement is generated shell, so it
// must parse.
func TestCommandScriptStaysValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user", Command: "echo hi"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(extractCommandScript(t, script))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("generated command script is not valid bash: %v\n%s", err, out)
	}
	_ = os.Stdout
}
