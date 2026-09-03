package taskproto

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// fullSpec exercises every wrapper section: env, inputs (file + prefix), a
// command with shell-metachar args, and outputs.
func fullSpec() *TaskSpec {
	return &TaskSpec{
		TaskID:  "align-42",
		Command: []string{"bash", "-c", "echo $HOME; rm -rf /tmp/x && true"},
		Env:     map[string]string{"THREADS": "16", "REF": "/data/ref.fa"},
		Inputs: []Manifest{
			{Source: "s3://in-bucket/ref.fa", Destination: "/data/ref.fa"},
			{Source: "s3://in-bucket/reads/", Destination: "/work/reads/"},
		},
		Outputs: []Manifest{
			{Source: "/work/out.bam", Destination: "s3://out-bucket/out.bam"},
		},
		Lifecycle: Lifecycle{TTL: "4h", OnComplete: "terminate"},
	}
}

func TestGenerateWrapper_Structure(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "spawn-results-123-us-east-1", "us-east-1")

	mustContain := []string{
		"#!/bin/bash",
		"set +e -u -o pipefail", // safe flags present, +e explicit (spawn#566)
		"RESULTS_PREFIX='s3://spawn-results-123-us-east-1/tasks/align-42'",
		"rc=$?", // exit-code capture
		"aws s3 cp /tmp/spawn-completion.json \"$RESULTS_PREFIX/completion.json\"",
		"aws s3 cp /tmp/spawn.exitcode \"$RESULTS_PREFIX/.exitcode\"",
		"> /tmp/SPAWN_COMPLETE", // spored signal
		"exit $rc",
		"--recursive",         // prefix input gets --recursive
		"export THREADS='16'", // env exported + quoted
	}
	for _, sub := range mustContain {
		if !strings.Contains(w, sub) {
			t.Errorf("wrapper missing %q\n---\n%s", sub, w)
		}
	}

	// set -e must NOT be present (it would abort before stage-out/record).
	for _, line := range strings.Split(w, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -e" || strings.HasPrefix(trimmed, "set -e ") || trimmed == "set -eu" {
			t.Errorf("wrapper must not use 'set -e' (aborts before completion record): %q", line)
		}
	}
}

// TestGeneratePooledJobScript_OmitsSelfTerminate: the pooled per-job script must
// be identical to the wrapper EXCEPT for the SPAWN_COMPLETE signal — it still
// writes the durable completion record (so the submitter's poll is unchanged) but
// must NOT write /tmp/SPAWN_COMPLETE (which would self-terminate the worker after
// its first task, defeating pool reuse — #70).
func TestGeneratePooledJobScript_OmitsSelfTerminate(t *testing.T) {
	spec := fullSpec()
	pooled := GeneratePooledJobScript(spec, "spawn-results-123-us-east-1", "us-east-1")

	if strings.Contains(pooled, "/tmp/SPAWN_COMPLETE") {
		t.Errorf("pooled job script must NOT write /tmp/SPAWN_COMPLETE (would self-terminate the worker)\n---\n%s", pooled)
	}
	// The durable record is still written — the submitter polls completion.json.
	for _, sub := range []string{
		"aws s3 cp /tmp/spawn-completion.json \"$RESULTS_PREFIX/completion.json\"",
		"aws s3 cp /tmp/spawn.exitcode \"$RESULTS_PREFIX/.exitcode\"",
		"exit $rc",
	} {
		if !strings.Contains(pooled, sub) {
			t.Errorf("pooled job script missing %q\n---\n%s", sub, pooled)
		}
	}

	// It differs from the wrapper ONLY by the SPAWN_COMPLETE block: strip that
	// block from the wrapper and the two must be byte-identical.
	wrapper := GenerateWrapper(spec, "spawn-results-123-us-east-1", "us-east-1")
	marker := "# ---- signal spored"
	idx := strings.Index(wrapper, marker)
	if idx < 0 {
		t.Fatalf("wrapper missing the SPAWN_COMPLETE block marker %q", marker)
	}
	// The block is the marker line + the heredoc through the blank line before
	// "exit $rc". Reconstruct: wrapper[:idx] + "exit $rc\n".
	wrapperSansSignal := wrapper[:idx] + "exit $rc\n"
	if pooled != wrapperSansSignal {
		t.Errorf("pooled script differs from wrapper beyond the SPAWN_COMPLETE block\n--- pooled ---\n%s\n--- wrapper-sans-signal ---\n%s", pooled, wrapperSansSignal)
	}
}

