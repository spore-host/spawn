package aws

import (
	"context"
	"testing"
)

// TestNewClientWithRegion_PinsRegionOverAmbient is the #276 regression: an
// explicit region must win over the ambient AWS_REGION / AWS_DEFAULT_REGION so
// the whole client (STS/pricing/AMI/AZ/RunInstances) runs in the target region.
func TestNewClientWithRegion_PinsRegionOverAmbient(t *testing.T) {
	// Ambient env points elsewhere; the explicit region must override it.
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")

	c, err := NewClientWithRegion(context.Background(), "us-west-2")
	if err != nil {
		t.Fatalf("NewClientWithRegion: %v", err)
	}
	if got := c.Config().Region; got != "us-west-2" {
		t.Errorf("client region = %q, want us-west-2 (explicit region must beat ambient AWS_REGION)", got)
	}
}

// TestNewClientWithRegion_EmptyFallsBackToAmbient: an empty region preserves the
// default-chain behavior (used by commands that legitimately span regions).
func TestNewClientWithRegion_EmptyFallsBackToAmbient(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-central-1")

	c, err := NewClientWithRegion(context.Background(), "")
	if err != nil {
		t.Fatalf("NewClientWithRegion: %v", err)
	}
	if got := c.Config().Region; got != "eu-central-1" {
		t.Errorf("client region = %q, want eu-central-1 (empty region should use the ambient default)", got)
	}
}
