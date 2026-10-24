package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestNoFlagGrantsTheStagingBucket: --s3-read/--s3-write on launch and sweep
// (validateS3BucketFlags) and on pool create (refuseStagingBucketGrants) refuse
// a spawn staging bucket, whatever its account or region (#1274), so no
// instance role spawn builds from flags can read or list other tasks' staged
// wrappers.
func TestNoFlagGrantsTheStagingBucket(t *testing.T) {
	staging := taskproto.StagingBucket("999999999999", "ap-south-1")
	for name, check := range map[string]func() error{
		"launch --s3-read":      func() error { return validateS3BucketFlags([]string{staging}, nil) },
		"launch --s3-write":     func() error { return validateS3BucketFlags(nil, []string{staging}) },
		"pool --s3-read":        func() error { return refuseStagingBucketGrants([]string{staging}, nil) },
		"pool --s3-write":       func() error { return refuseStagingBucketGrants(nil, []string{staging}) },
		"pool --s3-read as URI": func() error { return refuseStagingBucketGrants([]string{"s3://" + staging + "/k"}, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := check(); err == nil || !strings.Contains(err.Error(), "staging bucket") {
				t.Errorf("err = %v, want a refusal", err)
			}
		})
	}
	if err := validateS3BucketFlags([]string{"spawn-results-123456789012-us-east-1"}, []string{"my-bucket"}); err != nil {
		t.Errorf("ordinary buckets were refused: %v", err)
	}
	if err := refuseStagingBucketGrants([]string{"my-inputs"}, []string{"s3://my-outputs/x"}); err != nil {
		t.Errorf("ordinary buckets were refused: %v", err)
	}
}

// TestPoolWorkerPolicyNeverNamesTheStagingBucket: the pool worker's policy,
// whose spec bucket is the default results bucket, has no grant on the staging
// bucket, with or without extra buckets.
func TestPoolWorkerPolicyNeverNamesTheStagingBucket(t *testing.T) {
	for _, extra := range [][]string{nil, {"my-inputs", "spawn-results-123456789012-us-east-1"}} {
		pol := poolWorkerPolicy("123456789012", "us-east-1", poolSpecBucket("123456789012", "us-east-1"), "run1", extra, extra)
		if strings.Contains(pol, taskproto.StagingBucketPrefix) || strings.Contains(pol, "arn:aws:s3:::spawn-*") || strings.Contains(pol, `"*"`) {
			t.Errorf("the pool worker policy reaches the staging bucket:\n%s", pol)
		}
	}
}
