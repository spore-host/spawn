package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestVerifySporedReady_WaitsForSSMOnlineThenSucceeds is the #277-A regression:
// verifySporedReady must first wait for the SSM agent to REGISTER (Online) before
// sending `spored status`, otherwise SendCommand fails until the whole gate times
// out ("context deadline exceeded"). Against the emulator, an instance with an
// IAM profile is Online and SendCommand returns Success, so the gate passes.
func TestVerifySporedReady_WaitsForSSMOnlineThenSucceeds(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnclient.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	ec2c := ec2.NewFromConfig(env.AWSConfig)
	run, err := ec2c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String("ami-base"), InstanceType: ec2types.InstanceTypeM7iXlarge,
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String("spawn-spored-profile"),
		},
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	id := aws.ToString(run.Instances[0].InstanceId)

	if err := verifySporedReady(ctx, client, "us-east-1", id, 60*time.Second); err != nil {
		t.Fatalf("verifySporedReady(instance w/ profile, SSM online) = %v, want nil", err)
	}
}

// TestVerifySporedReady_NoProfileFailsFast: an instance with no IAM profile can
// never register with SSM, so the gate must fail FAST (via WaitForSSMOnline's
// ErrSSMUnreachable precheck) instead of burning the full timeout on SendCommand
// deadline-exceeded — the core #277-A improvement.
func TestVerifySporedReady_NoProfileFailsFast(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnclient.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	ec2c := ec2.NewFromConfig(env.AWSConfig)
	run, err := ec2c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String("ami-base"), InstanceType: ec2types.InstanceTypeM7iXlarge,
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	id := aws.ToString(run.Instances[0].InstanceId)

	start := time.Now()
	err = verifySporedReady(ctx, client, "us-east-1", id, 60*time.Second)
	if err == nil {
		t.Fatal("verifySporedReady(no profile) = nil, want an error (agent can never register)")
	}
	if !errors.Is(err, spawnclient.ErrSSMUnreachable) {
		t.Errorf("verifySporedReady(no profile) err = %v, want wrapped ErrSSMUnreachable", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("no-profile gate took %v — should fail fast, not wait out the 60s timeout", elapsed)
	}
}
