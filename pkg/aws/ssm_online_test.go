package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// launchWithProfile launches a substrate instance with an IAM instance profile
// attached (the structural precondition for SSM registration). Skips the test if
// the emulator can't run instances.
func launchWithProfile(t *testing.T, ctx context.Context, env *testutil.TestEnv) string {
	t.Helper()
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
	return aws.ToString(run.Instances[0].InstanceId)
}

// TestWaitForSSMOnline_Online: a running instance WITH an instance profile is
// reported SSM-Online by the emulator, so WaitForSSMOnline returns nil promptly.
func TestWaitForSSMOnline_Online(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	id := launchWithProfile(t, ctx, env)
	if err := client.WaitForSSMOnline(ctx, "us-east-1", id, 30*time.Second); err != nil {
		t.Fatalf("WaitForSSMOnline(running instance w/ profile) = %v, want nil", err)
	}
}

// TestInstanceHasInstanceProfile_NoProfile: the structural "can this ever
// register with SSM?" probe reports false for an instance launched without a
// profile (the warm-AMI #98 field failure was exactly this — the seed had no
// profile, so SSM could never come Online). This is the decisive "dead vs slow"
// signal.
//
// NOTE: the full WaitForSSMOnline dead-path (not-Online + no-profile →
// ErrSSMUnreachable, fast) can't be exercised end-to-end here: the Substrate
// emulator reports every running instance as SSM PingStatus=Online and does not
// echo IamInstanceProfile in DescribeInstances, so it has no
// running-but-unregistered state to model. Tracked as a substrate gap; we test
// the decision input (this probe) directly instead.
func TestInstanceHasInstanceProfile_NoProfile(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
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

	has, err := client.instanceHasInstanceProfile(ctx, "us-east-1", id)
	if err != nil {
		t.Fatalf("instanceHasInstanceProfile = error %v", err)
	}
	if has {
		t.Error("instanceHasInstanceProfile = true for an instance launched without a profile, want false")
	}
}

// TestWaitForSSMOnline_Timeout: an unknown instance never registers, so the
// call times out (and respects a short deadline rather than hanging).
func TestWaitForSSMOnline_Timeout(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	start := time.Now()
	err := client.WaitForSSMOnline(context.Background(), "us-east-1", "i-doesnotexist", 2*time.Second)
	if err == nil {
		t.Fatal("expected timeout error for unknown instance")
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("WaitForSSMOnline overran its 2s timeout: %v", time.Since(start))
	}
}
