package launcher

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// #1057: an image baked from a --command instance must not carry that run's
// state, and the CreateImage reboot must not re-run the command.
//
// "Run state" is discovered from the rendered bootstrap, not listed:
//   - every file it writes under /tmp, which is on the root volume — and so
//     in the image — on distros without a tmpfs /tmp (Rocky 9 Base);
//   - the completion file, wherever --completion-file put it;
//   - every file it both writes and tests for ([ -s|-f|-e PATH ]): the inputs a
//     later run of the bootstrap reads as "a command ran" or "there is a
//     command to run".
//
// A log the bootstrap appends to and never tests (/var/log/spawn-command.log)
// is not run state, and is left alone on the source.

const testCompletionFile = "/data/out dir/done.json"

func renderedCommandBootstrap(t *testing.T) string {
	t.Helper()
	s, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:       "ec2-user",
		PublicKey:      []byte("ssh-ed25519 AAAAtest test"),
		Command:        "echo build",
		CompletionFile: testCompletionFile,
		ReadyGates:     []string{MPIReadyGate},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	// A redirect target or `cat > target`: optional quote, an absolute path
	// (which may carry a $VAR), the matching quote.
	writeTarget = regexp.MustCompile(`>>?\s*(['"]?)(/[^\s'";|&)<>]+)(['"]?)`)
	testTarget  = regexp.MustCompile(`\[ -[sfe] (['"]?)(/[^\s'"\]]+)(['"]?) \]`)
)

