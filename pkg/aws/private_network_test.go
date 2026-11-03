package aws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// --- --no-public-ip (#1433) ---

const pnRegion = "us-west-2"

// fullPrivateNetwork is a subnet whose VPC has everything a private-only
// launch needs by default and no NAT: an S3 gateway endpoint on its route
// table, and private-DNS interface endpoints in its AZ for ec2 and the SSM
// agent's three services. The us-east-1 default VPC the issue tests with has
// these (and more).
func fullPrivateNetwork() PrivateNetwork {
	n := PrivateNetwork{Region: pnRegion, SubnetID: "subnet-1", VPCID: "vpc-1", AvailabilityZone: "us-west-2a", RouteTableID: "rtb-1"}
	n.Endpoints = append(n.Endpoints, PrivateEndpoint{ID: "vpce-s3", Service: "s3", Type: "Gateway", State: "available", RouteTableIDs: []string{"rtb-1"}})
	for _, svc := range []string{"ec2", "ssm", "ssmmessages", "ec2messages"} {
		n.Endpoints = append(n.Endpoints, PrivateEndpoint{ID: "vpce-" + svc, Service: svc, Type: "Interface", State: "available",
			PrivateDNS: true, AvailabilityZones: []string{"us-west-2a"}})
	}
	return n
}

func without(n PrivateNetwork, service string) PrivateNetwork {
	var keep []PrivateEndpoint
	for _, e := range n.Endpoints {
		if e.Service != service {
			keep = append(keep, e)
		}
	}
	n.Endpoints = keep
	return n
}

func missingNames(c PrivateCheck) []string {
	var out []string
	for _, m := range c.Missing {
		out = append(out, strings.SplitN(m, ":", 2)[0])
	}
	return out
}

var privateCfg = LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", Region: pnRegion}

