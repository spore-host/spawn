package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// cliTags is a member's tags as the CLI ships them (encodeDetachedTags): the
// baseline buildTags always writes, rendered at some earlier instant, plus
// extra.
func cliTags(extra map[string]string) map[string]interface{} {
	m := map[string]interface{}{
		"spawn:managed":     "true",
		"spawn:root":        "true",
		"spawn:created-by":  "spawn",
		"spawn:launch-time": "2026-01-01T00:00:00Z",
		"Name":              "sw-3",
		"spawn:sweep-id":    "scheduled",
		"spawn:sweep-name":  "sw",
		"spawn:sweep-size":  "4",
		"spawn:sweep-index": "3",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func mustMemberTags(t *testing.T, config map[string]interface{}, now time.Time) []ec2types.Tag {
	t.Helper()
	tags, err := memberTags(&SweepRecord{SweepID: "sw-20261026", SweepName: "sw"}, config, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

// TestSweepRowIsReapable is spawn#725: spawn:managed=true is the rail the TTL
// reaper's terminate is conditioned on (ec2:ResourceTag/spawn:managed), so a
// member without it can be stopped by nothing out of band. The Lambda owns it:
// a member carries it whatever the shipped tags say.
func TestSweepRowIsReapable(t *testing.T) {
	missing := cliTags(nil)
	delete(missing, "spawn:managed")
	for name, shipped := range map[string]map[string]interface{}{
		"absent": missing, "false": cliTags(map[string]string{"spawn:managed": "false"}),
	} {
		v, ok := tagValue(mustMemberTags(t, map[string]interface{}{tagsParam: shipped}, time.Now()), "spawn:managed")
		if !ok || v != "true" {
			t.Errorf("shipped spawn:managed %s: got %q (present %v), want \"true\": the reaper's StringEquals "+
				"condition leaves any other value unreapable", name, v, ok)
		}
	}
}

// TestMemberTTLCountsFromTheRealLaunch is #1330's deadline half: a member with
// a ttl carries spawn:ttl-deadline, measured from when this Lambda launched it,
// not from when the CLI rendered its tags (a scheduled member launches days
// later), and its spawn:launch-time is that same instant. Day units parse as
// the CLI's do (#990).
func TestMemberTTLCountsFromTheRealLaunch(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for ttl, want := range map[string]time.Duration{"45m": 45 * time.Minute, "24h0m0s": 24 * time.Hour, "1d": 24 * time.Hour, "2d12h": 60 * time.Hour} {
		shipped := cliTags(map[string]string{"spawn:ttl": ttl, "spawn:ttl-deadline": "2026-01-01T00:45:00Z"})
		tags := mustMemberTags(t, map[string]interface{}{tagsParam: shipped}, now)
		v, ok := tagValue(tags, "spawn:ttl-deadline")
		if !ok {
			t.Errorf("ttl %q: no spawn:ttl-deadline", ttl)
			continue
		}
		if got, err := time.Parse(time.RFC3339, v); err != nil || !got.Equal(now.Add(want)) {
			t.Errorf("ttl %q: deadline %q, want %v (the launch plus the ttl)", ttl, v, now.Add(want))
		}
		if lt, _ := tagValue(tags, "spawn:launch-time"); lt != now.Format(time.RFC3339) {
			t.Errorf("ttl %q: spawn:launch-time %q, want the real launch %s", ttl, lt, now.Format(time.RFC3339))
		}
	}

	// A ttl the deadline cannot be derived from refuses the member: it would
	// otherwise launch with no deadline.
	_, err := memberTags(&SweepRecord{SweepID: "s"}, map[string]interface{}{tagsParam: cliTags(map[string]string{"spawn:ttl": "soon"})}, 0, now)
	if err == nil {
		t.Error("a member whose spawn:ttl cannot be parsed was given tags; it would run with no deadline")
	}

	// No ttl, no deadline: one would terminate work the user put no clock on.
	// (The CLI gives such a member the 1h idle default, in its shipped tags.)
	tags := mustMemberTags(t, map[string]interface{}{tagsParam: cliTags(map[string]string{"spawn:idle-timeout": "1h"})}, now)
	if v, ok := tagValue(tags, "spawn:ttl-deadline"); ok {
		t.Errorf("a member with no ttl got a deadline %q", v)
	}
	if v, _ := tagValue(tags, "spawn:idle-timeout"); v != "1h" {
		t.Errorf("spawn:idle-timeout = %q, want the CLI's 1h", v)
	}
}

// TestMemberLaunchesWithTheCLIsTags is #1330's Lambda half: RunInstances tags
// the instance and its volumes with exactly the CLI's rendered set, re-anchored
// (spawnaws.AnchorLaunchTags), with the sweep identity this Lambda owns. The
// CLI half (cmd/launch_sweep_detached_bootstrap_test.go) proves the rendered
// set is what a foreground launch of the member writes.
func TestMemberLaunchesWithTheCLIsTags(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	shipped := cliTags(map[string]string{
		"spawn:command": "python train.py", "spawn:param:alpha": "0.1", "spawn:cost-limit": "8.0000",
		"spawn:on-complete": "terminate", "spawn:ttl": "2h", "spawn:ttl-deadline": "2026-01-01T02:00:00Z",
	})
	state := &SweepRecord{SweepID: "sw-20261026", SweepName: "sw"}
	in, err := buildRunInstancesInput(state, map[string]interface{}{userDataParam: "x", tagsParam: shipped, launchSettingsParam: cliSettings(t, nil)}, 3, "t3.micro", now)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{}
	for k, v := range shipped {
		want[k] = v.(string)
	}
	want["spawn:sweep-id"] = "sw-20261026" // a schedule's run gets its ID when it fires
	want["spawn:launch-time"] = now.Format(time.RFC3339)
	want["spawn:ttl-deadline"] = now.Add(2 * time.Hour).Format(time.RFC3339)

	specs := map[ec2types.ResourceType]bool{}
	for _, spec := range in.TagSpecifications {
		specs[spec.ResourceType] = true
		got := map[string]string{}
		for _, tg := range spec.Tags {
			if _, dup := got[awssdk.ToString(tg.Key)]; dup {
				t.Errorf("%s: duplicate tag %s, which fails RunInstances", spec.ResourceType, awssdk.ToString(tg.Key))
			}
			got[awssdk.ToString(tg.Key)] = awssdk.ToString(tg.Value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s tags =\n  %v\nwant\n  %v", spec.ResourceType, got, want)
		}
	}
	if !specs[ec2types.ResourceTypeInstance] || !specs[ec2types.ResourceTypeVolume] {
		t.Errorf("tag specifications cover %v, want the instance and its volumes, as a foreground launch", specs)
	}

	// No rendered tags: refused, as for a missing bootstrap.
	in, err = buildRunInstancesInput(state, map[string]interface{}{userDataParam: "x"}, 0, "t3.micro", now)
	if err == nil || in != nil || !strings.Contains(err.Error(), tagsParam) {
		t.Errorf("a member with no rendered tags: input=%v err=%v, want a refusal naming %s", in, err, tagsParam)
	}
}

// readMainForTest returns every non-test Go source of the orchestrator,
// concatenated: the launch input is built in spored.go, the calls are in main.go.
func readMainForTest(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(src)
	}
	if b.Len() == 0 {
		t.Fatal("found no orchestrator sources")
	}
	return b.String()
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

// A member whose shipped tags would not fit in AWS's 50-tag limit is refused
// before RunInstances, which would reject it (#1336). The CLI refuses it first;
// this is the orchestrator's own backstop. At the limit it launches.
func TestMemberOverTheTagLimitIsRefused(t *testing.T) {
	now := time.Now()
	shipped := cliTags(nil)
	for i := 0; len(shipped) < maxInstanceTags; i++ {
		shipped[fmt.Sprintf("user%02d", i)] = "v"
	}
	if tags := mustMemberTags(t, map[string]interface{}{tagsParam: shipped}, now); len(tags) != maxInstanceTags {
		t.Fatalf("a member at the limit got %d tags, want %d", len(tags), maxInstanceTags)
	}
	shipped["one-more"] = "v"
	_, err := memberTags(&SweepRecord{SweepID: "s", SweepName: "sw"}, map[string]interface{}{tagsParam: shipped}, 0, now)
	if err == nil || !strings.Contains(err.Error(), "51 instance tags") {
		t.Fatalf("a member with 51 tags: err=%v, want a refusal naming the count", err)
	}
}
