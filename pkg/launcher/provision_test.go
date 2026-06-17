package launcher

import (
	"context"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

func TestProvision_RejectsMissingFields(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	if _, err := Provision(ctx, client, spawnaws.LaunchConfig{Region: "us-east-1"}, Options{}); err == nil {
		t.Error("expected error when instance type is missing")
	}
	if _, err := Provision(ctx, client, spawnaws.LaunchConfig{InstanceType: "m7i.large"}, Options{}); err == nil {
		t.Error("expected error when region is missing")
	}
}

func TestProvision_RejectsWindows(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	_, err := Provision(context.Background(), client, spawnaws.LaunchConfig{
		InstanceType: "m7i.large", Region: "us-east-1", TargetOS: "windows",
	}, Options{})
	if err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Errorf("expected Windows-unsupported error, got %v", err)
	}
}

// TestProvision_EndToEnd exercises the full headless path against substrate:
// AMI auto-detection (empty AMI), spored IAM profile setup (empty profile),
// bootstrap user-data injection, and Launch. This is the path lagotto's
// capacity-poller takes (lagotto#19).
func TestProvision_EndToEnd(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	// Seed the SSM AMI parameters GetRecommendedAMI reads.
	ssmClient := ssm.NewFromConfig(env.AWSConfig)
	for name, val := range map[string]string{
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64": "ami-x86-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64":  "ami-arm-standard",
	} {
		if _, err := ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
			Name: awssdk.String(name), Value: awssdk.String(val), Type: ssmtypes.ParameterTypeString,
		}); err != nil {
			t.Skipf("substrate SSM PutParameter unavailable: %v", err)
		}
	}

	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	result, err := Provision(ctx, client, spawnaws.LaunchConfig{
		InstanceType: "m7i.large",
		Region:       "us-east-1",
		TTL:          "4h",
		OnComplete:   "terminate",
	}, Options{}) // keyless: SSM-only, like the Lambda
	if err != nil {
		// Substrate may not fully model IAM CreateRole or RunInstances; in that
		// case skip rather than fail (mirrors the other substrate launch tests).
		if strings.Contains(err.Error(), "IAM") || strings.Contains(err.Error(), "launch") {
			t.Skipf("substrate does not fully model the launch path: %v", err)
		}
		t.Fatalf("Provision: %v", err)
	}
	if result.InstanceID == "" {
		t.Error("Provision returned empty instance ID")
	}
}

// TestProvision_FSxLifecycleFailClosed verifies the headless FSx-create contract
// (#202/#193): durable is rejected on this path, and a create with no/invalid
// lifecycle errors — before any AWS call. AMI/profile/user-data are pre-set so we
// reach the FSx step without earlier AWS lookups.
func TestProvision_FSxLifecycleFailClosed(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	base := func() spawnaws.LaunchConfig {
		return spawnaws.LaunchConfig{
			InstanceType:       "m7i.large",
			Region:             "us-east-1",
			AMI:                "ami-caller",
			IamInstanceProfile: "some-profile",
			UserData:           "#!/bin/bash\ntrue",
			FSxLustreCreate:    true,
			FSxS3Bucket:        "b",
		}
	}

	cases := map[string]string{
		"durable": "durable", // rejected on headless path
		"":        "",        // missing lifecycle
		"forever": "forever", // invalid
	}
	for name, lc := range cases {
		t.Run("rejects_"+name, func(t *testing.T) {
			cfg := base()
			cfg.FSxLifecycle = lc
			if lc == "durable" {
				cfg.FSxTTL = "30d"
			}
			if _, err := Provision(ctx, client, cfg, Options{}); err == nil {
				t.Errorf("expected FSx lifecycle %q to be rejected on the headless path", lc)
			}
		})
	}

	t.Run("rejects_create_without_bucket", func(t *testing.T) {
		cfg := base()
		cfg.FSxLifecycle = "ephemeral"
		cfg.FSxS3Bucket = ""
		if _, err := Provision(ctx, client, cfg, Options{}); err == nil {
			t.Error("ephemeral FSx create without an S3 bucket must error")
		}
	})
}

// TestValidateEphemeralFSx is the #213 fail-fast guarantee: the ephemeral FSx
// request is validated BEFORE the instance launches, so a bad config errors
// without billing an instance (the actual filesystem is created post-launch).
func TestValidateEphemeralFSx(t *testing.T) {
	ok := spawnaws.LaunchConfig{FSxLifecycle: "ephemeral", FSxS3Bucket: "b"}
	if err := validateEphemeralFSx(&ok); err != nil {
		t.Errorf("valid ephemeral config rejected: %v", err)
	}

	bad := map[string]spawnaws.LaunchConfig{
		"durable rejected":    {FSxLifecycle: "durable", FSxS3Bucket: "b"},
		"empty lifecycle":     {FSxLifecycle: "", FSxS3Bucket: "b"},
		"invalid lifecycle":   {FSxLifecycle: "forever", FSxS3Bucket: "b"},
		"ephemeral no bucket": {FSxLifecycle: "ephemeral", FSxS3Bucket: ""},
	}
	for name, cfg := range bad {
		t.Run(name, func(t *testing.T) {
			c := cfg
			if err := validateEphemeralFSx(&c); err == nil {
				t.Errorf("%s: expected validation error", name)
			}
		})
	}
}

// TestProvision_PreservesCallerAMI confirms a caller-supplied AMI is NOT
// overwritten by auto-detection.
func TestProvision_PreservesCallerAMI(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	// No SSM params seeded: if Provision tried to auto-detect it would fail.
	// With a caller AMI set, it must skip detection (and then proceed to IAM,
	// which may skip below).
	_, err := Provision(context.Background(), client, spawnaws.LaunchConfig{
		InstanceType: "m7i.large",
		Region:       "us-east-1",
		AMI:          "ami-caller-supplied",
		// Pre-set the profile + user-data too, so the only remaining step is
		// Launch — isolating "caller AMI is respected" from AMI lookup failure.
		IamInstanceProfile: "some-profile",
		UserData:           "#!/bin/bash\necho hi",
	}, Options{})
	if err != nil && !strings.Contains(err.Error(), "launch") {
		t.Fatalf("unexpected non-launch error (AMI lookup should have been skipped): %v", err)
	}
}
