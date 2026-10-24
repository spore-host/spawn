package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/shellmin"
	"github.com/spore-host/spawn/pkg/plugin"
)

// These tests run the generated bootstrap under bash with a stubbed spored, for
// #1007: a replacement EC2 launches from a leaked spawn persistent Spot request
// boots this same user data, and must not re-run the user's workload.
//
// The contract, from the issue: `spored is-spot-replacement` exits 0 on a
// replacement, 1 on an original, 2 when it cannot tell. Only 0 skips the
// workload; everything else runs it, as the original always has.

const (
	workloadMarker = "WORKLOAD_RAN_MARKER"
	userDataMarker = "USERDATA_RAN_MARKER"
	appendedMarker = "APPENDED_SCRIPT_RAN_MARKER"
)

// sporedStub describes the fake spored: exit code, or absent.
type sporedStub struct {
	code    int
	missing bool
}

// spotSandbox is a temp root the bootstrap's absolute paths are rewritten into,
// with stub spored and su on PATH.
type spotSandbox struct {
	dir string
	rw  *strings.Replacer
}

func newSpotSandbox(t *testing.T, stub sporedStub) *spotSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux user-data bootstrap (/run/spawn, /etc/spawn) under bash with stubbed spored and su (#1002)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	for _, d := range []string{"bin", "tmp", "etc/spawn", "var/log", "run/spawn"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !stub.missing {
		writeExec(t, filepath.Join(dir, "bin", "spored"), fmt.Sprintf(
			"#!/bin/bash\necho \"$@\" >> %q\necho stub-spored-verdict\nexit %d\n",
			filepath.Join(dir, "spored-calls"), stub.code))
	}
	// su - USER -c SCRIPT: run SCRIPT. The real one switches user.
	writeExec(t, filepath.Join(dir, "bin", "su"), "#!/bin/bash\nexec bash \"$4\"\n")
	return &spotSandbox{dir: dir, rw: strings.NewReplacer(
		"/usr/local/bin/spored", filepath.Join(dir, "bin", "spored"),
		"/run/spawn", filepath.Join(dir, "run", "spawn"),
		"/etc/spawn/", filepath.Join(dir, "etc", "spawn")+"/",
		"/var/log/", filepath.Join(dir, "var", "log")+"/",
		"/var/lib/spawn", filepath.Join(dir, "var", "lib", "spawn"),
		"/tmp/", filepath.Join(dir, "tmp")+"/",
	)}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (sb *spotSandbox) run(t *testing.T, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", sb.rw.Replace(script)) //nolint:gosec // nosemgrep
	cmd.Env = []string{"PATH=" + filepath.Join(sb.dir, "bin") + ":/usr/bin:/bin", "HOME=" + sb.dir}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (sb *spotSandbox) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(sb.dir, name))
	return string(b)
}

func (sb *spotSandbox) sporedCalls() int {
	return strings.Count(sb.read("spored-calls"), "is-spot-replacement")
}

// between returns s from the first start to the next end (end excluded).
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found in the bootstrap", start)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, start)
	}
	return s[i : i+j]
}

// completionAssignment is the script's own assignment of the completion-file
// variable the --command block writes to (#1274), for a test that runs the
// block on its own.
func completionAssignment(t *testing.T, script string) string {
	t.Helper()
	m := completionAssignRe.FindAllString(script, -1)
	if len(m) != 1 {
		t.Fatalf("the bootstrap assigns %s %d times, want once", completionFileVar, len(m))
	}
	return m[0] + "\n"
}

// checkFunc is the generated spawn_is_spot_replacement definition.
func checkFunc(t *testing.T, script string) string {
	t.Helper()
	return between(t, script, "spawn_is_spot_replacement() {", "\n}\n") + "\n}\n"
}

