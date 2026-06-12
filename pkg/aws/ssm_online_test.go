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

// TestWaitForSSMOnline_Online: a running instance is reported SSM-Online by the
// emulator, so WaitForSSMOnline returns nil promptly.
func TestWaitForSSMOnline_Online(t *testing.T) {
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

	if err := client.WaitForSSMOnline(ctx, "us-east-1", id, 30*time.Second); err != nil {
		t.Fatalf("WaitForSSMOnline(running instance) = %v, want nil", err)
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
