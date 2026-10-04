package mpicohort

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runProbe executes the generated probe script the way SSM does: a NON-LOGIN
// shell with a minimal PATH and no inherited environment.
//
// extraBin, if non-empty, is prepended to PATH to stand in for an install
// directory. Returns the exit code and stderr.
func runProbe(t *testing.T, script, extraBin string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	// The PATH an SSM shell actually has, measured on a real AL2023 instance.
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"
	if extraBin != "" {
		path = extraBin + ":" + path
	}

	// Test-only, and executing the script IS the point: the probe comes from
	// enrollProbeScript, a fixed template with no caller input, and the gap this
	// closes (#693) is that inspecting its TEXT cannot tell whether it can
	// succeed on a real install layout.
	//
	// Two things about this directive, both of which cost a CI round-trip:
	// it must be the line IMMEDIATELY above the finding (semgrep honours it
	// there or on the offending line, nowhere else), and the rule ID must be
	// the FULL id — which ends in a doubled segment,
	// ...dangerous-exec-command.dangerous-exec-command. Dropping the last
	// segment is silently ignored, not an error.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + path}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running probe: %v", err)
	}
	return code, stderr.String()
}

// fakeTool writes an executable stub named tool into a fresh temp dir and
// returns the dir.
func fakeTool(t *testing.T, tool string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, tool)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return dir
}

// TestProbeSucceedsWhenToolsArePresent is the test spawn#693 asked for, and the
// gap it correctly identified in #684's tests.
//
// Those asserted the probe CONTAINS a check and sources a profile. They could
// not tell whether the check can ever SUCCEED on a real install layout — and it
// could not, for EFA: aws-efa-installer puts fi_info in /opt/amazon/efa/bin and
// its PATH line in /etc/profile.d/zippy_efa.sh, while the efa.sh spawn writes
// exports only FI_PROVIDER and FI_EFA_USE_DEVICE_RDMA. So the probe reported
// "efa provider missing" every 5 seconds until the 5-minute budget expired,
// whether EFA was installed or not.
//
// This executes the script instead, under the PATH an SSM shell really gets.
func TestProbeSucceedsWhenToolsArePresent(t *testing.T) {
	tests := []struct {
		name string
		enr  Enroller
		tool string // the stub the probe must find
	}{
		{"mpi only", Enroller{}, "mpirun"},
		{"efa only", Enroller{EFAEnabled: true, SkipMPIInstall: true}, "fi_info"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := tt.enr.enrollProbeScript()
			bin := fakeTool(t, tt.tool)

			code, stderr := runProbe(t, script, bin)
			if code != 0 {
				t.Errorf("probe exited %d with %s present on PATH; stderr: %s\n\nscript:\n%s\n\n"+
					"A probe that cannot succeed makes every cohort die at phase=enrolled "+
					"after the full budget (#693).", code, tt.tool, stderr, script)
			}
		})
	}
}

// TestProbeFailsWhenToolsAreAbsent is the other half: the probe must still
// FAIL when the tool really is missing, or it would wave through a node that
// cannot run the job. Without this, "always exit 0" would pass the test above.
func TestProbeFailsWhenToolsAreAbsent(t *testing.T) {
	tests := []struct {
		name    string
		enr     Enroller
		wantMsg string
	}{
		{"mpi missing", Enroller{}, "mpirun missing"},
		{"efa missing", Enroller{EFAEnabled: true, SkipMPIInstall: true}, "efa provider missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stderr := runProbe(t, tt.enr.enrollProbeScript(), "")
			if code == 0 {
				t.Errorf("probe exited 0 with the tool absent — it would enroll a node that "+
					"cannot run the job. stderr: %s", stderr)
			}
			if !strings.Contains(stderr, tt.wantMsg) {
				t.Errorf("stderr = %q, want it to name the missing tool (%q)", stderr, tt.wantMsg)
			}
		})
	}
}

// TestProbeAddsRealInstallDirsToPATH covers what an executable test cannot: the
// absolute directories, which a test cannot create. Executing the script proves
// the PATH mechanism works; this proves it points at the right places.
func TestProbeAddsRealInstallDirsToPATH(t *testing.T) {
	script := Enroller{EFAEnabled: true}.enrollProbeScript()
	for _, dir := range []string{
		"/usr/lib64/openmpi/bin", // AL2023 openmpi package
		"/opt/amazon/efa/bin",    // aws-efa-installer
	} {
		if !strings.Contains(script, dir) {
			t.Errorf("probe does not add %s to PATH. Neither toolchain installs onto the "+
				"default SSM PATH, so omitting it makes the check unsatisfiable (#684/#693).", dir)
		}
	}

	// The installer's own profile script is zippy_efa.sh, not efa.sh — a glob
	// covers it without hard-coding a third party's filename.
	if !strings.Contains(script, "*efa*.sh") {
		t.Error("the profile glob must match the installer's zippy_efa.sh, not just the " +
			"efa.sh spawn writes itself (which sets no PATH at all)")
	}

	// PATH must be APPENDED, never replaced: the probe still needs the system
	// PATH for bash, grep and friends.
	if strings.Contains(script, `PATH="/usr`) || strings.Contains(script, "PATH=/usr/lib64") {
		t.Error("PATH appears to be replaced rather than appended; the probe would lose " +
			"the system directories it needs")
	}
}

// TestProbeIsTriviallyReadyWithNothingToCheck: a custom AMI with no EFA has
// nothing to probe, and must not be made to fail by PATH plumbing.
func TestProbeIsTriviallyReadyWithNothingToCheck(t *testing.T) {
	script := Enroller{SkipMPIInstall: true}.enrollProbeScript()
	if script != "exit 0" {
		t.Errorf("expected a trivial probe, got %q", script)
	}
	if code, _ := runProbe(t, script, ""); code != 0 {
		t.Errorf("trivial probe exited %d", code)
	}
}