func TestGenerateWrapper_QuotesMetacharArgs(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "t",
		Command:   []string{"echo", "a; rm -rf /", "$(whoami)", "it's"},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	// Each arg must appear single-quoted; the single-quote in "it's" is escaped.
	for _, want := range []string{`'echo'`, `'a; rm -rf /'`, `'$(whoami)'`, `'it'\''s'`} {
		if !strings.Contains(w, want) {
			t.Errorf("expected quoted arg %q in wrapper\n---\n%s", want, w)
		}
	}
	// The dangerous forms must NOT appear unquoted as their own tokens.
	if strings.Contains(w, "( echo a; rm -rf / ") {
		t.Error("metachar arg leaked unquoted into the command subshell")
	}
}

func TestGenerateWrapper_StageOutAfterCommand(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	runIdx := strings.Index(w, "rc=$?")
	outIdx := strings.Index(w, "s3://out-bucket/out.bam")
	recIdx := strings.Index(w, "completion.json")
	if runIdx < 0 || outIdx < 0 || recIdx < 0 {
		t.Fatalf("wrapper missing expected sections")
	}
	if !(runIdx < outIdx && outIdx < recIdx) {
		t.Errorf("ordering wrong: want command(%d) < stage-out(%d) < record(%d)", runIdx, outIdx, recIdx)
	}
}

func TestGenerateWrapper_NoInputsNoOutputs(t *testing.T) {
	spec := &TaskSpec{TaskID: "t", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"}}
	w := GenerateWrapper(spec, "b", "us-east-1")
	// Still writes the completion record + signals spored even with no staging.
	for _, sub := range []string{"completion.json", "/tmp/SPAWN_COMPLETE", "exit $rc"} {
		if !strings.Contains(w, sub) {
			t.Errorf("minimal wrapper missing %q", sub)
		}
	}
}

func TestGenerateWrapper_HostHasNoDocker(t *testing.T) {
	// A container-less task must never emit docker/ECR machinery.
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	for _, bad := range []string{"docker run", "docker pull", "dnf install -y docker", "ecr get-login-password"} {
		if strings.Contains(w, bad) {
			t.Errorf("host task wrapper unexpectedly contains %q", bad)
		}
	}
	if !strings.Contains(w, "( 'bash'") {
		t.Errorf("host task should run argv in a subshell")
	}
}

func TestGenerateWrapper_PublicContainer(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "align",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/data/ref.fa"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out/out.bam"}},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	mustContain := []string{
		"command -v docker",                  // install guard
		"sudo dnf install -y docker",         // install (root)
		"sudo systemctl enable --now docker", // start daemon (root)
		"sudo docker info",                   // bounded wait for the socket
		"sudo docker pull 'quay.io/biocontainers/bwa:0.7.18'",
		"sudo docker run --rm ",                                         // run
		"'quay.io/biocontainers/bwa:0.7.18' 'bwa' 'mem' '/data/ref.fa'", // image + argv
		"-v '/data':'/data'",                                            // input dir mount
		"-v '/work':'/work'",                                            // output dir mount
		"aws s3 cp 's3://in/ref.fa' '/data/ref.fa'",                     // stage-in still on host
		"completion.json",                                               // record unchanged
	}
	for _, sub := range mustContain {
		if !strings.Contains(w, sub) {
			t.Errorf("public-container wrapper missing %q\n---\n%s", sub, w)
		}
	}
	// Public image: NO ECR login, NO --gpus.
	if strings.Contains(w, "ecr get-login-password") {
		t.Error("public image should not emit an ECR login")
	}
	if strings.Contains(w, "--gpus") {
		t.Error("no GPU requested; --gpus must not appear")
	}
}

