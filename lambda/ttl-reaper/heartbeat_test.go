package main

import (
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

func TestClassifyHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	hb := func(age time.Duration) map[string]string {
		return map[string]string{tagprefix.Tag("last-heartbeat"): now.Add(-age).Format(time.RFC3339)}
	}

	cases := []struct {
		name string
		tags map[string]string
		want heartbeatState
	}{
		{"no tag at all is unknown, not stale", map[string]string{}, heartbeatUnknown},
		{"empty value is unknown", map[string]string{tagprefix.Tag("last-heartbeat"): ""}, heartbeatUnknown},
		{"unparseable stamp is unknown, not stale", map[string]string{tagprefix.Tag("last-heartbeat"): "last tuesday"}, heartbeatUnknown},
		{"just written is fresh", hb(0), heartbeatFresh},
		{"one tick behind is fresh", hb(time.Minute), heartbeatFresh},
		{"exactly at the threshold is still fresh", hb(heartbeatStaleAfter), heartbeatFresh},
		{"past the threshold is stale", hb(heartbeatStaleAfter + time.Second), heartbeatStale},
		{"an hour behind is stale", hb(time.Hour), heartbeatStale},
		{"a future stamp is clock skew, not a symptom", hb(-5 * time.Minute), heartbeatFresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, age := classifyHeartbeat(tc.tags, now)
			if got != tc.want {
				t.Errorf("classifyHeartbeat = %v, want %v", got, tc.want)
			}
			// The age is part of the contract, not just a log detail: a negative
			// duration would read as "has not checked in for -5m0s" the moment a
			// future caller reports it on a skewed clock.
			if age < 0 {
				t.Errorf("age = %s, want >= 0", age)
			}
		})
	}
}

func TestClassifyHeartbeatReportsAge(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tags := map[string]string{
		tagprefix.Tag("last-heartbeat"): now.Add(-90 * time.Minute).Format(time.RFC3339),
	}
	state, age := classifyHeartbeat(tags, now)
	if state != heartbeatStale {
		t.Fatalf("state = %v, want stale", state)
	}
	// The age is what the operator reads to tell "wedged a minute ago" from
	// "dead since yesterday", so it must be the real lapse, not the threshold.
	if age != 90*time.Minute {
		t.Errorf("age = %s, want 90m", age)
	}
}

func hbInstance(id string, state ec2types.InstanceStateName, tags map[string]string) ec2types.Instance {
	t := make([]ec2types.Tag, 0, len(tags))
	for k, v := range tags {
		t = append(t, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return ec2types.Instance{
		InstanceId: awssdk.String(id),
		State:      &ec2types.InstanceState{Name: state},
		Tags:       t,
	}
}

func TestNoteHeartbeatCounts(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	managed := func(extra map[string]string) map[string]string {
		m := map[string]string{tagprefix.Tag("managed"): "true"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	stamp := func(age time.Duration) map[string]string {
		return map[string]string{tagprefix.Tag("last-heartbeat"): now.Add(-age).Format(time.RFC3339)}
	}

	r := &reaper{}
	var sum Summary
	insts := []ec2types.Instance{
		hbInstance("i-fresh", ec2types.InstanceStateNameRunning, managed(stamp(time.Minute))),
		hbInstance("i-stale", ec2types.InstanceStateNameRunning, managed(stamp(2*time.Hour))),
		hbInstance("i-notag", ec2types.InstanceStateNameRunning, managed(nil)),
		// Not running: a stopped instance is not expected to tick, and counting
		// it would make every stopped box look like a dead spored.
		hbInstance("i-stopped", ec2types.InstanceStateNameStopped, managed(stamp(2*time.Hour))),
		// Not spawn-managed: none of our business.
		hbInstance("i-foreign", ec2types.InstanceStateNameRunning, stamp(2*time.Hour)),
	}
	for _, inst := range insts {
		r.noteHeartbeat(inst, "us-east-1", "dev", now, &sum)
	}

	if sum.HeartbeatFresh != 1 {
		t.Errorf("HeartbeatFresh = %d, want 1", sum.HeartbeatFresh)
	}
	if sum.HeartbeatStale != 1 {
		t.Errorf("HeartbeatStale = %d, want 1", sum.HeartbeatStale)
	}
	if sum.HeartbeatUnknown != 1 {
		t.Errorf("HeartbeatUnknown = %d, want 1", sum.HeartbeatUnknown)
	}
}

// A stale heartbeat must NOT be a reap reason. The instance's lifetime is still
// governed by the deadline paths; terminating on a missed tick would kill a box
// whose workload is fine and whose spored is merely wedged.
func TestStaleHeartbeatDoesNotExpireAnInstance(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	inst := hbInstance("i-stale", ec2types.InstanceStateNameRunning, map[string]string{
		tagprefix.Tag("managed"):        "true",
		tagprefix.Tag("last-heartbeat"): now.Add(-6 * time.Hour).Format(time.RFC3339),
		tagprefix.Tag("ttl-deadline"):   now.Add(6 * time.Hour).Format(time.RFC3339),
	})
	inst.LaunchTime = awssdk.Time(now.Add(-time.Hour))

	r := &reaper{maxAge: 24 * time.Hour}
	var sum Summary
	r.noteHeartbeat(inst, "us-east-1", "dev", now, &sum)
	if sum.HeartbeatStale != 1 {
		t.Fatalf("precondition: HeartbeatStale = %d, want 1", sum.HeartbeatStale)
	}
	if _, expired := r.evaluate(inst, "us-east-1", now); expired {
		t.Error("stale heartbeat made the instance expire; it has 6h of TTL left")
	}
}
