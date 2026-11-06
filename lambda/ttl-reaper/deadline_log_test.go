package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// #1524: the "unparseable ttl-deadline" line fired for every instance whose
// deadline parsed and was still in the future, because it sat after the parse
// block instead of in its else. A real unparseable value was then lost in the
// noise. It must fire for garbage and only for garbage, and the reap decision
// must be the same either way.
func TestEvaluate_UnparseableDeadlineLogOnlyForGarbage(t *testing.T) {
	now := time.Date(2026, 11, 9, 11, 59, 0, 0, time.UTC)
	const msg = "unparseable ttl-deadline"
	inst := func(deadline string, launchAgo time.Duration, extra ...ec2types.Tag) ec2types.Instance {
		tags := []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl-deadline", deadline),
			tag("spawn:launch-time", now.Add(-launchAgo).Format(time.RFC3339)),
		}
		return ec2types.Instance{InstanceId: aws.String("i-dl"), Tags: append(tags, extra...)}
	}

	cases := []struct {
		name       string
		inst       ec2types.Instance
		wantLog    bool
		wantReap   bool
		wantReason string
	}{
		{"valid future deadline", inst(now.Add(time.Minute).Format(time.RFC3339), time.Hour), false, false, ""},
		{"valid past deadline", inst(now.Add(-time.Hour).Format(time.RFC3339), 2*time.Hour), false, true, "ttl-deadline"},
		{"valid future deadline past max-age", inst(now.Add(time.Hour).Format(time.RFC3339), defaultMaxAge+time.Hour), false, true, "max-age"},
		{"garbage, young", inst("not-a-time", time.Hour), true, false, ""},
		{"garbage, past max-age", inst("not-a-time", defaultMaxAge+time.Hour), true, true, "max-age"},
		// A bad deadline still falls back to spawn:ttl: haveDeadline stays false.
		{"garbage, spawn:ttl expired", inst("2026-13-99", 2*time.Hour, tag("spawn:ttl", "1h")), true, true, "ttl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c candidate
			var expired bool
			out := captureLogs(func() { c, expired = newReaper().evaluate(tc.inst, "us-east-1", now) })
			if got := strings.Contains(out, msg); got != tc.wantLog {
				t.Errorf("log contains %q = %v, want %v; log:\n%s", msg, got, tc.wantLog, out)
			}
			if expired != tc.wantReap || c.reason != tc.wantReason {
				t.Errorf("decision = (%v, %q), want (%v, %q)", expired, c.reason, tc.wantReap, tc.wantReason)
			}
		})
	}
}
