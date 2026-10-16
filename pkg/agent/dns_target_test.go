package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/provider"
)

// #891: a spore launched against a custom DNS deployment carries the zone and the
// updater endpoint as spawn:dns-domain / spawn:dns-endpoint. spored must register
// THERE — before this it read only SPORED_DNS_DOMAIN, which nothing set, so every
// spore registered under spore.host whatever it was launched for.

const customEndpoint = "https://abc123.lambda-url.eu-west-1.on.aws/"

func TestResolveDNSTarget(t *testing.T) {
	t.Setenv("SPORED_DNS_DOMAIN", "")
	cases := []struct {
		name            string
		cfg             provider.Config
		env             string
		wantDom, wantEP string
		wantErr         string
	}{
		{"nothing set: spore.host, compiled-in endpoint", provider.Config{}, "", dns.DefaultDomain, "", ""},
		{"tags win", provider.Config{DNSDomain: "spore.example.edu", DNSEndpoint: customEndpoint}, "", "spore.example.edu", customEndpoint, ""},
		{"tag beats the env", provider.Config{DNSDomain: "spore.example.edu"}, "other.example.org", "spore.example.edu", "", ""},
		{"env when no tag", provider.Config{}, "other.example.org", "other.example.org", "", ""},
		{"endpoint canonicalised", provider.Config{DNSEndpoint: "https://ABC123.lambda-url.eu-west-1.on.aws"}, "", dns.DefaultDomain, customEndpoint, ""},
		{"invalid domain refused", provider.Config{DNSDomain: "Not A Domain"}, "", "", "", "spawn:dns-domain"},
		{"non-Function-URL endpoint refused", provider.Config{DNSEndpoint: "https://evil.example/"}, "", "", "", "spawn:dns-endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPORED_DNS_DOMAIN", tc.env)
			cfg := tc.cfg
			dom, ep, err := resolveDNSTarget(&cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tc.wantErr)
				}
				if ep != "" {
					t.Errorf("an invalid target still returned endpoint %q", ep)
				}
				return
			}
			if err != nil || dom != tc.wantDom || ep != tc.wantEP {
				t.Errorf("got (%q, %q, %v), want (%q, %q, nil)", dom, ep, err, tc.wantDom, tc.wantEP)
			}
		})
	}
}

// Through NewAgent: the DNS client is built from the tags, and the request names
// the custom domain.
func TestNewAgentRegistersWithTheCustomTarget(t *testing.T) {
	t.Setenv("SPORED_DNS_DOMAIN", "")
	fake := installFakeActivation(t)
	var gotDomain, gotEndpoint string
	inner := newDNSClient
	newDNSClient = func(ctx context.Context, domain, endpoint string) (*dns.Client, error) {
		gotDomain, gotEndpoint = domain, endpoint
		return inner(ctx, domain, endpoint) // the fake: serves the request locally
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p := queryProvider()
	p.config.DNSDomain = "spore.example.edu"
	p.config.DNSEndpoint = customEndpoint
	a, err := NewAgent(ctx, p)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if gotDomain != "spore.example.edu" || gotEndpoint != customEndpoint {
		t.Errorf("DNS client built for (%q, %q), want (spore.example.edu, %s)", gotDomain, gotEndpoint, customEndpoint)
	}
	if a.dnsDomain != "spore.example.edu" {
		t.Errorf("dnsDomain = %q; the DCV ready URL and lease log would name the wrong zone", a.dnsDomain)
	}
	reqs := fake.dnsRequests()
	if len(reqs) != 1 || reqs[0].Domain != "spore.example.edu" {
		t.Fatalf("DNS requests = %+v, want one naming spore.example.edu", reqs)
	}
}

// A tampered or mistyped endpoint is reported, never quietly swapped for
// spore.host's: no request is sent, and spawn:dns-status says why.
func TestNewAgentRefusesAnInvalidTarget(t *testing.T) {
	fake := installFakeActivation(t)
	built := false
	inner := newDNSClient
	newDNSClient = func(ctx context.Context, domain, endpoint string) (*dns.Client, error) {
		built = true
		return inner(ctx, domain, endpoint)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p := queryProvider()
	p.config.DNSEndpoint = "https://collector.example.net/"
	a, err := NewAgent(ctx, p)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if built || len(fake.dnsRequests()) != 0 {
		t.Errorf("an invalid endpoint still produced a DNS client (%v) / %d request(s)", built, len(fake.dnsRequests()))
	}
	if a.dnsClient != nil {
		t.Error("dnsClient is set, so the lease timer would renew against an endpoint that failed validation")
	}
	if got := fake.tags.tags["spawn:dns-status"]; got != "failed" {
		t.Errorf("spawn:dns-status = %q, want failed", got)
	}
	if got := fake.tags.tags["spawn:dns-error"]; !strings.Contains(got, "spawn:dns-endpoint") {
		t.Errorf("spawn:dns-error = %q, want it to name the bad tag", got)
	}
}
