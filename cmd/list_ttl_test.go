package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestTTLRemaining pins the TTL column's meaning (#898): time LEFT before the
// termination deadline, not the TTL the instance was launched with.
func TestTTLRemaining(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	launched7mAgo := now.Add(-7 * time.Minute)
	cases := []struct {
		name string
		inst aws.InstanceInfo
		want string
	}{
		{
			// The issue's exact case: --ttl 1h, 7 minutes old → 53m, not "1h".
			"deadline tag, 7m into a 1h TTL",
			aws.InstanceInfo{State: "running", TTL: "1h", LaunchTime: launched7mAgo,
				Tags: map[string]string{"spawn:ttl-deadline": launched7mAgo.Add(time.Hour).Format(time.RFC3339)}},
			"53m",
		},
		{
			"no deadline tag: launch time + TTL",
			aws.InstanceInfo{State: "running", TTL: "1h", LaunchTime: launched7mAgo},
			"53m",
		},
		{
			// `spawn extend` moves the tag; the tag must beat launch+TTL.
			"extended deadline tag wins over launch + TTL",
			aws.InstanceInfo{State: "running", TTL: "1h", LaunchTime: launched7mAgo,
				Tags: map[string]string{"spawn:ttl-deadline": now.Add(3 * time.Hour).Format(time.RFC3339)}},
			"3h",
		},
		{
			"no TTL at all",
			aws.InstanceInfo{State: "running", LaunchTime: launched7mAgo},
			"none",
		},
		{
			"deadline passed",
			aws.InstanceInfo{State: "running", TTL: "1h", LaunchTime: now.Add(-2 * time.Hour)},
			"expired",
		},
		{
			"stopped instance still counts down",
			aws.InstanceInfo{State: "stopped", TTL: "1h", LaunchTime: launched7mAgo},
			"53m",
		},
		{
			"terminated instance has nothing left to count",
			aws.InstanceInfo{State: "terminated", TTL: "1h", LaunchTime: launched7mAgo},
			"-",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ttlRemaining(c.inst, now); got != c.want {
				t.Errorf("ttlRemaining = %q, want %q", got, c.want)
			}
		})
	}
}

// TestListJSONKeepsConfiguredTTL: -o json is machine-readable, so "ttl" keeps
// meaning the configured TTL; the remaining time is a new field beside it.
func TestListJSONKeepsConfiguredTTL(t *testing.T) {
	launched := time.Now().Add(-7 * time.Minute)
	deadline := launched.Add(time.Hour).UTC().Truncate(time.Second)
	insts := []aws.InstanceInfo{
		{InstanceID: "i-ttl", State: "running", TTL: "1h", LaunchTime: launched,
			Tags: map[string]string{"spawn:ttl-deadline": deadline.Format(time.RFC3339)}},
		{InstanceID: "i-none", State: "running", LaunchTime: launched},
	}
	out := captureStdout(t, func() {
		if err := outputJSON(insts); err != nil {
			t.Fatal(err)
		}
	})
	var got []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got[0]["ttl"] != "1h" {
		t.Errorf("ttl = %v, want the configured \"1h\"", got[0]["ttl"])
	}
	if got[0]["ttl_deadline"] != deadline.Format(time.RFC3339) {
		t.Errorf("ttl_deadline = %v, want %s", got[0]["ttl_deadline"], deadline.Format(time.RFC3339))
	}
	secs, ok := got[0]["ttl_remaining_seconds"].(float64)
	if !ok || secs < 52*60 || secs > 53*60+5 {
		t.Errorf("ttl_remaining_seconds = %v, want about 53m", got[0]["ttl_remaining_seconds"])
	}
	if v, present := got[1]["ttl_remaining_seconds"]; !present || v != nil {
		t.Errorf("no-TTL instance: ttl_remaining_seconds = %v (present=%v), want null", v, present)
	}
}
