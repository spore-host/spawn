package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- #1270: a container task's /tmp ----

// tmpfsWord returns the argument of the --tmpfs flag on a docker run line, or "".
func tmpfsWord(run string) string {
	_, after, ok := strings.Cut(run, " --tmpfs ")
	if !ok {
		return ""
	}
	w, _, _ := strings.Cut(after, " ")
	return w
}

// #1270: with every manifest entry a directory under /tmp, only those
// directories were bind-mounted, so the container's /tmp was the image overlay
// on the root disk. Intent (owner decision): the container gets its own
// RAM-backed /tmp, sized like AL2023's (half of RAM), executable, and never a
// bind of the host /tmp, which holds spored's files.
func TestContainerTmpIsItsOwnTmpfs(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "plink2",
		Container: "quay.io/biocontainers/plink2:2.0",
		Command:   []string{"plink2", "--out", "/tmp/out/kg"},
		Inputs:    []Manifest{{Source: "s3://b/kg/", Destination: "/tmp/in/"}},
		Outputs:   []Manifest{{Source: "/tmp/out/", Destination: "s3://b/res/"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)
	runs := dockerRunLines(w)
	if len(runs) != 1 {
		t.Fatalf("want one docker run line, got %d:\n%s", len(runs), w)
	}
	run := runs[0]
	tw := tmpfsWord(run)
	if !strings.HasPrefix(tw, "'/tmp:") {
		t.Fatalf("docker run line has no --tmpfs /tmp:\n%s", run)
	}
	for _, opt := range []string{"size=50%", "exec", "mode=1777"} {
		if !strings.Contains(","+strings.TrimPrefix(strings.Trim(tw, "'"), "/tmp:")+",", ","+opt+",") {
			t.Errorf("--tmpfs %s lacks %q (size half of RAM like AL2023's /tmp; exec because Docker defaults to noexec; 1777 for a non-root --user)", tw, opt)
		}
	}
	if strings.Contains(run, "-v '/tmp':'/tmp'") {
		t.Errorf("host /tmp is bind-mounted into the container (spored keeps its files there):\n%s", run)
	}
	for _, d := range []string{"/tmp/in", "/tmp/out"} {
		if !strings.Contains(run, "-v "+shQuote(d)+":"+shQuote(d)+" ") {
			t.Errorf("staged dir %s is no longer bind-mounted:\n%s", d, run)
		}
	}
	if img := strings.Index(run, " "+shQuote(spec.Container)+" "); img < 0 || strings.Index(run, " --tmpfs ") > img {
		t.Errorf("--tmpfs must precede the image (after it, it is the container's argv):\n%s", run)
	}
	if !strings.Contains(w, "echo \"spawn: container /tmp is tmpfs, size=50% of RAM") {
		t.Errorf("command.log must say what backs the container's /tmp:\n%s", w)
	}
}

// A manifest path directly in /tmp makes /tmp itself a mount dir. That keeps
// today's behaviour (host /tmp bind-mounted) and must then NOT add --tmpfs
// /tmp: Docker refuses the pair with "Duplicate mount point: /tmp" (checked
// against Docker 29.9.2), which would fail every such task.
func TestContainerTmpStaysHostBindWhenAManifestPathIsDirectlyInTmp(t *testing.T) {
	spec := &TaskSpec{
		TaskID:    "plink2",
		Container: "quay.io/biocontainers/plink2:2.0",
		Command:   []string{"bash", "/tmp/p2-task.sh"},
		Inputs: []Manifest{
			{Source: "s3://b/p2-task.sh", Destination: "/tmp/p2-task.sh"},
			{Source: "s3://b/kg/", Destination: "/tmp/in/"},
		},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)
	runs := dockerRunLines(w)
	if len(runs) != 1 {
		t.Fatalf("want one docker run line, got %d:\n%s", len(runs), w)
	}
	if !strings.Contains(runs[0], "-v '/tmp':'/tmp' ") {
		t.Fatalf("a file staged directly in /tmp must still be visible (host /tmp bind):\n%s", runs[0])
	}
	if strings.Contains(runs[0], "--tmpfs") {
		t.Errorf("--tmpfs /tmp together with -v /tmp:/tmp is a duplicate mount point:\n%s", runs[0])
	}
	if !strings.Contains(w, "spawn: container /tmp is the host /tmp") {
		t.Errorf("command.log must say the container's /tmp is the host's here:\n%s", w)
	}
}

// The host path has no container, so no --tmpfs anywhere.
func TestHostTaskHasNoTmpfs(t *testing.T) {
	spec := &TaskSpec{TaskID: "h", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"},
		Inputs: []Manifest{{Source: "s3://b/kg/", Destination: "/tmp/in/"}}}
	if w := genWrapper(spec, "b", "us-east-1", false, testRunID); strings.Contains(w, "--tmpfs") {
		t.Errorf("host task emitted --tmpfs:\n%s", w)
	}
}

// The same property, executed: the whole wrapper runs under bash with a
// stand-in docker recording the run call's argv, one word per line.
func TestContainerTmpfsReachesDockerRunArgv(t *testing.T) {
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in")+"/", filepath.Join(dir, "out")+"/"
	spec := &TaskSpec{
		TaskID:    "tmpfs-argv",
		Container: "quay.io/biocontainers/plink2:2.0",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://b/kg/", Destination: in}},
		Outputs:   []Manifest{{Source: out, Destination: "s3://b/res/"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
	binDir := runtimeStandInBin(t)
	// Stage one file so the directory input is not empty (#1207).
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\ndst=\"${@: -1}\"\ncase \"$dst\" in s3://*) ;; */) mkdir -p \"$dst\" && touch \"$dst/f\" ;; esac\nexit 0\n")
	writeFakeExe(t, filepath.Join(binDir, "systemctl"), "#!/bin/bash\nexit 0\n")
	writeFakeExe(t, filepath.Join(binDir, "docker"), "#!/bin/bash\nif [ \"$1\" = run ]; then printf '%s\\n' \"$@\" > \"$DOCKER_RUN\"; fi\nexit 0\n")
	writeFakeSudo(t, binDir)
	runArgv := filepath.Join(t.TempDir(), "run.argv")
	scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(w), 0o600); err != nil {
		t.Fatal(err)
	}
	completionPath := CompletionRecordPath()
	_ = os.Remove(completionPath)
	t.Cleanup(func() { _ = os.Remove(completionPath) })
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = []string{"PATH=" + binDir, "HOME=" + t.TempDir(), "DOCKER_RUN=" + runArgv}
	output, _ := cmd.CombinedOutput()
	data, err := os.ReadFile(runArgv)
	if err != nil {
		t.Fatalf("docker run was never called: %v\noutput:\n%s", err, output)
	}
	argv := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	tmpfs := ""
	for i, a := range argv {
		if a == "--tmpfs" && i+1 < len(argv) {
			tmpfs = argv[i+1]
		}
		if a == "-v" && i+1 < len(argv) && argv[i+1] == "/tmp:/tmp" {
			t.Errorf("host /tmp bind-mounted: %q", argv)
		}
	}
	if tmpfs != "/tmp:"+containerTmpfsOptions || !strings.Contains(tmpfs, "size=50%") {
		t.Errorf("--tmpfs argument = %q, want /tmp with size=50%%: argv %q", tmpfs, argv)
	}
	if !strings.Contains(string(output), "spawn: container /tmp is tmpfs, size=50% of RAM") {
		t.Errorf("command.log does not say what backs /tmp:\n%s", output)
	}
}

// ---- #1207: an empty or missing directory input ----

// runStageInWrapper runs the whole host-path wrapper for spec under bash, with
// awsScript as the `aws` and the real tools after it on PATH, and returns the
// completion record and the script's output.
func runStageInWrapper(t *testing.T, spec *TaskSpec, awsScript string) (*CompletionRecord, string) {
	t.Helper()
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "aws"), awsScript)
	writeFakeSudo(t, binDir)
	installStandInDate(t, binDir)
	scriptPath := filepath.Join(t.TempDir(), "wrapper.sh")
	if err := os.WriteFile(scriptPath, []byte(w), 0o600); err != nil {
		t.Fatal(err)
	}
	completionPath := CompletionRecordPath()
	_ = os.Remove(completionPath)
	t.Cleanup(func() { _ = os.Remove(completionPath) })
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = []string{"PATH=" + binDir + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, _ := cmd.CombinedOutput()
	data, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatalf("no completion record: %v\noutput:\n%s", err, out)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("parse record: %v\nraw: %s", err, data)
	}
	return rec, string(out)
}

