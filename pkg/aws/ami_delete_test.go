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
