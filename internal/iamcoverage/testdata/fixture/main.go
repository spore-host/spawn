// Command fixture is the input to discover_test.go: one of each shape in which
// spawn reaches AWS, so the discovery is tested against code whose answer is
// known. It is type-checked by the test and never built or run.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// describer is a narrow interface over the client, the way spawn's packages
// take their clients.
type describer interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

func main() {
	ctx := context.Background()
	cfg := aws.Config{}
	c := ec2.NewFromConfig(cfg)

	direct(ctx, c)
	viaInterface(ctx, c)
	methodValue(ctx, c)
	paginate(ctx, c)
	wait(ctx, c)

	s3c := s3.NewFromConfig(cfg)
	_, _ = s3c.HeadBucket(ctx, &s3.HeadBucketInput{})
	_, _ = tm.New(s3c).UploadObject(ctx, &tm.UploadObjectInput{})

	stsc := sts.NewFromConfig(cfg)
	_ = stscreds.NewAssumeRoleProvider(stsc, "arn:aws:iam::123456789012:role/x")
	_, _ = stsc.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})

	sign(ctx, v4.NewSigner())
	signDynamic(ctx, v4.NewSigner(), os.Getenv("SERVICE"))

	feature(ctx, c)

	implied(ctx, cfg, c, s3c)
}

// queueCreator is a spawn-style narrow interface over the SQS client.
type queueCreator interface {
	CreateQueue(ctx context.Context, in *sqs.CreateQueueInput, optFns ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error)
}

// implied exercises impliedRules (#1377): each function below either sets the
// field a rule names, or provably does not, or builds its input where the
// analysis cannot see.
func implied(ctx context.Context, cfg aws.Config, c *ec2.Client, s3c *s3.Client) {
	q := sqs.NewFromConfig(cfg)
	queueTagged(ctx, q)
	queueUntagged(ctx, q)
	queueTaggedByAssignment(ctx, q, len(os.Args) > 1)
	queueFromParam(ctx, q, &sqs.CreateQueueInput{})
	queueFromParam(ctx, q, &sqs.CreateQueueInput{Tags: map[string]string{"k": "v"}})
	queueFromUntaggedParam(ctx, q, &sqs.CreateQueueInput{QueueName: aws.String("q")})
	queueUntaggedEitherWay(ctx, q, len(os.Args) > 1)
	queueTaggedViaInterface(ctx, q)
	queueMethodValue(ctx, q)
	bucketTagged(ctx, s3c)
	bucketUntagged(ctx, s3c)
	uploadTagged(ctx, s3c)
	runTagged(ctx, c)
}

func queueTagged(ctx context.Context, q *sqs.Client) {
	_, _ = q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q"), Tags: map[string]string{"k": "v"}})
}

func queueUntagged(ctx context.Context, q *sqs.Client) {
	_, _ = q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q")})
	_, _ = q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q"), Tags: nil})
}

func queueTaggedByAssignment(ctx context.Context, q *sqs.Client, tag bool) {
	in := &sqs.CreateQueueInput{QueueName: aws.String("q")}
	if tag {
		in.Tags = map[string]string{"k": "v"}
	}
	_, _ = q.CreateQueue(ctx, in)
}

// queueFromParam's input is built by its callers, one of which tags it.
func queueFromParam(ctx context.Context, q *sqs.Client, in *sqs.CreateQueueInput) {
	_, _ = q.CreateQueue(ctx, in)
}

// queueFromUntaggedParam's only caller builds its input without Tags.
func queueFromUntaggedParam(ctx context.Context, q *sqs.Client, in *sqs.CreateQueueInput) {
	_, _ = q.CreateQueue(ctx, in)
}

func queueUntaggedEitherWay(ctx context.Context, q *sqs.Client, fifo bool) {
	var in *sqs.CreateQueueInput
	if fifo {
		in = &sqs.CreateQueueInput{QueueName: aws.String("q.fifo")}
	} else {
		in = &sqs.CreateQueueInput{QueueName: aws.String("q")}
	}
	_, _ = q.CreateQueue(ctx, in)
}

func queueTaggedViaInterface(ctx context.Context, q queueCreator) {
	_, _ = q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q"), Tags: map[string]string{"k": "v"}})
}

// queueMethodValue calls through a method value, whose input the analysis
// does not follow: unknown, so tagged.
func queueMethodValue(ctx context.Context, q *sqs.Client) {
	create := q.CreateQueue
	_, _ = create(ctx, &sqs.CreateQueueInput{QueueName: aws.String("q")})
}

func bucketTagged(ctx context.Context, s3c *s3.Client) {
	_, _ = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{Tags: []s3types.Tag{{Key: aws.String("k")}}}})
}

func bucketUntagged(ctx context.Context, s3c *s3.Client) {
	_, _ = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraintEuWest1}})
}

func uploadTagged(ctx context.Context, s3c *s3.Client) {
	_, _ = tm.New(s3c).UploadObject(ctx, &tm.UploadObjectInput{Bucket: aws.String("b"), Tagging: aws.String("k=v")})
}

func runTagged(ctx context.Context, c *ec2.Client) {
	_, _ = c.RunInstances(ctx, &ec2.RunInstancesInput{TagSpecifications: []ec2types.TagSpecification{{}}})
}

func direct(ctx context.Context, c *ec2.Client) {
	_, _ = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{})
}

func viaInterface(ctx context.Context, d describer) {
	_, _ = d.DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
}

func methodValue(ctx context.Context, c *ec2.Client) {
	stop := c.StopInstances
	_, _ = stop(ctx, &ec2.StopInstancesInput{})
}

func paginate(ctx context.Context, c *ec2.Client) {
	p := ec2.NewDescribeVolumesPaginator(c, &ec2.DescribeVolumesInput{})
	for p.HasMorePages() {
		if _, err := p.NextPage(ctx); err != nil {
			return
		}
	}
}

func wait(ctx context.Context, c *ec2.Client) {
	_ = ec2.NewImageAvailableWaiter(c).Wait(ctx, &ec2.DescribeImagesInput{}, time.Minute)
}

func sign(ctx context.Context, s *v4.Signer) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	_ = s.SignHTTP(ctx, aws.Credentials{}, r, "", "lambda", "us-east-1", time.Now())
}

// signDynamic signs for a service the discovery cannot know, which it must
// report rather than guess.
func signDynamic(ctx context.Context, s *v4.Signer, service string) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	_ = s.SignHTTP(ctx, aws.Credentials{}, r, "", service, "us-east-1", time.Now())
}

// feature stands for a subtree that runs under a different role: the tests
// cut it.
func feature(ctx context.Context, c *ec2.Client) {
	_, _ = c.RebootInstances(ctx, &ec2.RebootInstancesInput{})
}

// neverCalled is unreachable, so its call must not be in any inventory.
func neverCalled(ctx context.Context, c *ec2.Client) {
	_, _ = c.DeleteVpc(ctx, &ec2.DeleteVpcInput{})
}
