package launcher

import (
	"strconv"
	"strings"
	"testing"
)

// buildForGates is the common fixture: a bootstrap with a --command workload.
func buildForGates(t *testing.T, cfg BootstrapConfig) string {
	t.Helper()
	cfg.Username = "ec2-user"
	script, err := BuildLinuxBootstrap(cfg)
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	return script
}

// TestCommandWaitsForStorage is spawn#668.
//
// --command is launched from linuxBootstrapBody, which is concatenated at a
// FIXED point in BuildLinuxBootstrap — before StorageScript. So the workload
// started while its mounts did not yet exist, and the reporter saw
// `df: /efs: No such file or directory` from a command that had asked for
// --efs-id. #166 fixed exactly this for --user-data by appending it after the
// storage script; --command lives inside the static body and could not move.
//
// The ordering is the bug, so the ordering is what this asserts: the storage
// script genuinely comes after the command block, and the gate declaration
// genuinely comes before it.
func TestCommandWaitsForStorage(t *testing.T) {
	script := buildForGates(t, BootstrapConfig{
		Command:          "df -h /efs",
		StorageScript:    "#!/bin/bash\nmount -a\n",
		ReadyMountPoints: []string{"/efs"},
	})

	gateDecl := strings.Index(script, RequiredGatesFile)
	cmdBlock := strings.Index(script, "/tmp/spawn-command.sh")
	storageSignal := strings.LastIndex(script, StorageReadyGate)

	if gateDecl < 0 {
		t.Fatal("no gate declaration: --command has nothing to wait on, so it starts " +
			"before the mounts exist (#668)")
	}
	if cmdBlock < 0 {
		t.Fatal("command block missing from the bootstrap")
	}
	if gateDecl > cmdBlock {
		t.Errorf("the gate list is written at offset %d but --command is launched at %d. "+
			"The list MUST be on disk first: the workload reads it when it begins waiting, "+
			"so a declaration appearing later is simply missed and the wait is skipped.",
			gateDecl, cmdBlock)
	}
	if storageSignal < cmdBlock {
		t.Errorf("the storage gate is signalled at %d, before the command block at %d — "+
			"if storage really did run first, no gate would be needed at all. This test is "+
			"asserting the ordering that makes #668 real; re-check the assumption.",
			storageSignal, cmdBlock)
	}

	// The declared gate must be the storage one, and the mount point must be
	// verified rather than assumed.
	if !strings.Contains(script, StorageReadyGate) {
		t.Error("storage gate not declared despite a StorageScript")
	}
	if !strings.Contains(script, "/proc/mounts") {
		t.Error("the storage gate must VERIFY the mount: the storage script ends in " +
			"`echo >> /etc/fstab`, which exits 0 whether or not anything mounted, so an " +
			"exit-code check would report success for a failed mount")
	}
	if !strings.Contains(script, "'/efs'") {
		t.Errorf("the required mount point must be single-quoted in the verify loop:\n%s",
			script[storageSignal:min(len(script), storageSignal+600)])
	}
}

// TestGateWaitIsInsideTheBackgroundedSubshell guards the constraint that makes
// this safe at all.
//
// The command block runs in a backgrounded subshell on purpose: spored is
// installed LATER in the same script, so blocking cloud-init here would leave
// TTL, idle and cost enforcement unarmed for as long as the command runs. That
// is the spored#65 failure, and a wait placed outside the subshell would
// reintroduce it — turning a cost-safety mechanism off for up to the gate
// timeout on every launch.
func TestGateWaitIsInsideTheBackgroundedSubshell(t *testing.T) {
	script := buildForGates(t, BootstrapConfig{
		Command:          "true",
		StorageScript:    "mount -a\n",
		ReadyMountPoints: []string{"/efs"},
	})

	subshellStart := strings.Index(script, "    (\n")
	waitLoop := strings.Index(script, "waiting for $GATE")
	subshellEnd := strings.Index(script, "    ) &")

	if subshellStart < 0 || subshellEnd < 0 {
		t.Fatal("could not locate the backgrounded subshell in the command block")
	}
	if waitLoop < 0 {
		t.Fatal("no wait loop found")
	}
	if waitLoop < subshellStart || waitLoop > subshellEnd {
		t.Errorf("the gate wait is at offset %d, outside the backgrounded subshell "+
			"(%d..%d). Waiting there blocks cloud-init, so spored never starts and "+
			"TTL/idle/cost enforcement stays unarmed for the whole wait — spored#65, which "+
			"is worse than the bug this fixes.", waitLoop, subshellStart, subshellEnd)
	}
}

