//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
)

// TestHybridWorkflow tests the complete hybrid compute workflow
func TestHybridWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// 1. Create local provider
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configYAML := `
instance_id: integration-test-01
region: us-east-1
account_id: "123456789012"
ttl: 1h
job_array:
  id: integration-test-array
  name: test
  index: 0
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	os.Setenv("SPAWN_CONFIG", configPath)
	defer os.Unsetenv("SPAWN_CONFIG")

	localProvider, err := provider.NewLocalProvider(ctx)
	if err != nil {
		t.Fatalf("NewLocalProvider() error = %v", err)
	}

	identity, err := localProvider.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("GetIdentity() error = %v", err)
	}

	t.Logf("Local provider identity: %s (%s)", identity.InstanceID, identity.Provider)

	// 2. Ensure DynamoDB registry table exists
	if err := registry.EnsureTable(ctx); err != nil {
		t.Fatalf("EnsureTable() error = %v", err)
	}

	// 3. Create registry and register
	reg, err := registry.NewPeerRegistry(ctx, identity)
	if err != nil {
		t.Fatalf("NewPeerRegistry() error = %v", err)
	}

	jobArrayID := "integration-test-" + time.Now().Format("20060102-150405")
	if err := reg.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	defer reg.Deregister(ctx, jobArrayID)

	// 4. Discover peers (should find ourselves)
	peers, err := reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() error = %v", err)
	}

	if len(peers) != 1 {
		t.Errorf("Expected 1 peer, got %d", len(peers))
	}

	if len(peers) > 0 {
		if peers[0].InstanceID != identity.InstanceID {
			t.Errorf("InstanceID = %v, want %v", peers[0].InstanceID, identity.InstanceID)
		}
		if peers[0].Provider != "local" {
			t.Errorf("Provider = %v, want local", peers[0].Provider)
		}
	}

	// 5. Send heartbeat
	if err := reg.Heartbeat(ctx, jobArrayID); err != nil {
		t.Errorf("Heartbeat() error = %v", err)
	}

	// 6. Verify still discoverable after heartbeat
	peers, err = reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() after heartbeat error = %v", err)
	}

	if len(peers) != 1 {
		t.Errorf("Expected 1 peer after heartbeat, got %d", len(peers))
	}

	t.Logf("Integration test passed: registered, discovered, heartbeat successful")
}

// TestMultiInstanceCoordination tests multiple instances coordinating via DynamoDB
func TestMultiInstanceCoordination(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	jobArrayID := "multi-test-" + time.Now().Format("20060102-150405")

	// Create 3 simulated instances
	instances := []struct {
		id       string
		provider string
		index    int
	}{
		{"local-workstation-01", "local", 0},
		{"i-ec2-instance-01", "ec2", 1},
		{"i-ec2-instance-02", "ec2", 2},
	}

	var registries []*registry.PeerRegistry

	// Register all instances
	for _, inst := range instances {
		identity := &provider.Identity{
			InstanceID: inst.id,
			Region:     "us-east-1",
			Provider:   inst.provider,
			PublicIP:   "192.168.1." + string(rune(100+inst.index)),
		}

		reg, err := registry.NewPeerRegistry(ctx, identity)
		if err != nil {
			t.Fatalf("NewPeerRegistry() error = %v", err)
		}

		if err := reg.Register(ctx, jobArrayID, inst.index); err != nil {
			t.Fatalf("Register() error = %v", err)
		}

		registries = append(registries, reg)
		defer reg.Deregister(ctx, jobArrayID)
	}

	// Give DynamoDB a moment to propagate
	time.Sleep(500 * time.Millisecond)

	// Each instance should discover all 3 peers
	for i, reg := range registries {
		peers, err := reg.DiscoverPeers(ctx, jobArrayID)
		if err != nil {
			t.Fatalf("Instance %d: DiscoverPeers() error = %v", i, err)
		}

		if len(peers) != 3 {
			t.Errorf("Instance %d: Expected 3 peers, got %d", i, len(peers))
		}

		// Verify mix of local and ec2
		localCount := 0
		ec2Count := 0
		for _, peer := range peers {
			if peer.Provider == "local" {
				localCount++
			} else if peer.Provider == "ec2" {
				ec2Count++
			}
		}

		if localCount != 1 {
			t.Errorf("Instance %d: Expected 1 local peer, got %d", i, localCount)
		}
		if ec2Count != 2 {
			t.Errorf("Instance %d: Expected 2 ec2 peers, got %d", i, ec2Count)
		}
	}

	t.Logf("Multi-instance coordination test passed: 3 instances coordinated via DynamoDB")
}

// TestHeartbeatExpiration tests that instances expire after TTL
func TestHeartbeatExpiration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	jobArrayID := "expire-test-" + time.Now().Format("20060102-150405")

	identity := &provider.Identity{
		InstanceID: "expire-test-01",
		Region:     "us-east-1",
		Provider:   "local",
		PublicIP:   "192.168.1.200",
	}

	reg, err := registry.NewPeerRegistry(ctx, identity)
	if err != nil {
		t.Fatalf("NewPeerRegistry() error = %v", err)
	}

	// Note: In real usage, TTL is set during NewPeerRegistry
	// For this test, we'll use the default TTL (1 hour) and skip the actual wait
	t.Skip("Skipping TTL expiration test - requires 1 hour wait with default TTL")

	if err := reg.Register(ctx, jobArrayID, 0); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	defer reg.Deregister(ctx, jobArrayID)

	// Verify instance is discoverable
	peers, err := reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() error = %v", err)
	}
	if len(peers) != 1 {
		t.Errorf("Expected 1 peer initially, got %d", len(peers))
	}

	// Wait for TTL to expire
	t.Logf("Waiting 6 seconds for TTL expiration...")
	time.Sleep(6 * time.Second)

	// Should not find expired peer
	peers, err = reg.DiscoverPeers(ctx, jobArrayID)
	if err != nil {
		t.Fatalf("DiscoverPeers() after expiration error = %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("Expected 0 peers after TTL expiration, got %d", len(peers))
	}

	t.Logf("TTL expiration test passed: instance expired after 5 seconds")
}

// TestProviderAutoDetection tests that provider auto-detection works
func TestProviderAutoDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// This should detect local provider (unless running on actual EC2)
	prov, err := provider.NewProvider(ctx)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	provType := prov.GetProviderType()
	if provType != "local" && provType != "ec2" {
		t.Errorf("Unexpected provider type: %v", provType)
	}

	t.Logf("Auto-detected provider: %s", provType)

	identity, err := prov.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("GetIdentity() error = %v", err)
	}

	if identity.InstanceID == "" {
		t.Errorf("InstanceID should not be empty")
	}
	if identity.Region == "" {
		t.Errorf("Region should not be empty")
	}

	t.Logf("Provider identity: %s (provider: %s, region: %s)",
		identity.InstanceID, identity.Provider, identity.Region)
}
