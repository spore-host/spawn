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
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