// The issue's own two VPCs, and the cases in between. A subnet with no S3
// endpoint and no NAT is refused; one with every endpoint, or with a NAT
// route and no endpoints at all, passes.
func TestCheckPrivateNetwork(t *testing.T) {
	bare := PrivateNetwork{Region: pnRegion, SubnetID: "subnet-1", VPCID: "vpc-1", AvailabilityZone: "us-west-2a", RouteTableID: "rtb-1"}
	nat := bare
	nat.NATGatewayID = "nat-1"
	igw := bare
	igw.DefaultRoute = "internet gateway igw-1"

	noDNS := fullPrivateNetwork()
	for i := range noDNS.Endpoints {
		if noDNS.Endpoints[i].Service == "ssm" {
			noDNS.Endpoints[i].PrivateDNS = false
		}
	}
	unassociated := fullPrivateNetwork()
	unassociated.Endpoints[0].RouteTableIDs = []string{"rtb-other"}
	s3Interface := without(fullPrivateNetwork(), "s3")
	s3Interface.Endpoints = append(s3Interface.Endpoints, PrivateEndpoint{ID: "vpce-s3i", Service: "s3", Type: "Interface",
		State: "available", PrivateDNS: true, AvailabilityZones: []string{"us-west-2a"}})
	s3InboundOnly := without(fullPrivateNetwork(), "s3")
	s3InboundOnly.Endpoints = append(s3InboundOnly.Endpoints, PrivateEndpoint{ID: "vpce-s3i", Service: "s3", Type: "Interface",
		State: "available", PrivateDNS: true, InboundResolverOnly: true, AvailabilityZones: []string{"us-west-2a"}})
	unpublished := fullPrivateNetwork()
	unpublished.Region = "ap-south-1"
	pending := fullPrivateNetwork()
	pending.Endpoints[1].State = "pending"
	otherAZ := fullPrivateNetwork()
	for i := range otherAZ.Endpoints {
		if otherAZ.Endpoints[i].Service == "ec2" {
			otherAZ.Endpoints[i].AvailabilityZones = []string{"us-west-2b"}
		}
	}

	for _, tc := range []struct {
		name        string
		n           PrivateNetwork
		cfg         LaunchConfig
		missing     []string
		mentions    string // in the missing entry
		warns       bool
		internetNAT bool
	}{
		{name: "no endpoints, no NAT (us-west-2 default VPC)", n: bare, missing: []string{"s3", "ec2", "ssm", "ssmmessages", "ec2messages"}},
		{name: "an internet gateway is not a way out", n: igw, missing: []string{"s3", "ec2", "ssm", "ssmmessages", "ec2messages"}, mentions: "internet gateway igw-1"},
		{name: "every endpoint (us-east-1 default VPC)", n: fullPrivateNetwork()},
		{name: "a NAT route and no endpoints", n: nat},
		{name: "no S3 endpoint", n: without(fullPrivateNetwork(), "s3"), missing: []string{"s3"}},
		{name: "no ec2messages endpoint", n: without(fullPrivateNetwork(), "ec2messages"), missing: []string{"ec2messages"}},
		{name: "interface endpoint with private DNS disabled", n: noDNS, missing: []string{"ssm"}, mentions: "private DNS disabled"},
		{name: "gateway endpoint not on the subnet's route table", n: unassociated, missing: []string{"s3"}, mentions: "not associated with the subnet's route table rtb-1"},
		// The bootstrap uses only regional S3 names (#1436), which an
		// interface endpoint's private DNS serves.
		{name: "an S3 interface endpoint with private DNS serves the bootstrap download", n: s3Interface},
		{name: "an S3 interface endpoint with private DNS only for inbound Resolver endpoints does not", n: s3InboundOnly, missing: []string{"s3"}, mentions: "only for inbound Resolver endpoints"},
		// No spawn-binaries bucket in the region: the bootstrap's only source
		// is the us-east-1 fallback, which no endpoint in this VPC reaches.
		{name: "a region spawn does not publish spored to needs NAT", n: unpublished, missing: []string{"s3"}, mentions: "falls back to spawn-binaries-us-east-1"},
		{name: "an endpoint that is not available", n: pending, missing: []string{"ec2"}, mentions: "pending"},
		{name: "an interface endpoint in another AZ works, with a warning", n: otherAZ, warns: true},
		{name: "burst needs dynamodb", n: fullPrivateNetwork(), cfg: LaunchConfig{Tags: map[string]string{"spawn:burst": "true"}}, missing: []string{"dynamodb"}},
		{name: "--fsx-create needs fsx", n: fullPrivateNetwork(), cfg: LaunchConfig{FSxLustreCreate: true}, missing: []string{"fsx"}},
		{name: "a webhook needs the internet", n: fullPrivateNetwork(), cfg: LaunchConfig{CompletionWebhookURL: "https://hooks.example.org/x"}, missing: []string{"internet"}, mentions: "hooks.example.org"},
		{name: "a webhook through NAT", n: nat, cfg: LaunchConfig{CompletionWebhookURL: "https://hooks.example.org/x"}},
		{name: "Windows needs the internet", n: fullPrivateNetwork(), cfg: LaunchConfig{TargetOS: "windows"}, missing: []string{"internet"}, mentions: "awscli.amazonaws.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := CheckPrivateNetwork(tc.n, tc.cfg, nil)
			if got := missingNames(c); !reflect.DeepEqual(got, tc.missing) {
				t.Fatalf("missing = %v, want %v (%q)", got, tc.missing, c.Missing)
			}
			if tc.mentions != "" && !strings.Contains(strings.Join(c.Missing, "\n"), tc.mentions) {
				t.Errorf("missing %q does not say %q", c.Missing, tc.mentions)
			}
			if c.OK() != (len(tc.missing) == 0) {
				t.Errorf("OK() = %t with missing %v", c.OK(), c.Missing)
			}
			if (len(c.Warnings) > 0) != tc.warns {
				t.Errorf("warnings = %q, want any: %t", c.Warnings, tc.warns)
			}
		})
	}

	// The CLI's extra needs (the MPI bootstrap's EFA installer) are met only by NAT.
	if c := CheckPrivateNetwork(fullPrivateNetwork(), LaunchConfig{}, []string{"the EFA installer"}); c.OK() {
		t.Error("an internet need passed a VPC with endpoints and no NAT")
	}
	if c := CheckPrivateNetwork(nat, LaunchConfig{}, []string{"the EFA installer"}); !c.OK() {
		t.Errorf("an internet need refused with a NAT route: %q", c.Missing)
	}
}

// Every LaunchConfig field naming a URL or endpoint is somewhere spored or the
// bootstrap sends traffic. With --no-public-ip each must either be a need only
// NAT meets or be refused, so a new one cannot quietly fail on a private
// instance. Discovered by reflection.
func TestEveryLaunchURLFieldIsAPrivateNeedOrRefused(t *testing.T) {
	typ := reflect.TypeOf(LaunchConfig{})
	found := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String || !(strings.HasSuffix(f.Name, "URL") || strings.HasSuffix(f.Name, "Endpoint")) {
			continue
		}
		found++
		cfg := privateCfg
		reflect.ValueOf(&cfg).Elem().Field(i).SetString("https://abc.example.org/hook")
		if cfg.ValidateNoPublicIP() != nil {
			continue
		}
		if len(privateInternetNeeds(cfg)) == 0 {
			t.Errorf("LaunchConfig.%s is a URL, and with --no-public-ip it is neither refused (ValidateNoPublicIP) nor an "+
				"internet need (privateInternetNeeds): a private instance would fail to reach it with nothing said", f.Name)
		}
	}
	if found < 3 {
		t.Fatalf("found %d URL/endpoint fields; the discovery has broken", found)
	}
}

