package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
)

// Only a hybrid job-array member registers with the hybrid registry (#904): a
// local node, or an EC2 instance `spawn burst` launched (spawn:burst=true). An
// ordinary `spawn launch --count`/`--mpi` member also carries
// spawn:job-array-id, but nothing reads the registry for its array — EC2 peer
// discovery uses tags and MPI peers come from the cohort Assembler.
//
// Tested on the predicate directly (rule 2), including combinations NewAgent's
// callers do not produce today.
func TestJoinsHybridRegistry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity *provider.Identity
		config   *provider.Config
		want     bool
	}{
		{"ordinary --count/--mpi EC2 member", &provider.Identity{Provider: "ec2"}, &provider.Config{JobArrayID: "ja-1"}, false},
		{"spawn burst EC2 member", &provider.Identity{Provider: "ec2"}, &provider.Config{JobArrayID: "ja-1", Burst: true}, true},
		{"local node in a job array", &provider.Identity{Provider: "local"}, &provider.Config{JobArrayID: "ja-1"}, true},
		{"EC2 instance outside any array", &provider.Identity{Provider: "ec2"}, &provider.Config{}, false},
		{"burst tag but no array", &provider.Identity{Provider: "ec2"}, &provider.Config{Burst: true}, false},
		{"local node outside any array", &provider.Identity{Provider: "local"}, &provider.Config{}, false},
		{"nil identity", nil, &provider.Config{JobArrayID: "ja-1", Burst: true}, false},
		{"nil config", &provider.Identity{Provider: "local"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinsHybridRegistry(tc.identity, tc.config); got != tc.want {
				t.Errorf("joinsHybridRegistry = %v, want %v", got, tc.want)
			}
		})
	}
}

// Through the daemon's own activation path: NewAgent must build the registry
// client for a hybrid member and must not for an ordinary member — while still
// discovering peers for every job-array member, since the EC2 provider's
// discovery is tag-based and does not depend on the registry.
func TestNewAgent_RegistersOnlyHybridMembersWithTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		burst    bool
		want     bool
	}{
		{"ordinary --count/--mpi EC2 member", "ec2", false, false},
		{"spawn burst EC2 member", "ec2", true, true},
		{"local node", "local", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			orig := newPeerRegistry
			newPeerRegistry = func(context.Context, *provider.Identity) (*registry.PeerRegistry, error) {
				calls++
				// An error keeps the test off DynamoDB; reaching here is the signal.
				return nil, errors.New("test: registry not available")
			}
			t.Cleanup(func() { newPeerRegistry = orig })

			p := &countingProvider{stubProvider: &stubProvider{
				identity: &provider.Identity{
					InstanceID: "i-0123456789abcdef0",
					Region:     "us-west-2",
					Provider:   tc.provider,
				},
				config: &provider.Config{
					JobArrayID:    "ja-abc",
					EBSHourlyCost: 0.01, // skip the async EBS lookup
					Burst:         tc.burst,
				},
				otherInstances: -1,
			}}
			a, err := NewAgent(context.Background(), p)
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			if got := calls > 0; got != tc.want {
				t.Errorf("hybrid registry client built = %v (%d calls), want %v", got, calls, tc.want)
			}
			if !tc.want && a.registry != nil {
				t.Error("an ordinary job-array member holds a registry client, so Cleanup would Deregister it")
			}
			if p.peerCalls != 1 {
				t.Errorf("DiscoverPeers called %d times, want 1 for every job-array member", p.peerCalls)
			}
		})
	}
}
