package s3transfer

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Moved here from pkg/pipeline so every caller that reaches a bucket in another
// region (pipeline stages, `spawn results retention` reading task inputs) uses
// one implementation.

// BucketRegionConfig returns cfg in bucket's region. BucketRegion is an
// unsigned HeadBucket, so it needs no IAM grant; if it cannot answer, cfg is
// returned unchanged and the call that follows reports the real error.
func BucketRegionConfig(ctx context.Context, cfg aws.Config, bucket string) aws.Config {
	if bucket == "" {
		return cfg
	}
	region := BucketRegion(ctx, s3.NewFromConfig(cfg), bucket)
	if region == "" || region == cfg.Region {
		return cfg
	}
	out := cfg.Copy()
	out.Region = region
	return out
}

// BucketRegion is an unsigned HeadBucket that returns the bucket's
// x-amz-bucket-region header, or "" if the response carried none. It stands in
// for the deprecated feature/s3/manager.GetBucketRegion, which pkg/s3transfer
// replaced (#932). The header is read from error responses too: S3 sends it on
// the 301 for a bucket in another region and on a 403.
func BucketRegion(ctx context.Context, client *s3.Client, bucket string) string {
	var region string
	capture := middleware.DeserializeMiddlewareFunc("CaptureBucketRegion",
		func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
			out, md, err := next.HandleDeserialize(ctx, in)
			if resp, ok := out.RawResponse.(*smithyhttp.Response); ok && resp != nil {
				region = resp.Header.Get("X-Amz-Bucket-Region")
			}
			return out, md, err
		})
	// The error is deliberately ignored: a 301 or 403 still carries the header,
	// and the capture sits innermost in the deserialize step, beneath the
	// middleware that turns those statuses into errors.
	_, _ = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, func(o *s3.Options) {
		o.Credentials = nil
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Deserialize.Add(capture, middleware.After)
		})
	})
	return region
}
