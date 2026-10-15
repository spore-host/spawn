package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/provider"
)

// onAnInstance strips every ambient source of a region, leaving the environment
// spored actually runs in: no AWS_REGION, no AWS_DEFAULT_REGION and no shared
// config naming one (#899).
func onAnInstance(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
}

// The hybrid-registry client must carry a region on an instance, where nothing
// ambient supplies one. It used to load the ambient chain alone and every
// Register failed with "Missing Region" before reaching DynamoDB (#899). The
// region is the instance's own: `spawn burst` creates the table in the region
// it launches the burst instances into.
func TestNewPeerRegistry_UsesTheInstanceRegionWhenNothingAmbientSetsOne(t *testing.T) {
	onAnInstance(t)
	reg, err := NewPeerRegistry(context.Background(), &provider.Identity{
		InstanceID: "i-0123456789abcdef0", Region: "us-west-2", Provider: "ec2",
	})
	if err != nil {
		t.Fatalf("NewPeerRegistry: %v", err)
	}
	if got := reg.client.Options().Region; got != "us-west-2" {
		t.Fatalf("registry client region = %q, want the instance's region us-west-2", got)
	}
}

// The instance's region beats an ambient one, matching every other spored AWS
// client (agent.go pins identity.Region): the table is where the instance is.
func TestNewPeerRegistry_InstanceRegionBeatsAmbient(t *testing.T) {
	onAnInstance(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	reg, err := NewPeerRegistry(context.Background(), &provider.Identity{Region: "us-west-2", Provider: "ec2"})
	if err != nil {
		t.Fatalf("NewPeerRegistry: %v", err)
	}
	if got := reg.client.Options().Region; got != "us-west-2" {
		t.Fatalf("registry client region = %q, want us-west-2", got)
	}
}

// A local provider with no configured region reports the placeholder "local".
// That is not a region; pinning to it would send DynamoDB to
// dynamodb.local.amazonaws.com. The ambient chain must still decide.
func TestNewPeerRegistry_LocalPlaceholderFallsBackToAmbient(t *testing.T) {
	for _, region := range []string{"", "local"} {
		t.Run("region="+region, func(t *testing.T) {
			onAnInstance(t)
			t.Setenv("AWS_REGION", "eu-west-1")
			reg, err := NewPeerRegistry(context.Background(), &provider.Identity{Region: region, Provider: "local"})
			if err != nil {
				t.Fatalf("NewPeerRegistry: %v", err)
			}
			if got := reg.client.Options().Region; got != "eu-west-1" {
				t.Fatalf("registry client region = %q, want the ambient eu-west-1", got)
			}
		})
	}
}