// TestGenerateWrapper_ContainerRunsAsInvokingUser guards against #555: the
// container must run as the same uid:gid that staged the bind-mounted dirs
// (the instance user), not whatever USER the image declares. Without this,
// any bioconda/conda-forge image — the entire mambaorg/micromamba-based
// ecosystem, which defaults to uid 57439 (mambauser), not root and not the
// instance's uid 1000 — gets EACCES writing into its own staged inputs/outputs.
func TestGenerateWrapper_ContainerRunsAsInvokingUser(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "align",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/data/ref.fa"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out/out.bam"}},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	if !strings.Contains(w, `sudo docker run --rm --user "$(id -u):$(id -g)" `) {
		t.Errorf("container run must pass --user \"$(id -u):$(id -g)\" so the container can write into the host-uid-owned staged dirs (#555)\n---\n%s", w)
	}
}

// TestGenerateWrapper_ContainerRunLineExecutesWithRealUID is a stronger,
// exec-based sibling of TestGenerateWrapper_ContainerRunsAsInvokingUser (#555):
// rather than asserting on the generated *text*, it extracts the actual
// `docker run` line the wrapper emits and runs it for real under bash, with
// stand-in `sudo` and `docker` shell scripts on PATH (`sudo` just execs its
// argv; `docker` echoes its argv back) — the same "prove real shell behavior,
// don't just check the Go string" pattern as
// pkg/launcher/bootstrap_test.go's TestBuildLinuxBootstrap_ParamValueSurvivesRealShellParsing.
// This catches a regression where the `--user` value is well-formed text but
// fails to actually substitute to the real invoking uid:gid under bash's
// command substitution (e.g. a quoting mistake that leaves `$(id -u)` inside
// single quotes, where it wouldn't expand at all).
func TestGenerateWrapper_ContainerRunLineExecutesWithRealUID(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	spec := &TaskSpec{
		TaskID:    "align",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/data/ref.fa"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out/out.bam"}},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	var dockerRunLine string
	for _, line := range strings.Split(w, "\n") {
		if strings.Contains(line, "docker run") {
			dockerRunLine = strings.TrimSpace(line)
			break
		}
	}
	if dockerRunLine == "" {
		t.Fatalf("no docker run line found in generated wrapper\n---\n%s", w)
	}

	// Stand-in PATH: `sudo` drops itself and execs its argv (so `sudo docker
	// ...` really invokes our stand-in docker); `docker` just echoes argv so we
	// can inspect exactly what the real shell substituted.
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "sudo"), "#!/bin/bash\nexec \"$@\"\n")
	writeFakeExe(t, filepath.Join(binDir, "docker"), "#!/bin/bash\necho \"$@\"\n")

	cmd := exec.Command("bash", "-c", dockerRunLine) //nolint:gosec // nosemgrep
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec of extracted docker run line failed: %v\noutput: %s\nline: %s", err, out, dockerRunLine)
	}

	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	wantUser := fmt.Sprintf("--user %s:%s", u.Uid, u.Gid)
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, wantUser) {
		t.Errorf("docker run line did not substitute the real invoking uid:gid under bash: got %q, want it to contain %q", got, wantUser)
	}
}

// writeFakeExe writes an executable shell script to path, failing the test on
// any error.
func writeFakeExe(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("writeFakeExe(%s): %v", path, err)
	}
}

func TestGenerateWrapper_PrivateECRContainerWithGPU(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "infer",
		Container: "123456789012.dkr.ecr.us-west-2.amazonaws.com/model:v3",
		Command:   []string{"python", "infer.py"},
		Resources: ResourceRequest{GPUs: 1},
		Lifecycle: Lifecycle{TTL: "2h"},
	}
	w := GenerateWrapper(spec, "b", "us-west-2")

	if !strings.Contains(w, "aws ecr get-login-password --region 'us-west-2' | sudo docker login --username AWS --password-stdin '123456789012.dkr.ecr.us-west-2.amazonaws.com'") {
		t.Errorf("private ECR image must emit a docker login to its registry host\n---\n%s", w)
	}
	if !strings.Contains(w, `sudo docker run --rm --user "$(id -u):$(id -g)" --gpus all `) {
		t.Errorf("GPU task must pass --gpus all\n---\n%s", w)
	}
}

