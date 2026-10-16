package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/sweep"
	"github.com/spore-host/spawn/pkg/testutil"
)

// `spawn collect-results` loads the infra config in us-east-1 and reads
// spawn-results-<account>-<region> for each of the sweep's regions. Real S3
// answers a request signed for us-east-1 against a bucket elsewhere with a 301,
// which collect only logged as a warning before printing "No results found".
// Each region's requests must be signed for that region.
func TestCollectFromMultipleRegionsSignsForEachBucketRegion(t *testing.T) {
	ctx := context.Background()
	const account = "123456789012"
	regions := []string{"eu-west-1", "ap-southeast-2"}

	env := testutil.SubstrateServer(t)
	if env.AWSConfig.Region != "us-east-1" {
		t.Fatalf("precondition: emulator config in %q, want us-east-1 (what collect loads)", env.AWSConfig.Region)
	}
	seed := s3.NewFromConfig(env.AWSConfig)
	for _, r := range regions {
		if _, err := seed.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(fmt.Sprintf("spawn-results-%s-%s", account, r))}); err != nil {
			t.Fatal(err)
		}
	}

	cfg, rec := testutil.RecordSigning(env.AWSConfig)
	rec2 := &sweep.SweepRecord{AWSAccountID: account}
	if _, err := collectFromMultipleRegions(ctx, cfg, rec2, regions, "sweep-x"); err != nil {
		t.Fatalf("collectFromMultipleRegions: %v", err)
	}

	reqs := rec.Requests("s3")
	seen := map[string]bool{}
	for _, r := range reqs {
		for _, region := range regions {
			if !strings.HasPrefix(r.Path, fmt.Sprintf("/spawn-results-%s-%s", account, region)) {
				continue
			}
			seen[region] = true
			if r.Region != region {
				t.Errorf("%s %s was signed for %q, but the bucket is in %q: real S3 answers that with a 301",
					r.Method, r.Path, r.Region, region)
			}
		}
	}
	for _, region := range regions {
		if !seen[region] {
			t.Errorf("no S3 request reached the %s results bucket; recorded %+v", region, reqs)
		}
	}
}
