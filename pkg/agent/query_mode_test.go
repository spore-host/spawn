package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
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
			Burst:      true,     // only hybrid/burst members join the registry (#904)
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

// fakeActivation stands in for everything NewAgent's activation reaches: the
// spore.host DNS API (an httptest server behind a real dns.Client), the hybrid
// registry's DynamoDB (an httptest server behind a real PeerRegistry), and the EC2
// tag writer. The real clients are built from ambient credentials, which is how
// this test used to make a real Route53 UPSERT, CreateTags and PutItem on every
// developer's `make check` (#968).
type fakeActivation struct {
	mu       sync.Mutex
	dnsReqs  []dns.DNSUpdateRequest
	ddbCalls []string // X-Amz-Target of every DynamoDB call
	tags     *fakeTagPutter
}

func (f *fakeActivation) dnsRequests() []dns.DNSUpdateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dns.DNSUpdateRequest(nil), f.dnsReqs...)
}

func (f *fakeActivation) dynamoCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ddbCalls...)
}

func installFakeActivation(t *testing.T) *fakeActivation {
	t.Helper()
	f := &fakeActivation{tags: &fakeTagPutter{}}

	dnsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req dns.DNSUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("fake DNS API: undecodable request: %v", err)
		}
		f.mu.Lock()
		f.dnsReqs = append(f.dnsReqs, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"change_id":"/change/FAKE"}`))
	}))
	t.Cleanup(dnsAPI.Close)

	dynamo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.ddbCalls = append(f.ddbCalls, r.Header.Get("X-Amz-Target"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(dynamo.Close)

	origDNS, origReg, origTags := newDNSClient, newPeerRegistry, newTagPutter
	t.Cleanup(func() { newDNSClient, newPeerRegistry, newTagPutter = origDNS, origReg, origTags })

	newDNSClient = func(ctx context.Context, domain, _ string) (*dns.Client, error) {
		return dns.NewClient(ctx, domain, dnsAPI.URL)
	}
	newPeerRegistry = func(_ context.Context, id *provider.Identity) (*registry.PeerRegistry, error) {
		return registry.NewPeerRegistryFromConfig(id, aws.Config{
			Region:       "us-west-2",
			Credentials:  credentials.NewStaticCredentialsProvider("AKIAFAKE", "fake", ""),
			BaseEndpoint: aws.String(dynamo.URL),
		}), nil
	}
	newTagPutter = func(string) tagPutter { return f.tags }
	return f
}

// ...and the daemon path must still do the work, or this fix would have disabled
// DNS registration and job-array membership instead of relocating them.
//
// Asserted through fakes (#968): this used to call the real clients, so it
// registered demo.<account>.spore.host for real whenever credentials were around
// — and passed whether or not they were, since NewAgent only logs those failures.
func TestNewAgent_StillActivates(t *testing.T) {
	fake := installFakeActivation(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // stops the registry heartbeat goroutine

	p := queryProvider()
	a, err := NewAgent(ctx, p)
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

	// DNS: one leased UPSERT of the configured name to the public IP, recorded
	// as registered on the instance.
	reqs := fake.dnsRequests()
	if len(reqs) != 1 {
		t.Fatalf("DNS API got %d request(s), want 1 registration: %+v", len(reqs), reqs)
	}
	if r := reqs[0]; r.Action != "UPSERT" || r.RecordName != "demo" || r.IPAddress != "203.0.113.10" || !r.Lease {
		t.Errorf("DNS registration = %+v, want a leased UPSERT of demo -> 203.0.113.10", r)
	}
	if a.lastDNSStatus != "registered" {
		t.Errorf("lastDNSStatus = %q, want registered", a.lastDNSStatus)
	}
	if got := fake.tags.tags["spawn:dns-status"]; got != "registered" {
		t.Errorf("spawn:dns-status tag = %q, want registered", got)
	}

	// Registry: the job-array member registered (PutItem) and kept the client.
	if a.registry == nil {
		t.Error("NewAgent did not keep a registry client for a job-array member")
	}
	var put bool
	for _, c := range fake.dynamoCalls() {
		put = put || strings.HasSuffix(c, ".PutItem")
	}
	if !put {
		t.Errorf("registry made no PutItem; DynamoDB calls: %v", fake.dynamoCalls())
	}
}