func TestContainerMountDirs(t *testing.T) {
	spec := &TaskSpec{
		Inputs: []Manifest{
			{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"},
			{Source: "s3://in/x", Destination: "/data/sub/x"}, // /data/sub distinct from /data
		},
		Outputs: []Manifest{
			{Source: "/work/out.bam", Destination: "s3://out/out.bam"},
			{Source: "/data/also.txt", Destination: "s3://out/also.txt"}, // /data again → deduped
		},
	}
	got := containerMountDirs(spec)
	want := []string{"/data", "/data/sub", "/work"} // sorted, deduped
	if len(got) != len(want) {
		t.Fatalf("containerMountDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("containerMountDirs = %v, want %v", got, want)
			break
		}
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"simple": "'simple'",
		"a b":    "'a b'",
		"it's":   `'it'\''s'`,
		"$(x)":   "'$(x)'",
		"`cmd`":  "'`cmd`'",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- spawn#561: a failed output upload must fail the task, not silently ----
// ---- report completed/exit_code=0.                                     ----

// TestGenerateWrapper_OutputDeliveryFailureClassification asserts the generated
// text has a THIRD classification branch that consults OUT_RC — before this fix,
// OUT_RC was assigned in the stage-out loop and never read again anywhere in the
// script (confirmed by grep in the issue report), so a failed `aws s3 cp` for a
// declared output left STATE=completed/RETRY_CLASS=”. The new branch must be
// reachable only once STAGE_RC and rc are BOTH clean, so a command failure keeps
// priority over an output-delivery failure when both occur.
func TestGenerateWrapper_OutputDeliveryFailureClassification(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")

	if !strings.Contains(w, `elif [ "$OUT_RC" -ne 0 ]; then`) {
		t.Errorf("wrapper missing the OUT_RC classification branch (spawn#561)\n---\n%s", w)
	}
	wantRetry := fmt.Sprintf("STATE='failed'; RETRY_CLASS='%s'", RetryOutputDeliveryError)
	if !strings.Contains(w, wantRetry) {
		t.Errorf("wrapper missing %q (new output-delivery retry class)\n---\n%s", wantRetry, w)
	}

	// Ordering: STAGE_RC branch, then rc branch, then OUT_RC branch, then the
	// completed else — each elif must consult the previous cases first so a
	// stage-in or command failure still wins over an output-delivery failure.
	stageIdx := strings.Index(w, `if [ "$STAGE_RC" -ne 0 ]; then`)
	rcIdx := strings.Index(w, `elif [ "$rc" -ne 0 ]; then`)
	outIdx := strings.Index(w, `elif [ "$OUT_RC" -ne 0 ]; then`)
	elseIdx := strings.Index(w, "STATE='completed'; RETRY_CLASS=''")
	if stageIdx < 0 || rcIdx < 0 || outIdx < 0 || elseIdx < 0 {
		t.Fatalf("classification block missing an expected branch\n---\n%s", w)
	}
	if !(stageIdx < rcIdx && rcIdx < outIdx && outIdx < elseIdx) {
		t.Errorf("classification branch order wrong: want STAGE_RC(%d) < rc(%d) < OUT_RC(%d) < completed(%d)",
			stageIdx, rcIdx, outIdx, elseIdx)
	}
}

// TestGenerateWrapper_StageOutFailureExecClassifiesAsFailed is the exec-based
// sibling: it actually RUNS the generated script under bash with stand-in
// `docker`/`aws`/`sudo` on PATH — the user command succeeds, but the stage-out
// `aws s3 cp` for the declared output fails — and asserts the real completion
// record written to disk says state=failed with the new retry class, not
// completed/exit_code=0. Before the wrapper.go fix (OUT_RC read in the
// classification block), this test fails: the written completion.json says
// state=completed, retry_class="". After the fix, it passes.
func TestGenerateWrapper_StageOutFailureExecClassifiesAsFailed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	spec := &TaskSpec{
		TaskID:  "out-rc-fail",
		Command: []string{"true"}, // the user command succeeds
		Outputs: []Manifest{
			{Source: "/work/good.txt", Destination: "s3://out/good.txt"}, // this cp succeeds
			{Source: "/work/bad.txt", Destination: "s3://out/bad.txt"},   // this cp fails
		},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "results-bucket", "us-east-1")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(w), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write wrapper script: %v", err)
	}

	// Stand-in PATH: `aws s3 cp ... bad.txt` fails (exit 1), every other `aws`
	// invocation (good.txt, command.log, completion.json, .exitcode) succeeds —
	// so the completion record itself is still written to a real path we can
	// inspect (aws is stubbed, not the local /tmp writes).
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "aws"), `#!/bin/bash
for a in "$@"; do
  if [ "$a" = "/work/bad.txt" ]; then
    exit 1
  fi
done
exit 0
`)

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	// The script's own final `exit $rc` is 0 (the user command succeeded) even
	// though the task must be classified failed — exit code and STATE are
	// deliberately different signals here, so don't assert on cmd's exit status.
	_ = err

	completionPath := "/tmp/spawn-completion.json"
	data, readErr := os.ReadFile(completionPath)
	if readErr != nil {
		t.Fatalf("completion record not written at %s: %v\nscript output: %s", completionPath, readErr, out)
	}
	t.Cleanup(func() { _ = os.Remove(completionPath) })

	rec, parseErr := ParseCompletionRecord(data)
	if parseErr != nil {
		t.Fatalf("parse completion record: %v\nraw: %s", parseErr, data)
	}
	if rec.State != StateFailed {
		t.Errorf("state = %q, want %q (a lost output must fail the task)\nrecord: %s", rec.State, StateFailed, data)
	}
	if rec.RetryClass != RetryOutputDeliveryError {
		t.Errorf("retry_class = %q, want %q\nrecord: %s", rec.RetryClass, RetryOutputDeliveryError, data)
	}
	if rec.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (the user command itself succeeded)\nrecord: %s", rec.ExitCode, data)
	}
}

