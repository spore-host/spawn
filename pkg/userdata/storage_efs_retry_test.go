package userdata

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// efsScript renders the EFS branch of the storage user-data.
func efsScript(t *testing.T) string {
	t.Helper()
	script, err := GenerateStorageUserData(StorageConfig{
		EFSEnabled:       true,
		EFSFilesystemDNS: "fs-0bc771ea6abff6cec.efs.us-east-1.amazonaws.com",
		EFSMountPoint:    "/efs",
		EFSMountOptions:  "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2",
	})
	if err != nil {
		t.Fatalf("GenerateStorageUserData: %v", err)
	}
	return script
}

// extractMountFunc pulls the real spawn_mount_efs function out of the generated
// script and rewrites `mount`, `mountpoint` and `sleep` to call stubs on PATH, so
// the retry logic runs for real under bash without root or an actual filesystem.
//
// Fails the test if the markers move, so a refactor of storage.go that breaks
// the extraction is caught rather than silently testing stale text.
func extractMountFunc(t *testing.T, script string) string {
	t.Helper()
	const start = "spawn_mount_efs() {"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("spawn_mount_efs not found in the generated script (marker moved?)")
	}
	rest := script[i:]
	const end = "\n}\n"
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatal("could not find the end of spawn_mount_efs")
	}
	return rest[:j+len(end)]
}

// stubBin writes fake mount/mountpoint/sleep onto a temp PATH.
//
// mount succeeds on the attempt number in successOn (0 = never); every call
// appends a line to $SPAWN_TEST_LOG so the test can count attempts. sleep is
// stubbed to a no-op so a 60-second retry budget costs nothing.
func stubBin(t *testing.T, successOn int) string {
	t.Helper()
	dir := t.TempDir()

	mount := fmt.Sprintf(`#!/bin/sh
echo mount >> "$SPAWN_TEST_LOG"
n=$(grep -c '^mount$' "$SPAWN_TEST_LOG")
[ "%d" -ne 0 ] && [ "$n" -ge "%d" ] && exit 0
echo "mount.nfs4: Failed to resolve server" >&2
exit 32
`, successOn, successOn)

	for name, body := range map[string]string{
		"mount": mount,
		// Never already-mounted: that shortcut must not be what makes the test
		// pass, or it would hide a broken retry.
		"mountpoint": "#!/bin/sh\nexit 1\n",
		"sleep":      "#!/bin/sh\necho sleep >> \"$SPAWN_TEST_LOG\"\nexit 0\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

func runMountFunc(t *testing.T, fn, bin string) (code int, log string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	logPath := filepath.Join(t.TempDir(), "calls.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	script := fn + "\nspawn_mount_efs\n"
	// Executing the generated shell IS the point: #704 was invisible to every
	// text assertion, because the text was correct and attempted exactly once.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"SPAWN_TEST_LOG=" + logPath,
	}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read log: %v", rerr)
	}
	return code, string(b)
}

// TestEFSMountRetriesAfterATransientDNSFailure is spawn#704.
//
// A mount target that was already `available` in the instance's own subnet
// still produced "Failed to resolve server fs-….efs.us-east-1.amazonaws.com"
// while general DNS worked and the mount target's IP mounted first try. The
// generated script attempted the mount exactly once, so one DNS blip became a
// launch that boots, bills and runs nothing — #668's readiness barrier correctly
// refuses to start a workload against an unmounted directory.
//
// This runs the real generated function under bash with a stubbed mount, because
// the bug was invisible to text assertions: the mount command itself was correct.
func TestEFSMountRetriesAfterATransientDNSFailure(t *testing.T) {
	fn := extractMountFunc(t, efsScript(t))

	// Fails twice, succeeds on the third — the shape of EFS DNS lagging.
	code, log := runMountFunc(t, fn, stubBin(t, 3))
	if code != 0 {
		t.Errorf("mount gave up (exit %d) though the third attempt would have succeeded.\n"+
			"calls:\n%s", code, log)
	}
	if n := strings.Count(log, "mount\n"); n != 3 {
		t.Errorf("made %d mount attempt(s), want 3 — a single attempt is #704", n)
	}
	if !strings.Contains(log, "sleep") {
		t.Error("no sleep between attempts; retrying instantly does not wait out a DNS lag")
	}
}

// TestEFSMountEventuallyGivesUp: the retry must be bounded. An unbounded loop
// would hang the bootstrap, which is worse than a failed mount the readiness
// barrier can report.
func TestEFSMountEventuallyGivesUp(t *testing.T) {
	fn := extractMountFunc(t, efsScript(t))

	code, log := runMountFunc(t, fn, stubBin(t, 0)) // never succeeds
	if code == 0 {
		t.Error("reported success though the mount never succeeded")
	}
	n := strings.Count(log, "mount\n")
	if n < 2 {
		t.Errorf("only %d attempt(s) — the retry did not engage", n)
	}
	if n > 10 {
		t.Errorf("%d attempts; the budget should be bounded so a persistent failure does "+
			"not hang the bootstrap", n)
	}
	// No pointless wait after the final attempt.
	if s := strings.Count(log, "sleep\n"); s >= n {
		t.Errorf("%d sleeps for %d attempts — the last attempt should not be followed by a "+
			"wait nothing uses", s, n)
	}
}

// TestEFSMountSucceedsFirstTryWithoutWaiting: the happy path must not have
// gained a delay. A retry that costs every healthy launch ten seconds would be
// paid by everyone to fix an intermittent failure.
func TestEFSMountSucceedsFirstTryWithoutWaiting(t *testing.T) {
	fn := extractMountFunc(t, efsScript(t))

	code, log := runMountFunc(t, fn, stubBin(t, 1))
	if code != 0 {
		t.Errorf("exit %d on a mount that succeeds immediately; calls:\n%s", code, log)
	}
	if n := strings.Count(log, "mount\n"); n != 1 {
		t.Errorf("made %d attempts when the first succeeded", n)
	}
	if strings.Contains(log, "sleep") {
		t.Error("slept on the happy path; every healthy launch would pay for the retry")
	}
}

// TestEFSFstabKeepsTheDNSName: whatever the boot-time mount does, fstab must
// carry the DNS name. An IP there would not survive a mount-target replacement —
// which is why the IP is only ever a fallback (#704).
func TestEFSFstabKeepsTheDNSName(t *testing.T) {
	script := efsScript(t)
	if !strings.Contains(script, "fs-0bc771ea6abff6cec.efs.us-east-1.amazonaws.com:/ /efs nfs4") {
		t.Error("the fstab line does not carry the EFS DNS name")
	}
	// The retry must still produce valid bash.
	if _, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader("#!/bin/bash\n" + script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("generated storage script is not valid bash: %v\n%s", err, out)
		}
	}
}

