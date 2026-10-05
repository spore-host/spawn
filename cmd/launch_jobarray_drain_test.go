package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestInstanceBelongsToJobArray is spawn#683.
//
// drainJobArray filtered on `in.Tags["spawn:job-array-id"]`, which is ALWAYS
// EMPTY. listInstancesInRegion lifts the spawn:* tags it recognises into named
// fields via a switch, and only the switch's `default` branch writes into the
// Tags map — so every recognised tag is absent from Tags by construction.
//
// The filter therefore compared "" against the cohort id for every instance and
// skipped all of them: the drain terminated NOTHING, ever. #671 made the drain
// run unconditionally, which was the right gating change, but the drain it was
// gating could not work.
//
// The fixture is the important part of this test. It mirrors what ListInstances
// was OBSERVED to return for two live instances — JobArrayID populated,
// Tags["spawn:job-array-id"] empty, 25 other tags present:
//
//	i-0b3f45f11da96cfe8 JobArrayID="repro682-20261004-1931e7"
//	                    Tags[spawn:job-array-id]="" len(Tags)=25
//
// A fixture that put the id in Tags would have passed against the broken code,
// which is exactly how this survived having a drain at all.
func TestInstanceBelongsToJobArray(t *testing.T) {
	const arrayID = "repro682-20261004-1931e7"

	realistic := aws.InstanceInfo{
		InstanceID: "i-0b3f45f11da96cfe8",
		State:      "running",
		JobArrayID: arrayID,
		// Deliberately NOT containing spawn:job-array-id — that is the point.
		Tags: map[string]string{
			"Name":          "repro682-0",
			"spawn:managed": "true",
		},
	}

	if !instanceBelongsToJobArray(realistic, arrayID) {
		t.Errorf("a member of the failed cohort was not matched, so the drain skips it and "+
			"it keeps billing. The id lives in InstanceInfo.JobArrayID; Tags never carries a "+
			"recognised spawn:* key. got JobArrayID=%q Tags[spawn:job-array-id]=%q",
			realistic.JobArrayID, realistic.Tags["spawn:job-array-id"])
	}

	// A different array's instance must not be touched: an over-broad drain
	// terminating someone else's cohort is worse than the leak.
	other := realistic
	other.JobArrayID = "someone-elses-array"
	if instanceBelongsToJobArray(other, arrayID) {
		t.Error("matched an instance from a DIFFERENT job array; the drain would terminate " +
			"an unrelated running cohort")
	}

	// An instance with no array id at all (a plain launch) must never match.
	plain := aws.InstanceInfo{InstanceID: "i-plain", Tags: map[string]string{}}
	if instanceBelongsToJobArray(plain, arrayID) {
		t.Error("matched a non-job-array instance; a plain `spawn launch` box must never be " +
			"caught by a cohort drain")
	}

	// And an empty cohort id must not match everything, which would make a drain
	// with a missing id terminate the whole account.
	if instanceBelongsToJobArray(realistic, "") {
		t.Error("an empty jobArrayID matched a real instance — that turns a drain into an " +
			"account-wide terminate")
	}
}

// TestNoCodeReadsRecognisedSpawnTagsFromTheTagsMap is the class gate for #683.
//
// The bug was not a typo, it was a false assumption about a data structure: that
// InstanceInfo.Tags holds every tag. It holds only the ones the extraction switch
// does NOT recognise. Any lookup of a recognised spawn:* key in that map silently
// yields "", and silence is the whole problem — the drain reported success while
// doing nothing.
//
// So rather than fixing one call site, fail the build for the pattern.
func TestNoCodeReadsRecognisedSpawnTagsFromTheTagsMap(t *testing.T) {
	// The keys listInstancesInRegion lifts into named fields. Reading any of
	// these from Tags always gives "".
	recognised := []string{
		"spawn:ttl", "spawn:idle-timeout",
		"spawn:job-array-id", "spawn:job-array-name", "spawn:job-array-index",
		"spawn:job-array-size",
		"spawn:sweep-id", "spawn:sweep-name", "spawn:sweep-index", "spawn:sweep-size",
	}

	pattern := regexp.MustCompile(`\.Tags\[\s*"(spawn:[a-z-]+)"\s*\]`)

	var offenders []string
	for _, dir := range []string{".", "../pkg"} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			// Line by line, skipping comments: the fix for this very bug
			// documents the bad pattern in a comment, and a gate that cannot
			// tell code from prose would flag its own explanation.
			for i, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				for _, m := range pattern.FindAllStringSubmatch(line, -1) {
					for _, r := range recognised {
						if m[1] == r {
							offenders = append(offenders, fmt.Sprintf("%s:%d: %s is always empty — use the named InstanceInfo field", path, i+1, m[0]))
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("code reads a recognised spawn:* tag from InstanceInfo.Tags, which "+
			"listInstancesInRegion never populates for recognised keys — the lookup always "+
			"returns \"\" and the branch silently does nothing (#683):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestAbandonedPGCleanupUsesTheRetryingDelete is the cmd-side half of spawn#685.
//
// The retry lives in pkg/aws and is tested there; what this guards is that the
// cleanup path actually CALLS it. A revert to the plain DeletePlacementGroup
// would still compile, still pass every pkg/aws test, and resume losing the race
// against asynchronous termination on every failed cohort.
func TestAbandonedPGCleanupUsesTheRetryingDelete(t *testing.T) {
	b, err := os.ReadFile("launch_jobarray.go")
	if err != nil {
		t.Fatalf("read launch_jobarray.go: %v", err)
	}
	src := string(b)

	start := strings.Index(src, "func cleanupAbandonedPGs(")
	if start < 0 {
		t.Fatal("cleanupAbandonedPGs not found; this gate would pass vacuously")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of cleanupAbandonedPGs")
	}
	body := src[start : start+end]

	if !strings.Contains(body, "DeletePlacementGroupWithRetry") {
		t.Error("cleanupAbandonedPGs does not use DeletePlacementGroupWithRetry — the " +
			"single-shot delete loses the race against asynchronous termination, which is #685")
	}
	// Match the non-retrying call specifically: `DeletePlacementGroup(` without
	// the WithRetry suffix.
	if regexp.MustCompile(`DeletePlacementGroup\(`).MatchString(body) {
		t.Error("cleanupAbandonedPGs still contains a bare DeletePlacementGroup( call")
	}
	// When it does give up, the operator's next step is a manual delete — and
	// they need the command, not only the complaint. ~35 of these were cleaned by
	// hand in one session.
	if !strings.Contains(body, "aws ec2 delete-placement-group --region") {
		t.Error("the give-up message does not name the command that finishes the job")
	}
}
