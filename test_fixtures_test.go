package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Test fixtures must not be fabricated, and a test must not skip because its own
// fixture is wrong (#809, from the Phase 4 audit).
//
// Thirteen tests were skipping. Eleven of them skipped on the same cause: they
// passed an invented AMI id — "ami-base", "ami-dcv", "ami-x86-standard" — to
// RunInstances, substrate rejected it as `InvalidAMIID.Malformed`, and each test
// reported itself skipped for an emulator limitation that did not exist.
// Substrate has validated image ids since v0.120.0 and ships with none, so the
// fixture was the defect; `testutil.RegisterTestAMI` registers a real one and had
// been in use by other tests the whole time.
//
// What that cost: coverage of irreversible AMI/snapshot deletion, of the
// "no instance profile is dead, not slow" SSM path, of `spawn app launch`
// readiness polling, and of the headless `Provision` path lagotto's
// capacity-poller takes (lagotto#19) — all reported as present.
//
// A skip is the dangerous shape here because it is counted as a pass. These
// gates make the two mistakes that produced it unrepeatable.

// amiLiteral matches an invented AMI id: `ami-` followed by anything that is not
// a plain hex suffix. A real id is ami- plus 8 or 17 hex digits.
var amiLiteral = regexp.MustCompile(`"ami-(?:[0-9a-f]{8}|[0-9a-f]{17})"`)
var anyAMILiteral = regexp.MustCompile(`"ami-[A-Za-z0-9_-]+"`)

// imageIDAssign matches an ImageId struct field being given something.
var imageIDAssign = regexp.MustCompile(`ImageId:\s*(\S[^,\n]*)`)

// Every ImageId a test hands to the EC2 API must come from a registered AMI.
//
// Discovery over the whole tree rather than the files that were wrong, so a new
// test cannot reintroduce the fixture. The pure-logic tests that legitimately
// keep a literal (pkg/aws/ami_test.go maps SSM parameter paths to ids) are
// untouched, because they never assign it to ImageId.
func TestNoTestFabricatesAnAMIForTheEC2API(t *testing.T) {
	checked := 0
	for _, f := range goTestFiles(t) {
		body := readRepoFile(t, f)
		for _, m := range imageIDAssign.FindAllStringSubmatch(body, -1) {
			value := strings.TrimSpace(m[1])
			checked++
			if strings.Contains(value, "RegisterTestAMI") {
				continue
			}
			// A variable is fine — it was resolved from somewhere, and that
			// somewhere is covered by the same rule where it is a literal.
			if !anyAMILiteral.MatchString(value) {
				continue
			}
			if amiLiteral.MatchString(value) {
				continue // a well-formed id; substrate accepts the shape
			}
			// Note the division of labour with the test itself: this gate catches a
			// MALFORMED id, and a well-formed-but-unregistered one
			// ("ami-0123456789abcdef0") passes here and then fails the test
			// loudly, because the skips that used to swallow that were converted
			// to t.Fatalf. Neither shape can report coverage it does not have.
			t.Errorf("%s assigns ImageId = %s. Substrate validates image ids, so an invented one "+
				"fails RunInstances with InvalidAMIID.Malformed — and the usual response has been to "+
				"skip the test, which reports coverage and exercises nothing. Use "+
				"testutil.RegisterTestAMI(t, ec2Client).", f, value)
		}
	}
	if checked == 0 {
		t.Fatal("found no ImageId assignments in any test — discovery has broken, it has not proven " +
			"the fixtures clean")
	}
}

// A gate removed after running it, kept as a note.
//
// The obvious companion to the test above is "no test may skip on a substrate
// failure while containing an invented AMI id". I wrote it, because the skip
// MESSAGES are what made the defect survive review — "substrate RunInstances
// unavailable", "substrate does not fully model the launch path", each a true
// description of what happened and a false diagnosis.
//
// Run, it reported eight sites and every one was a false positive. Invented ids
// appear legitimately all over the tests: `AMI: "ami-test"` in a LaunchConfig that
// deliberately pre-sets the field to skip the lookup, `others: []string{"ami-aaa",
// "ami-bbb"}` as table expectations for a snapshot-sharing check. None of them
// reach the EC2 API. The gate was keying on a file containing both things, which
// is guilt by association, not a mechanism.
//
// That is the third gate in this audit removed for the same reason: it gated a
// CORRELATE of the defect rather than the defect. The others were "no local flag
// may shadow a persistent one" (#807 — shadowing was how the symptom presented,
// not the bug) and "was a flag value supplied?" computed from whether any later
// flag appeared on the line (#808). The precise rule each time turned out to be
// narrower and duller than the one suggested by the story of how the bug was
// found — here, "an ImageId handed to the EC2 API must come from
// RegisterTestAMI", which is the whole mechanism and needs no reference to skips
// at all.

// goTestFiles returns every _test.go file in the repo, excluding vendored and
// generated trees.
func goTestFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "docs-gen", "bin", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("found only %d test files; the walk has broken rather than the repo having shrunk",
			len(out))
	}
	return out
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // walking the repo's own tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
