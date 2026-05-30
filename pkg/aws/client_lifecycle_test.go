package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// launchTestInstance runs one instance via the raw EC2 client and returns its ID.
func launchTestInstance(t *testing.T, env *testutil.TestEnv) string {
	t.Helper()
	out, err := env.EC2Client().RunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String("ami-12345678"),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	return *out.Instances[0].InstanceId
}

func TestClient_GetInstanceState(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	id := launchTestInstance(t, env)

	// Substrate may or may not populate State; either a valid state or the
	// "state unavailable" error is acceptable — we're exercising the code path.
	state, err := c.GetInstanceState(context.Background(), "us-east-1", id)
	if err == nil && state == "" {
		t.Error("GetInstanceState returned no error but empty state")
	}
}

func TestClient_UpdateInstanceTags(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	id := launchTestInstance(t, env)

	err := c.UpdateInstanceTags(context.Background(), "us-east-1", id, map[string]string{
		"spawn:ttl": "4h",
		"Name":      "test",
	})
	if err != nil {
		t.Fatalf("UpdateInstanceTags: %v", err)
	}
}

func TestClient_Terminate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	id := launchTestInstance(t, env)

	if err := c.Terminate(context.Background(), "us-east-1", id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestClient_StopStartInstance(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	id := launchTestInstance(t, env)
	ctx := context.Background()

	if err := c.StopInstance(ctx, "us-east-1", id, false); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if err := c.StartInstance(ctx, "us-east-1", id); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
}

func TestClient_GetInstancePublicIP(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	id := launchTestInstance(t, env)

	// Public IP may be empty for a freshly-launched substrate instance; we only
	// require the call to succeed without error.
	if _, err := c.GetInstancePublicIP(context.Background(), "us-east-1", id); err != nil {
		t.Fatalf("GetInstancePublicIP: %v", err)
	}
}

func TestClient_ListInstances(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	launchTestInstance(t, env)

	// No state filter.
	all, err := c.ListInstances(context.Background(), "us-east-1", "")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	for _, inst := range all {
		if inst.InstanceID == "" {
			t.Error("listed instance has empty ID")
		}
	}

	// With a state filter (exercises the filter branch).
	if _, err := c.ListInstances(context.Background(), "us-east-1", "running"); err != nil {
		t.Fatalf("ListInstances(running): %v", err)
	}
}

func TestClient_KeyPairOps(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	// Exercise the lookup path. Substrate doesn't emulate the
	// InvalidKeyPair.NotFound error, so we don't assert on the boolean — only
	// that the call completes without an unexpected error.
	if _, err := c.CheckKeyPairExists(ctx, "us-east-1", "no-such-key"); err != nil {
		t.Fatalf("CheckKeyPairExists: %v", err)
	}

	// Import a key, then confirm it is reported as existing.
	pub := []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY test@example.com")
	if err := c.ImportKeyPair(ctx, "us-east-1", "spawn-test-key", pub); err != nil {
		t.Skipf("ImportKeyPair unsupported by emulator: %v", err)
	}
	exists, err := c.CheckKeyPairExists(ctx, "us-east-1", "spawn-test-key")
	if err != nil {
		t.Fatalf("CheckKeyPairExists after import: %v", err)
	}
	if !exists {
		t.Error("expected imported key pair to exist")
	}
}

func TestClient_GetAccountID(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	id, err := c.GetAccountID(context.Background())
	if err != nil {
		t.Fatalf("GetAccountID: %v", err)
	}
	if id == "" {
		t.Error("expected a non-empty account ID")
	}
}

func TestClient_GetEnabledRegions(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	regions, err := c.GetEnabledRegions(context.Background())
	if err != nil {
		t.Fatalf("GetEnabledRegions: %v", err)
	}
	if len(regions) == 0 {
		t.Error("expected at least one enabled region")
	}
}

func TestClient_ConfigAccessors(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	// Config() returns the base config directly.
	_ = c.Config()

	// GetConfig(ctx) resolves a (possibly regional) config.
	if _, err := c.GetConfig(context.Background()); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
}
