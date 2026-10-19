package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/dns"
)

// The DCV fallback host is the instance's own FQDN — its account, its zone —
// never one hardcoded account's subdomain (#1152), and a custom DNS domain
// (#891) is honoured.
func TestDCVFallbackHost(t *testing.T) {
	const acct = "123456789012"
	label := dns.EncodeAccountID(acct) // 1kpqzg2c
	inst := func(tags map[string]string) *aws.InstanceInfo {
		return &aws.InstanceInfo{PublicIP: "203.0.113.7", Tags: tags}
	}
	cases := []struct {
		name   string
		inst   *aws.InstanceInfo
		caller string
		want   string
	}{
		{"custom dns-domain", inst(map[string]string{
			"spawn:dns-name": "paraview-1", "spawn:account-id": acct, "spawn:dns-domain": "prismcloud.host",
		}), "", "paraview-1." + label + ".prismcloud.host"},
		{"default zone", inst(map[string]string{
			"spawn:dns-name": "paraview-1", "spawn:account-id": acct,
		}), "", "paraview-1." + label + ".spore.host"},
		{"base36 tag only", inst(map[string]string{
			"spawn:dns-name": "paraview-1", "spawn:account-base36": label, "spawn:dns-domain": "example.org",
		}), "", "paraview-1." + label + ".example.org"},
		{"instance's account beats the caller's", inst(map[string]string{
			"spawn:dns-name": "paraview-1", "spawn:account-id": acct,
		}), "210987654321", "paraview-1." + label + ".spore.host"},
		{"no account tags: caller's account", inst(map[string]string{
			"spawn:dns-name": "paraview-1",
		}), acct, "paraview-1." + label + ".spore.host"},
		{"no account at all: public IP", inst(map[string]string{"spawn:dns-name": "paraview-1"}), "", "203.0.113.7"},
		{"no dns name: public IP", inst(map[string]string{"spawn:account-id": acct}), "", "203.0.113.7"},
	}
	for _, tc := range cases {
		if got := dcvFallbackHost(tc.inst, tc.caller); got != tc.want {
			t.Errorf("%s: dcvFallbackHost = %q, want %q", tc.name, got, tc.want)
		}
	}
}
