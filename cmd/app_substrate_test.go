package cmd

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// seedInstance launches a spawn-managed instance in the Substrate emulator and
// applies the given tags (mirroring spored's CreateTags), returning its ID.
func seedInstance(t *testing.T, ec2Client *ec2.Client, tags map[string]string) string {
	t.Helper()
	ctx := context.Background()
	run, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-dcv"),
		InstanceType: ec2types.InstanceTypeG4dnXlarge,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil || len(run.Instances) == 0 {
		t.Skipf("substrate RunInstances unavailable: %v", err)
	}
	instID := aws.ToString(run.Instances[0].InstanceId)

	ec2Tags := []ec2types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}
	for k, v := range tags {
		k, v := k, v
		ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	if _, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instID},
		Tags:      ec2Tags,
	}); err != nil {
		t.Skipf("substrate CreateTags unavailable: %v", err)
	}
	return instID
}

// TestAppLaunchPoll_ReadyOnSuccess is the spawn#282 Tier-0 round-trip: spored
// writes spawn:ready-* tags, the CLI's poll path reads them back via the real
// ListInstances + scanDCVReady, and recovers the token/host. This is the exact
// branch that previously surfaced only the generic timeout — now exercised
// off-instance against the Substrate emulator.
func TestAppLaunchPoll_ReadyOnSuccess(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)
	client := spawnclient.NewClientFromConfig(env.AWSConfig)

	const token = "abc123def456"
	instID := seedInstance(t, ec2Client, map[string]string{
		"spawn:ready-status": dcvStatusReady,
		"spawn:ready-token":  token,
		"spawn:ready-url":    "https://host.123.spore.host:8443/?authToken=" + token + "#console",
		"spawn:dns-name":     "host.123.spore.host",
	})

	instances, err := client.ListInstances(ctx, "us-east-1", "running")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	scan := scanDCVReady(instances, instID)

	if scan.status != dcvStatusReady {
		t.Errorf("status = %q, want %q", scan.status, dcvStatusReady)
	}
	if scan.token != token {
		t.Errorf("token = %q, want %q", scan.token, token)
	}
	if scan.host != "host.123.spore.host" {
		t.Errorf("host = %q, want host.123.spore.host", scan.host)
	}
	if dcvStatusTerminal(scan.status) {
		t.Error("ready status must not be terminal (the poll loop would stop without a token)")
	}
}

// TestAppLaunchPoll_NamedReasonOnFailure asserts the failure side of the same
// round-trip: a terminal ready-status (here dcv-not-installed) is read back, is
// classified terminal so the poll loop breaks early, and maps to a specific
// actionable message — not the opaque timeout that caused the feature's churn.
func TestAppLaunchPoll_NamedReasonOnFailure(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(env.AWSConfig)
	client := spawnclient.NewClientFromConfig(env.AWSConfig)

	instID := seedInstance(t, ec2Client, map[string]string{
		"spawn:ready-status": dcvStatusNotInstalled,
	})

	instances, err := client.ListInstances(ctx, "us-east-1", "running")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	scan := scanDCVReady(instances, instID)

	if scan.token != "" {
		t.Errorf("token = %q, want empty on a failure status", scan.token)
	}
	if !dcvStatusTerminal(scan.status) {
		t.Fatalf("status %q must be terminal so the poll loop breaks early", scan.status)
	}
	msg := dcvFailureMessage(scan.status, instID)
	if msg == "" || msg == dcvFailureMessage("", instID) {
		t.Errorf("failure message for %q should be specific, got %q", scan.status, msg)
	}
}
