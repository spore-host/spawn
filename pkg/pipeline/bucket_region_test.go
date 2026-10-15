package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// A stage in one region must reach a pipeline bucket in another: the runner's
// S3 clients take the bucket's region, not the instance's. The fake answers
// HeadBucket the way S3 does, with x-amz-bucket-region.
func TestBucketRegionConfigUsesTheBucketsRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("X-Amz-Bucket-Region", "eu-west-2")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := aws.Config{
		Region:       "us-west-2",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  credentials.NewStaticCredentialsProvider("a", "b", ""),
	}
	got := bucketRegionConfig(context.Background(), cfg, "pipeline-bucket")
	if got.Region != "eu-west-2" {
		t.Errorf("region = %q, want the bucket's eu-west-2", got.Region)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("the caller's config was modified: %q", cfg.Region)
	}
}

// When the region cannot be learned the config is returned unchanged, so the
// next call reports the real error.
func TestBucketRegionConfigFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := aws.Config{Region: "us-west-2", BaseEndpoint: aws.String(srv.URL)}
	if got := bucketRegionConfig(context.Background(), cfg, "missing"); got.Region != "us-west-2" {
		t.Errorf("region = %q, want the unchanged us-west-2", got.Region)
	}
}

// A bucket in another region answers HeadBucket with a 301 that carries the
// region header. That error response is the common cross-region case, so the
// header must be read from it too, not only from a 200.
func TestBucketRegionConfigReadsTheRedirectsRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Bucket-Region", "ap-southeast-2")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()
	cfg := aws.Config{Region: "us-west-2", BaseEndpoint: aws.String(srv.URL)}
	if got := bucketRegionConfig(context.Background(), cfg, "elsewhere"); got.Region != "ap-southeast-2" {
		t.Errorf("region = %q, want the redirect's ap-southeast-2", got.Region)
	}
}
