package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestSnapshotDeletionDecision covers the pure retain-shared-snapshots logic:
// delete only when a snapshot is exclusive and we could confirm it.
func TestSnapshotDeletionDecision(t *testing.T) {
	tests := []struct {
		name       string
		others     []string
		lookupErr  error
		wantDelete bool
		reasonSub  string // substring expected in the retain reason (when not deleting)
	}{
		{name: "exclusive → delete", others: nil, lookupErr: nil, wantDelete: true},
		{name: "shared → retain", others: []string{"ami-aaa", "ami-bbb"}, lookupErr: nil, wantDelete: false, reasonSub: "still used by ami-aaa, ami-bbb"},
		{name: "lookup error → retain (safe)", others: nil, lookupErr: errors.New("throttled"), wantDelete: false, reasonSub: "could not verify"},
		{name: "lookup error wins over empty others", others: []string{}, lookupErr: errors.New("boom"), wantDelete: false, reasonSub: "could not verify"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			del, reason := snapshotDeletionDecision(tt.others, tt.lookupErr)
			if del != tt.wantDelete {
				t.Fatalf("delete=%v, want %v (reason %q)", del, tt.wantDelete, reason)
			}
			if !del && !strings.Contains(reason, tt.reasonSub) {
				t.Errorf("reason %q does not contain %q", reason, tt.reasonSub)
			}
			if del && reason != "" {
				t.Errorf("delete=true should have empty reason, got %q", reason)
			}
		})
	}
}

// TestDeleteAMI_NotFound: deleting a non-existent AMI is a clean error, not a
// panic, and never touches Image Builder.
func TestDeleteAMI_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	_, err := client.DeleteAMI(context.Background(), "us-east-1", "ami-does-not-exist")
	if err == nil {
		t.Fatal("expected an error deleting a non-existent AMI")
	}
}

// TestDeleteAMI_NonImageBuilder_NoIBCall is the answer to "does delete fail on a
// missing Image Builder resource for a non-IB AMI?" — it must NOT. We register
// an AMI WITHOUT the Ec2ImageBuilderArn tag (via CreateImage on a substrate
// instance), then delete it: the result must report no ImageBuilderArn and no
// ImageBuilderError (the IB DeleteImage call is skipped entirely).
func TestDeleteAMI_NonImageBuilder_NoIBCall(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	// Need an instance to CreateImage from (substrate models RunInstances).
	run, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-base"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	instID := aws.ToString(run.Instances[0].InstanceId)

	amiID, err := client.CreateAMI(ctx, "us-east-1", CreateAMIInput{
		InstanceID: instID,
		Name:       "non-ib-test-ami",
		Tags:       map[string]string{"spawn:managed": "true"}, // NO Ec2ImageBuilderArn
	})
	if err != nil {
		t.Skipf("substrate CreateImage unavailable: %v", err)
	}

	res, err := client.DeleteAMI(ctx, "us-east-1", amiID)
	if res == nil {
		t.Fatalf("expected a result, got nil (err=%v)", err)
	}
	if res.ImageBuilderArn != "" {
		t.Errorf("non-IB AMI must have no ImageBuilderArn, got %q", res.ImageBuilderArn)
	}
	if res.ImageBuilderError != "" {
		t.Errorf("non-IB AMI must not attempt (or fail) IB deletion, got error %q", res.ImageBuilderError)
	}
}

// TestDeleteAMI_RetainsSharedSnapshot is the end-to-end shared-snapshot path
// (unblocked by substrate#322/#328 in v0.70.0): AMI-A owns a snapshot via
// CreateImage; AMI-B is RegisterImage'd to share that same snapshot. Deleting
// AMI-A must RETAIN the snapshot (still referenced by AMI-B), not delete it.
func TestDeleteAMI_RetainsSharedSnapshot(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	run, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String("ami-base"), InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	amiA, err := client.CreateAMI(ctx, "us-east-1", CreateAMIInput{
		InstanceID: aws.ToString(run.Instances[0].InstanceId), Name: "shared-snap-A",
	})
	if err != nil {
		t.Skipf("substrate CreateImage unavailable: %v", err)
	}

	// Resolve AMI-A's backing snapshot.
	snaps, err := client.GetAMISnapshots(ctx, "us-east-1", amiA)
	if err != nil || len(snaps) == 0 {
		t.Skipf("substrate snapshot modeling unavailable: %v", err)
	}
	shared := snaps[0].SnapshotID

	// Register AMI-B pointing at the SAME snapshot.
	regB, err := ec2Client.RegisterImage(ctx, &ec2.RegisterImageInput{
		Name:           aws.String("shared-snap-B"),
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2types.EbsBlockDevice{SnapshotId: aws.String(shared)},
		}},
	})
	if err != nil {
		t.Skipf("substrate RegisterImage unavailable: %v", err)
	}
	amiB := aws.ToString(regB.ImageId)

	// Delete AMI-A: the shared snapshot must be RETAINED (AMI-B still uses it).
	res, err := client.DeleteAMI(ctx, "us-east-1", amiA)
	if err != nil {
		t.Fatalf("DeleteAMI(A): %v (result=%+v)", err, res)
	}
	if sliceHas(res.DeletedSnapshots, shared) {
		t.Errorf("shared snapshot %s was deleted; it must be retained (used by %s)", shared, amiB)
	}
	reason, retained := res.RetainedSnapshots[shared]
	if !retained {
		t.Fatalf("shared snapshot %s not recorded as retained; result=%+v", shared, res)
	}
	if !strings.Contains(reason, amiB) {
		t.Errorf("retain reason %q should name the sharing AMI %s", reason, amiB)
	}
}

// TestDeleteAMI_DeletesExclusiveSnapshot is the companion: a snapshot referenced
// only by the AMI being deleted IS removed.
func TestDeleteAMI_DeletesExclusiveSnapshot(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	run, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: aws.String("ami-base"), InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	ami, err := client.CreateAMI(ctx, "us-east-1", CreateAMIInput{
		InstanceID: aws.ToString(run.Instances[0].InstanceId), Name: "exclusive-snap",
	})
	if err != nil {
		t.Skipf("substrate CreateImage unavailable: %v", err)
	}
	snaps, err := client.GetAMISnapshots(ctx, "us-east-1", ami)
	if err != nil || len(snaps) == 0 {
		t.Skipf("substrate snapshot modeling unavailable: %v", err)
	}
	exclusive := snaps[0].SnapshotID

	res, err := client.DeleteAMI(ctx, "us-east-1", ami)
	if err != nil {
		t.Fatalf("DeleteAMI: %v (result=%+v)", err, res)
	}
	if !sliceHas(res.DeletedSnapshots, exclusive) {
		t.Errorf("exclusive snapshot %s should have been deleted; result=%+v", exclusive, res)
	}
	if _, retained := res.RetainedSnapshots[exclusive]; retained {
		t.Errorf("exclusive snapshot %s was retained; it should be deleted", exclusive)
	}
}

func sliceHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
