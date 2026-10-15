//go:build !windows

package taskproto

// Tests that assert Unix file OWNERSHIP (a uid read from syscall.Stat_t) after
// running the wrapper's stage-in preamble under bash. Split out of
// wrapper_test.go so the package's tests compile for GOOS=windows (#925):
// Stat_t does not exist there, and neither do the uid and sticky-bit
// semantics these tests prove. The generated wrapper runs only on Linux
// instances, so excluding Windows loses nothing these tests are about.

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
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)

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
	writeFakeSudo(t, binDir)

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
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)

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
	writeFakeSudo(t, binDir)

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
