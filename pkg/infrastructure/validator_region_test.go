package infrastructure

import (
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// `spawn validate --infrastructure --region eu-west-1` resolves
// spawn-binaries-eu-west-1 and spawn-schedules-eu-west-1, but hands the
// validator a config in the ambient region (us-east-1 here). Real S3 answers a
// HeadBucket signed for the wrong region with a 301, which the validator
// reported as "Error accessing S3 bucket". Every S3 request must be signed for
// the region the buckets are named for.
func TestValidateS3BucketsSignsForTheBucketRegion(t *testing.T) {
	const bucketRegion = "eu-west-1"
	env := testutil.SubstrateServer(t)
	if env.AWSConfig.Region == bucketRegion {
		t.Fatalf("precondition: the emulator config must be in a region other than %s", bucketRegion)
	}
	cfg, rec := testutil.RecordSigning(env.AWSConfig)

	v := NewValidator(NewResolver(defaultInfraConfig(), bucketRegion, testAccountID), cfg)
	result := &ValidationResult{Resources: map[string]ResourceStatus{}}
	v.validateS3Buckets(context.Background(), result)

	reqs := rec.Requests("s3")
	if len(reqs) == 0 {
		t.Fatal("recorded no S3 requests; the recording client is not in the path")
	}
	for _, r := range reqs {
		if !strings.HasSuffix(strings.Trim(r.Path, "/"), "-"+bucketRegion) {
			t.Errorf("unexpected S3 request %s %s", r.Method, r.Path)
		}
		if r.Region != bucketRegion {
			t.Errorf("%s %s was signed for %q, but the bucket is in %q: real S3 answers that with a 301",
				r.Method, r.Path, r.Region, bucketRegion)
		}
	}
}
