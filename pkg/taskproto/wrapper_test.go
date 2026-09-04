package taskproto

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
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

// TestGenerateWrapper_MkdirCoversOutputOnlyDir guards against spawn#564: an
// output whose Source directory is NOT shared with any input's Destination
// directory must still get a `mkdir -p` on the host before `docker run` runs.
// Without it, `docker run -v /tmp/out:/tmp/out` auto-creates the missing
// bind-mount source as root, mode 0755 — and the container (running as the
// invoking uid per #555, not root) then gets EACCES on its first write.
//
// The spec below has an output-only directory (/tmp/out, from Source
// "/tmp/out/result.txt") that shares nothing with the single input's
// destination directory (/data). The OLD wrapper generation (mkdir loop over
// spec.Inputs only) omitted /tmp/out entirely — this test's job is to catch
// exactly that regression, so it must fail against the pre-fix code and pass
// against the fix.
func TestGenerateWrapper_MkdirCoversOutputOnlyDir(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "index",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/tmp/out/result.txt", Destination: "s3://out/result.txt"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	if !strings.Contains(w, "mkdir -p '/tmp/out'") {
		t.Errorf("wrapper must mkdir -p the output-only directory '/tmp/out' before it can be written to or bind-mounted\n---\n%s", w)
	}
	// Sanity: the shared case (input dir) still gets its mkdir too.
	if !strings.Contains(w, "mkdir -p '/data'") {
		t.Errorf("wrapper must still mkdir -p the input destination directory '/data'\n---\n%s", w)
	}
	// The mkdir loop must run BEFORE any docker/user-command execution, so the
	// directory exists by the time anything tries to write into it.
	mkdirIdx := strings.Index(w, "mkdir -p '/tmp/out'")
	stageIdx := strings.Index(w, "aws s3 cp 's3://in/ref.fa'")
	if mkdirIdx < 0 || stageIdx < 0 || mkdirIdx > stageIdx {
		t.Errorf("mkdir -p '/tmp/out' must precede stage-in/command execution; mkdirIdx=%d stageIdx=%d\n---\n%s", mkdirIdx, stageIdx, w)
	}
}

// TestGenerateWrapper_MkdirLoopMatchesManifestMountDirs proves the mkdir loop
// and manifestMountDirs can never drift apart again (spawn#564's fix reuses
// manifestMountDirs' output directly): every directory manifestMountDirs
// returns must appear as a `mkdir -p` in the generated wrapper, for a spec
// with multiple inputs and outputs across overlapping and non-overlapping
// directories. (Renamed from ...MatchesContainerMountDirs when spawn#570 split
// the mkdir set from the docker-mount set — see TestGenerateWrapper_
// PlacementMountsNotInMkdirLoop below for the split itself.)
func TestGenerateWrapper_MkdirLoopMatchesManifestMountDirs(t *testing.T) {
	spec := &TaskSpec{
		TaskID:  "multi",
		Command: []string{"true"},
		Inputs: []Manifest{
			{Source: "s3://in/a", Destination: "/data/a"},
			{Source: "s3://in/b", Destination: "/data/sub/b"},
		},
		Outputs: []Manifest{
			{Source: "/work/out.bam", Destination: "s3://out/out.bam"},   // shares nothing with inputs
			{Source: "/data/also.txt", Destination: "s3://out/also.txt"}, // shares /data with an input
		},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	for _, dir := range manifestMountDirs(spec) {
		want := "mkdir -p " + shQuote(dir)
		if !strings.Contains(w, want) {
			t.Errorf("wrapper missing %q for manifestMountDirs entry %q\n---\n%s", want, dir, w)
		}
	}
}

// TestGenerateWrapper_PlacementMountsIncludeInDockerRun (spawn#570 sub-issue
// 1, the blocking bug): a containerized task with a Placement (EFS + FSx +
// volume) but NO manifest entry under any of those paths must still get them
// bind-mounted into the container via `docker run -v` — otherwise the
// filesystem is genuinely mounted on the HOST (taskStorageScript's job,
// unaffected by this fix) but invisible to the container, which is where the
// aarch.bio/aarch.science one-tool-per-image model runs every command.
func TestGenerateWrapper_PlacementMountsIncludeInDockerRun(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "refalign",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/efs/refs/GRCh38.fa"},
		Placement: Placement{
			EFSID:       "fs-efs123",
			FSxLustreID: "fs-fsx456",
			Volumes:     []VolumeRef{{Snapshot: "snap-abc", MountPath: "/refdata", ReadOnly: true}},
		},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	for _, want := range []string{
		"-v '/efs':'/efs'",
		"-v '/fsx':'/fsx'",
		"-v '/refdata':'/refdata'",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("docker run -v list missing placement mount %q (spawn#570 sub-issue 1: container never sees the mounted filesystem)\n---\n%s", want, w)
		}
	}
}