// efsScriptWithIP renders the EFS branch with a mount-target IP fallback.
func efsScriptWithIP(t *testing.T) string {
	t.Helper()
	script, err := GenerateStorageUserData(StorageConfig{
		EFSEnabled:       true,
		EFSFilesystemDNS: "fs-0cc326d51039a4df6.efs.us-east-1.amazonaws.com",
		EFSMountTargetIP: "172.31.1.24",
		EFSMountPoint:    "/efs",
		EFSMountOptions:  "nfsvers=4.1,hard,timeo=600,retrans=2",
	})
	if err != nil {
		t.Fatalf("GenerateStorageUserData: %v", err)
	}
	return script
}

// stubBinDNSFails writes a mount stub that fails for the DNS NAME and succeeds
// for the IP — which is exactly what a real instance did: six DNS failures over
// ~60s, then a first-try success by IP with identical options.
func stubBinDNSFails(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mount := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *amazonaws.com:/*) echo "dns" >> "$SPAWN_TEST_LOG"
                       echo "mount.nfs4: Failed to resolve server" >&2; exit 32 ;;
    172.31.1.24:/)     echo "ip" >> "$SPAWN_TEST_LOG"; exit 0 ;;
  esac
done
echo "unknown" >> "$SPAWN_TEST_LOG"; exit 32
`
	for name, body := range map[string]string{
		"mount":      mount,
		"mountpoint": "#!/bin/sh\nexit 1\n",
		"sleep":      "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestEFSMountsByTheIPFirst is spawn#718: the IP is the PRIMARY path, not a
// fallback.
//
// #704 shipped the obvious order — DNS, then the mount-target IP — and a
// hardware smoke showed it was backwards. EFS DNS is unreliable at first boot:
// twice out of three runs a mount target already `available` IN THE INSTANCE'S
// OWN SUBNET produced "Failed to resolve server" for the full 60-second budget,
// while this exact mount by IP succeeded first try.
//
// None of the usual reasons to prefer the name apply to spawn: encryption in
// transit would need it, but EFSMountOptions emits a fixed set and
// ParseCustomOptions accepts no tls key; fstab durability across a mount-target
// replacement is worthless for an instance that lives minutes to hours; and DNS
// gives AZ affinity for free, which spawn already computes deterministically
// from the instance's subnet.
//
// This ordering also fixes a verification problem rather than tolerating it.
// With DNS first, the IP path ran only when DNS failed — load-bearing and
// unexercised — and because the failure is intermittent, a passing smoke could
// never prove it. Now the primary path runs on every launch.
func TestEFSMountsByTheIPFirst(t *testing.T) {
	fn := extractMountFunc(t, efsScriptWithIP(t))

	// Both would succeed. The IP must be the one tried.
	code, log := runMountFunc(t, fn, stubBin(t, 1))
	if code != 0 {
		t.Fatalf("mount failed (exit %d); calls:\n%s", code, log)
	}
	if n := strings.Count(log, "mount\n"); n != 1 {
		t.Errorf("made %d mount attempts when the first should have succeeded", n)
	}
	if strings.Contains(log, "sleep") {
		t.Error("slept on the happy path; the IP mount must not pay the DNS retry budget")
	}

	// And the script must try the IP before the DNS name, which the call count
	// alone cannot show.
	script := efsScriptWithIP(t)
	ipIdx := strings.Index(script, "172.31.1.24")
	dnsIdx := strings.Index(script, "fs-0cc326d51039a4df6.efs")
	if ipIdx < 0 || dnsIdx < 0 {
		t.Fatal("expected both the IP and the DNS name in the script")
	}
	if ipIdx > dnsIdx {
		t.Errorf("the DNS name (offset %d) is attempted before the mount-target IP "+
			"(offset %d) — that is the #704 ordering the hardware smoke disproved",
			dnsIdx, ipIdx)
	}
}

// TestEFSFallsBackToDNSWhenTheIPFails: the name is still the fallback, for a
// stale IP or a mount target replaced between launch and boot.
func TestEFSFallsBackToDNSWhenTheIPFails(t *testing.T) {
	fn := extractMountFunc(t, efsScriptWithIP(t))

	// mount fails for the IP and succeeds for the DNS name — the inverse of the
	// stub used for the old ordering.
	dir := t.TempDir()
	mount := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    172.31.1.24:/)     echo "ip" >> "$SPAWN_TEST_LOG"; echo "mount.nfs4: timed out" >&2; exit 32 ;;
    *amazonaws.com:/*) echo "dns" >> "$SPAWN_TEST_LOG"; exit 0 ;;
  esac
done
echo "unknown" >> "$SPAWN_TEST_LOG"; exit 32
`
	for name, body := range map[string]string{
		"mount":      mount,
		"mountpoint": "#!/bin/sh\nexit 1\n",
		"sleep":      "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	code, log := runMountFunc(t, fn, dir)
	if code != 0 {
		t.Errorf("mount failed (exit %d) though the DNS name would have worked; calls:\n%s", code, log)
	}
	if !strings.Contains(log, "ip\n") {
		t.Error("never tried the mount-target IP first")
	}
	if !strings.Contains(log, "dns\n") {
		t.Errorf("did not fall back to the DNS name after the IP failed:\n%s", log)
	}
}

// TestEFSUsesDNSAloneWhenNoIPIsKnown: without a resolved mount-target IP — chiefly
// a caller lacking elasticfilesystem:DescribeMountTargets — the script must fall
// back to the DNS name WITH its retries, not emit a half-built mount.
func TestEFSUsesDNSAloneWhenNoIPIsKnown(t *testing.T) {
	// Assert on the MOUNT COMMANDS, not on prose: with an IP the script emits two
	// `mount -t nfs4` invocations (IP then DNS), without one it emits exactly one.
	// Counting them is immune to comment wording, which an earlier version of this
	// test was not.
	script := efsScript(t) // no EFSMountTargetIP
	if n := strings.Count(script, "mount -t nfs4"); n != 1 {
		t.Errorf("rendered %d `mount -t nfs4` commands with no IP configured, want exactly "+
			"1 (the DNS attempt); a half-built IP mount would fail at boot", n)
	}
	if n := strings.Count(efsScriptWithIP(t), "mount -t nfs4"); n != 2 {
		t.Errorf("rendered %d `mount -t nfs4` commands WITH an IP, want 2 (IP then DNS)", n)
	}
	if !strings.Contains(script, "attempt %s/6 failed") {
		t.Error("the DNS retry disappeared when no IP was configured; a caller without " +
			"DescribeMountTargets would get a single attempt")
	}
}

// TestEFSFstabNeverCarriesTheIP: a mount-target replacement changes the IP, so
// an IP in fstab would silently break on the next reboot. The DNS name is the
// durable reference; the IP is strictly a boot-time workaround.
func TestEFSFstabNeverCarriesTheIP(t *testing.T) {
	script := efsScriptWithIP(t)
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "/etc/fstab") && strings.Contains(line, "172.31.1.24") {
			t.Errorf("fstab line carries the mount-target IP, which does not survive a "+
				"mount-target replacement: %s", line)
		}
	}
	if !strings.Contains(script, "fs-0cc326d51039a4df6.efs.us-east-1.amazonaws.com:/ /efs nfs4") {
		t.Error("fstab no longer carries the EFS DNS name")
	}
}
