package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// s3CopyInvocation matches an `aws s3 cp|sync` INVOCATION in generated shell:
// the command word at the start of a line or after if/then/!/;/&&/||, with an
// optional quoted global argument (the flush script's "$AWS_REGION_ARG") and
// --cli-*-timeout options, and behind the flush script's spawn_up retry helper
// (#1186). It
// does not match the words inside a message, such as the provenance fallback's
// 'uploading untagged with aws s3 cp'.
var s3CopyInvocation = regexp.MustCompile(`(?:^|\bif\s+(?:!\s+)?|\bthen\s+|;\s*|&&\s*|\|\|\s*)(?:spawn_up\s+)?aws(?:\s+"[^"]*"|\s+--cli-[a-z-]+\s+\d+)*\s+s3\s+(?:cp|sync)\b(.*)$`)

// #1072: every S3 copy in the generated scripts runs without the progress
// meter, which filled command.log with ~20 \r-separated lines per file. The
// invocations are discovered from the scripts, not listed, and the discovery
// must find the stage-in and stage-out copies or it is broken.
func TestWrapperS3CopiesAreQuiet(t *testing.T) {
	host := fullSpec()
	host.Inputs = append(host.Inputs, Manifest{Source: "s3://in/refs/", Destination: "/data/refs/"})
	host.Outputs = append(host.Outputs, Manifest{Source: "/work/out/", Destination: "s3://out/run/"})
	container := *host
	container.Container = "quay.io/biocontainers/bwa:0.7.18"
	flush, err := GenerateFlushScript(host, legacyOpts("b", host.TaskID, "us-east-1", testRunID, false))
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"host wrapper":      genWrapper(host, "b", "us-east-1", false, testRunID),
		"container wrapper": genWrapper(&container, "b", "us-east-1", false, testRunID),
		"pooled job script": genPooled(host, "b", "us-east-1", false, testRunID),
		"flush script":      flush,
	}
	for name, script := range scripts {
		var found, stageIn, stageOut int
		for _, line := range strings.Split(script, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			m := s3CopyInvocation.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			found++
			for _, in := range host.Inputs {
				if strings.Contains(line, shQuote(in.Source)) {
					stageIn++
				}
			}
			for _, out := range host.Outputs {
				if strings.Contains(line, shQuote(out.Destination)) {
					stageOut++
				}
			}
			args := strings.Fields(m[1])
			if !contains(args, "--no-progress") && !contains(args, "--only-show-errors") {
				t.Errorf("%s: S3 copy without --no-progress: %s", name, line)
			}
		}
		if found == 0 {
			t.Errorf("%s: found no aws s3 cp/sync invocation; the discovery is broken", name)
		}
		if name != "flush script" && (stageIn < len(host.Inputs) || stageOut < len(host.Outputs)) {
			t.Errorf("%s: discovered %d stage-in and %d stage-out copies, want at least %d and %d",
				name, stageIn, stageOut, len(host.Inputs), len(host.Outputs))
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// #1072: phase markers carry milliseconds. Executed under bash, so the format
// is what `date` actually printed, not what the generator meant to emit; on
// Linux CI the stand-in date is GNU date itself.
func TestWrapperPhaseTimestampsHaveMilliseconds(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if PhaseTimestampFormat != "+%Y-%m-%dT%H:%M:%S.%3NZ" {
		t.Errorf("PhaseTimestampFormat = %q, want the millisecond UTC form", PhaseTimestampFormat)
	}
	spec := &TaskSpec{TaskID: "phase-ms", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"}}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
	if !strings.Contains(w, "spawn_phase() { echo \"spawn: [$(date -u "+PhaseTimestampFormat+")] $*\"; }") {
		t.Fatalf("spawn_phase does not stamp with PhaseTimestampFormat:\n%s", w)
	}
	out, _ := runWrapper(t, bash, w, runtimeStandInBin(t), nil)
	stamp := regexp.MustCompile(`^spawn: \[([^\]]*)\] `)
	n := 0
	for _, line := range strings.Split(out, "\n") {
		m := stamp.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n++
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", m[1]); err != nil {
			t.Errorf("phase timestamp %q is not UTC with milliseconds: %v", m[1], err)
		}
	}
	if n < 4 {
		t.Errorf("found %d phase markers in the output, want at least 4 (wrapper start, stage-in, command):\n%s", n, out)
	}
}

// #1072: a container task's Docker install starts before stage-in and runs
// alongside it, and nothing touches Docker until the install has finished. A
// failed install is still instance_health (#1180), and a failed stage-in still
// waits for the install rather than leaving it running.
//
// Order is read from one event log the stand-ins append to. The stand-in dnf
// does not finish until it has seen stage-in happen, so an install that is
// serial with staging (in either order) cannot produce "dnf-begin, stage-in,
// dnf-end".
func TestWrapperDockerInstallOverlapsStageIn(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	for _, tc := range []struct {
		name      string
		dnfRC     string
		stageFail bool
		wantClass RetryClass
		wantRun   bool
	}{
		{"install succeeds", "0", false, RetryNone, true},
		{"install killed", "137", false, RetryInstanceHealth, false},
		{"stage-in fails", "0", true, RetryStagingError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "in", "ref.fa")
			spec := &TaskSpec{
				TaskID:    "overlap",
				Container: "quay.io/biocontainers/bwa:0.7.18",
				Command:   []string{"true"},
				Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: dest}},
				Lifecycle: Lifecycle{TTL: "1h"},
			}
			w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
			binDir := runtimeStandInBin(t)
			events := filepath.Join(t.TempDir(), "events.log")
			stageRC := "0"
			if tc.stageFail {
				stageRC = "1"
			}
			// The stage-in copy waits (bounded) for the install to have begun, and
			// dnf waits (bounded) for stage-in to have happened. Running in
			// parallel, each sees the other at once. Run serially, in either
			// order, the first one waits out its bound and the log shows the
			// serial order.
			//
			// dnf then holds for up to a second, finishing early only if the
			// wrapper touches Docker (systemctl, docker) while it is still
			// running. A correct wrapper never does, so the hold expires; one that
			// does not wait has logged the call before dnf-end. A wrapper that
			// does not wait on the stage-in failure path exits first and the log
			// has no dnf-end.
			writeFakeExe(t, filepath.Join(binDir, "aws"), `#!/bin/bash
case "$*" in *s3://in/ref.fa*)
  for _ in $(seq 1 150); do grep -q dnf-begin "$EVENTS" 2>/dev/null && break; "$REAL_SLEEP" 0.02; done
  echo stage-in >> "$EVENTS"; exit "$FAKE_STAGE_RC" ;;
esac
exit 0
`)
			writeFakeExe(t, filepath.Join(binDir, "dnf"), `#!/bin/bash
echo dnf-begin >> "$EVENTS"
for _ in $(seq 1 150); do grep -q stage-in "$EVENTS" 2>/dev/null && break; "$REAL_SLEEP" 0.02; done
for _ in $(seq 1 50); do grep -qE '^(systemctl|docker )' "$EVENTS" 2>/dev/null && break; "$REAL_SLEEP" 0.02; done
if [ "$DNF_RC" = 0 ]; then
  printf '#!/bin/bash\necho "docker $1" >> "$EVENTS"\nexit 0\n' > "$BIN_DIR/docker"; "$REAL_CHMOD" +x "$BIN_DIR/docker"
fi
echo dnf-end >> "$EVENTS"
exit "$DNF_RC"
`)
			writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\necho systemctl >> \"$EVENTS\"\nexit 0\n")
			writeFakeExe(t, filepath.Join(binDir, "grep"), "#!/bin/bash\nexec "+shQuote(mustLookPath(t, "grep"))+" \"$@\"\n")
			writeFakeSudo(t, binDir)

			out, rec := runWrapper(t, bash, w, binDir, []string{
				"EVENTS=" + events, "FAKE_STAGE_RC=" + stageRC, "REAL_CHMOD=" + mustLookPath(t, "chmod"), "DNF_RC=" + tc.dnfRC,
				"REAL_SLEEP=" + realSleep, "BIN_DIR=" + binDir, "TMPDIR=" + t.TempDir(),
			})
			data, _ := os.ReadFile(events)
			ev := strings.Split(strings.TrimSpace(string(data)), "\n")
			at := func(e string) int {
				for i, x := range ev {
					if x == e || strings.HasPrefix(x, e+" ") {
						return i
					}
				}
				return -1
			}
			begin, staged, end := at("dnf-begin"), at("stage-in"), at("dnf-end")
			if begin < 0 || staged < 0 || end < 0 {
				t.Fatalf("missing events (the wrapper must wait for the install on every path): %q\noutput:\n%s", ev, out)
			}
			if !(begin < staged && staged < end) {
				t.Errorf("install did not overlap stage-in: events %q", ev)
			}
			for i, e := range ev {
				if (e == "systemctl" || strings.HasPrefix(e, "docker ")) && i < end {
					t.Errorf("%q ran before the Docker install finished: events %q", e, ev)
				}
			}
			if ran := at("docker run") >= 0; ran != tc.wantRun {
				t.Errorf("docker run happened=%v, want %v: events %q", ran, tc.wantRun, ev)
			}
			if rec.RetryClass != tc.wantClass {
				t.Errorf("retry_class = %q, want %q\noutput:\n%s", rec.RetryClass, tc.wantClass, out)
			}
			// A failed install skips everything after it (#1180); a later step
			// failing too is not what classified it.
			if tc.dnfRC != "0" && at("systemctl") >= 0 {
				t.Errorf("the daemon was started after the install failed: events %q", ev)
			}
			if tc.dnfRC != "0" && !strings.Contains(out, "container runtime unavailable") {
				t.Errorf("a failed install is not reported in the log:\n%s", out)
			}
		})
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available", name)
	}
	return p
}

// runWrapper runs a generated wrapper under bash with PATH limited to binDir
// and returns its output and the completion record it wrote.
func runWrapper(t *testing.T, bash, script, binDir string, env []string) (string, *CompletionRecord) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	completionPath := CompletionRecordPath()
	_ = os.Remove(completionPath)
	t.Cleanup(func() { _ = os.Remove(completionPath) })
	// nosemgrep
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append([]string{"PATH=" + binDir, "HOME=" + t.TempDir()}, env...)
	out, _ := cmd.CombinedOutput()
	data, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatalf("no completion record: %v\noutput:\n%s", err, out)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("parse record: %v\nraw: %s", err, data)
	}
	return string(out), rec
}