// TestGenerateWrapper_PlacementMountOverrideHonored covers spawn#570
// sub-issue 3's other half: when a spec overrides Placement.EFSMountPoint /
// FSxMountPoint away from the "/efs"/"/fsx" defaults, the docker `-v` list
// must bind-mount the OVERRIDDEN path, not the hardcoded default — the mount
// list has to track wherever taskStorageScript actually mounted it.
func TestGenerateWrapper_PlacementMountOverrideHonored(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "refalign",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/mnt/refdata/GRCh38.fa"},
		Placement: Placement{
			EFSID:         "fs-efs123",
			EFSMountPoint: "/mnt/refdata",
			FSxLustreID:   "fs-fsx456",
			FSxMountPoint: "/mnt/scratch",
		},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	for _, want := range []string{"-v '/mnt/refdata':'/mnt/refdata'", "-v '/mnt/scratch':'/mnt/scratch'"} {
		if !strings.Contains(w, want) {
			t.Errorf("docker run -v list missing overridden placement mount %q\n---\n%s", want, w)
		}
	}
	for _, notWant := range []string{"-v '/efs':'/efs'", "-v '/fsx':'/fsx'"} {
		if strings.Contains(w, notWant) {
			t.Errorf("docker run -v list should not contain the un-overridden default %q once the spec overrides it\n---\n%s", notWant, w)
		}
	}
}

// TestGenerateWrapper_PlacementMountsNotInMkdirLoop is the critical regression
// guard for spawn#570's resolution of the #564 invariant tension: placement
// mount points (EFS/FSx/volume) must appear in the docker `-v` mount list
// (proven above) but must NOT be `mkdir -p`'d by the stage-in preamble. Those
// paths are mounted by the boot-time storage script (cmd/task.go's
// taskStorageScript) BEFORE this wrapper runs; mkdir'ing them here risks
// racing that mount (a plain local directory created first would then be
// silently shadowed once the real mount lands) rather than the harmless
// no-op #564 established for manifest-derived, non-placement directories.
func TestGenerateWrapper_PlacementMountsNotInMkdirLoop(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "refalign",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/efs/refs/GRCh38.fa"},
		Placement: Placement{
			EFSID:       "fs-efs123",
			FSxLustreID: "fs-fsx456",
			Volumes:     []VolumeRef{{Snapshot: "snap-abc", MountPath: "/refdata", ReadOnly: true}},
		},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	// Extract just the stage-in preamble (through the mkdir loop) so a mount
	// path incidentally appearing later in the script (e.g. inside the docker
	// run line itself) doesn't produce a false negative.
	marker := "# ---- run user command ----"
	idx := strings.Index(w, marker)
	if idx < 0 {
		t.Fatalf("wrapper missing marker %q", marker)
	}
	preamble := w[:idx]

	for _, bad := range []string{
		"mkdir -p '/efs'",
		"mkdir -p '/fsx'",
		"mkdir -p '/refdata'",
	} {
		if strings.Contains(preamble, bad) {
			t.Errorf("stage-in preamble must NOT mkdir a placement mount point %q (races the boot-time mount / #564's invariant is 'mkdir what gets mounted BY THIS SCRIPT', not placement mounts done earlier by taskStorageScript)\n---\n%s", bad, preamble)
		}
	}
}

