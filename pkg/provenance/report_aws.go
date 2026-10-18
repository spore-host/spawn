package provenance

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/s3transfer"
)

// BuildReportFromConfig builds the report with real S3 clients. The bucket is
// read in its own region, and each input bucket in its own, since task inputs
// live in buckets spawn doesn't own, often elsewhere.
func BuildReportFromConfig(ctx context.Context, cfg aws.Config, opt ReportOptions) (*Report, error) {
	if opt.InputHeaderFor == nil {
		opt.InputHeaderFor = inputHeaders(cfg)
	}
	return BuildReport(ctx, s3.NewFromConfig(s3transfer.BucketRegionConfig(ctx, cfg, opt.Bucket)), opt)
}

func inputHeaders(cfg aws.Config) func(context.Context, string) InputHeader {
	var mu sync.Mutex
	clients := map[string]InputHeader{}
	return func(ctx context.Context, bucket string) InputHeader {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := clients[bucket]; ok {
			return c
		}
		c := s3.NewFromConfig(s3transfer.BucketRegionConfig(ctx, cfg, bucket))
		clients[bucket] = c
		return c
	}
}