// TestCommandSkippedOnlyOnASpotReplacement runs the real --command block of the
// bootstrap: the user's command must not start on a replacement, and must start
// on every other answer, including no answer at all.
func TestCommandSkippedOnlyOnASpotReplacement(t *testing.T) {
	cases := map[string]struct {
		stub    sporedStub
		wantRun bool
	}{
		"replacement exit 0": {sporedStub{code: 0}, false},
		"original exit 1, or an older spored without the subcommand": {sporedStub{code: 1}, true},
		"unknown exit 2":     {sporedStub{code: 2}, true},
		"unexpected exit 3":  {sporedStub{code: 3}, true},
		"timed out exit 124": {sporedStub{code: 124}, true},
		"spored missing":     {sporedStub{missing: true}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester", Command: "echo " + workloadMarker})
			if err != nil {
				t.Fatal(err)
			}
			sb := newSpotSandbox(t, tc.stub)
			if err := os.WriteFile(filepath.Join(sb.dir, "etc", "spawn", "command"), []byte("echo "+workloadMarker+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			block := between(t, script, "if [ -s /etc/spawn/command ]; then\n    WORKFLOW_COMMAND=", "\necho \"Creating login banner...\"")
			out, err := sb.run(t, "set -e\nLOCAL_USERNAME=tester\n"+completionAssignment(t, script)+checkFunc(t, script)+block+"\nwait\n")
			if err != nil {
				t.Fatalf("command block failed: %v\n%s", err, out)
			}
			log := sb.read("var/log/spawn-command.log")
			ran := strings.Contains(log, workloadMarker)
			if ran != tc.wantRun {
				t.Fatalf("workload ran=%v, want %v\nstdout:\n%s\ncommand log:\n%s", ran, tc.wantRun, out, log)
			}
			complete := sb.read("tmp/SPAWN_COMPLETE")
			if tc.wantRun && !strings.Contains(complete, `"completed"`) {
				t.Errorf("an original's command finished but SPAWN_COMPLETE = %q", complete)
			}
			if !tc.wantRun {
				if complete != "" {
					t.Errorf("a skipped workload wrote SPAWN_COMPLETE (%q): nothing ran, so nothing completed", complete)
				}
				if !strings.Contains(log, "Spot replacement") {
					t.Errorf("the skip is not explained in spawn-command.log:\n%s", log)
				}
			}
		})
	}
}

// TestNothingAfterSporedStartsOnASpotReplacement runs everything the bootstrap
// emits after `systemctl start spored`: custom user data, plus a script appended
// after the bootstrap the way the MPI and queue-runner paths do. None of it may
// run on a replacement; all of it must run otherwise.
func TestNothingAfterSporedStartsOnASpotReplacement(t *testing.T) {
	for _, tc := range []struct {
		stub    sporedStub
		wantRun bool
	}{
		{sporedStub{code: 0}, false},
		{sporedStub{code: 1}, true},
		{sporedStub{code: 2}, true},
		{sporedStub{missing: true}, true},
	} {
		t.Run(fmt.Sprintf("code=%d missing=%v", tc.stub.code, tc.stub.missing), func(t *testing.T) {
			script, err := BuildLinuxBootstrap(BootstrapConfig{
				Username:       "tester",
				CustomUserData: "echo " + userDataMarker + "\n",
			})
			if err != nil {
				t.Fatal(err)
			}
			// From the Spot-replacement stop at the end of the body: spored now
			// starts early in the body (#1274), so what follows it there is the
			// body's own setup, not the post-body content this test is about.
			start := shellmin.Strip(spotReplacementStopScript)
			i := strings.Index(script, start)
			if i < 0 {
				t.Fatal("the Spot-replacement stop was not found")
			}
			if j := strings.Index(script, "systemctl start spored\n"); j < 0 || j > i {
				t.Fatal("spored does not start before the Spot-replacement stop")
			}
			tail := script[i:] + "\necho " + appendedMarker + "\n"

			sb := newSpotSandbox(t, tc.stub)
			out, err := sb.run(t, "set -e\n"+checkFunc(t, script)+tail)
			if err != nil {
				t.Fatalf("bootstrap tail failed: %v\n%s", err, out)
			}
			for _, m := range []string{userDataMarker, appendedMarker} {
				if got := strings.Contains(out, m); got != tc.wantRun {
					t.Errorf("%s ran=%v, want %v\n%s", m, got, tc.wantRun, out)
				}
			}
		})
	}
}

// The check sits inside the backgrounded --command subshell, ahead of the
// command: outside it, a slow check would delay spored's start (spored#65).
func TestSpotReplacementCheckIsInsideTheCommandSubshell(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester", Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	su := strings.Index(script, `su - "$LOCAL_USERNAME" -c '/tmp/spawn-command.sh'`)
	open := strings.LastIndex(script[:su], "\n    (\n")
	guard := strings.Index(script, "\n        if spawn_is_spot_replacement; then")
	if su < 0 || open < 0 || guard < 0 {
		t.Fatalf("markers not found (su=%d open=%d guard=%d)", su, open, guard)
	}
	if guard < open || guard > su {
		t.Fatal("the Spot replacement check is not inside the --command subshell ahead of the command")
	}
}

// spored reads /etc/spawn/plugins.json once, at startup, so the bootstrap
// writes it before starting spored (#1023). The post-spored check costs an EC2
// round trip and can exit the script, so placed ahead of the declarations it
// would make spored miss them on every original instance.
func TestSpotReplacementStopComesAfterPluginDeclarations(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username: "tester",
		Plugins:  []plugin.Declaration{{Ref: "spore-host/jupyter"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plugins := strings.Index(script, "cat > /etc/spawn/plugins.json")
	stop := strings.Index(script, "\nif spawn_is_spot_replacement; then")
	if plugins < 0 || stop < 0 {
		t.Fatalf("markers not found (plugins=%d stop=%d)", plugins, stop)
	}
	if stop < plugins {
		t.Error("the Spot replacement stop runs before the plugin declarations are written, " +
			"so the check's latency makes spored start without them")
	}
}

// A definitive answer is asked once; "unknown" is asked again by the next
// caller, which may get a better answer.
func TestSpotReplacementCheckCachesOnlyDefinitiveAnswers(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	for code, wantCalls := range map[int]int{0: 1, 1: 1, 2: 2} {
		sb := newSpotSandbox(t, sporedStub{code: code})
		out, err := sb.run(t, checkFunc(t, script)+"spawn_is_spot_replacement || true\nspawn_is_spot_replacement || true\n")
		if err != nil {
			t.Fatalf("exit %d: %v\n%s", code, err, out)
		}
		if got := sb.sporedCalls(); got != wantCalls {
			t.Errorf("stub exit %d: spored asked %d times, want %d", code, got, wantCalls)
		}
	}
}

func TestSpotReplacementBootstrapIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester", Command: "true", CustomUserData: "true"})
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", "-n")
	c.Stdin = strings.NewReader(script)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("not valid bash: %v\n%s", err, out)
	}
}