// Stand-in `aws s3 cp` downloads, by what the prefix holds. Uploads succeed.
const (
	// An empty or missing prefix: the real CLI copies nothing and exits 0.
	awsCopiesNothing = "#!/bin/bash\nexit 0\n"
	// A prefix holding only a zero-byte "folder marker" key (ends in "/"): the
	// real CLI skips such keys on download (awscli/customizations/s3/
	// filegenerator.py yields a Size==0 key ending in "/" only for delete). At
	// most a directory appears; this stand-in creates one, nested, to prove only
	// regular files count.
	awsCopiesOnlyDirs = "#!/bin/bash\ndst=\"${@: -1}\"\ncase \"$dst\" in s3://*) ;; *) mkdir -p \"$dst/sub\" ;; esac\nexit 0\n"
	// A prefix holding one (zero-byte, but real) object.
	awsCopiesAFile = "#!/bin/bash\ndst=\"${@: -1}\"\ncase \"$dst\" in s3://*) ;; *) mkdir -p \"$dst\" && : > \"$dst/ref.json\" ;; esac\nexit 0\n"
)

func emptyDirSpec(t *testing.T, allowEmpty bool) *TaskSpec {
	return &TaskSpec{
		TaskID:    "zarr-b",
		Command:   []string{"true"},
		Inputs:    []Manifest{{Source: "s3://b/runs/ncref/", Destination: filepath.Join(t.TempDir(), "ncref") + "/", AllowEmpty: allowEmpty}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
}

// #1207: a directory input that copies nothing fails stage-in, through the
// same path as a missing single object (staging_error, command not run), and
// says which prefix and how to opt out.
func TestEmptyDirectoryInputFailsStageIn(t *testing.T) {
	for _, tc := range []struct{ name, aws string }{
		{"missing or empty prefix", awsCopiesNothing},
		{"folder marker only", awsCopiesOnlyDirs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, out := runStageInWrapper(t, emptyDirSpec(t, false), tc.aws)
			if rec.State != StateFailed || rec.RetryClass != RetryStagingError {
				t.Errorf("state=%q retry_class=%q, want failed/%s\noutput:\n%s", rec.State, rec.RetryClass, RetryStagingError, out)
			}
			want := "spawn: input s3://b/runs/ncref/ copied no objects: the prefix is empty or missing (set allow_empty: true on this input if that is expected)"
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
			if !strings.Contains(out, "stage-in failed; skipping user command") {
				t.Errorf("the user command must not run on an empty input:\n%s", out)
			}
		})
	}
}

// Controls: allow_empty restores the old behaviour, and a prefix that copies
// a file (even a zero-byte one) is not empty.
func TestDirectoryInputStagesWhenAllowedEmptyOrNonEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		aws   string
	}{
		{"allow_empty with nothing copied", true, awsCopiesNothing},
		{"a file copied", false, awsCopiesAFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, out := runStageInWrapper(t, emptyDirSpec(t, tc.allow), tc.aws)
			if rec.State != StateCompleted || rec.RetryClass != RetryNone {
				t.Errorf("state=%q retry_class=%q, want completed\noutput:\n%s", rec.State, rec.RetryClass, out)
			}
			if strings.Contains(out, "copied no objects") {
				t.Errorf("empty-input failure reported:\n%s", out)
			}
		})
	}
}

