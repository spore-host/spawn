package launcher

import (
	"os"
	"os/exec"
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

// TestCommandAnnouncesInheritedShellOptions is spawn#707.
//
// --command runs with errexit ALREADY enabled, and `set -uo pipefail` does not
// clear it — so a script opening with that line, believing it chose its own
// error policy, actually runs -e -u -o pipefail. The failure mode is silent and
// actively misleading: the script exits BEFORE the line that would report why.
//
// Two ordinary idioms are the casualties. A SIGPIPE'd pipeline under pipefail
// (`... | head -n N` returns 141), and `wait $pid` on a failed background job —
// which is specifically a pattern for CAPTURING a failure, destroyed at the
// moment it matters. It cost the reporter four r8gd.8xlarge runs and three
// misdiagnoses, one of which reached a published page and had to be withdrawn.
//
// The fix is to say so, not to change it: fail-fast is defensible, and running
// on after a broken step until TTL would be worse. This test EXECUTES the
// generated script because the whole bug is about what the shell actually does.
func TestCommandAnnouncesInheritedShellOptions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username: "ec2-user",
		Command:  "set -uo pipefail\necho AFTER=$-\n",
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}

	body := extractCommandScript(t, script) + "\nset -uo pipefail\necho AFTER=$-\n"
	cmd := exec.Command("bash", "-c", body) //nolint:gosec // nosemgrep
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("the generated command script failed to run: %v\noutput:\n%s", err, got)
	}

	if !strings.Contains(got, "$-=") {
		t.Errorf("the script does not announce $-, so the inherited options stay invisible "+
			"— which is the whole of #707:\n%s", got)
	}
	// The announcement must name errexit and the two things a reader needs: that
	// the idiomatic hardening line does NOT clear it, and how to opt out.
	for _, want := range []string{"errexit is ON", "set -uo pipefail does NOT clear it", "set +e"} {
		if !strings.Contains(got, want) {
			t.Errorf("announcement missing %q:\n%s", want, got)
		}
	}
	// And the measurement that makes the report true: errexit survives
	// `set -uo pipefail`. If this ever stops being the case the announcement is
	// wrong and must change with it.
	if !strings.Contains(got, "AFTER=") {
		t.Fatalf("the user's command did not run:\n%s", got)
	}
	after := got[strings.Index(got, "AFTER=")+len("AFTER="):]
	if !strings.Contains(strings.SplitN(after, "\n", 2)[0], "e") {
		t.Errorf("errexit is NOT set after `set -uo pipefail` (%q) — the announcement now "+
			"tells users something untrue", strings.SplitN(after, "\n", 2)[0])
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
