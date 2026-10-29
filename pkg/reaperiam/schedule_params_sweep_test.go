package reaperiam

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/schedulesweep"
)

// Gate (#1398): the params sweep's grants are exactly a bucket listing (names
// only), a list of the schedules buckets under schedules/ and nothing else,
// and tag reads and deletes on schedules/* in the schedules buckets. The
// resources are derived from the prefixes pkg/schedulesweep sweeps, so a grant
// wider than what the sweep touches, or narrower, fails.
func TestScheduleParamsSweepIsScopedToTheParams(t *testing.T) {
	bucketARN := "arn:aws:s3:::" + schedulesweep.BucketPrefix + "*"
	objectARN := bucketARN + "/" + schedulesweep.KeyPrefix + "*"
	if SchedulesBucketARN != bucketARN || ScheduleParamsARN != objectARN {
		t.Errorf("SchedulesBucketARN/ScheduleParamsARN = %q/%q, but the sweep works on %q/%q",
			SchedulesBucketARN, ScheduleParamsARN, bucketARN, objectARN)
	}
	listCond := canonicalJSON(t, map[string]map[string]string{"StringLike": {"s3:prefix": schedulesweep.KeyPrefix + "*"}})
	want := map[grant]bool{
		{Effect: "Allow", Action: "s3:ListAllMyBuckets", Resources: "*"}:                      true,
		{Effect: "Allow", Action: "s3:ListBucket", Resources: bucketARN, Condition: listCond}: true,
		{Effect: "Allow", Action: "s3:GetObjectTagging", Resources: objectARN}:                true,
		{Effect: "Allow", Action: "s3:DeleteObject", Resources: objectARN}:                    true,
	}
	got := grantsOf(t, ScheduleParamsSweepStatements())
	if strings.Join(sortedGrants(got), "|") != strings.Join(sortedGrants(want), "|") {
		t.Errorf("ScheduleParamsSweepStatements grants\n  %s\nwant exactly\n  %s",
			strings.Join(sortedGrants(got), "\n  "), strings.Join(sortedGrants(want), "\n  "))
	}

	// Nothing the reaper holds on its own credentials deletes outside the
	// params prefix, or deletes a bucket.
	for g := range scheduleGrants(t) {
		if strings.HasPrefix(g.Action, "s3:Delete") && g.Resources != objectARN {
			t.Errorf("own-account grant %s deletes outside %s", g, objectARN)
		}
	}
}