func discoverRunState(t *testing.T, script string) []string {
	t.Helper()
	writes := map[string]bool{}
	for _, m := range writeTarget.FindAllStringSubmatch(script, -1) {
		writes[m[2]] = true
	}
	// The completion file is shell-quoted with a space in it; find it by value.
	quotedCF := "'" + testCompletionFile + "'"
	if strings.Contains(script, "> "+quotedCF) {
		writes[testCompletionFile] = true
	}
	set := map[string]bool{}
	for p := range writes {
		if strings.HasPrefix(p, "/tmp/") || p == testCompletionFile {
			set[p] = true
		}
	}
	for _, m := range testTarget.FindAllStringSubmatch(script, -1) {
		if writes[m[2]] {
			set[m[2]] = true
		}
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	if !set[testCompletionFile] || len(out) < 3 {
		t.Fatalf("discovered run state %v: the completion file or the /tmp writes were not found, so discovery is broken", out)
	}
	return out
}

func runCleanup(t *testing.T, root, instanceID string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the cleanup is a POSIX sh script run on a Linux instance")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-c", ImageStateCleanupScript(instanceID, testCompletionFile))
	cmd.Env = append(os.Environ(), "SPAWN_IMAGE_ROOT="+root)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImageStateCleanupRemovesEveryDiscoveredRunStatePath(t *testing.T) {
	discovered := discoverRunState(t, renderedCommandBootstrap(t))
	t.Logf("discovered run state: %v", discovered)

	root := t.TempDir()
	const self, parent = "i-0self", "i-0parent"
	var keep string
	var mustGo []string
	for _, p := range discovered {
		if strings.Contains(p, "$INSTANCE_ID") {
			// Keyed on the instance: this instance's copy must survive (it is
			// what stops the CreateImage reboot re-running the command); one
			// inherited from a parent image must not.
			keep = root + strings.ReplaceAll(p, "$INSTANCE_ID", self)
			touch(t, keep)
			inherited := root + strings.ReplaceAll(p, "$INSTANCE_ID", parent)
			touch(t, inherited)
			mustGo = append(mustGo, inherited)
			continue
		}
		touch(t, root+p)
		mustGo = append(mustGo, root+p)
	}
	if keep == "" {
		t.Fatal("discovered no instance-keyed command-ran marker: the bootstrap no longer guards against re-running --command (#1057)")
	}
	// Spored also accepts the default path whatever --completion-file says.
	touch(t, root+defaultCompletionFile)
	mustGo = append(mustGo, root+defaultCompletionFile)
	// Not run state: the command log.
	logPath := root + "/var/log/spawn-command.log"
	touch(t, logPath)

	out, code := runCleanup(t, root, self)
	if code != 0 || !strings.Contains(out, ImageCleanOK) {
		t.Fatalf("cleanup exit %d, output %q", code, out)
	}
	for _, p := range mustGo {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived the cleanup and would be baked into the image", strings.TrimPrefix(p, root))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("this instance's command-ran marker was removed: the CreateImage reboot would re-run --command")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("the command log was removed; it is a record, not run state")
	}
}

func TestImageStateCleanupRefusesWhileTheCommandIsRunning(t *testing.T) {
	root := t.TempDir()
	// Started (marker) but not finished (no exit code, no completion file).
	touch(t, root+CommandRanMarkerDir+"/command-ran-i-0self")
	touch(t, root+"/etc/spawn/command")
	out, code := runCleanup(t, root, "i-0self")
	if code != ImageCleanCommandRunning || strings.Contains(out, ImageCleanOK) {
		t.Fatalf("exit %d, output %q; want exit %d and no ok line", code, out, ImageCleanCommandRunning)
	}
}

// TestBootstrapRunsCommandOncePerInstance: the marker is checked before the
// command starts and written before it runs, keyed on the instance ID.
func TestBootstrapRunsCommandOncePerInstance(t *testing.T) {
	s := renderedCommandBootstrap(t)
	marker := `"/var/lib/spawn/command-ran-$INSTANCE_ID"`
	check := strings.Index(s, `if [ -e `+marker+` ]; then`)
	write := strings.Index(s, `> `+marker)
	run := strings.Index(s, `su - "$LOCAL_USERNAME" -c '/tmp/spawn-command.sh'`)
	if check < 0 || write < 0 || run < 0 {
		t.Fatalf("marker check %d / write %d / command run %d: one is missing", check, write, run)
	}
	if !(check < write && write < run) {
		t.Errorf("order check=%d write=%d run=%d; want check, then write, then run", check, write, run)
	}
	if !strings.Contains(s[check:write], "exit 0") {
		t.Error("an existing marker does not stop the command")
	}
	if CommandRanMarkerDir+"/command-ran-$INSTANCE_ID" != strings.Trim(marker, `"`) {
		t.Errorf("CommandRanMarkerDir %q disagrees with the bootstrap's marker %s", CommandRanMarkerDir, marker)
	}
}

// TestCommandRunsOncePerInstanceID runs the real --command block three times
// in one sandbox (#1057): the first run on an instance runs the workload; a
// second run of the bootstrap on the SAME instance — the CreateImage reboot
// after a 'cloud-init clean' — does not; and a NEW instance launched from the
// image, which inherits the first instance's marker, runs its own.
func TestCommandRunsOncePerInstanceID(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "tester", Command: "echo " + workloadMarker})
	if err != nil {
		t.Fatal(err)
	}
	sb := newSpotSandbox(t, sporedStub{code: 1}) // an original, not a Spot replacement
	if err := os.WriteFile(filepath.Join(sb.dir, "etc", "spawn", "command"), []byte("echo "+workloadMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	block := between(t, script, "if [ -s /etc/spawn/command ]; then\n    WORKFLOW_COMMAND=", "\necho \"Creating login banner...\"")
	runAs := func(id string) {
		t.Helper()
		out, err := sb.run(t, "set -e\nLOCAL_USERNAME=tester\nINSTANCE_ID="+id+"\n"+checkFunc(t, script)+block+"\nwait\n")
		if err != nil {
			t.Fatalf("command block on %s failed: %v\n%s", id, err, out)
		}
	}
	runs := func() int { return strings.Count(sb.read("var/log/spawn-command.log"), workloadMarker+"\n") }

	runAs("i-0first")
	if runs() != 1 {
		t.Fatalf("first boot: workload ran %d times, want 1\n%s", runs(), sb.read("var/log/spawn-command.log"))
	}
	runAs("i-0first")
	if runs() != 1 {
		t.Fatalf("the bootstrap re-ran on the same instance and the workload ran again (%d runs)", runs())
	}
	if !strings.Contains(sb.read("var/log/spawn-command.log"), "already ran on this instance") {
		t.Errorf("the skip is not explained in spawn-command.log")
	}
	runAs("i-0fromimage")
	if runs() != 2 {
		t.Fatalf("an instance launched from the image did not run its own --command (%d runs, want 2)", runs())
	}
}