func TestValidateAllowEmpty(t *testing.T) {
	base := func() *TaskSpec {
		return &TaskSpec{TaskID: "t", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"}}
	}
	ok := base()
	ok.Inputs = []Manifest{{Source: "s3://b/p/", Destination: "/data/p/", AllowEmpty: true}}
	if err := ok.Validate(); err != nil {
		t.Errorf("allow_empty on a directory input rejected: %v", err)
	}
	file := base()
	file.Inputs = []Manifest{{Source: "s3://b/ref.json", Destination: "/data/ref.json", AllowEmpty: true}}
	if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "inputs[0].allow_empty applies only to a directory input") {
		t.Errorf("allow_empty on a file input: err = %v", err)
	}
	outSpec := base()
	outSpec.Outputs = []Manifest{{Source: "/data/out/", Destination: "s3://b/out/", AllowEmpty: true}}
	if err := outSpec.Validate(); err == nil || !strings.Contains(err.Error(), "outputs[0].allow_empty is not supported") {
		t.Errorf("allow_empty on an output: err = %v", err)
	}
	// The field parses under the strict decoder.
	if _, err := ParseSpec([]byte(`{"task_id":"t","command":["true"],"lifecycle":{"ttl":"1h"},"inputs":[{"source":"s3://b/p/","destination":"/d/","allow_empty":true}]}`)); err != nil {
		t.Errorf("allow_empty does not parse: %v", err)
	}
}