// ---- spawn#566: a failing command must still reach the completion-write ----
// ---- block, even when this script is concatenated after a caller's own ----
// ---- `set -e` preamble rather than run as its own bash process.        ----

// TestGenerateWrapper_ExplicitSetPlusE guards the exact mechanism found for
// spawn#566: pkg/launcher/bootstrap.go's /tmp/spawn-command.sh preamble is a
// literal `#!/bin/bash\nset -e\n...`, and the embedded --command payload (this
// wrapper, for `spawn task run`) is concatenated onto the END of that same file
// and executed as ONE bash invocation — not sourced or exec'd as a separate
// process. A shell option enabled early in a script stays in effect for
// everything after it in that same script, so without an explicit `set +e` of
// its own, this wrapper silently inherited -e from that preamble: a failing
// `docker run` (or the host command's subshell) then aborted the whole script
// right at that line, before `rc=$?` was ever reached, so stage-out and the
// completion-record write never ran. The wrapper's own comment ("we do NOT set
// -e") was true of ITS OWN text but not of the effective shell state once
// embedded — this test pins the fix (`set +e` as the first shell option this
// script sets) so a future edit can't drop it and reintroduce the bug.
func TestGenerateWrapper_ExplicitSetPlusE(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	if !strings.Contains(w, "set +e -u -o pipefail") {
		t.Errorf("wrapper must explicitly `set +e` (spawn#566: it can be concatenated after a caller's own `set -e` preamble in the same bash process, e.g. pkg/launcher/bootstrap.go's /tmp/spawn-command.sh) — relying on a fresh, default (+e) bash process is not safe\n---\n%s", w)
	}
}

