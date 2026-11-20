package aws

import (
	"context"
	"testing"

	"github.com/aws/smithy-go/middleware"

	spawnconfig "github.com/spore-host/spawn/pkg/config"
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

// TestNewClientWithRegion_AddsTheCLIAPIOptions: the spawn CLI's account note
// (spawnconfig.SetCLIAPIOptions, the last mile's, libs#78) is on every
// client built for the caller's account, and nothing is added when no
// program set one (lagotto, calque and spore-host-mcp import this package).
func TestNewClientWithRegion_AddsTheCLIAPIOptions(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SPORE_ACCOUNT", "")
	c, err := NewClientWithRegion(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	base := len(c.Config().APIOptions)

	called := 0
	spawnconfig.SetCLIAPIOptions(func(*middleware.Stack) error { called++; return nil })
	t.Cleanup(func() { spawnconfig.SetCLIAPIOptions() })
	if c, err = NewClientWithRegion(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	opts := c.Config().APIOptions
	if len(opts) != base+1 {
		t.Fatalf("%d API option(s), want %d", len(opts), base+1)
	}
	_ = opts[len(opts)-1](nil)
	if called != 1 {
		t.Error("the added option is not the one set")
	}
}
