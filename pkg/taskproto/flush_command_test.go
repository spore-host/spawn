package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// flushCommandRun runs FlushCommandWithOutputs(why, maxGrant) under sh, as
// AWS-RunShellScript does, against a hook whose head is header. timeout(1) is
// a stub recording its bound, so nothing waits; find(1) a stub finding nothing
// (the trust check's GNU -perm /022 is not portable to the test host).
func flushCommandRun(t *testing.T, header string, maxGrant int) (bounds, hookArgv []string) {
	t.Helper()
	skipOnWindows(t, "runs the SSM flush command under sh with stubs on a POSIX PATH")
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	toLog, argvLog := filepath.Join(dir, "timeout.log"), filepath.Join(dir, "argv.log")
	writeFakeExe(t, filepath.Join(binDir, "timeout"), "#!/bin/sh\necho \"$1\" >> "+toLog+"\nshift\nexec \"$@\"\n")
	writeFakeExe(t, filepath.Join(binDir, "find"), "#!/bin/sh\nexit 0\n")
	hook := filepath.Join(dir, "task-flush.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+header+"echo \"$*\" >> "+argvLog+"\n"), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatal(err)
	}
	t.Cleanup(SetFlushScriptPathForTest(hook))

	// The command under test: this package's constants, no outside input.
	// nosemgrep
	cmd := exec.Command("sh", "-c", FlushCommandWithOutputs(ExitUserTerminated, maxGrant)) //nolint:gosec // the command under test
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flush command failed: %v\n%s", err, out)
	}
	return readLines(toLog), readLines(argvLog)
}

// spawn#1312: the SSM flush grants the hook its declared output budget, at
// most the caller's cap, as $2, and bounds it by 45 s + grant + slack, the
// same header and bound spored uses. A hook with no header (or from before
// #1297) and a cap of 0 run as they always did: 45 s, no $2.
func TestFlushCommandWithOutputsGrantsTheDeclaredBudget(t *testing.T) {
	bound := func(g int) string { return strconv.Itoa(FlushHookSeconds + g + FlushOutputsSlackSeconds) }
	for _, tc := range []struct {
		name     string
		header   string
		maxGrant int
		argv     string
		bound    string
	}{
		{"declared under the cap", flushOutputsHeader + "60\n", MaxFlushOutputsSeconds, "user_terminated 60", bound(60)},
		{"capped", flushOutputsHeader + "60\n", 30, "user_terminated 30", bound(30)},
		{"over the maximum", flushOutputsHeader + "9999\n", 1000, "user_terminated 300", bound(300)},
		{"declares zero", flushOutputsHeader + "0\n", MaxFlushOutputsSeconds, "user_terminated", "45"},
		{"no header", "", MaxFlushOutputsSeconds, "user_terminated", "45"},
		{"no grant", flushOutputsHeader + "60\n", 0, "user_terminated", "45"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounds, argv := flushCommandRun(t, tc.header, tc.maxGrant)
			if len(argv) != 1 || argv[0] != tc.argv {
				t.Errorf("hook argv = %v, want [%q]", argv, tc.argv)
			}
			if len(bounds) != 1 || bounds[0] != tc.bound {
				t.Errorf("timeout bounds = %v, want [%s]", bounds, tc.bound)
			}
		})
	}
}

// FlushCommand is the no-grant form, unchanged for every older caller.
func TestFlushCommandGrantsNothing(t *testing.T) {
	if FlushCommand(ExitTTLExpired) != FlushCommandWithOutputs(ExitTTLExpired, 0) {
		t.Error("FlushCommand must be FlushCommandWithOutputs with no grant")
	}
}
