package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1180: a container task whose host cannot provide Docker must not be
// reported as the application's failure. On a t4g.nano `dnf install docker`
// is OOM-killed, the run line then fails with "docker: command not found", and
// the record used to say app_error / retryable=false. Each case runs the whole
// generated wrapper under bash with stand-in tools and reads the record it
// wrote.
//
// PATH is ONLY the stand-in directory (real tools are symlinked in by name), so
// a docker binary installed on the CI host can never satisfy the wrapper's
// `command -v docker` and skip the install step under test.
func TestGenerateWrapper_ContainerRuntimeFailureIsNotAppError(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	cases := []struct {
		name      string
		dnf       string // "" = no dnf stand-in
		docker    string // "" = docker absent until/unless dnf "installs" it
		systemctl string
	}{
		{
			// The E1 failure: Docker absent, dnf killed by the OOM killer.
			name:      "dnf install killed",
			dnf:       "#!/bin/bash\necho Killed >&2\nexit 137\n",
			systemctl: "#!/bin/bash\nexit 0\n",
		},
		{
			name:      "daemon never answers",
			docker:    "#!/bin/bash\necho \"$*\" >> \"$DOCKER_LOG\"\ncase \"$1\" in info) exit 1 ;; *) exit 0 ;; esac\n",
			systemctl: "#!/bin/bash\nexit 0\n",
		},
		{
			name:      "service fails to start",
			docker:    "#!/bin/bash\necho \"$*\" >> \"$DOCKER_LOG\"\nexit 0\n",
			systemctl: "#!/bin/bash\nexit 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &TaskSpec{
				TaskID:    "runtime-fail",
				Container: "quay.io/biocontainers/bwa:0.7.18",
				Command:   []string{"true"},
				Lifecycle: Lifecycle{TTL: "1h"},
			}
			w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)

			binDir := runtimeStandInBin(t)
			writeFakeExe(t, filepath.Join(binDir, "systemctl"), tc.systemctl)
			if tc.dnf != "" {
				writeFakeExe(t, filepath.Join(binDir, "dnf"), tc.dnf)
			}
			if tc.docker != "" {
				writeFakeExe(t, filepath.Join(binDir, "docker"), tc.docker)
			}
			writeFakeSudo(t, binDir)
			dockerLog := filepath.Join(t.TempDir(), "docker.log")

			scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
			if err := os.WriteFile(scriptPath, []byte(w), 0o600); err != nil {
				t.Fatal(err)
			}
			completionPath := CompletionRecordPath()
			_ = os.Remove(completionPath)
			t.Cleanup(func() { _ = os.Remove(completionPath) })

			cmd := exec.Command(bash, scriptPath)
			cmd.Env = []string{"PATH=" + binDir, "HOME=" + t.TempDir(), "DOCKER_LOG=" + dockerLog}
			out, _ := cmd.CombinedOutput()

			data, err := os.ReadFile(completionPath)
			if err != nil {
				t.Fatalf("no completion record: %v\noutput:\n%s", err, out)
			}
			rec, err := ParseCompletionRecord(data)
			if err != nil {
				t.Fatalf("parse record: %v\nraw: %s", err, data)
			}
			if rec.State != StateFailed {
				t.Errorf("state = %q, want %q\nrecord: %s", rec.State, StateFailed, data)
			}
			if rec.RetryClass == RetryAppError || rec.RetryClass == RetryNone {
				t.Errorf("retry_class = %q: a Docker install/daemon failure is the host's, not the application's\nrecord: %s\noutput:\n%s", rec.RetryClass, data, out)
			}
			if !rec.RetryClass.Retryable() {
				t.Errorf("retry_class %q is not retryable; a fresh instance usually succeeds", rec.RetryClass)
			}
			if rec.ExitCode == 0 {
				t.Errorf("exit_code = 0 for a task whose command never ran\nrecord: %s", data)
			}
			if log, _ := os.ReadFile(dockerLog); strings.Contains(string(log), "run ") || strings.Contains(string(log), "pull ") {
				t.Errorf("the image was pulled or run on a host whose runtime failed:\n%s", log)
			}
		})
	}
}

// Control: with a working runtime, a failing command is still app_error, so
// the gate above is about the runtime step and not a blanket reclassification.
func TestGenerateWrapper_ContainerCommandFailureStaysAppError(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	spec := &TaskSpec{
		TaskID:    "app-fail",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"true"},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
	binDir := runtimeStandInBin(t)
	writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\nexit 0\n")
	writeFakeExe(t, filepath.Join(binDir, "docker"), "#!/bin/bash\ncase \"$1\" in run) exit 3 ;; *) exit 0 ;; esac\n")
	writeFakeSudo(t, binDir)
	scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(w), 0o600); err != nil {
		t.Fatal(err)
	}
	completionPath := CompletionRecordPath()
	_ = os.Remove(completionPath)
	t.Cleanup(func() { _ = os.Remove(completionPath) })
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = []string{"PATH=" + binDir, "HOME=" + t.TempDir()}
	out, _ := cmd.CombinedOutput()
	data, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatalf("no completion record: %v\noutput:\n%s", err, out)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RetryClass != RetryAppError || rec.ExitCode != 3 {
		t.Errorf("retry_class=%q exit_code=%d, want app_error/3\nrecord: %s\noutput:\n%s", rec.RetryClass, rec.ExitCode, data, out)
	}
}

// runtimeStandInBin builds the stand-in PATH directory: the real coreutils the
// wrapper needs, symlinked by name, plus a no-op sleep (so the 30-try daemon
// wait costs nothing) and a succeeding aws. Nothing else from the host PATH is
// reachable, in particular not a real docker or dnf.
func runtimeStandInBin(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	for _, tool := range []string{"seq", "mkdir", "cat", "id", "rm", "chown", "tee", "dirname", "basename", "env"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(binDir, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	installStandInDate(t, binDir)
	writeFakeExe(t, filepath.Join(binDir, "sleep"), "#!/bin/bash\nexit 0\n")
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\nexit 0\n")
	return binDir
}

// installStandInDate puts a `date` that understands the phase markers' %3N
// (PhaseTimestampFormat, #1072) into binDir. A GNU date, AL2023's, is used as
// is, which is the case on Linux CI. BSD date (macOS) prints "3N" literally, so
// there the stand-in is GNU's gdate when installed, else a shim that fills the
// milliseconds with 000 and hands the rest to the real date.
func installStandInDate(t *testing.T, binDir string) {
	t.Helper()
	real, err := exec.LookPath("date")
	if err != nil {
		t.Skip("date not available")
	}
	link := func(p string) {
		if err := os.Symlink(p, filepath.Join(binDir, "date")); err != nil {
			t.Fatal(err)
		}
	}
	if gnuDate(real) {
		link(real)
		return
	}
	if g, err := exec.LookPath("gdate"); err == nil && gnuDate(g) {
		link(g)
		return
	}
	writeFakeExe(t, filepath.Join(binDir, "date"),
		"#!/bin/bash\nargs=()\nfor a in \"$@\"; do args+=(\"${a//\\%3N/000}\"); done\nexec "+shQuote(real)+" \"${args[@]}\"\n")
}

// gnuDate reports whether the date at path expands %3N to three digits.
func gnuDate(path string) bool {
	// nosemgrep
	out, err := exec.Command(path, "-u", "+%3N").Output()
	s := strings.TrimSpace(string(out))
	return err == nil && len(s) == 3 && strings.Trim(s, "0123456789") == ""
}
