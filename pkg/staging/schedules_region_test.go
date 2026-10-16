package staging

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// `spawn schedule create --region eu-west-1` hands UploadScheduleParams a config
// loaded in us-east-1 (the infra account's home region) and asks for the
// spawn-schedules-eu-west-1 bucket. Against real S3 a us-east-1 client cannot
// work that bucket: CreateBucket's LocationConstraint is rejected by a
// mismatched endpoint, and HeadBucket / PutObject on a bucket elsewhere come
// back 301 / PermanentRedirect. Each test below starts from exactly that
// config: us-east-1, with a different bucket region requested.

const (
	configRegion = "us-east-1"
	bucketRegion = "eu-west-1"
)

func writeParamsFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "params.yaml")
	if err := os.WriteFile(p, []byte("defaults:\n  instance_type: t3.micro\nparams:\n  - name: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every S3 request UploadScheduleParams makes for a bucket in eu-west-1 must be
// signed for eu-west-1, whatever region the caller's config was loaded in.
func TestUploadScheduleParamsSignsForTheBucketRegion(t *testing.T) {
	env := testutil.SubstrateServer(t)
	if env.AWSConfig.Region != configRegion {
		t.Fatalf("precondition: the emulator config is in %q, want %q", env.AWSConfig.Region, configRegion)
	}
	cfg, rec := testutil.RecordSigning(env.AWSConfig)

	c := NewClient(cfg, "123456789012")
	bucket, _, _, _, err := c.UploadScheduleParams(context.Background(), writeParamsFile(t), "sched-1", bucketRegion)
	if err != nil {
		t.Fatalf("UploadScheduleParams: %v", err)
	}
	if bucket != "spawn-schedules-"+bucketRegion {
		t.Errorf("bucket = %q, want spawn-schedules-%s", bucket, bucketRegion)
	}

	reqs := rec.Requests("s3")
	if len(reqs) == 0 {
		t.Fatal("recorded no S3 requests; the recording client is not in the path")
	}
	sawPut := false
	for _, r := range reqs {
		if r.Region != bucketRegion {
			t.Errorf("%s %s was signed for %q, but the bucket is in %q: real S3 answers that with a redirect",
				r.Method, r.Path, r.Region, bucketRegion)
		}
		if r.Method == http.MethodPut && r.Path == "/"+bucket+"/schedules/sched-1/params.yaml" {
			sawPut = true
		}
	}
	if !sawPut {
		t.Errorf("no PutObject for the params file among %d recorded requests: %+v", len(reqs), reqs)
	}
}

// The emulator records the region a bucket was created in from the request's
// signing region, so a us-east-1 client creating spawn-schedules-eu-west-1 is
// visible here as a bucket that lives in the wrong region.
func TestUploadScheduleParamsCreatesTheBucketInItsRegion(t *testing.T) {
	ctx := context.Background()
	env := testutil.SubstrateServer(t)
	c := NewClient(env.AWSConfig, "123456789012")

	bucket, _, _, _, err := c.UploadScheduleParams(ctx, writeParamsFile(t), "sched-2", bucketRegion)
	if err != nil {
		t.Fatalf("UploadScheduleParams: %v", err)
	}

	// The emulator only reports BucketRegion on a filtered listing, so ask
	// which region holds the bucket by filtering on each candidate.
	inRegion := func(region string) bool {
		out, err := s3.NewFromConfig(env.AWSConfig).ListBuckets(ctx, &s3.ListBucketsInput{BucketRegion: aws.String(region)})
		if err != nil {
			t.Fatalf("ListBuckets(bucket-region=%s): %v", region, err)
		}
		for _, b := range out.Buckets {
			if aws.ToString(b.Name) == bucket {
				return true
			}
		}
		return false
	}
	if !inRegion(bucketRegion) {
		where := "neither region"
		if inRegion(configRegion) {
			where = configRegion + ", the caller's config region"
		}
		t.Errorf("%s was not created in %s; it is in %s", bucket, bucketRegion, where)
	}
}
