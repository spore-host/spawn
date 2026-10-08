package agent

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/provider"
)

// countingProvider embeds the package's stubProvider and counts the two calls
// that stand in for "did the constructor do anything". Embedding rather than
// re-implementing Provider, so adding a method to the interface does not break
// this file — which it did on the first attempt.
//
// The side effects #733 is about go through the provider (EBS cost tag write,
// peer discovery) or through the DNS/registry clients (whose construction is
// observable on the agent), so between them the two give full coverage.
type countingProvider struct {
	*stubProvider
	ebsLookups int
	peerCalls  int
}

func (p *countingProvider) GetProviderType() string { return "ec2" }
func (p *countingProvider) DiscoverPeers(ctx context.Context, id string) ([]provider.PeerInfo, error) {
	p.peerCalls++
	return p.stubProvider.DiscoverPeers(ctx, id)
}
func (p *countingProvider) LookupAndTagEBSCost(ctx context.Context) (float64, bool) {
	p.ebsLookups++
	return p.stubProvider.LookupAndTagEBSCost(ctx)
}

func queryProvider() *countingProvider {
	return &countingProvider{stubProvider: &stubProvider{
		identity: &provider.Identity{
			InstanceID: "i-0123456789abcdef0",
			Name:       "demo",
			Region:     "us-west-2",
			AccountID:  "123456789012",
			Provider:   "ec2",
			PublicIP:   "203.0.113.10",
		},
		config: &provider.Config{
			// Every field that triggers a side effect in NewAgent.
			DNSName:    "demo",   // -> DNS registration POST + tag write
			JobArrayID: "ja-abc", // -> registry Register + heartbeat goroutine
			// EBSHourlyCost zero -> cost lookup + tag write
		},
		otherInstances: -1,
	}}
}

// NewAgentForQuery must perform NO side effect. `spored status` used NewAgent,
// which registers DNS — so `spawn status`, a read-only query, did a
// control-plane WRITE on every invocation, and polling it (the normal way to
// watch a job) meant one write per poll (#733).
func TestNewAgentForQuery_PerformsNoSideEffects(t *testing.T) {
	p := queryProvider()
	a, err := NewAgentForQuery(context.Background(), p)
	if err != nil {
		t.Fatalf("NewAgentForQuery: %v", err)
	}

	if p.ebsLookups != 0 {
		t.Errorf("query mode performed %d EBS cost lookup(s) — each writes a tag", p.ebsLookups)
	}
	if p.peerCalls != 0 {
		t.Errorf("query mode discovered peers %d time(s)", p.peerCalls)
	}
	// A DNS client being constructed is what precedes the POST; its absence is
	// the observable that the registration block did not run.
	if a.dnsClient != nil {
		t.Error("query mode built a DNS client, so the registration block ran — that is #733")
	}
	if a.registry != nil {
		t.Error("query mode registered with the peer registry and started a heartbeat goroutine")
	}

	// State a query legitimately needs must still be present.
	if a.dnsDomain == "" {
		t.Error("dnsDomain was not set; a query may need to report it")
	}
	if a.identity == nil || a.config == nil {
		t.Error("query mode did not finish constructing the agent")
	}
	if a.GetUptime() < 0 {
		t.Error("query mode produced an agent that cannot answer basic questions")
	}
}

// ...and the daemon path must still do the work, or this fix would have disabled
// DNS registration and job-array membership instead of relocating them.
func TestNewAgent_StillActivates(t *testing.T) {
	p := queryProvider()
	a, err := NewAgent(context.Background(), p)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	// DiscoverPeers is synchronous in NewAgent, so it is a reliable signal that
	// the activation block ran. (The EBS lookup is deliberately in a goroutine,
	// so asserting on it here would be a race.)
	if p.peerCalls == 0 {
		t.Error("NewAgent no longer discovers peers — activation was disabled, not relocated")
	}
	if a.dnsDomain == "" {
		t.Error("dnsDomain was not set")
	}
}
