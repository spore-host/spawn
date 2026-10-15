package pipeline

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// loadAWSConfig is the AWS config every pkg/pipeline client is built from: the
// default chain, with the instance's own region from IMDS as the fallback when
// nothing else sets one (#906).
//
// The stage runner (`spored run-pipeline-stage`) runs on an EC2 instance, which
// carries no AWS_REGION, and the SDK does not consult IMDS for the region unless
// asked — so a bare LoadDefaultConfig left the S3 and EC2 clients with no region
// and every call failed with "Missing Region", as the queue runner's did (#899).
// An explicit AWS_REGION or profile region still wins; IMDS is consulted only
// when neither is set. Off EC2, with no region configured, the IMDS lookup fails
// and the config is returned regionless, exactly as before.
func loadAWSConfig(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithEC2IMDSRegion())
}