// TestGenerateWrapper_OutputOnlyDirExistsBeforeDockerRun is an exec-based
// sibling of TestGenerateWrapper_MkdirCoversOutputOnlyDir (spawn#564): rather
// than asserting on the generated text, it actually runs the wrapper's
// stage-in preamble (mkdir loop + stage-in block) under bash — with stand-in
// `aws`, `sudo`, and `docker` on PATH — and then checks, for real, that the
// output-only directory exists and is owned by the invoking user BEFORE the
// point where `docker run` would otherwise have auto-created it as root. This
// is the same "prove real shell/filesystem behavior, don't just check the Go
// string" pattern as TestGenerateWrapper_ContainerRunLineExecutesWithRealUID.
func TestGenerateWrapper_OutputOnlyDirExistsBeforeDockerRun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")
	dataDir := filepath.Join(tmp, "data")

	spec := &TaskSpec{
		TaskID:    "index",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: filepath.Join(dataDir, "ref.fa")}},
		Outputs:   []Manifest{{Source: filepath.Join(outDir, "result.txt"), Destination: "s3://out/result.txt"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	// Extract just the stage-in preamble (mkdir loop through the stage-in
	// `fi` blocks) — up to but not including the user-command/docker-run
	// section, so this test proves the directories exist BEFORE docker would
	// ever get a chance to auto-create one as root.
	marker := "# ---- run user command ----"
	idx := strings.Index(w, marker)
	if idx < 0 {
		t.Fatalf("wrapper missing marker %q", marker)
	}
	preamble := w[:idx]

	// Stand-in PATH: `aws` no-ops successfully (stage-in "succeeds" without
	// real S3 access); we only care that the mkdir loop ran for real under bash.
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\nexit 0\n")

	cmd := exec.Command("bash", "-c", preamble) //nolint:gosec // nosemgrep
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec of stage-in preamble failed: %v\noutput: %s\npreamble:\n%s", err, out, preamble)
	}

	info, err := os.Stat(outDir)
	if err != nil {
		t.Fatalf("output-only directory %s was not created by the stage-in preamble: %v", outDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", outDir)
	}

	// Ownership: mkdir -p run as the current test process's uid, so the dir
	// must be owned by that same uid — never root-auto-created, since this
	// process is not root when tests run (skip the assertion if it somehow is).
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	if me.Uid == "0" {
		t.Skip("running as root; ownership assertion is meaningless")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("could not read raw stat_t for %s", outDir)
	}
	gotUID := fmt.Sprintf("%d", st.Uid)
	if gotUID != me.Uid {
		t.Errorf("output-only dir %s owned by uid %s, want invoking uid %s (would have been root-owned if docker run had auto-created it)", outDir, gotUID, me.Uid)
	}
}

// TestGenerateWrapper_ChownsStagedInputToInvokingUID guards against spawn#565:
// each staged input must be chowned to "$(id -u):$(id -g)" — the SAME
// expression `docker run --user` uses (#555) — right after `aws s3 cp` lands
// it, so the file is owned by the exact uid the container will run as
// regardless of whether stage-in and docker-run happen to share one
// uninterrupted shell/uid context. Without this, a staged file living in
// host /tmp (mode 1777, sticky) can only be unlinked by whoever OWNS THE
// FILE — sharing the directory's uid is not enough under the sticky bit —
// so any future refactor that breaks the "stage-in and docker-run are the
// same uid" assumption reintroduces the EPERM the issue reported.
func TestGenerateWrapper_ChownsStagedInputToInvokingUID(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "align",
		Container: "quay.io/biocontainers/bwa:0.7.18",
		Command:   []string{"bwa", "mem", "/data/ref.fa"},
		Inputs:    []Manifest{{Source: "s3://in/ref.fa", Destination: "/data/ref.fa"}},
		Outputs:   []Manifest{{Source: "/work/out.bam", Destination: "s3://out/out.bam"}},
		Lifecycle: Lifecycle{TTL: "4h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	stageLine := `aws s3 cp 's3://in/ref.fa' '/data/ref.fa' || STAGE_RC=$?`
	chownLine := `sudo chown "$(id -u):$(id -g)" '/data/ref.fa'`
	if !strings.Contains(w, stageLine) {
		t.Fatalf("wrapper missing expected stage-in line %q\n---\n%s", stageLine, w)
	}
	if !strings.Contains(w, chownLine) {
		t.Errorf("wrapper missing chown of staged input to the invoking uid:gid (#565): want %q\n---\n%s", chownLine, w)
	}

	// The chown must run AFTER the aws s3 cp for the SAME input (chowning
	// before the file exists would be a no-op on the wrong target) and BEFORE
	// the container/user command runs.
	stageIdx := strings.Index(w, stageLine)
	chownIdx := strings.Index(w, chownLine)
	runIdx := strings.Index(w, "# ---- run user command ----")
	if stageIdx < 0 || chownIdx < 0 || runIdx < 0 || !(stageIdx < chownIdx && chownIdx < runIdx) {
		t.Errorf("ordering wrong: want stage-in(%d) < chown(%d) < run-command(%d)", stageIdx, chownIdx, runIdx)
	}
}

// TestGenerateWrapper_ChownRecursiveForPrefixInput guards against spawn#565's
// recursive case: a Manifest whose Source is an S3 prefix (trailing slash)
// stages a WHOLE TREE under Destination via `aws s3 cp --recursive` — the
// post-stage chown must walk that tree too (`-R`), not just the top-level
// destination path, or files nested under it keep the uid `aws s3 cp` ran
// as (correct already, since that's the invoking uid) but the invariant
// wouldn't generalize to any future staging mechanism that behaves
// differently for nested entries.
func TestGenerateWrapper_ChownRecursiveForPrefixInput(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "t",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://in-bucket/reads/", Destination: "/work/reads/"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	want := `sudo chown -R "$(id -u):$(id -g)" '/work/reads/'`
	if !strings.Contains(w, want) {
		t.Errorf("wrapper missing recursive chown for prefix input: want %q\n---\n%s", want, w)
	}
}

// TestGenerateWrapper_StagedFileOwnedByInvokingUID_RealFilesystem is an
// exec-based verification of the #565 mechanism, not just the generated
// text: it runs the wrapper's stage-in preamble for real, under bash, on a
// real sticky-bit (1777) directory on this machine's filesystem, then checks
// that the chown left the staged file owned by the invoking uid — the exact
// precondition sticky-bit unlink checks (you may remove a file you don't own
// in a sticky dir IF, and only if, you own the FILE itself; see the stronger
// docker-based sibling test below for a genuine cross-uid unlink proof).
func TestGenerateWrapper_StagedFileOwnedByInvokingUID_RealFilesystem(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	if me.Uid == "0" {
		t.Skip("running as root; ownership assertion is meaningless (root bypasses sticky-bit checks entirely)")
	}

	tmp := t.TempDir()
	stickyDir := filepath.Join(tmp, "sticky-tmp")
	if err := os.MkdirAll(stickyDir, 0o777); err != nil {
		t.Fatalf("mkdir sticky dir: %v", err)
	}
	// Real sticky bit (1777), matching host /tmp on the real instance. Shell
	// out to the external `chmod` rather than os.Chmod: on this platform/Go
	// combination os.Chmod(dir, 0o1777) silently drops the sticky bit (mode
	// comes back as 0o777), while `chmod 1777` sets it correctly — same
	// discrepancy the issue itself warns about for bind mounts vs. real
	// filesystems, just one level lower in the stack.
	if out, err := exec.Command("chmod", "1777", stickyDir).CombinedOutput(); err != nil {
		t.Fatalf("chmod 1777 %s: %v (%s)", stickyDir, err, out)
	}
	st, err := os.Stat(stickyDir)
	if err != nil || st.Mode()&os.ModeSticky == 0 {
		t.Fatalf("sticky bit did not take on this filesystem (mode=%v, err=%v); test needs a real sticky dir to be meaningful", st.Mode(), err)
	}

	staged := filepath.Join(stickyDir, "staged.tar")

	spec := &TaskSpec{
		TaskID:    "t",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://in/staged.tar", Destination: staged}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")

	marker := "# ---- run user command ----"
	idx := strings.Index(w, marker)
	if idx < 0 {
		t.Fatalf("wrapper missing marker %q", marker)
	}
	preamble := w[:idx]

	// Stand-in `aws`: instead of a real S3 fetch, `cp` writes the staged file
	// (proving the mkdir+stage-in+chown sequence for real under bash).
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\n# args: s3 cp <flags...> <src> <dst>\ndst=\"${@: -1}\"\ntouch \"$dst\"\n")

	cmd := exec.Command("bash", "-c", preamble) //nolint:gosec // nosemgrep
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec of stage-in preamble failed: %v\noutput: %s\npreamble:\n%s", err, out, preamble)
	}

	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("staged file %s not created: %v", staged, err)
	}
	fst, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("could not read raw stat_t for %s", staged)
	}
	gotUID := fmt.Sprintf("%d", fst.Uid)
	if gotUID != me.Uid {
		t.Fatalf("staged file %s owned by uid %s after chown, want invoking uid %s — sticky-bit unlink by the container (running as this same uid per #555) would fail EPERM otherwise", staged, gotUID, me.Uid)
	}

	// Since the file's owner now equals the invoking uid, this same uid can
	// unlink it even though it doesn't own the sticky directory (only root
	// created that dir in this test's setup) — the sticky-bit unlink rule is
	// "owns the dir OR owns the file", and this proves the file-ownership arm.
	if err := os.Remove(staged); err != nil {
		t.Errorf("removing the now-correctly-owned staged file failed: %v", err)
	}
}