// TestNoGatesWhenNothingToWaitFor: a gate declared with no producer would stall
// every launch for the full timeout and then fail it. So gates must appear only
// when something will actually signal them.
func TestNoGatesWhenNothingToWaitFor(t *testing.T) {
	t.Run("no storage and no MPI", func(t *testing.T) {
		script := buildForGates(t, BootstrapConfig{Command: "echo hi"})
		if strings.Contains(script, RequiredGatesFile+" <<") {
			t.Error("gates declared with no storage and no MPI: --command would wait " +
				"600s for a sentinel nothing writes, then fail a launch that works today")
		}
	})

	t.Run("storage but no command", func(t *testing.T) {
		script := buildForGates(t, BootstrapConfig{
			StorageScript:    "mount -a\n",
			ReadyMountPoints: []string{"/efs"},
		})
		if strings.Contains(script, "cat > "+RequiredGatesFile) {
			t.Error("gate list written with no --command: nothing waits on it, so it is " +
				"pure noise in the user-data")
		}
	})
}

// TestMPIGateIsDeclared is spawn#664: the MPI/EFA setup script is appended after
// the whole bootstrap (by buildJobArrayMemberConfig), so --command starts before
// mpirun has a hostfile or a peer. The gate must be declarable at BUILD time,
// since a declaration appended after the body would come too late to be read.
func TestMPIGateIsDeclared(t *testing.T) {
	script := buildForGates(t, BootstrapConfig{
		Command:    "mpirun ./gchp",
		ReadyGates: []string{MPIReadyGate},
	})

	if !strings.Contains(script, MPIReadyGate) {
		t.Fatalf("MPI gate not declared; --command would run before MPI setup (#664)")
	}
	gateDecl := strings.Index(script, MPIReadyGate)
	cmdBlock := strings.Index(script, "/tmp/spawn-command.sh")
	if gateDecl > cmdBlock {
		t.Errorf("MPI gate declared at %d, after the command block at %d — too late to "+
			"be read", gateDecl, cmdBlock)
	}
}

// TestGateFailureIsReportedInCommandLog: #668 asks specifically that a mount
// failure surface in spawn-command.log, not only cloud-init-output. A user
// debugging a failed workload reads the command's own log.
func TestGateFailureIsReportedInCommandLog(t *testing.T) {
	script := buildForGates(t, BootstrapConfig{
		Command:          "true",
		StorageScript:    "mount -a\n",
		ReadyMountPoints: []string{"/efs"},
	})

	for _, want := range []string{
		"/var/log/spawn-command.log",
		"prerequisite failed",
		"SPAWN_COMPLETE",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("expected %q in the gate failure path", want)
		}
	}

	// A failed prerequisite must mark the workload failed rather than leaving it
	// pending: --on-complete and `spawn status --check-complete` both read
	// SPAWN_COMPLETE, and without it the instance burns its whole TTL.
	if !strings.Contains(script, `{"status": "failed", "exit_code": 1, "source": "command"}`) {
		t.Error("an unmet prerequisite must write a failed SPAWN_COMPLETE, or --on-complete " +
			"never fires and the instance bills to TTL doing nothing")
	}
	if !strings.Contains(script, "--command NOT started") {
		t.Error("the log must say the command never ran; 'failed' alone reads as a workload " +
			"that ran and failed, which sends the user debugging the wrong thing")
	}
}

// TestGateTimeoutMatchesConstant stops the Go constant and the shell literal
// from drifting. linuxBootstrapBody is a plain const string with no
// interpolation, so the timeout is hardcoded there and nothing links the two.
func TestGateTimeoutMatchesConstant(t *testing.T) {
	want := strconv.Itoa(ReadyGateTimeoutSecs)
	if !strings.Contains(linuxBootstrapBody, `-ge `+want) {
		t.Errorf("the bootstrap body does not use ReadyGateTimeoutSecs (%s). The body is a "+
			"const string with no interpolation, so these drift silently — update both.", want)
	}
}

// TestShellQuoteJoinResistsExpansion. The mount-point list is interpolated into
// a shell `for` loop, so it must be quoted in a way that suppresses expansion.
//
// security.ShellEscape is strconv.Quote — Go/C escaping inside DOUBLE quotes,
// where $VAR, $(...) and backticks still expand. That is spawn#660, and reusing
// it here would have reintroduced it in a new place.
func TestShellQuoteJoinResistsExpansion(t *testing.T) {
	got := shellQuoteJoin([]string{"/efs", "/fsx"})
	if got != "'/efs' '/fsx'" {
		t.Errorf("shellQuoteJoin = %q, want single-quoted paths", got)
	}

	for _, nasty := range []string{"/a$HOME", "/a$(id)", "/a`id`", `/a"b`} {
		q := shellQuoteJoin([]string{nasty})
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Errorf("%q must be single-quoted, got %q", nasty, q)
		}
		if strings.HasPrefix(q, `"`) {
			t.Errorf("%q was double-quoted (%q) — $VAR and $(...) still expand inside "+
				"double quotes; that is the #660 bug", nasty, q)
		}
	}

	// An embedded single quote must be escaped, not allowed to terminate the
	// quoting and leak the rest as code.
	if got := shellQuoteJoin([]string{"/a'b"}); got != `'/a'\''b'` {
		t.Errorf("embedded quote mishandled: %q", got)
	}
}
