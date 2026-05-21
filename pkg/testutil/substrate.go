package testutil

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	substrate "github.com/scttfrdmn/substrate"
)

// TestEnv holds a running Substrate server and a pre-configured AWS config
// that points all SDK calls at the emulator.
type TestEnv struct {
	// URL is the base URL of the Substrate server.
	URL string
	// AWSConfig is a pre-configured aws.Config pointing at the Substrate server.
	AWSConfig aws.Config
}

// SubstrateServer starts a Substrate emulator and returns a TestEnv.
// The server is shut down automatically when the test ends.
func SubstrateServer(t *testing.T) *TestEnv {
	t.Helper()
	ts := substrate.StartTestServer(t)

	cfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test"),
		),
	)
	if err != nil {
		t.Fatalf("SubstrateServer: build AWS config: %v", err)
	}

	return &TestEnv{URL: ts.URL, AWSConfig: cfg}
}

// EC2Client returns an EC2 client pointed at the Substrate server.
func (e *TestEnv) EC2Client() *ec2.Client {
	return ec2.NewFromConfig(e.AWSConfig)
}

// DynamoClient returns a DynamoDB client pointed at the Substrate server.
func (e *TestEnv) DynamoClient() *dynamodb.Client {
	return dynamodb.NewFromConfig(e.AWSConfig)
}

// S3Client returns an S3 client pointed at the Substrate server.
func (e *TestEnv) S3Client() *s3.Client {
	return s3.NewFromConfig(e.AWSConfig)
}

// SQSClient returns an SQS client pointed at the Substrate server.
func (e *TestEnv) SQSClient() *sqs.Client {
	return sqs.NewFromConfig(e.AWSConfig)
}

// SchedulerClient returns an EventBridge Scheduler client pointed at the Substrate server.
func (e *TestEnv) SchedulerClient() *scheduler.Client {
	return scheduler.NewFromConfig(e.AWSConfig)
}

// STSClient returns an STS client pointed at the Substrate server.
func (e *TestEnv) STSClient() *sts.Client {
	return sts.NewFromConfig(e.AWSConfig)
}

// KMSClient returns a KMS client pointed at the Substrate server.
func (e *TestEnv) KMSClient() *kms.Client {
	return kms.NewFromConfig(e.AWSConfig)
}

// CloudWatchClient returns a CloudWatch client pointed at the Substrate server.
func (e *TestEnv) CloudWatchClient() *cloudwatch.Client {
	return cloudwatch.NewFromConfig(e.AWSConfig)
}

// SNSClient returns an SNS client pointed at the Substrate server.
func (e *TestEnv) SNSClient() *sns.Client {
	return sns.NewFromConfig(e.AWSConfig)
}

// SSMClient returns an SSM client pointed at the Substrate server.
func (e *TestEnv) SSMClient() *ssm.Client {
	return ssm.NewFromConfig(e.AWSConfig)
}

// IAMClient returns an IAM client pointed at the Substrate server.
func (e *TestEnv) IAMClient() *iam.Client {
	return iam.NewFromConfig(e.AWSConfig)
}

// LambdaClient returns a Lambda client pointed at the Substrate server.
func (e *TestEnv) LambdaClient() *lambda.Client {
	return lambda.NewFromConfig(e.AWSConfig)
}

// EFSClient returns an EFS client pointed at the Substrate server.
func (e *TestEnv) EFSClient() *efs.Client {
	return efs.NewFromConfig(e.AWSConfig)
}

// FSxClient returns an FSx client pointed at the Substrate server.
func (e *TestEnv) FSxClient() *fsx.Client {
	return fsx.NewFromConfig(e.AWSConfig)
}

// Route53Client returns a Route53 client pointed at the Substrate server.
func (e *TestEnv) Route53Client() *route53.Client {
	return route53.NewFromConfig(e.AWSConfig)
}

// CloudWatchLogsClient returns a CloudWatch Logs client pointed at the Substrate server.
func (e *TestEnv) CloudWatchLogsClient() *cloudwatchlogs.Client {
	return cloudwatchlogs.NewFromConfig(e.AWSConfig)
}
