package launcher

import (
	"os/exec"
	"strings"
	"testing"
)

func commandBootstrap(t *testing.T) string {
	t.Helper()
	s, err := BuildLinuxBootstrap(BootstrapConfig{
		Username: "tester",
		Command:  "aws s3 cp s3://b/x.sh . && ./x.sh",
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	return s
}

// TestCommandOutcomeIsCaptured is the spawn#614 regression.
//
// The old block was:
//
//	su - "$USER" -c '/tmp/spawn-command.sh' 2>&1 | tee /var/log/spawn-command.log &
//	echo "✅ Command execution started (logs: ...)"
//
// which discarded the exit code and printed success unconditionally, and never wrote
// /tmp/SPAWN_COMPLETE — so --on-complete never fired on this path at all and the
// instance burned its whole TTL whether the command passed or failed.
func TestCommandOutcomeIsCaptured(t *testing.T) {
	s := commandBootstrap(t)

	// PIPESTATUS, not $?. After `cmd | tee`, $? is TEE's status (0 even when the
	// command failed), so $? would report every failure as a success. Verified
	// under bash: $? = 0 while ${PIPESTATUS[0]} = 7 for a command exiting 7.
	if !strings.Contains(s, "${PIPESTATUS[0]}") {
		t.Error("the command's exit code must come from ${PIPESTATUS[0]}; $? after the tee pipe is tee's status")
	}
	if !strings.Contains(s, "/tmp/SPAWN_EXITCODE") {
		t.Error("the exit code must be recorded to /tmp/SPAWN_EXITCODE")
	}
	// Completion must be signalled so --on-complete can fire.
	if !strings.Contains(s, "/tmp/SPAWN_COMPLETE") {
		t.Error("the bootstrap must write /tmp/SPAWN_COMPLETE, or --on-complete never fires for a --command launch")
	}
	// Both outcomes must be reported, and the failure branch must be loud.
	for _, want := range []string{"CMD_STATE=completed", "CMD_STATE=failed", "--command FAILED"} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap missing %q", want)
		}
	}
}

// TestCommandSuccessClaimIsGone gates the specific sentence that lied, the way the
// #617 FSx promise gate does. "Command execution started" was printed before the
// command had done anything, and it was the only thing the user saw.
func TestCommandSuccessClaimIsGone(t *testing.T) {
	s := commandBootstrap(t)
	for _, banned := range []string{
		"✅ Command execution started",
		"Command execution started",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("the unconditional success line %q is back — it reported success before the command had run", banned)
		}
	}
}

// TestSporedStartsBeforeTheCommandWhichStaysBackgrounded is the spored#65
// guard, restated for #1274.
//
// spored used to start AFTER the --command block, so the block had to be
// backgrounded or TTL/idle/cost enforcement stayed unarmed for the command's
// whole run. #1274 moved spored's start ahead of every optional step, the
// command included, because a failure between the two (an OOM-killed dnf on a
// t4g.nano) left the workload running with no spored at all. The command must
// still be backgrounded: blocking cloud-init on it would hold back the storage,
// container and custom user data that follow, and the exit code is captured in
// the subshell rather than by waiting.
func TestSporedStartsBeforeTheCommandWhichStaysBackgrounded(t *testing.T) {
	s := commandBootstrap(t)

	cmdIdx := strings.Index(s, "/tmp/spawn-command.sh'")
	sporedIdx := strings.Index(s, "systemctl start spored")
	startedIdx := strings.Index(s, "--command started in the background")
	if cmdIdx < 0 || sporedIdx < 0 || startedIdx < 0 {
		t.Fatalf("could not locate the markers (command=%d spored=%d started=%d)", cmdIdx, sporedIdx, startedIdx)
	}
	if sporedIdx > cmdIdx {
		t.Fatal("spored starts after the --command block again; a failure between them leaves the workload running with no TTL (#1274)")
	}
	if !strings.Contains(s[cmdIdx:startedIdx], ") &") {
		t.Error("the command subshell is no longer backgrounded — cloud-init will block on the " +
			"command, and the rest of the bootstrap waits for it to finish (spored#65)")
	}
}

// TestBootstrapWithCommandIsValidBash runs the real shell parser over the generated
// script. A syntax error here breaks every launch, and the heredoc + subshell added
// for #614 is exactly the shape that is easy to get wrong — my first attempt put
// backticks in a comment inside a Go raw string and broke the Go build; the shell
// equivalent would have shipped.
func TestBootstrapWithCommandIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for name, cfg := range map[string]BootstrapConfig{
		"with command":    {Username: "tester", Command: "echo hi && exit 3"},
		"without command": {Username: "tester"},
		"command with quotes and $": {Username: "tester",
			Command: `echo "it's $HOME" && printf '%s\n' "a|b"`},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := BuildLinuxBootstrap(cfg)
			if err != nil {
				t.Fatalf("BuildLinuxBootstrap: %v", err)
			}
			c := exec.Command(bash, "-n")
			c.Stdin = strings.NewReader(s)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("generated bootstrap is not valid bash: %v\n%s", err, out)
			}
		})
	}
}
