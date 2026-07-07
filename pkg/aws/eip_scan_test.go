package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestGetInstanceElasticIP_Substrate exercises the real DescribeAddresses path
// (used by `spawn status`) end-to-end against the emulator: allocate an EIP,
// associate it to an instance, and confirm GetInstanceElasticIP surfaces it —
// and returns nil for an instance with no EIP. spawn never releases it (#262).
func TestGetInstanceElasticIP_Substrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	ec2c := env.EC2Client()

	// Launch an instance.
	run, err := ec2c.RunInstances(ctx, &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String("ami-12345678"),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	instID := *run.Instances[0].InstanceId

	// An instance with no EIP yields nil, no error.
	if eip, err := c.GetInstanceElasticIP(ctx, "us-east-1", instID); err != nil || eip != nil {
		t.Fatalf("expected no EIP, got eip=%v err=%v", eip, err)
	}

	// Allocate + associate an EIP.
	alloc, err := ec2c.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: ec2types.DomainTypeVpc})
	if err != nil {
		t.Fatalf("AllocateAddress: %v", err)
	}
	if _, err := ec2c.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: alloc.AllocationId,
		InstanceId:   aws.String(instID),
	}); err != nil {
		t.Fatalf("AssociateAddress: %v", err)
	}

	// Now it should be surfaced.
	eip, err := c.GetInstanceElasticIP(ctx, "us-east-1", instID)
	if err != nil {
		t.Fatalf("GetInstanceElasticIP: %v", err)
	}
	if eip == nil {
		t.Fatal("expected an EIP after associating one, got nil")
	}
	if eip.AllocationID != aws.ToString(alloc.AllocationId) {
		t.Errorf("AllocationID = %q, want %q", eip.AllocationID, aws.ToString(alloc.AllocationId))
	}
	if eip.PublicIP == "" {
		t.Error("expected a public IP on the surfaced EIP")
	}
}
