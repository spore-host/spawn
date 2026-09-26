package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `ephemeral` promise is the kind of text that regrows. "Reaped when this
// instance terminates" is a shorter, friendlier sentence than the true one, and
// it is exactly the sentence that cost a user a month of 1.2 TiB FSx billing
// (spawn#613): nothing in spawn's terminate path touches FSx, so the filesystem
// is reclaimed asynchronously by the out-of-band reaper — or not at all, in an
// account the reaper does not scan.
//
// These gates fail a test instead of quietly shipping the friendlier lie again.

// phrasesThatPromiseSynchronousReaping are substrings (lowercased) that assert or
// imply `spawn terminate` itself deletes the filesystem.
var phrasesThatPromiseSynchronousReaping = []string{
	"reaped when this instance terminates",
	"reaped when the instance terminates",
	"reaped with this instance",
	"reclaimed automatically when the instance terminates",
	"deleted when the instance terminates",
	"ephemeral filesystems are deleted when the job ends",
}

func TestNoSynchronousEphemeralReapPromises(t *testing.T) {
	var files []string
	for _, pattern := range []string{"*.go", "../docs/*.md", "../README.md", "../docs-gen/*.md"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("found no files to scan; the gate would pass vacuously")
	}

	for _, path := range files {
		// This file necessarily contains the forbidden phrases.
		if filepath.Base(path) == "fsx_promise_gate_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := strings.ToLower(string(data))
		for _, phrase := range phrasesThatPromiseSynchronousReaping {
			if strings.Contains(body, phrase) {
				t.Errorf("%s claims %q — but nothing reaps an FSx at terminate time; "+
					"reclamation is asynchronous and best-effort (spawn#613). Say when it "+
					"actually happens, or a user will believe a ~$174/month filesystem is gone when it isn't.",
					path, phrase)
			}
		}
	}
}

// TestFSxLifecycleHelpStatesAsynchronousReclamation is the positive half: the
// flag help must actively say reclamation is asynchronous, not merely omit the
// false claim (an omission reads as "it just works").
func TestFSxLifecycleHelpStatesAsynchronousReclamation(t *testing.T) {
	flag := launchCmd.Flags().Lookup("fsx-lifecycle")
	if flag == nil {
		t.Fatal("launch has no --fsx-lifecycle flag")
	}
	usage := strings.ToLower(flag.Usage)
	if !strings.Contains(usage, "asynchronous") {
		t.Errorf("--fsx-lifecycle help does not say reclamation is asynchronous: %q", flag.Usage)
	}
	if !strings.Contains(usage, "terminate") {
		t.Errorf("--fsx-lifecycle help should say `spawn terminate` is not what deletes it: %q", flag.Usage)
	}
}