func TestValidateNoPublicIP(t *testing.T) {
	for _, tc := range []struct {
		cfg  LaunchConfig
		want string // "" = accepted
	}{
		{LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1"}, ""},
		{LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-west-2a"}, ""},
		{LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", AllowMissingEndpoints: true}, ""},
		{LaunchConfig{NoPublicIP: true}, "--subnet-id, or --az"},
		{LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", DNSName: "box"}, "cannot register DNS"},
		{LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", DNSDomain: "example.org"}, "custom DNS target"},
		{LaunchConfig{AllowMissingEndpoints: true}, "only with --no-public-ip"},
		{LaunchConfig{DNSName: "box"}, ""},
	} {
		err := tc.cfg.ValidateNoPublicIP()
		if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: err = %v, want %q", tc.cfg, err, tc.want)
		}
	}
	if err := (LaunchConfig{NoPublicIP: true}).ValidateNoPublicIP(); !errors.Is(err, ErrNoPublicIPNeedsSubnet) {
		t.Errorf("err = %v, want ErrNoPublicIPNeedsSubnet", err)
	}
}

// --- through Client.Launch, wire level ---

// privateFake is a fake EC2 whose subnet-1 (vpc-1, us-west-2a) has route
// table rtb-1 and the given endpoints and default route.
func privateFake(endpoints, defaultRoute string) *fakeEC2 {
	return &fakeEC2{
		subnets: []fakeSubnet{{id: "subnet-1", vpc: "vpc-1", az: "us-west-2a", free: 200}},
		routeTablesXML: `<item><routeTableId>rtb-1</routeTableId><vpcId>vpc-1</vpcId><routeSet>` +
			`<item><destinationCidrBlock>172.31.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId><state>active</state></item>` +
			defaultRoute + `</routeSet><associationSet><item><routeTableAssociationId>rtbassoc-1</routeTableAssociationId>` +
			`<routeTableId>rtb-1</routeTableId><main>true</main></item></associationSet></item>`,
		vpcEndpointsXML: endpoints,
	}
}

func gatewayEndpointXML(id, svc string) string {
	return `<item><vpcEndpointId>` + id + `</vpcEndpointId><vpcEndpointType>Gateway</vpcEndpointType><vpcId>vpc-1</vpcId>` +
		`<serviceName>com.amazonaws.` + pnRegion + `.` + svc + `</serviceName><state>available</state>` +
		`<routeTableIdSet><item>rtb-1</item></routeTableIdSet><privateDnsEnabled>false</privateDnsEnabled></item>`
}

func interfaceEndpointXML(id, svc string, privateDNS bool) string {
	dns := "false"
	if privateDNS {
		dns = "true"
	}
	return `<item><vpcEndpointId>` + id + `</vpcEndpointId><vpcEndpointType>Interface</vpcEndpointType><vpcId>vpc-1</vpcId>` +
		`<serviceName>com.amazonaws.` + pnRegion + `.` + svc + `</serviceName><state>available</state>` +
		`<subnetIdSet><item>subnet-1</item></subnetIdSet><privateDnsEnabled>` + dns + `</privateDnsEnabled></item>`
}

func allEndpointsXML() string {
	x := gatewayEndpointXML("vpce-s3", "s3")
	for _, svc := range []string{"ec2", "ssm", "ssmmessages", "ec2messages"} {
		x += interfaceEndpointXML("vpce-"+svc, svc, true)
	}
	return x
}

const natRouteXML = `<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><natGatewayId>nat-1</natGatewayId><state>active</state></item>`

func multiCardPrivate(lc LaunchConfig) LaunchConfig {
	lc.InstanceType, lc.Region, lc.AMI, lc.PricePerHour = "c8gn.48xlarge", pnRegion, "ami-0123456789abcdef0", 1
	lc.EFAEnabled, lc.EFAInterfaces = true, 2
	return lc
}

// The gate for the issue's ask: a multi-card EFA launch with --no-public-ip
// requests no public IP on any interface, and makes no Elastic IP call at all:
// no AllocateAddress, no AssociateAddress, nothing to release.
func TestLaunch_NoPublicIPMultiCardEFASendsNoPublicIPAndNoElasticIP(t *testing.T) {
	fastEIPPolling(t)
	f := privateFake(allEndpointsXML(), "")
	f.efaCards = 2
	c := newFakeEC2Client(t, f)
	res, err := c.Launch(context.Background(), multiCardPrivate(LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"}}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run := f.request("RunInstances")
	if run == nil {
		t.Fatalf("no RunInstances (actions %v)", f.actions())
	}
	for k, want := range map[string]string{
		"NetworkInterface.1.InterfaceType": "efa", "NetworkInterface.2.InterfaceType": "efa-only",
		"NetworkInterface.1.AssociatePublicIpAddress": "", "NetworkInterface.2.AssociatePublicIpAddress": "",
	} {
		if got := run.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, a := range []string{"AllocateAddress", "AssociateAddress", "DescribeAddresses", "ReleaseAddress"} {
		if n := countAction(f, a); n != 0 {
			t.Errorf("%d %s call(s) with --no-public-ip (actions %v)", n, a, f.actions())
		}
	}
	if res.PublicIP != "" {
		t.Errorf("PublicIP = %q, want none", res.PublicIP)
	}
	if f.request("DescribeVpcEndpoints") == nil || f.request("DescribeRouteTables") == nil {
		t.Errorf("the subnet was not checked (actions %v)", f.actions())
	}
}

// Without the flag nothing changes: the same multi-card launch still gets its
// Elastic IP, and a single-interface launch still asks for a public IP.
func TestLaunch_WithoutNoPublicIPTheElasticIPAndPublicIPAreUnchanged(t *testing.T) {
	fastEIPPolling(t)
	f := privateFake(allEndpointsXML(), "")
	f.efaCards = 2
	c := newFakeEC2Client(t, f)
	if _, err := c.Launch(context.Background(), multiCardPrivate(LaunchConfig{SubnetID: "subnet-1"})); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if countAction(f, "AllocateAddress") != 1 || countAction(f, "AssociateAddress") != 1 {
		t.Errorf("multi-card launch without --no-public-ip: actions %v, want one AllocateAddress and one AssociateAddress", f.actions())
	}
	if f.request("DescribeVpcEndpoints") != nil {
		t.Error("a launch without --no-public-ip checked the subnet's endpoints")
	}
	run := launchWith(t, &fakeEC2{}, LaunchConfig{})
	if got := run.Get("NetworkInterface.1.AssociatePublicIpAddress"); got != "true" {
		t.Errorf("single interface without --no-public-ip: AssociatePublicIpAddress = %q, want true", got)
	}
}

// A single interface must say false: left unset, a default subnet
// (MapPublicIpOnLaunch) would assign a public IP anyway.
func TestLaunch_NoPublicIPSingleInterfaceAsksForNone(t *testing.T) {
	for _, lc := range []LaunchConfig{
		{NoPublicIP: true, SubnetID: "subnet-1"},
		{NoPublicIP: true, SubnetID: "subnet-1", EFAEnabled: true}, // the single EFA interface
	} {
		run := launchWith(t, privateFake(allEndpointsXML(), ""), lc)
		if got := run.Get("NetworkInterface.1.AssociatePublicIpAddress"); got != "false" {
			t.Errorf("%+v: AssociatePublicIpAddress = %q, want false", lc, got)
		}
		if got := run.Get("NetworkInterface.1.SubnetId"); got != "subnet-1" {
			t.Errorf("%+v: SubnetId = %q, want subnet-1", lc, got)
		}
	}
}

// The pre-flight: no S3 endpoint and no NAT is refused before RunInstances,
// naming what is missing; a NAT route alone passes; --allow-missing-endpoints
// launches past the refusal.
func TestLaunch_NoPublicIPRefusesASubnetThatCannotServeIt(t *testing.T) {
	launch := func(f *fakeEC2, lc LaunchConfig) error {
		c := newFakeEC2Client(t, f)
		lc.InstanceType, lc.Region, lc.AMI, lc.PricePerHour = "m8a.4xlarge", pnRegion, "ami-0123456789abcdef0", 1
		_, err := c.Launch(context.Background(), lc)
		return err
	}
	noS3 := privateFake(strings.Replace(allEndpointsXML(), gatewayEndpointXML("vpce-s3", "s3"), "", 1), "")
	err := launch(noS3, LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1"})
	if !errors.Is(err, ErrPrivateNetworkIncomplete) || !strings.Contains(err.Error(), "com.amazonaws.us-west-2.s3") ||
		!strings.Contains(err.Error(), "--allow-missing-endpoints") {
		t.Fatalf("err = %v, want the refusal naming the S3 endpoint and the override", err)
	}
	if noS3.request("RunInstances") != nil {
		t.Error("RunInstances was called for a refused subnet")
	}

	nat := privateFake("", natRouteXML)
	if err := launch(nat, LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1"}); err != nil {
		t.Errorf("a NAT route with no endpoints was refused: %v", err)
	}
	if nat.request("RunInstances") == nil {
		t.Error("no RunInstances behind a NAT route")
	}

	noDNS := privateFake(strings.Replace(allEndpointsXML(), interfaceEndpointXML("vpce-ssm", "ssm", true), interfaceEndpointXML("vpce-ssm", "ssm", false), 1), "")
	if err := launch(noDNS, LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1"}); err == nil || !strings.Contains(err.Error(), "private DNS disabled") {
		t.Errorf("err = %v, want the private-DNS refusal", err)
	}

	allowed := privateFake("", "")
	if err := launch(allowed, LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", AllowMissingEndpoints: true}); err != nil {
		t.Errorf("--allow-missing-endpoints still refused: %v", err)
	}
	if allowed.request("RunInstances") == nil {
		t.Error("--allow-missing-endpoints did not launch")
	}
}

// With only an AZ the subnet is picked and pinned, so the instance launches in
// the subnet that was checked.
func TestLaunch_NoPublicIPPinsTheSubnetItChecked(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	run := launchWith(t, f, LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-west-2a", VPCID: "vpc-1"})
	if got := run.Get("NetworkInterface.1.SubnetId"); got != "subnet-1" {
		t.Errorf("SubnetId = %q, want subnet-1 (the one checked)", got)
	}
}

// The pure refusals come before any AWS call.
func TestLaunch_NoPublicIPWithoutSubnetOrAZMakesNoAWSCall(t *testing.T) {
	f := &fakeEC2{}
	c := newFakeEC2Client(t, f)
	for _, lc := range []LaunchConfig{
		{NoPublicIP: true},
		{NoPublicIP: true, SubnetID: "subnet-1", DNSName: "box"},
		{AllowMissingEndpoints: true},
	} {
		lc.InstanceType, lc.Region, lc.AMI, lc.PricePerHour = "m8a.4xlarge", pnRegion, "ami-0123456789abcdef0", 1
		if _, err := c.Launch(context.Background(), lc); err == nil {
			t.Errorf("%+v launched", lc)
		}
	}
	if a := f.actions(); len(a) != 0 {
		t.Errorf("AWS calls before refusing: %v", a)
	}
}

// The CLI checks before creating anything, and Launch checks again: the
// second is the client's cache, not three more Describe calls.
func TestPreflightNoPublicIPIsCachedPerSubnet(t *testing.T) {
	f := privateFake(allEndpointsXML(), "")
	c := newFakeEC2Client(t, f)
	for i := 0; i < 3; i++ {
		cfg := privateCfg
		if _, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if n := countAction(f, "DescribeVpcEndpoints"); n != 1 {
		t.Errorf("%d DescribeVpcEndpoints calls for one subnet checked three times, want 1", n)
	}
}

// What DescribePrivateNetwork reads off the wire is what the check decides on.
func TestDescribePrivateNetworkReadsTheWire(t *testing.T) {
	// An S3 interface endpoint beside the gateway one, with private DNS only
	// for inbound Resolver endpoints: the VPC's own instances do not use it.
	s3i := strings.Replace(interfaceEndpointXML("vpce-s3i", "s3", true), `</privateDnsEnabled>`,
		`</privateDnsEnabled><dnsOptions><dnsRecordIpType>ipv4</dnsRecordIpType>`+
			`<privateDnsOnlyForInboundResolverEndpoint>true</privateDnsOnlyForInboundResolverEndpoint></dnsOptions>`, 1)
	f := privateFake(allEndpointsXML()+gatewayEndpointXML("vpce-ddb", "dynamodb")+s3i, natRouteXML)
	c := newFakeEC2Client(t, f)
	n, err := c.DescribePrivateNetwork(context.Background(), pnRegion, "subnet-1")
	if err != nil {
		t.Fatal(err)
	}
	if n.VPCID != "vpc-1" || n.AvailabilityZone != "us-west-2a" || n.RouteTableID != "rtb-1" || n.NATGatewayID != "nat-1" {
		t.Errorf("network = %+v", n)
	}
	got := map[string]PrivateEndpoint{}
	for _, e := range n.Endpoints {
		if e.ID == "vpce-s3i" {
			if e.Type != "Interface" || !e.PrivateDNS || !e.InboundResolverOnly {
				t.Errorf("s3 interface endpoint = %+v, want private DNS only for inbound Resolver endpoints", e)
			}
			continue
		}
		if e.InboundResolverOnly {
			t.Errorf("%s endpoint = %+v: InboundResolverOnly with no dnsOptions saying so", e.Service, e)
		}
		got[e.Service] = e
	}
	if e := got["s3"]; e.Type != "Gateway" || !reflect.DeepEqual(e.RouteTableIDs, []string{"rtb-1"}) {
		t.Errorf("s3 endpoint = %+v", e)
	}
	if e := got["ssm"]; e.Type != "Interface" || !e.PrivateDNS || !reflect.DeepEqual(e.AvailabilityZones, []string{"us-west-2a"}) {
		t.Errorf("ssm endpoint = %+v", e)
	}
	if len(got) != 6 {
		t.Errorf("endpoints = %v, want 6", got)
	}
}

// #1443: the CLI checks the subnet, then Launch checks it again (and a job
// array once per member). The repeat must print nothing the first printed: on
// a live launch every cross-AZ warning appeared twice.
func TestPreflightNoPublicIPRepeatPrintsNothingNew(t *testing.T) {
	// The interface endpoints sit only in subnet-2 (us-west-2b), so subnet-1
	// (us-west-2a) gets a cross-AZ warning for each.
	endpoints := strings.ReplaceAll(allEndpointsXML(), "<subnetIdSet><item>subnet-1</item>", "<subnetIdSet><item>subnet-2</item>")
	f := privateFake(endpoints, "")
	f.subnets = append(f.subnets, fakeSubnet{id: "subnet-2", vpc: "vpc-1", az: "us-west-2b", free: 200})
	c := newFakeEC2Client(t, f)

	var first, second bytes.Buffer
	for _, out := range []*bytes.Buffer{&first, &second} {
		cfg := privateCfg
		cfg.AllowMissingEndpoints = true
		if _, err := c.PreflightNoPublicIP(context.Background(), &cfg, nil, out); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(first.String(), "crosses AZs"); n != 4 {
		t.Fatalf("first check printed %d cross-AZ warnings, want 4 (one per interface endpoint):\n%s", n, first.String())
	}
	if second.Len() != 0 {
		t.Errorf("the repeated check printed again:\n%s", second.String())
	}
}

// --- a NAT instance as the way out (#1515) ---

// natAPI is a typed fake privateNetworkAPI: subnet-1 in vpc-1 has route table
// rtb-priv, whose 0.0.0.0/0 route is the case's; subnet-pub's rtb-pub sends
// 0.0.0.0/0 to igw-1; subnet-closed's rtb-closed has no default route.
type natAPI struct {
	route     types.Route
	instances []types.Instance
	descErr   error
	described int
}

func (f *natAPI) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{
		{SubnetId: aws.String("subnet-1"), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("us-west-2a")},
	}}, nil
}

func (f *natAPI) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	local := types.Route{DestinationCidrBlock: aws.String("10.0.0.0/16"), GatewayId: aws.String("local"), State: types.RouteStateActive}
	table := func(id, subnet string, routes ...types.Route) types.RouteTable {
		return types.RouteTable{RouteTableId: aws.String(id), VpcId: aws.String("vpc-1"), Routes: append([]types.Route{local}, routes...),
			Associations: []types.RouteTableAssociation{{SubnetId: aws.String(subnet)}}}
	}
	main := types.RouteTable{RouteTableId: aws.String("rtb-main"), VpcId: aws.String("vpc-1"), Routes: []types.Route{local},
		Associations: []types.RouteTableAssociation{{Main: aws.Bool(true)}}}
	return &ec2.DescribeRouteTablesOutput{RouteTables: []types.RouteTable{
		main,
		table("rtb-priv", "subnet-1", f.route),
		table("rtb-pub", "subnet-pub", types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-1"), State: types.RouteStateActive}),
		table("rtb-closed", "subnet-closed"),
	}}, nil
}

func (f *natAPI) DescribeVpcEndpoints(context.Context, *ec2.DescribeVpcEndpointsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcEndpointsOutput, error) {
	return &ec2.DescribeVpcEndpointsOutput{}, nil
}

func (f *natAPI) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.described++
	if f.descErr != nil {
		return nil, f.descErr
	}
	var out []types.Instance
	for _, i := range f.instances {
		if containsString(in.InstanceIds, aws.ToString(i.InstanceId)) {
			out = append(out, i)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: out}}}, nil
}

// natInstance is a working NAT instance i-nat: running, its primary interface
// eni-nat in subnet-pub with the source/destination check off and a public IP.
// The EC2 API reports the check on the instance too, for the primary.
func natInstance(edit ...func(*types.Instance)) types.Instance {
	i := types.Instance{
		InstanceId: aws.String("i-nat"), VpcId: aws.String("vpc-1"), SubnetId: aws.String("subnet-pub"),
		State:           &types.InstanceState{Name: types.InstanceStateNameRunning},
		SourceDestCheck: aws.Bool(false), PublicIpAddress: aws.String("203.0.113.7"),
		NetworkInterfaces: []types.InstanceNetworkInterface{{
			NetworkInterfaceId: aws.String("eni-nat"), SubnetId: aws.String("subnet-pub"), VpcId: aws.String("vpc-1"),
			SourceDestCheck: aws.Bool(false), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
			Association: &types.InstanceNetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.7")},
		}},
	}
	for _, e := range edit {
		e(&i)
	}
	return i
}

// twoInterfaceNAT is a working two-interface NAT i-nat: its primary, inside
// interface eni-in in subnet-1 forwards and has no public IP; its outside
// interface eni-out, in outsideSubnet, has the public IP and keeps its
// source/destination check on.
func twoInterfaceNAT(outsideSubnet string) types.Instance {
	return natInstance(func(i *types.Instance) {
		i.SubnetId, i.PublicIpAddress = aws.String("subnet-1"), nil
		i.NetworkInterfaces = []types.InstanceNetworkInterface{{
			NetworkInterfaceId: aws.String("eni-in"), SubnetId: aws.String("subnet-1"), VpcId: aws.String("vpc-1"),
			SourceDestCheck: aws.Bool(false), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
		}, {
			NetworkInterfaceId: aws.String("eni-out"), SubnetId: aws.String(outsideSubnet), VpcId: aws.String("vpc-1"),
			SourceDestCheck: aws.Bool(true), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(1)},
			Association: &types.InstanceNetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.8")},
		}}
	})
}

func defaultRouteTo(instance, eni string) types.Route {
	r := types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), State: types.RouteStateActive}
	if instance != "" {
		r.InstanceId = aws.String(instance)
	}
	if eni != "" {
		r.NetworkInterfaceId = aws.String(eni)
	}
	return r
}

// The subnet has no VPC endpoints at all, and the launch needs the internet
// (the EFA installer), so only a way out through the default route passes.
func TestNATInstanceIsAWayOut(t *testing.T) {
	const notNAT = "0.0.0.0/0 goes to instance i-nat"
	secondary := func(check bool) func(*types.Instance) {
		return func(i *types.Instance) {
			i.NetworkInterfaces = append(i.NetworkInterfaces, types.InstanceNetworkInterface{
				NetworkInterfaceId: aws.String("eni-2"), SubnetId: aws.String("subnet-pub"), VpcId: aws.String("vpc-1"),
				SourceDestCheck: aws.Bool(check), Attachment: &types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(1)},
			})
		}
	}
	for _, tc := range []struct {
		name      string
		api       natAPI
		ok        bool
		mentions  string // in Met when ok, else in every Missing entry
		absent    string // in no Missing entry
		described int    // DescribeInstances calls
	}{
		{name: "a working NAT instance, routed by instance ID", ok: true, described: 1, mentions: "everything, through NAT instance i-nat on route table rtb-priv",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance()}}},
		{name: "a working NAT instance, routed by its interface", ok: true, described: 1, mentions: "everything, through NAT instance i-nat on route table rtb-priv",
			api: natAPI{route: defaultRouteTo("i-nat", "eni-nat"), instances: []types.Instance{natInstance()}}},
		// The routed interface's check is the one that matters, not the primary's.
		{name: "routed through a secondary interface that forwards", ok: true, described: 1, mentions: "NAT instance i-nat",
			api: natAPI{route: defaultRouteTo("i-nat", "eni-2"), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.SourceDestCheck, i.NetworkInterfaces[0].SourceDestCheck = aws.Bool(true), aws.Bool(true)
			}, secondary(false))}}},
		{name: "routed through a secondary interface that does not forward", described: 1,
			mentions: notNAT + " (network interface eni-2), which is not a working NAT instance: its source/destination check is on (network interface eni-2)",
			api:      natAPI{route: defaultRouteTo("i-nat", "eni-2"), instances: []types.Instance{natInstance(secondary(true))}}},
		{name: "source/destination check on", described: 1,
			mentions: notNAT + ", which is not a working NAT instance: its source/destination check is on (network interface eni-nat)",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.SourceDestCheck, i.NetworkInterfaces[0].SourceDestCheck = aws.Bool(true), aws.Bool(true)
			})}}},
		{name: "no public IP", described: 1,
			mentions: notNAT + ", which is not a working NAT instance: it has no public IPv4 address",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.PublicIpAddress, i.NetworkInterfaces[0].Association = nil, nil
			})}}},
		{name: "the NAT's subnet has no internet gateway route", described: 1,
			mentions: notNAT + ", which is not a working NAT instance: no interface with a public IPv4 address sits in a subnet routed to an internet gateway: " +
				"network interface eni-nat is in subnet-closed, whose route table rtb-closed has no 0.0.0.0/0 route to an internet gateway",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.SubnetId, i.NetworkInterfaces[0].SubnetId = aws.String("subnet-closed"), aws.String("subnet-closed")
			})}}},
		// A single-interface NAT in the private subnet itself: its public IP
		// leaves by a subnet whose default route points back at it.
		{name: "a single-interface NAT in the subnet that routes to it", described: 1,
			mentions: "network interface eni-nat is in subnet-1, whose route table rtb-priv sends 0.0.0.0/0 to instance i-nat, not to an internet gateway",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.SubnetId, i.NetworkInterfaces[0].SubnetId = aws.String("subnet-1"), aws.String("subnet-1")
			})}}},
		// A two-interface NAT: the routed inside interface sits in the private
		// subnet (whose default route points back at it), the public IP on the
		// outside interface in a public subnet. The egress check belongs to the
		// interface with the public IP; the forwarding check to the routed one.
		{name: "a two-interface NAT, inside interface routed, outside one public", ok: true, described: 1,
			mentions: "everything, through NAT instance i-nat on route table rtb-priv",
			api:      natAPI{route: defaultRouteTo("i-nat", "eni-in"), instances: []types.Instance{twoInterfaceNAT("subnet-pub")}}},
		{name: "a two-interface NAT routed by instance ID uses its inside primary", ok: true, described: 1,
			mentions: "NAT instance i-nat",
			api:      natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{twoInterfaceNAT("subnet-pub")}}},
		{name: "a two-interface NAT whose public interface has no internet gateway route", described: 1,
			mentions: notNAT + " (network interface eni-in), which is not a working NAT instance: no interface with a public IPv4 address sits in a " +
				"subnet routed to an internet gateway: network interface eni-out is in subnet-closed, whose route table rtb-closed has no 0.0.0.0/0 route to an internet gateway",
			api: natAPI{route: defaultRouteTo("i-nat", "eni-in"), instances: []types.Instance{twoInterfaceNAT("subnet-closed")}}},
		{name: "a NAT instance that is not running (route not yet blackholed)", described: 1,
			mentions: notNAT + ", which is not a working NAT instance: it is stopping, not running",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.State.Name = types.InstanceStateNameStopping
			})}}},
		{name: "every failed check is named", described: 1,
			mentions: "it is stopping, not running; its source/destination check is on (network interface eni-nat), so it drops traffic it is not the destination of; it has no public IPv4 address",
			api: natAPI{route: defaultRouteTo("i-nat", ""), instances: []types.Instance{natInstance(func(i *types.Instance) {
				i.State.Name = types.InstanceStateNameStopping
				i.SourceDestCheck, i.NetworkInterfaces[0].SourceDestCheck = aws.Bool(true), aws.Bool(true)
				i.PublicIpAddress, i.NetworkInterfaces[0].Association = nil, nil
			})}}},
		{name: "a stopped NAT's blackholed route is no route", described: 0, mentions: "no NAT route in rtb-priv", absent: "0.0.0.0/0 goes to",
			api: natAPI{route: func() types.Route {
				r := defaultRouteTo("i-nat", "eni-nat")
				r.State = types.RouteStateBlackhole
				return r
			}()}},
		{name: "an interface attached to no instance", described: 0,
			mentions: "0.0.0.0/0 goes to network interface eni-x, which is not a working NAT instance: the network interface is attached to no instance",
			api:      natAPI{route: defaultRouteTo("", "eni-x")}},
		{name: "the instance cannot be read", described: 1,
			mentions: notNAT + ", which is not a working NAT instance: spawn could not read it (ec2:DescribeInstances: denied)",
			api:      natAPI{route: defaultRouteTo("i-nat", ""), descErr: errors.New("denied")}},
		{name: "the instance is gone", described: 1, mentions: "spawn could not find it",
			api: natAPI{route: defaultRouteTo("i-nat", "")}},
		{name: "a NAT gateway is unchanged", ok: true, described: 0, mentions: "everything, through NAT gateway nat-1 on route table rtb-priv",
			api: natAPI{route: types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String("nat-1"), State: types.RouteStateActive}}},
		{name: "an internet gateway is unchanged", described: 0,
			mentions: "0.0.0.0/0 goes to internet gateway igw-9, which spawn does not count as a way out for an instance with no public IP",
			api:      natAPI{route: types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-9"), State: types.RouteStateActive}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := describePrivateNetwork(context.Background(), &tc.api, pnRegion, "subnet-1")
			if err != nil {
				t.Fatal(err)
			}
			if tc.api.described != tc.described {
				t.Errorf("%d DescribeInstances calls, want %d", tc.api.described, tc.described)
			}
			c := CheckPrivateNetwork(n, privateCfg, []string{"the EFA installer"})
			if c.OK() != tc.ok {
				t.Fatalf("OK() = %t, want %t: met %q, missing %q", c.OK(), tc.ok, c.Met, c.Missing)
			}
			if tc.ok {
				if len(c.Met) != 1 || !strings.Contains(c.Met[0], tc.mentions) {
					t.Errorf("met = %q, want it to say %q", c.Met, tc.mentions)
				}
				instance := strings.Contains(tc.mentions, "NAT instance")
				if warned := len(c.Warnings) == 1 && strings.Contains(c.Warnings[0], "trusts the route"); warned != instance {
					t.Errorf("warnings = %q; a NAT instance must warn that spawn trusts the route, and nothing else may", c.Warnings)
				}
				return
			}
			for _, m := range c.Missing {
				if !strings.Contains(m, tc.mentions) {
					t.Errorf("missing entry %q does not say %q", m, tc.mentions)
				}
				if tc.absent != "" && strings.Contains(m, tc.absent) {
					t.Errorf("missing entry %q says %q", m, tc.absent)
				}
			}
		})
	}
}
