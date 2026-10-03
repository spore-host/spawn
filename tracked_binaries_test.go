package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// binaryMagics are the leading bytes of a compiled executable.
//
// Matching magic bytes rather than shelling out to `file` is deliberate: `file`
// describes every shell and Python script in this repo as "... text
// executable", and there are 20+ of those under lambda/ alone (deploy.sh,
// handler.py, the dashboard-api setup scripts). A substring check on "executable"
// would fail on all of them, which is how a gate like this ends up deleted
// instead of fixed.
var binaryMagics = []struct {
	name  string
	magic []byte
}{
	{"ELF", []byte{0x7f, 'E', 'L', 'F'}},
	{"Mach-O 64-bit", []byte{0xcf, 0xfa, 0xed, 0xfe}},
	{"Mach-O 32-bit", []byte{0xce, 0xfa, 0xed, 0xfe}},
	// 0xcafebabe is both a universal ("fat") Mach-O and a Java .class file. This
	// repo has no Java, so the ambiguity costs nothing here; if one is ever added,
	// narrow this entry rather than dropping it.
	{"Mach-O universal", []byte{0xca, 0xfe, 0xba, 0xbe}},
	{"PE/COFF (Windows)", []byte{'M', 'Z'}},
}

// TestNoTrackedCompiledBinaries fails if any file tracked in git is a compiled
// executable (spawn#637).
//
// The repo had exactly one: lambda/autoscale-orchestrator/autoscale-orchestrator,
// 22 MiB of Mach-O arm64 — a macOS build, so it could never even have run on
// Lambda's provided.al2023/arm64. It was the single name missing from
// .gitignore's HAND-MAINTAINED per-function list, which is the real defect: that
// list has to be extended by hand for every new lambda, and nothing noticed the
// omission. A plain `go build ./...` in that module — which CI itself runs on
// every lambda module — rewrote the tracked file, and `git add -A` then staged a
// 22 MiB diff. That happened twice in one session before this gate existed.
//
// Two further costs made it worth a gate rather than just a .gitignore line:
//   - template.yaml uses `CodeUri: .` with no .samignore, so SAM packaged the
//     whole directory — the dead binary rode along inside every deployed Lambda
//     artifact.
//   - A committed binary is unverifiable: nothing ties those bytes to a commit.
//     scripts/check-release-version.sh exists precisely because a binary that
//     reports the wrong thing is hard to notice.
//
// This asserts the invariant generically, so the next lambda cannot reintroduce
// the problem by being forgotten in a list.
func TestNoTrackedCompiledBinaries(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable (not a git checkout?): %v", err)
	}

	var offenders []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" {
			continue
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			continue // deleted-but-tracked, or a symlink/submodule
		}

		f, openErr := os.Open(path) //nolint:gosec // paths come from git ls-files
		if openErr != nil {
			continue
		}
		head := make([]byte, 4)
		n, _ := f.Read(head)
		_ = f.Close()
		head = head[:n]

		for _, m := range binaryMagics {
			if bytes.HasPrefix(head, m.magic) {
				offenders = append(offenders, path+" ("+m.name+")")
				break
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("compiled binaries are tracked in git:\n  %s\n\n"+
			"Build output must not be committed. Add the path to .gitignore and\n"+
			"`git rm --cached` it. Committing one means every clone pays for it\n"+
			"forever, a routine `go build` dirties the tree so `git add -A` stages\n"+
			"megabytes, and the bytes are unverifiable — nothing ties them to a commit.",
			strings.Join(offenders, "\n  "))
	}
}

// TestEveryGoLambdaBinaryIsIgnored checks the .gitignore list is complete for
// the lambdas that actually build a Go binary, so the failure mode behind #637 —
// a new lambda silently absent from a hand-maintained list — is caught at the
// source rather than only once a binary has been committed.
//
// Scoped to modules with a go.mod: lambda/github-oauth is Python (handler.py)
// and has no Go build output, so requiring an entry for it would be noise.
func TestEveryGoLambdaBinaryIsIgnored(t *testing.T) {
	gitignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(string(gitignore), "\n") {
		lines[strings.TrimSpace(l)] = true
	}

	entries, err := os.ReadDir("lambda")
	if err != nil {
		t.Skipf("no lambda/ directory: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := os.Stat("lambda/" + name + "/go.mod"); err != nil {
			continue // not a Go module (e.g. the Python github-oauth lambda)
		}
		// `go build` with no -o writes a binary named after the module directory.
		want := "lambda/" + name + "/" + name
		if !lines[want] {
			t.Errorf(".gitignore is missing %q.\n"+
				"A `go build ./...` in lambda/%s would leave a committable binary there — "+
				"that is exactly how #637 happened.", want, name)
		}
	}
}
