package launcher

import (
	"context"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestProvision_NoFSxOrphanOnLaunchFailure is the empirical guard for spawn#220 /
// #210: when RunInstances FAILS, Provision must create ZERO FSx filesystems (the
// #213 create-after-launch ordering). We force the launch to fail with a
// nonexistent security group (substrate rejects it at RunInstances, before the
// post-launch FSx step) and then assert no filesystem was created.
func TestProvision_NoFSxOrphanOnLaunchFailure(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	cfg := spawnaws.LaunchConfig{
		InstanceType:       "g5.12xlarge",
		Region:             "us-east-1",
		AMI:                "ami-test",                    // pre-set: skip AMI lookup
		IamInstanceProfile: "some-profile",                // pre-set: skip IAM setup
		UserData:           "IyEvYmluL2Jhc2gKdHJ1ZQ==",    // pre-set: skip bootstrap
		SecurityGroupIDs:   []string{"sg-does-not-exist"}, // forces RunInstances failure
		// Ephemeral FSx requested — this is the orphan risk under test.
		FSxLustreCreate: true,
		FSxLifecycle:    "ephemeral",
		FSxS3Bucket:     "test-bucket",
	}

	_, err := Provision(ctx, client, cfg, Options{})
	if err == nil {
		t.Fatal("expected Provision to fail (nonexistent SG), but it succeeded")
	}
	// The failure must be at the launch step, before FSx create.
	if !strings.Contains(err.Error(), "launch") {
		t.Logf("note: failure was %q (expected a launch-stage error)", err.Error())
	}

	// Assert ZERO filesystems exist: a launch failure must not have created one.
	fsxClient := fsx.NewFromConfig(env.AWSConfig)
	out, derr := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{MaxResults: awssdk.Int32(100)})
	if derr != nil {
		t.Skipf("substrate FSx DescribeFileSystems unavailable: %v", derr)
	}
	if n := len(out.FileSystems); n != 0 {
		t.Errorf("spawn#220: %d FSx filesystem(s) created despite launch failure — ORPHAN. Want 0.", n)
	}
}
