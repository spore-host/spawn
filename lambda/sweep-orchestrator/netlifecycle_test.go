package main

import (
	"os"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func tagValue(tags []ec2types.Tag, key string) (string, bool) {
	for _, t := range tags {
		if awssdk.ToString(t.Key) == key {
			return awssdk.ToString(t.Value), true
		}
	}
	return "", false
}

// TestSweepRowIsReapable is spawn#725.
//
// A detached sweep is the DEFAULT — launchParameterSweep auto-enables it — and
// these rows carried only spawn:sweep-id, spawn:sweep-index and Name. With no
// UserData there is no spored, so nothing in-instance enforces TTL, idle or
// cost; and with no spawn:managed tag the reaper could not act either, because
// its ec2:TerminateInstances grant is conditioned on
// ec2:ResourceTag/spawn:managed=true.
//
// So the rows could be stopped by NOTHING: no enforcement inside, no permission
// outside. That inverts the lifecycle invariant (#70/#72). This tag restores the
// backstop.
func TestSweepRowIsReapable(t *testing.T) {
	tags := sweepRowLifecycleTags(map[string]interface{}{}, time.Now())

	v, ok := tagValue(tags, "spawn:managed")
	if !ok {
		t.Fatal("no spawn:managed tag — the reaper's terminate is conditioned on exactly " +
			"this tag, so without it a detached sweep row cannot be stopped by anything")
	}
	if v != "true" {
		t.Errorf("spawn:managed = %q, want \"true\" — the condition is StringEquals, so any "+
			"other value is an implicitDeny and the instance is unreapable", v)
	}
}

// TestSweepRowCarriesTheTTLDeadlineWhenThereIsOne: the reaper prefers
// spawn:ttl-deadline over its own max-age ceiling, so passing the sweep's ttl
// through means a row is reclaimed near when the user asked rather than at the
// global ceiling.
func TestSweepRowCarriesTheTTLDeadlineWhenThereIsOne(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tags := sweepRowLifecycleTags(map[string]interface{}{"ttl": "45m"}, now)

	v, ok := tagValue(tags, "spawn:ttl-deadline")
	if !ok {
		t.Fatal("no spawn:ttl-deadline despite ttl=45m in the sweep's defaults — the CLI " +
			"already writes ttl there (applyCLISpendControlsToSweep) and it reaches this " +
			"Lambda, so there is nothing to plumb")
	}
	got, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("spawn:ttl-deadline = %q, which the reaper parses as RFC3339: %v", v, err)
	}
	if want := now.Add(45 * time.Minute); !got.Equal(want) {
		t.Errorf("deadline = %v, want %v", got, want)
	}
}

// TestSweepRowOmitsTheDeadlineWhenThereIsNoTTL: an absent deadline is correct
// rather than guessed. The reaper falls back to REAPER_MAX_AGE, which still
// bounds the instance — whereas a fabricated deadline would terminate work the
// user never put a clock on.
func TestSweepRowOmitsTheDeadlineWhenThereIsNoTTL(t *testing.T) {
	for _, cfg := range []map[string]interface{}{
		{},                        // no ttl at all
		{"ttl": ""},               // present but empty
		{"ttl": "not-a-duration"}, // unparseable
		{"ttl": "0s"},             // zero
	} {
		tags := sweepRowLifecycleTags(cfg, time.Now())
		if v, ok := tagValue(tags, "spawn:ttl-deadline"); ok {
			t.Errorf("config %v produced a deadline %q; a guessed deadline would terminate "+
				"work the user never put a clock on", cfg, v)
		}
		// managed must still be present — reapability does not depend on a ttl.
		if _, ok := tagValue(tags, "spawn:managed"); !ok {
			t.Errorf("config %v lost spawn:managed; the backstop must not depend on a ttl", cfg)
		}
	}
}

// TestBothLaunchPathsStampTheTags: the orchestrator has TWO RunInstances call
// sites — single-region and multi-region. Fixing one and not the other would
// leave the tags depending on which sweep shape a caller used, which is exactly
// the kind of half-fix that hides.
func TestBothLaunchPathsStampTheTags(t *testing.T) {
	src := readMainForTest(t)
	runs := strings.Count(src, "ec2types.ResourceTypeInstance")
	stamps := strings.Count(src, "sweepRowLifecycleTags(config,")
	if runs == 0 {
		t.Fatal("found no instance tag specifications; the matcher is stale")
	}
	if stamps != runs {
		t.Errorf("%d instance tag specification(s) but only %d call sweepRowLifecycleTags — "+
			"a row's reapability would depend on which launch path ran", runs, stamps)
	}
}

func readMainForTest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(b)
}

// TestCompletionStatusReflectsFailures is #749's worst half: this branch used to
// log "All instances launched and completed" and set COMPLETED unconditionally,
// so a sweep whose EVERY row was rejected by EC2 recorded itself as a success.
// For the whole life of the detached path that is what happened — no `ami` was
// seeded, so RunInstances was rejected every time — and the only evidence
// anywhere was the Lambda's own log.
//
// Asserts the decision table directly rather than through a run, because the
// inputs (launched/failed/total) are already the state the loop maintains.
func TestCompletionStatusReflectsFailures(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		launched, failed, total int
		want                    string
		why                     string
	}{
		{
			"every row rejected", 0, 2, 2, "FAILED",
			"a sweep that launched nothing is not completed — this is #749",
		},
		{
			"every row rejected, large sweep", 0, 200, 200, "FAILED",
			"scale does not change it",
		},
		{
			"partial failure", 1, 1, 2, "COMPLETED",
			"the sweep did run; the count makes the failures visible, and a new " +
				"status value would send every consumer down a default branch",
		},
		{
			"all launched", 2, 0, 2, "COMPLETED",
			"the ordinary case",
		},
		{
			// A zero-row sweep cannot be a failure: nothing was asked for.
			"nothing to launch", 0, 0, 0, "COMPLETED",
			"no rows, no failures",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := completionStatus(tc.launched, tc.failed)
			if got != tc.want {
				t.Errorf("completionStatus(launched=%d, failed=%d) = %q, want %q — %s",
					tc.launched, tc.failed, got, tc.want, tc.why)
			}
		})
	}
}

// FAILED must be an EXISTING status value, not a new one. `cancel` and `resume`
// both branch on `Status == "COMPLETED"`, and six workflow adapters read sweep
// records; a novel value would send all of them down a default path.
func TestCompletionStatusUsesOnlyKnownValues(t *testing.T) {
	known := map[string]bool{"COMPLETED": true, "FAILED": true}
	for _, pair := range [][2]int{{0, 1}, {1, 1}, {2, 0}, {0, 0}} {
		if s := completionStatus(pair[0], pair[1]); !known[s] {
			t.Errorf("completionStatus(%d, %d) = %q, which is not an existing sweep status",
				pair[0], pair[1], s)
		}
	}
}