// TestGenerateWrapper_SurvivesEmbeddingAfterSetE is the exec-based reproduction
// for spawn#566. It builds the SAME two-file concatenation
// pkg/launcher/bootstrap.go performs — a literal `#!/bin/bash\nset -e\n[ -f
// /etc/profile.d/spawn-params.sh ] && source ...\n` preamble with this
// generated wrapper appended verbatim — and runs the result under real bash
// with stand-in docker/sudo/systemctl/aws on PATH, where the stand-in `docker
// run` exits 3 (simulating a failing containerized user command, matching the
// issue's suggested repro: `command: ["bash","-c","exit 3"]` plus one declared
// output).
//
// Before the fix (no `set +e` in the wrapper's own preamble), this test fails:
// bash aborts at the `docker run` line under inherited -e and none of
// completion.json/.exitcode/SPAWN_COMPLETE are written. After the fix, it
// passes: the script reaches the classification/completion-write block despite
// the command's non-zero exit, and self-terminates via SPAWN_COMPLETE instead
// of riding to TTL — exactly the gap #566 reported ("neither box self-terminated
// on failure — both sat until TTL").
func TestGenerateWrapper_SurvivesEmbeddingAfterSetE(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	spec := &TaskSpec{
		TaskID:    "exit3-under-outer-sete",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bash", "-c", "exit 3"},
		Outputs:   []Manifest{{Source: "/work/out.txt", Destination: "s3://out/out.txt"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "results-bucket", "us-east-1")

	// Reproduce pkg/launcher/bootstrap.go's exact concatenation: its own
	// `set -e` preamble (bootstrap.go ~line 586-590), then this wrapper appended
	// verbatim — as ONE file, run as ONE bash invocation, matching how
	// /tmp/spawn-command.sh is actually built and executed on a real instance.
	const bootstrapPreamble = "#!/bin/bash\nset -e\n[ -f /etc/profile.d/spawn-params.sh ] && source /etc/profile.d/spawn-params.sh\n"
	combined := bootstrapPreamble + w

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "spawn-command.sh")
	if err := os.WriteFile(scriptPath, []byte(combined), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write combined script: %v", err)
	}

	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "sudo"), "#!/bin/bash\nexec \"$@\"\n")
	writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\nexit 0\n")
	writeFakeExe(t, filepath.Join(binDir, "docker"), `#!/bin/bash
case "$1" in
  run) exit 3 ;;   # simulates the user's "exit 3" command failing inside the container
  info) exit 0 ;;
  pull) exit 0 ;;
  *) exit 0 ;;
esac
`)
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\nexit 0\n")

	completionPath := "/tmp/spawn-completion.json"
	exitcodePath := "/tmp/spawn.exitcode"
	completePath := "/tmp/SPAWN_COMPLETE"
	for _, p := range []string{completionPath, exitcodePath, completePath} {
		_ = os.Remove(p)
	}
	t.Cleanup(func() {
		for _, p := range []string{completionPath, exitcodePath, completePath} {
			_ = os.Remove(p)
		}
	})

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin:/usr/sbin:/sbin")
	out, _ := cmd.CombinedOutput()

	data, readErr := os.ReadFile(completionPath)
	if readErr != nil {
		t.Fatalf("completion record not written at %s (spawn#566: script aborted before reaching the completion-write block): %v\nscript output:\n%s", completionPath, readErr, out)
	}
	rec, parseErr := ParseCompletionRecord(data)
	if parseErr != nil {
		t.Fatalf("parse completion record: %v\nraw: %s", parseErr, data)
	}
	if rec.State != StateFailed {
		t.Errorf("state = %q, want %q\nrecord: %s", rec.State, StateFailed, data)
	}
	if rec.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3\nrecord: %s", rec.ExitCode, data)
	}
	if rec.RetryClass != RetryAppError {
		t.Errorf("retry_class = %q, want %q\nrecord: %s", rec.RetryClass, RetryAppError, data)
	}
	if _, err := os.Stat(exitcodePath); err != nil {
		t.Errorf(".exitcode not written: %v", err)
	}
	if _, err := os.Stat(completePath); err != nil {
		t.Errorf("SPAWN_COMPLETE not written (spored would never self-terminate the box): %v", err)
	}
}
