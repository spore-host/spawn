package aws

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// --bootstrap-egress proxy (#1515 phase 3) in the --no-public-ip preflight.

const efaNeed = "the MPI bootstrap's EFA installer download (efa-installer.amazonaws.com)"

// fullPrivateNetwork with a public subnet, where an egress proxy can run.
func proxyableNetwork() PrivateNetwork {
	n := fullPrivateNetwork()
	n.PublicSubnets = []PublicSubnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-west-2a"}}
	return n
}

func proxyCfg() LaunchConfig {
	c := privateCfg
	c.BootstrapEgress = BootstrapEgressProxy
	return c
}

// Opted in: an internet need on the proxy's allowlist is met through the
// proxy, and recorded as such; the VPC-endpoint needs are unchanged.
func TestCheckPrivateNetworkProxyMeetsAllowlistedNeeds(t *testing.T) {
	c := CheckPrivateNetwork(proxyableNetwork(), proxyCfg(), []string{efaNeed})
	if !c.OK() {
		t.Fatalf("missing %v", c.Missing)
	}
	want := "internet: " + efaNeed + ", through the spawn egress proxy in vpc-1"
	found := false
	for _, m := range c.Met {
		found = found || m == want
	}
	if !found {
		t.Errorf("met = %q, want %q among them", c.Met, want)
	}
	if len(c.ViaEgressProxy) != 1 || c.ViaEgressProxy[0] != efaNeed {
		t.Errorf("ViaEgressProxy = %q, want the EFA need", c.ViaEgressProxy)
	}
	// The proxy is not for AWS APIs: a missing endpoint stays missing.
	c = CheckPrivateNetwork(without(proxyableNetwork(), "ssm"), proxyCfg(), []string{efaNeed})
	if got := missingNames(c); len(got) != 1 || got[0] != "ssm" {
		t.Errorf("without ssm, missing = %v, want [ssm]", got)
	}
}

// Whether a need goes through the proxy is the proxy's allowlist's call, not
// an assumption about the kind of need: a webhook to some other host is not
// met, opted in or not.
func TestCheckPrivateNetworkProxyOnlyForAllowlistedHosts(t *testing.T) {
	cfg := proxyCfg()
	cfg.NotifyURL = "https://hooks.example.org/x"
	c := CheckPrivateNetwork(proxyableNetwork(), cfg, nil)
	if c.OK() || len(c.ViaEgressProxy) != 0 {
		t.Fatalf("a non-allowlisted host was met through the proxy: met %q, via %q", c.Met, c.ViaEgressProxy)
	}
	if !strings.Contains(c.Missing[0], "hooks.example.org") || !strings.Contains(c.Missing[0], "forwards only to spawn's own bootstrap hosts") {
		t.Errorf("missing = %q", c.Missing)
	}
	if egressproxy.Allowed("hooks.example.org") {
		t.Fatal("test premise: hooks.example.org must not be allowlisted")
	}
}

// Opted in, but no public subnet routed to an internet gateway: refused, and
// the refusal says why.
func TestCheckPrivateNetworkProxyNeedsAPublicSubnet(t *testing.T) {
	c := CheckPrivateNetwork(fullPrivateNetwork(), proxyCfg(), []string{efaNeed})
	if c.OK() || len(c.ViaEgressProxy) != 0 {
		t.Fatalf("no public subnet, yet met: %q", c.Met)
	}
	if !strings.Contains(c.Missing[0], "cannot run the egress proxy: it has no public subnet whose route table sends 0.0.0.0/0 to an internet gateway") {
		t.Errorf("missing = %q", c.Missing)
	}
}

// Not opted in: the refusal of an allowlisted need names the flag, and the
// proxy is never counted.
func TestCheckPrivateNetworkNotOptedInNamesTheFlag(t *testing.T) {
	c := CheckPrivateNetwork(proxyableNetwork(), privateCfg, []string{efaNeed})
	if c.OK() || len(c.ViaEgressProxy) != 0 {
		t.Fatalf("not opted in, yet met through the proxy: %q", c.Met)
	}
	if !strings.Contains(c.Missing[0], "Pass "+BootstrapEgressFlag) {
		t.Errorf("missing = %q, want it to name %s", c.Missing, BootstrapEgressFlag)
	}
	// A need the proxy cannot carry does not suggest it.
	cfg := privateCfg
	cfg.NotifyURL = "https://hooks.example.org/x"
	if c := CheckPrivateNetwork(proxyableNetwork(), cfg, nil); strings.Contains(c.Missing[0], BootstrapEgressFlag) {
		t.Errorf("a need the proxy cannot carry suggests it: %q", c.Missing)
	}
}

// A NAT route meets everything: no proxy is needed, so none is counted on.
func TestCheckPrivateNetworkNATNeedsNoProxy(t *testing.T) {
	n := proxyableNetwork()
	n.NATGatewayID = "nat-1"
	if c := CheckPrivateNetwork(n, proxyCfg(), []string{efaNeed}); !c.OK() || len(c.ViaEgressProxy) != 0 {
		t.Errorf("with a NAT gateway: ok %v, via proxy %q", c.OK(), c.ViaEgressProxy)
	}
}

// The Windows bootstrap does not route its AWS CLI download through the
// proxy, so a Windows launch never counts on one.
func TestCheckPrivateNetworkProxyIsLinuxOnly(t *testing.T) {
	cfg := proxyCfg()
	cfg.TargetOS = "windows"
	c := CheckPrivateNetwork(proxyableNetwork(), cfg, nil)
	if c.OK() || len(c.ViaEgressProxy) != 0 {
		t.Fatalf("Windows counted on the proxy: met %q", c.Met)
	}
	if !strings.Contains(c.Missing[0], "serves the Linux bootstrap only") {
		t.Errorf("missing = %q", c.Missing)
	}
}