// TestGenerateWrapper_ChownFixesCrossUIDStickyUnlink_Docker is the strongest
// available proof of the #565 mechanism, and — unlike a hand-written repro —
// it runs the ACTUAL chown line GenerateWrapper emits, extracted from the
// generated script's text, so it tests the real code path rather than an
// equivalent hardcoded command. It uses real Linux semantics inside a Docker
// container (this repo's test environment has no root/setpriv to force a uid
// mismatch directly, and a macOS bind mount does not enforce sticky-bit
// ownership at all — the issue's own repro note). Inside ONE alpine
// container with a fresh tmpfs at /sticky chmod'd 1777, as uid 1000 (the
// stand-in "invoking uid", so `id -u`/`id -g` resolve to 1000:1000 exactly
// like they would in the real wrapper's `su - <user>` session):
//  1. WITHOUT the fix (the extracted chown line commented out): a file
//     created by uid 1000, then chowned to uid 6000 (a stand-in for "the
//     image's declared user", e.g. bioconda's mambauser) — uid 1000 (matching
//     --user "$(id -u):$(id -g)" from #555) fails to unlink it. This
//     reproduces the issue's real-world failure exactly.
//  2. WITH the fix: same setup, but this time the extracted chown line runs
//     (as uid 1000, so "$(id -u):$(id -g)" expands to 1000:1000) before the
//     unlink attempt — it now succeeds.
//
// Skips cleanly if Docker isn't available (e.g. sandboxed CI).
func TestGenerateWrapper_ChownFixesCrossUIDStickyUnlink_Docker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	spec := &TaskSpec{
		TaskID:    "t",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://in/mytar", Destination: "/sticky/mytar"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := GenerateWrapper(spec, "b", "us-east-1")
	var chownLine string
	for _, line := range strings.Split(w, "\n") {
		if strings.Contains(line, "chown") && strings.Contains(line, "/sticky/mytar") {
			chownLine = strings.TrimSpace(line)
			break
		}
	}
	if chownLine == "" {
		t.Fatalf("no chown line found for the staged input in generated wrapper\n---\n%s", w)
	}
	t.Logf("extracted chown line under test: %s", chownLine)

	// The inner "fix step" line is written to its OWN file and bind-mounted
	// in (rather than substituted into a shell template via fmt/%q), so the
	// extracted chown line's own quoting is interpreted exactly once — by the
	// container's shell — with no intermediate Go-string-escaping layer that
	// could silently rewrite it into something that no longer matches what
	// GenerateWrapper actually emits.
	const outerTmpl = `apk add --no-cache sudo >/dev/null 2>&1
mkdir -p /sticky && chmod 1777 /sticky
adduser -D -u 1000 spawnuser >/dev/null 2>&1
adduser -D -u 6000 imageuser >/dev/null 2>&1
# Passwordless sudo for the invoking user, matching bootstrap.go's real
# instance setup (the precondition the wrapper's "sudo chown" relies on).
echo 'spawnuser ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/spawnuser

su spawnuser -c 'touch /sticky/mytar'
chown 6000:6000 /sticky/mytar   # simulate landing under a mismatched uid
chmod +x /fixstep.sh
su spawnuser -c /fixstep.sh
su spawnuser -c 'rm -f /sticky/mytar' 2>/tmp/err
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "UNLINK_OK"
else
  echo "UNLINK_FAILED: $(cat /tmp/err)"
fi
`
	run := func(t *testing.T, fixStep string) string {
		dir := t.TempDir()
		outerPath := filepath.Join(dir, "outer.sh")
		fixPath := filepath.Join(dir, "fixstep.sh")
		if err := os.WriteFile(outerPath, []byte(outerTmpl), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
			t.Fatalf("write outer script: %v", err)
		}
		if err := os.WriteFile(fixPath, []byte("#!/bin/sh\n"+fixStep+"\n"), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
			t.Fatalf("write fix-step script: %v", err)
		}
		cmd := exec.Command("docker", "run", "--rm", //nolint:gosec // nosemgrep
			"-v", outerPath+":/outer.sh",
			"-v", fixPath+":/fixstep.sh",
			"alpine", "sh", "/outer.sh")
		out, err := cmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "UNLINK_") {
			t.Fatalf("docker run failed: %v\noutput:\n%s", err, out)
		}
		return string(out)
	}

	// Case 1 (no fix): confirms the bug mechanism itself — the "fix step" is a
	// no-op, so the mismatched-uid file cannot be unlinked.
	withoutFix := run(t, "true")
	if !strings.Contains(withoutFix, "UNLINK_FAILED") {
		t.Errorf("expected unlink to FAIL without the chown (bug reproduction failed)\noutput:\n%s", withoutFix)
	}

	// Case 2 (with fix): the "fix step" is the ACTUAL extracted wrapper chown
	// line, run verbatim as uid 1000 — so its "$(id -u):$(id -g)" expands to
	// 1000:1000, matching #555's --user value for this same su session —
	// after which the same unlink must succeed.
	withFix := run(t, chownLine)
	if !strings.Contains(withFix, "UNLINK_OK") {
		t.Errorf("expected unlink to SUCCEED after running the wrapper's real chown line %q\noutput:\n%s", chownLine, withFix)
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

// TestGenerateWrapper_PhaseMarkersHelperDefinedBeforeFirstUse guards the
// ordering the rest of #571's markers depend on: `spawn_phase()` must be
// defined before "wrapper start" (its first call site) or every marker after
// it is a "command not found" instead of a timestamp.
func TestGenerateWrapper_PhaseMarkersHelperDefinedBeforeFirstUse(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")
	defIdx := strings.Index(w, "spawn_phase()")
	firstCallIdx := strings.Index(w, "spawn_phase 'wrapper start'")
	if defIdx < 0 || firstCallIdx < 0 {
		t.Fatalf("wrapper missing spawn_phase definition or first call")
	}
	if !(defIdx < firstCallIdx) {
		t.Errorf("spawn_phase() defined at %d, first called at %d — helper must be defined first", defIdx, firstCallIdx)
	}
}

// TestGenerateWrapper_PhaseMarkersHostPath (#571) — completion.json's
// started_at/ended_at previously made stage-in, the user command, and
// stage-out one unattributable interval. The host path (no Container) should
// get every phase EXCEPT the Docker-specific ones.
func TestGenerateWrapper_PhaseMarkersHostPath(t *testing.T) {
	w := GenerateWrapper(fullSpec(), "b", "us-east-1")

	mustContain := []string{
		"spawn_phase 'wrapper start'",
		"spawn_phase 'stage-in start'",
		`spawn_phase "stage-in done rc=$STAGE_RC"`,
		"spawn_phase 'command start'",
		`spawn_phase "command exit rc=$rc"`,
		"spawn_phase 'stage-out start'",
		`spawn_phase "stage-out done rc=$OUT_RC"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(w, want) {
			t.Errorf("host wrapper missing phase marker %q", want)
		}
	}
	mustNotContain := []string{
		"spawn_phase 'docker install start'",
		"spawn_phase 'docker install done'",
		"spawn_phase 'image pull start'",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(w, bad) {
			t.Errorf("host wrapper (no Container) unexpectedly contains Docker phase marker %q", bad)
		}
	}

	// Ordering matters as much as presence: a marker after the thing it
	// announces is worse than no marker (it lies about when the phase ran).
	idx := func(sub string) int {
		i := strings.Index(w, sub)
		if i < 0 {
			t.Fatalf("marker %q not found", sub)
		}
		return i
	}
	wrapperStart := idx("spawn_phase 'wrapper start'")
	stageInStart := idx("spawn_phase 'stage-in start'")
	stageInDone := idx(`spawn_phase "stage-in done rc=$STAGE_RC"`)
	cmdStart := idx("spawn_phase 'command start'")
	cmdExit := idx(`spawn_phase "command exit rc=$rc"`)
	stageOutStart := idx("spawn_phase 'stage-out start'")
	stageOutDone := idx(`spawn_phase "stage-out done rc=$OUT_RC"`)
	if !(wrapperStart < stageInStart && stageInStart < stageInDone &&
		stageInDone < cmdStart && cmdStart < cmdExit &&
		cmdExit < stageOutStart && stageOutStart < stageOutDone) {
		t.Errorf("phase markers out of order:\nwrapperStart=%d stageInStart=%d stageInDone=%d cmdStart=%d cmdExit=%d stageOutStart=%d stageOutDone=%d",
			wrapperStart, stageInStart, stageInDone, cmdStart, cmdExit, stageOutStart, stageOutDone)
	}
}

// TestGenerateWrapper_PhaseMarkersContainerPath (#571) — the container path
// gets the host markers plus the Docker-specific ones (install, image pull),
// in the order the issue specifies: this is exactly the breakdown that let a
// 94s command window (25s of real work, ~69s unattributable) actually be
// attributed.
func TestGenerateWrapper_PhaseMarkersContainerPath(t *testing.T) {
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
		"spawn_phase 'wrapper start'",
		"spawn_phase 'stage-in start'",
		`spawn_phase "stage-in done rc=$STAGE_RC"`,
		"spawn_phase 'docker install start'",
		"spawn_phase 'docker install done'",
		"spawn_phase 'image pull start'",
		"spawn_phase 'image pull done; command start'",
		`spawn_phase "command exit rc=$rc"`,
		"spawn_phase 'stage-out start'",
		`spawn_phase "stage-out done rc=$OUT_RC"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(w, want) {
			t.Errorf("container wrapper missing phase marker %q", want)
		}
	}
	// The host-path-only markers must not leak into the container path — the
	// container path calls writeContainerRun instead, which never emits them.
	mustNotContain := []string{
		"spawn_phase 'command start'\n",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(w, bad) {
			t.Errorf("container wrapper unexpectedly contains host-only phase marker %q", bad)
		}
	}

	idx := func(sub string) int {
		i := strings.Index(w, sub)
		if i < 0 {
			t.Fatalf("marker %q not found", sub)
		}
		return i
	}
	dockerInstallStart := idx("spawn_phase 'docker install start'")
	dockerInstallDone := idx("spawn_phase 'docker install done'")
	imagePullStart := idx("spawn_phase 'image pull start'")
	imagePullDoneCmdStart := idx("spawn_phase 'image pull done; command start'")
	cmdExit := idx(`spawn_phase "command exit rc=$rc"`)
	if !(dockerInstallStart < dockerInstallDone &&
		dockerInstallDone < imagePullStart &&
		imagePullStart < imagePullDoneCmdStart &&
		imagePullDoneCmdStart < cmdExit) {
		t.Errorf("container phase markers out of order:\ndockerInstallStart=%d dockerInstallDone=%d imagePullStart=%d imagePullDoneCmdStart=%d cmdExit=%d",
			dockerInstallStart, dockerInstallDone, imagePullStart, imagePullDoneCmdStart, cmdExit)
	}
}
