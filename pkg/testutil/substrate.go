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
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
	substrate "github.com/scttfrdmn/substrate/emulator"
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

// RegisterTestAMI creates a real AMI in the substrate emulator and returns its
// id, for tests that need to launch an instance.
//
// Why this exists. Emulator-backed tests across this repo passed a fabricated
// "ami-12345678" to RunInstances. That worked only because substrate v0.97.0
// did not validate image ids. v0.120.0 does — correctly, since real EC2 answers
// InvalidAMIID.NotFound — and the emulator ships with ZERO images, so there was
// no valid id to use at all. Bumping truffle to v0.58.0 pulled substrate
// v0.120.0 in transitively and broke 21 tests in one step.
//
// Registering an AMI is the fix rather than a workaround: it is what a caller
// would do against real EC2, so a test stops depending on how lenient the
// emulator happens to be. Same reasoning as truffle#177, which decoupled three
// of its own tests from emulator gaps instead of pinning the emulator back.
//
// Only for tests that actually call EC2. A pure-logic test should keep a literal
// id and stay independent of the emulator entirely.
func RegisterTestAMI(t *testing.T, ec2Client *ec2.Client) string {
	return registerTestAMI(t, ec2Client, aws.String("/dev/xvda"))
}

// RegisterTestAMIWithoutRootDevice registers an AMI with NO root device, so an
// instance launched from it has no EBS block device mappings.
//
// For the one test that genuinely needs an instance with no discoverable
// volumes (spawn#517's "a fallback must be distinguishable from a real
// measurement"). That test used to get the condition for free, because
// substrate v0.97.0 did not populate block device mappings at all — its own
// comment said so. v0.120.0 does, once a real AMI with a root device is used,
// so the scenario now has to be asked for explicitly rather than inherited from
// an emulator gap. Asking for it is better: the test says what it needs.
func RegisterTestAMIWithoutRootDevice(t *testing.T, ec2Client *ec2.Client) string {
	return registerTestAMI(t, ec2Client, nil)
}

func registerTestAMI(t *testing.T, ec2Client *ec2.Client, rootDevice *string) string {
	t.Helper()
	out, err := ec2Client.RegisterImage(context.Background(), &ec2.RegisterImageInput{
		Name:           aws.String("spawn-test-ami"),
		Architecture:   ec2types.ArchitectureValuesX8664,
		RootDeviceName: rootDevice,
	})
	if err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}
	id := aws.ToString(out.ImageId)
	if id == "" {
		t.Fatal("RegisterImage returned an empty image id")
	}
	return id
}