func TestInternetNeedHost(t *testing.T) {
	for need, want := range map[string]string{
		efaNeed:                                  "efa-installer.amazonaws.com",
		"notifications (hooks.example.org:8443)": "hooks.example.org",
		"the Windows bootstrap's AWS CLI installer (awscli.amazonaws.com)": "awscli.amazonaws.com",
		"no host here": "",
	} {
		if got := InternetNeedHost(need); got != want {
			t.Errorf("InternetNeedHost(%q) = %q, want %q", need, got, want)
		}
	}
	// Every need the config itself produces names a host the helper finds.
	cfg := LaunchConfig{NotifyURL: "https://a.example/x", SpotInterruptionWebhookURL: "https://b.example/y",
		CompletionWebhookURL: "https://c.example/z", DNSEndpoint: "https://d.example", TargetOS: "windows"}
	for _, need := range privateInternetNeeds(cfg) {
		if InternetNeedHost(need) == "" {
			t.Errorf("need %q names no host InternetNeedHost can read", need)
		}
	}
}

func TestValidateNoPublicIPBootstrapEgress(t *testing.T) {
	for _, tc := range []struct {
		cfg  LaunchConfig
		want string
	}{
		{LaunchConfig{NoPublicIP: true, SubnetID: "s", BootstrapEgress: BootstrapEgressProxy}, ""},
		{LaunchConfig{NoPublicIP: true, SubnetID: "s", BootstrapEgress: BootstrapEgressNone}, ""},
		{LaunchConfig{BootstrapEgress: BootstrapEgressNone}, ""},
		{LaunchConfig{BootstrapEgress: BootstrapEgressProxy}, "only with --no-public-ip"},
		{LaunchConfig{NoPublicIP: true, SubnetID: "s", BootstrapEgress: "nat"}, "use none or proxy"},
	} {
		err := tc.cfg.ValidateNoPublicIP()
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: err = %v, want %q", tc.cfg, err, tc.want)
		}
	}
}

// Launch refuses a check that counts on the proxy when the config names none:
// only the CLI's find-or-create makes "met through the proxy" true.
func TestRequireEgressProxy(t *testing.T) {
	c := PrivateCheck{ViaEgressProxy: []string{efaNeed}}
	if err := c.requireEgressProxy(LaunchConfig{}); !errors.Is(err, ErrNoEgressProxy) {
		t.Errorf("no proxy address: err = %v, want ErrNoEgressProxy", err)
	}
	if err := c.requireEgressProxy(LaunchConfig{EgressProxyAddr: "10.0.0.5:3128"}); err != nil {
		t.Errorf("with a proxy address: %v", err)
	}
	if err := (PrivateCheck{}).requireEgressProxy(LaunchConfig{}); err != nil {
		t.Errorf("no need through the proxy: %v", err)
	}
}

// Through the wire: a refused launch the proxy would serve names the flag in
// its remedy.
func TestPreflightNoPublicIPRefusalNamesTheFlag(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	c := newFakeEC2Client(t, f)
	cfg := privateCfg
	_, err := c.PreflightNoPublicIP(context.Background(), &cfg, []string{efaNeed}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pass "+BootstrapEgressFlag+" for the internet needs marked with it") {
		t.Errorf("err = %v, want the remedy to name %s", err, BootstrapEgressFlag)
	}
}

// DescribePrivateNetwork finds the VPC's public subnets from the route tables
// it already reads: a subnet whose table sends 0.0.0.0/0 to an igw-, and not
// the private one, whose main table has no default route.
func TestDescribePrivateNetworkFindsPublicSubnets(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	f.subnets = append(f.subnets,
		fakeSubnet{id: "subnet-pub", vpc: "vpc-1", az: "us-west-2b", free: 200},
		fakeSubnet{id: "subnet-nat", vpc: "vpc-1", az: "us-west-2a", free: 200})
	f.routeTablesXML += `<item><routeTableId>rtb-pub</routeTableId><vpcId>vpc-1</vpcId><routeSet>` +
		`<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><gatewayId>igw-1</gatewayId><state>active</state></item>` +
		`</routeSet><associationSet><item><routeTableAssociationId>rtbassoc-2</routeTableAssociationId>` +
		`<routeTableId>rtb-pub</routeTableId><subnetId>subnet-pub</subnetId><main>false</main></item></associationSet></item>` +
		`<item><routeTableId>rtb-nat</routeTableId><vpcId>vpc-1</vpcId><routeSet>` +
		`<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><natGatewayId>nat-9</natGatewayId><state>active</state></item>` +
		`</routeSet><associationSet><item><routeTableAssociationId>rtbassoc-3</routeTableAssociationId>` +
		`<routeTableId>rtb-nat</routeTableId><subnetId>subnet-nat</subnetId><main>false</main></item></associationSet></item>`
	c := newFakeEC2Client(t, f)
	n, err := c.DescribePrivateNetwork(context.Background(), pnRegion, "subnet-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.PublicSubnets) != 1 || n.PublicSubnets[0] != (PublicSubnet{SubnetID: "subnet-pub", AvailabilityZone: "us-west-2b"}) {
		t.Errorf("PublicSubnets = %+v, want only subnet-pub in us-west-2b", n.PublicSubnets)
	}
}
