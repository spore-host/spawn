package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/sporedinstall"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// A private-only launch (LaunchConfig.NoPublicIP, #1433) has no route to the
// public AWS endpoints unless its subnet has a NAT route or VPC endpoints for
// them. Without them the bootstrap cannot download spored, spored cannot read
// its tags or terminate the instance at its TTL or cost limit, and the SSM
// agent cannot register, so `spawn connect` has nothing to reach: the instance
// runs, and bills, with no safety net. So the launch checks the subnet first
// and refuses one that cannot serve it.

// PrivateService is one thing a private-only instance must reach.
type PrivateService struct {
	// Endpoint is the VPC endpoint service, com.amazonaws.<region>.<Endpoint>.
	// Empty for a need no VPC endpoint serves, which only a NAT route meets.
	Endpoint string
	// IAMPrefixes are the IAM service prefixes of spored's own AWS calls this
	// entry serves. The discovery gate (internal/iamcoverage) holds every
	// prefix spored's call graph reaches to this table, so a new service
	// spored starts calling cannot be forgotten here. Empty for the SSM
	// agent's services, which spored does not call.
	IAMPrefixes []string
	// Gateway: a gateway endpoint associated with the subnet's route table
	// serves it, as well as an interface endpoint.
	Gateway bool
	// Why is what fails without it.
	Why string
	// When is the condition under which a launch needs it; "" for every
	// private-only launch.
	When    string
	applies func(LaunchConfig) bool
}

// AppliesTo reports whether a launch of cfg needs s.
func (s PrivateService) AppliesTo(cfg LaunchConfig) bool {
	return s.applies == nil || s.applies(cfg)
}

func privateNever(LaunchConfig) bool { return false }

// privateServices is every service a private-only instance can need. Its
// spored half is held to spored's call graph by
// TestPrivateServicesCoverEverySporedService.
var privateServices = []PrivateService{
	{
		// Every S3 name the bootstrap uses is the regional one (#1436,
		// TestEveryBootstrapS3HostIsRegional), which both a gateway endpoint
		// and an interface endpoint's private DNS serve.
		Endpoint: "s3", IAMPrefixes: []string{"s3"}, Gateway: true,
		// The same bucket mirrors the pinned AWS CLI an image without one
		// (Ubuntu) installs (#1494), so no Linux launch needs the internet for it.
		Why: "the bootstrap downloads spored (and, on an image with no aws CLI, the pinned AWS CLI) from " +
			"https://spawn-binaries-<region>.s3.<region>.amazonaws.com, and spored reads its launch config object and stages data",
	},
	{
		Endpoint: "ec2", IAMPrefixes: []string{"ec2"},
		Why: "spored reads its tags and stops or terminates the instance at its TTL, idle timeout and cost limit",
	},
	{Endpoint: "ssm", Why: "the SSM agent registers here: `spawn connect` and spored's verification go over SSM"},
	{Endpoint: "ssmmessages", Why: "the SSM agent's session channel: `spawn connect`"},
	{Endpoint: "ec2messages", Why: "the SSM agent's command channel: `spawn connect -- cmd` and spored's verification"},
	{
		Endpoint: "dynamodb", IAMPrefixes: []string{"dynamodb"}, Gateway: true,
		Why:  "spored registers in and reads the hybrid peer registry",
		When: "a `spawn burst` member (spawn:burst=true)",
		applies: func(cfg LaunchConfig) bool {
			return cfg.Tags[tagprefix.Tag("burst")] == "true"
		},
	},
	{
		Endpoint: "fsx", IAMPrefixes: []string{"fsx"},
		Why:  "spored waits for the filesystem and links its S3 data repository",
		When: "an FSx for Lustre created with the launch (--fsx-create)",
		applies: func(cfg LaunchConfig) bool {
			return cfg.FSxLustreCreate || cfg.FSxPending != ""
		},
	},
	{
		Endpoint: "sqs", IAMPrefixes: []string{"sqs"},
		Why:     "a pool worker polls its queue",
		When:    "a pool worker (spawn pool); no pool launch takes --no-public-ip",
		applies: privateNever,
	},
	{
		Endpoint: "xray", IAMPrefixes: []string{"xray"},
		Why:     "spored exports traces",
		When:    "tracing to X-Ray, which a launch refuses (#1335)",
		applies: privateNever,
	},
	{
		IAMPrefixes: []string{"lambda"},
		Why:         "spored registers DNS through the updater's Lambda Function URL, which no VPC endpoint serves",
		When:        "DNS registration, which --no-public-ip refuses",
		applies:     func(cfg LaunchConfig) bool { return cfg.DNSName != "" },
	},
}

// PrivateServices returns every service a private-only instance can need,
// with the condition under which it does.
func PrivateServices() []PrivateService {
	return append([]PrivateService(nil), privateServices...)
}

// privateInternetNeeds are the places outside AWS a launch of cfg sends
// traffic to, which only a NAT route reaches. Every LaunchConfig URL or
// endpoint field is here or refused by ValidateNoPublicIP
// (TestEveryLaunchURLFieldIsAPrivateNeedOrRefused).
func privateInternetNeeds(cfg LaunchConfig) []string {
	var out []string
	add := func(what, raw string) {
		if raw == "" {
			return
		}
		host := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		out = append(out, what+" ("+host+")")
	}
	add("notifications", cfg.NotifyURL)
	add("the Spot interruption webhook", cfg.SpotInterruptionWebhookURL)
	add("the completion webhook", cfg.CompletionWebhookURL)
	add("the custom DNS updater", cfg.DNSEndpoint)
	if cfg.TargetOS == "windows" {
		out = append(out, "the Windows bootstrap's AWS CLI installer (awscli.amazonaws.com)")
	}
	return out
}

// ErrNoPublicIPNeedsSubnet is wrapped by [LaunchConfig.ValidateNoPublicIP]: a
// private-only launch named neither a subnet nor an AZ, so there is no subnet
// to check (#1433).
var ErrNoPublicIPNeedsSubnet = errors.New("--no-public-ip needs a subnet")

// ErrPrivateNetworkIncomplete is wrapped by a refused private-only launch:
// the subnet cannot reach something the instance needs (#1433).
var ErrPrivateNetworkIncomplete = errors.New("the subnet cannot serve a private-only instance")

// ValidateNoPublicIP is the check NoPublicIP needs no AWS call for (#1433):
// AllowMissingEndpoints only with NoPublicIP; a subnet or an AZ to check; and
// nothing that needs a public IP. DNS registration writes the instance's
// public IP, and there is none. Pure; [Client.Launch] calls it before any AWS
// call.
func (cfg LaunchConfig) ValidateNoPublicIP() error {
	if !cfg.NoPublicIP {
		if cfg.AllowMissingEndpoints {
			return errors.New("--allow-missing-endpoints applies only with --no-public-ip")
		}
		return nil
	}
	switch {
	case cfg.DNSName != "":
		return fmt.Errorf("--no-public-ip cannot register DNS (%s): spored registers the instance's public IP, and it has none. "+
			"Pass --no-dns, or drop --no-public-ip", cfg.DNSName)
	case cfg.DNSDomain != "" || cfg.DNSEndpoint != "" || cfg.DNSFunctionARN != "":
		return errors.New("--no-public-ip cannot use a custom DNS target (--dns-domain, --dns-api-endpoint, --dns-function-arn): " +
			"spored registers the instance's public IP, and it has none")
	case cfg.SubnetID == "" && cfg.AvailabilityZone == "":
		return fmt.Errorf("%w: pass --subnet-id, or --az to let spawn pick one. The VPC endpoints and route a private "+
			"instance needs are a subnet's, so spawn checks the one it launches in", ErrNoPublicIPNeedsSubnet)
	}
	return nil
}

// PrivateNetwork is what decides whether a subnet can serve a private-only
// instance: its VPC and AZ, its route table's default route, and the VPC's
// endpoints.
type PrivateNetwork struct {
	Region, SubnetID, VPCID, AvailabilityZone string
	// RouteTableID is the subnet's route table: its explicit association, else
	// the VPC's main table.
	RouteTableID string
	// NATGatewayID is the NAT gateway the route table sends 0.0.0.0/0 to.
	NATGatewayID string
	// NATInstanceID is the instance the route table sends 0.0.0.0/0 to (#1515),
	// directly or through one of its network interfaces. It is a way out only
	// when NATInstanceProblem is empty.
	NATInstanceID string
	// NATInstanceProblem is why the route's instance, or its network interface
	// with no instance, is not a working NAT instance: every check it failed.
	NATInstanceProblem string
	// DefaultRoute describes a 0.0.0.0/0 route to anything but a NAT gateway (an
	// internet gateway, a transit gateway, an instance or network interface).
	// Of these spawn counts only a working NAT instance as a way out.
	DefaultRoute string
	Endpoints    []PrivateEndpoint
}

// PrivateEndpoint is one of the VPC's endpoints.
type PrivateEndpoint struct {
	ID string
	// Service is the endpoint service's last component: "s3", "ssm".
	Service string
	// Type is "Gateway" or "Interface".
	Type       string
	State      string
	PrivateDNS bool
	// InboundResolverOnly is an interface endpoint's "private DNS only for
	// inbound Resolver endpoints" (S3's, beside a gateway endpoint): the
	// VPC's own instances do not resolve to it, but to the gateway endpoint.
	InboundResolverOnly bool
	// RouteTableIDs are a gateway endpoint's route tables.
	RouteTableIDs []string
	// AvailabilityZones are an interface endpoint's subnets' AZs.
	AvailabilityZones []string
}

// PrivateCheck is the verdict on a subnet for a private-only launch.
type PrivateCheck struct {
	Network PrivateNetwork
	// Met says how each needed service is reached; Missing what cannot be,
	// and why it matters; Warnings what works with a caveat.
	Met, Missing, Warnings []string
}

// OK reports whether nothing is missing.
func (c PrivateCheck) OK() bool { return len(c.Missing) == 0 }

// CheckPrivateNetwork decides whether n serves a private-only launch of cfg.
// internet are needs only a NAT route meets, beyond what cfg names itself
// (the CLI adds the MPI bootstrap's EFA installer download). Pure.
func CheckPrivateNetwork(n PrivateNetwork, cfg LaunchConfig, internet []string) PrivateCheck {
	out := PrivateCheck{Network: n}
	if n.NATGatewayID != "" {
		out.Met = append(out.Met, fmt.Sprintf("everything, through NAT gateway %s on route table %s", n.NATGatewayID, n.RouteTableID))
		return out
	}
	if n.NATInstanceID != "" && n.NATInstanceProblem == "" {
		out.Met = append(out.Met, fmt.Sprintf("everything, through NAT instance %s on route table %s", n.NATInstanceID, n.RouteTableID))
		out.Warnings = append(out.Warnings, fmt.Sprintf("NAT instance %s is running, has its source/destination check off, has a public IPv4 "+
			"address and sits in a subnet routed to an internet gateway, but spawn cannot see its own IP forwarding and iptables (masquerade) "+
			"configuration, so it trusts the route", n.NATInstanceID))
		return out
	}
	noNAT := "no NAT route in " + orNone(n.RouteTableID)
	switch {
	case n.NATInstanceProblem != "":
		noNAT += " (0.0.0.0/0 goes to " + n.DefaultRoute + ", which is not a working NAT instance: " + n.NATInstanceProblem + ")"
	case n.DefaultRoute != "":
		noNAT += " (0.0.0.0/0 goes to " + n.DefaultRoute + ", which spawn does not count as a way out for an instance with no public IP)"
	}
	for _, s := range privateServices {
		if !s.AppliesTo(cfg) {
			continue
		}
		if s.Endpoint == "" {
			out.Missing = append(out.Missing, fmt.Sprintf("internet: %s; %s", s.Why, noNAT))
			continue
		}
		met, why, warn := endpointFor(n, s)
		if met != "" && s.Endpoint == "s3" && !sporedinstall.Published(n.Region) {
			out.Missing = append(out.Missing, fmt.Sprintf("s3: spawn publishes spored to no bucket in %s, so the bootstrap "+
				"falls back to spawn-binaries-%s, in another region, which %s does not serve, and %s. Needed because %s",
				n.Region, sporedinstall.FallbackRegion, met, noNAT, s.Why))
			continue
		}
		if met != "" {
			out.Met = append(out.Met, s.Endpoint+": "+met)
			if warn != "" {
				out.Warnings = append(out.Warnings, s.Endpoint+": "+warn)
			}
			continue
		}
		out.Missing = append(out.Missing, fmt.Sprintf("%s: %s and %s. Needed because %s", s.Endpoint, why, noNAT, s.Why))
	}
	for _, need := range append(privateInternetNeeds(cfg), internet...) {
		out.Missing = append(out.Missing, fmt.Sprintf("internet: %s, which no VPC endpoint serves; %s", need, noNAT))
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "the subnet's route table"
	}
	return s
}

// endpointFor finds the endpoint serving s in n: how it is met, or why not,
// and any caveat.
func endpointFor(n PrivateNetwork, s PrivateService) (met, why, warn string) {
	name := "com.amazonaws." + n.Region + "." + s.Endpoint
	var reasons []string
	for _, e := range n.Endpoints {
		if e.Service != s.Endpoint {
			continue
		}
		if !strings.EqualFold(e.State, "available") {
			reasons = append(reasons, fmt.Sprintf("%s endpoint %s is %s, not available", strings.ToLower(e.Type), e.ID, e.State))
			continue
		}
		switch {
		case strings.EqualFold(e.Type, "Gateway"):
			if !s.Gateway {
				continue
			}
			if !containsString(e.RouteTableIDs, n.RouteTableID) {
				reasons = append(reasons, fmt.Sprintf("gateway endpoint %s is not associated with the subnet's route table %s", e.ID, n.RouteTableID))
				continue
			}
			return "gateway endpoint " + e.ID, "", ""
		case strings.EqualFold(e.Type, "Interface"):
			if !e.PrivateDNS {
				reasons = append(reasons, fmt.Sprintf("interface endpoint %s has private DNS disabled, so the default %s.%s.amazonaws.com "+
					"name still resolves to the public endpoint; enable private DNS on it", e.ID, s.Endpoint, n.Region))
				continue
			}
			if e.InboundResolverOnly {
				reasons = append(reasons, fmt.Sprintf("interface endpoint %s has private DNS only for inbound Resolver endpoints, "+
					"so this VPC's instances resolve %s.%s.amazonaws.com to the gateway endpoint, not to it", e.ID, s.Endpoint, n.Region))
				continue
			}
			if len(e.AvailabilityZones) == 0 {
				reasons = append(reasons, fmt.Sprintf("interface endpoint %s has no subnets", e.ID))
				continue
			}
			if !containsString(e.AvailabilityZones, n.AvailabilityZone) {
				warn = fmt.Sprintf("interface endpoint %s has no subnet in %s (only %s), so traffic crosses AZs: it works, is charged "+
					"per GB, and fails with that AZ", e.ID, n.AvailabilityZone, strings.Join(e.AvailabilityZones, ", "))
			}
			return "interface endpoint " + e.ID, "", warn
		}
	}
	kind := "interface endpoint"
	if s.Gateway {
		kind = "gateway or interface endpoint"
	}
	why = fmt.Sprintf("no %s (%s)", kind, name)
	if len(reasons) > 0 {
		why += ": " + strings.Join(reasons, "; ")
	}
	return "", why, ""
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// privateNetworkAPI is the slice of EC2 DescribePrivateNetwork reads.
type privateNetworkAPI interface {
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeRouteTables(ctx context.Context, in *ec2.DescribeRouteTablesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	DescribeVpcEndpoints(ctx context.Context, in *ec2.DescribeVpcEndpointsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVpcEndpointsOutput, error)
	// DescribeInstances reads the instance a 0.0.0.0/0 route sends to (#1515).
	// Every launch path already calls it, so the check needs no new IAM; it
	// deliberately reads nothing else (no DescribeNetworkInterfaces).
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// DescribePrivateNetwork reads what [CheckPrivateNetwork] decides on for a
// subnet: read-only (ec2:DescribeSubnets, ec2:DescribeRouteTables,
// ec2:DescribeVpcEndpoints, and ec2:DescribeInstances for a default route to
// an instance), and cached on the client per region and subnet.
func (c *Client) DescribePrivateNetwork(ctx context.Context, region, subnetID string) (PrivateNetwork, error) {
	key := region + "|" + subnetID
	if v, ok := c.privateNets.Load(key); ok {
		return v.(PrivateNetwork), nil
	}
	n, err := describePrivateNetwork(ctx, c.regionalEC2(region), region, subnetID)
	if err != nil {
		return PrivateNetwork{}, err
	}
	c.privateNets.Store(key, n)
	return n, nil
}

func describePrivateNetwork(ctx context.Context, api privateNetworkAPI, region, subnetID string) (PrivateNetwork, error) {
	n := PrivateNetwork{Region: region, SubnetID: subnetID}
	sub, err := api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil {
		return n, fmt.Errorf("describe subnet %s: %w", subnetID, err)
	}
	if len(sub.Subnets) == 0 {
		return n, fmt.Errorf("subnet %s not found in %s", subnetID, region)
	}
	n.VPCID = aws.ToString(sub.Subnets[0].VpcId)
	n.AvailabilityZone = aws.ToString(sub.Subnets[0].AvailabilityZone)
	vpcFilter := []types.Filter{{Name: aws.String("vpc-id"), Values: []string{n.VPCID}}}

	var tables []types.RouteTable
	rtp := ec2.NewDescribeRouteTablesPaginator(api, &ec2.DescribeRouteTablesInput{Filters: vpcFilter})
	for rtp.HasMorePages() {
		page, err := rtp.NextPage(ctx)
		if err != nil {
			return n, fmt.Errorf("describe route tables of %s: %w", n.VPCID, err)
		}
		tables = append(tables, page.RouteTables...)
	}
	rt := subnetRouteTable(tables, subnetID)
	if rt == nil {
		return n, fmt.Errorf("no route table for subnet %s (no explicit association and no main table in %s)", subnetID, n.VPCID)
	}
	n.RouteTableID = aws.ToString(rt.RouteTableId)
	route := defaultRoute(*rt)
	n.NATGatewayID, n.DefaultRoute = route.nat, route.desc
	switch {
	case route.instance != "":
		n.NATInstanceID = route.instance
		n.NATInstanceProblem = describeNATInstance(ctx, api, route.instance, route.eni, n.VPCID, tables)
	case route.eni != "":
		// DescribeRouteTables names the instance of an attached interface, so
		// this one is attached to none: nothing forwards its traffic.
		n.NATInstanceProblem = "the network interface is attached to no instance, so nothing forwards its traffic"
	}

	// The VPC's subnets, for each interface endpoint's AZs.
	azOf := map[string]string{}
	snp := ec2.NewDescribeSubnetsPaginator(api, &ec2.DescribeSubnetsInput{Filters: vpcFilter})
	for snp.HasMorePages() {
		page, err := snp.NextPage(ctx)
		if err != nil {
			return n, fmt.Errorf("describe subnets of %s: %w", n.VPCID, err)
		}
		for _, s := range page.Subnets {
			azOf[aws.ToString(s.SubnetId)] = aws.ToString(s.AvailabilityZone)
		}
	}

	prefix := "com.amazonaws." + region + "."
	vep := ec2.NewDescribeVpcEndpointsPaginator(api, &ec2.DescribeVpcEndpointsInput{Filters: vpcFilter})
	for vep.HasMorePages() {
		page, err := vep.NextPage(ctx)
		if err != nil {
			return n, fmt.Errorf("describe VPC endpoints of %s: %w", n.VPCID, err)
		}
		for _, e := range page.VpcEndpoints {
			svc := aws.ToString(e.ServiceName)
			if !strings.HasPrefix(svc, prefix) {
				continue // another region's, a partner's or a PrivateLink service
			}
			pe := PrivateEndpoint{
				ID:            aws.ToString(e.VpcEndpointId),
				Service:       strings.TrimPrefix(svc, prefix),
				Type:          string(e.VpcEndpointType),
				State:         string(e.State),
				PrivateDNS:    aws.ToBool(e.PrivateDnsEnabled),
				RouteTableIDs: e.RouteTableIds,
			}
			if e.DnsOptions != nil {
				pe.InboundResolverOnly = aws.ToBool(e.DnsOptions.PrivateDnsOnlyForInboundResolverEndpoint)
			}
			seen := map[string]bool{}
			for _, s := range e.SubnetIds {
				if az := azOf[s]; az != "" && !seen[az] {
					seen[az] = true
					pe.AvailabilityZones = append(pe.AvailabilityZones, az)
				}
			}
			sort.Strings(pe.AvailabilityZones)
			n.Endpoints = append(n.Endpoints, pe)
		}
	}
	return n, nil
}

// subnetRouteTable is the subnet's explicitly associated route table, else the
// VPC's main one.
func subnetRouteTable(tables []types.RouteTable, subnetID string) *types.RouteTable {
	var main *types.RouteTable
	for i := range tables {
		for _, a := range tables[i].Associations {
			if aws.ToString(a.SubnetId) == subnetID {
				return &tables[i]
			}
			if aws.ToBool(a.Main) {
				main = &tables[i]
			}
		}
	}
	return main
}

// routeTarget is a route table's 0.0.0.0/0 target.
type routeTarget struct {
	// nat is a NAT gateway's ID; gateway an internet (or other) gateway's.
	nat, gateway string
	// instance is the instance the route sends to, directly or through its
	// network interface eni. A route to an attached interface carries both.
	instance, eni string
	// desc describes any target but a NAT gateway.
	desc string
}

// defaultRoute is the route table's 0.0.0.0/0 target. A blackholed route (to a
// stopped instance or a deleted gateway, say) is no route.
func defaultRoute(rt types.RouteTable) routeTarget {
	var t routeTarget
	for _, r := range rt.Routes {
		if aws.ToString(r.DestinationCidrBlock) != "0.0.0.0/0" || r.State == types.RouteStateBlackhole {
			continue
		}
		t = routeTarget{}
		switch {
		case aws.ToString(r.NatGatewayId) != "":
			return routeTarget{nat: aws.ToString(r.NatGatewayId)}
		case aws.ToString(r.GatewayId) != "":
			t.gateway = aws.ToString(r.GatewayId)
			t.desc = "internet gateway " + t.gateway
		case aws.ToString(r.TransitGatewayId) != "":
			t.desc = "transit gateway " + aws.ToString(r.TransitGatewayId)
		case aws.ToString(r.InstanceId) != "":
			t.instance, t.eni = aws.ToString(r.InstanceId), aws.ToString(r.NetworkInterfaceId)
			t.desc = "instance " + t.instance
			if t.eni != "" {
				t.desc += " (network interface " + t.eni + ")"
			}
		case aws.ToString(r.NetworkInterfaceId) != "":
			t.eni = aws.ToString(r.NetworkInterfaceId)
			t.desc = "network interface " + t.eni
		default:
			t.desc = "an unrecognised target"
		}
	}
	return t
}

// describeNATInstance reads the instance a default route sends to, with
// ec2:DescribeInstances only, and returns why it is not a working NAT
// instance, or "" if it is. A failed read is a problem, not an error: the
// launch is refused saying so, and --allow-missing-endpoints still lists the
// rest.
func describeNATInstance(ctx context.Context, api privateNetworkAPI, instanceID, eniID, vpcID string, tables []types.RouteTable) string {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return fmt.Sprintf("spawn could not read it (ec2:DescribeInstances: %v)", err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if aws.ToString(inst.InstanceId) == instanceID {
				return natInstanceProblem(inst, eniID, vpcID, tables)
			}
		}
	}
	return "spawn could not find it (ec2:DescribeInstances returned no such instance)"
}

// natInstanceProblem is every reason inst, routed to through eniID (its
// primary interface when ""), is not a working NAT instance for vpcID, whose
// route tables are tables; "" if it is one. It must be running; the routed
// interface, the one that forwards, must have its source/destination check
// off; and an interface with a public IPv4 address, the one its traffic
// leaves by, must sit in a subnet that sends 0.0.0.0/0 to an internet gateway.
// The two interfaces may differ: a two-interface NAT has its inside one in the
// private subnet (whose default route points back at it) and its outside one,
// with the public IP, in a public subnet. Pure.
func natInstanceProblem(inst types.Instance, eniID, vpcID string, tables []types.RouteTable) string {
	var problems []string
	state := "in an unknown state"
	if inst.State != nil && inst.State.Name != "" {
		state = string(inst.State.Name)
	}
	if state != string(types.InstanceStateNameRunning) {
		problems = append(problems, "it is "+state+", not running")
	}

	var routed *types.InstanceNetworkInterface
	var public []*types.InstanceNetworkInterface
	for i := range inst.NetworkInterfaces {
		ni := &inst.NetworkInterfaces[i]
		primary := ni.Attachment != nil && aws.ToInt32(ni.Attachment.DeviceIndex) == 0
		switch {
		case eniID != "" && aws.ToString(ni.NetworkInterfaceId) == eniID:
			routed = ni
		case eniID == "" && primary:
			routed = ni
		}
		// The instance's PublicIpAddress is its primary interface's.
		if (ni.Association != nil && aws.ToString(ni.Association.PublicIp) != "") ||
			(primary && aws.ToString(inst.PublicIpAddress) != "") {
			public = append(public, ni)
		}
	}

	switch {
	case routed == nil && eniID != "":
		problems = append(problems, "the routed network interface "+eniID+" is not one of its interfaces")
	case routed == nil:
		problems = append(problems, "spawn could not find its primary network interface")
	default:
		check := routed.SourceDestCheck
		if check == nil && aws.ToString(routed.NetworkInterfaceId) == primaryInterface(inst) {
			check = inst.SourceDestCheck // the instance's is its primary interface's
		}
		// EC2 turns the check on by default; unreported is not off.
		if check == nil || *check {
			problems = append(problems, "its source/destination check is on (network interface "+
				aws.ToString(routed.NetworkInterfaceId)+"), so it drops traffic it is not the destination of")
		}
	}

	if len(public) == 0 {
		problems = append(problems, "it has no public IPv4 address, so what it forwards cannot leave through an internet gateway")
	} else {
		var why []string
		for _, ni := range public {
			p := natSubnetProblem(ni, vpcID, tables)
			if p == "" {
				why = nil
				break
			}
			why = append(why, "network interface "+aws.ToString(ni.NetworkInterfaceId)+" is in "+p)
		}
		if len(why) > 0 {
			problems = append(problems, "no interface with a public IPv4 address sits in a subnet routed to an internet gateway: "+
				strings.Join(why, ", and "))
		}
	}
	return strings.Join(problems, "; ")
}

// primaryInterface is the ID of inst's interface at device index 0.
func primaryInterface(inst types.Instance) string {
	for _, ni := range inst.NetworkInterfaces {
		if ni.Attachment != nil && aws.ToInt32(ni.Attachment.DeviceIndex) == 0 {
			return aws.ToString(ni.NetworkInterfaceId)
		}
	}
	return ""
}

// natSubnetProblem is why eni's subnet does not send 0.0.0.0/0 to an internet
// gateway, phrased to follow "is in" ("subnet-x, whose route table ..."), or
// "" if it does.
func natSubnetProblem(eni *types.InstanceNetworkInterface, vpcID string, tables []types.RouteTable) string {
	subnet := aws.ToString(eni.SubnetId)
	if v := aws.ToString(eni.VpcId); v != "" && v != vpcID {
		return fmt.Sprintf("%s, not %s", v, vpcID)
	}
	rt := subnetRouteTable(tables, subnet)
	if rt == nil {
		return fmt.Sprintf("%s, which has no route table", orNone(subnet))
	}
	t := defaultRoute(*rt)
	if strings.HasPrefix(t.gateway, "igw-") {
		return ""
	}
	to := "has no 0.0.0.0/0 route to an internet gateway"
	switch {
	case t.nat != "":
		to = "sends 0.0.0.0/0 to NAT gateway " + t.nat + ", not to an internet gateway"
	case t.gateway != "": // a virtual private gateway, say
		to = "sends 0.0.0.0/0 to gateway " + t.gateway + ", not to an internet gateway"
	case t.desc != "":
		to = "sends 0.0.0.0/0 to " + t.desc + ", not to an internet gateway"
	}
	return fmt.Sprintf("%s, whose route table %s %s", subnet, aws.ToString(rt.RouteTableId), to)
}

// applyNoPublicIP makes a private-only launch (#1433) ask for no public IP on
// any interface. A single interface says so explicitly: left unset, a subnet
// with MapPublicIpOnLaunch (every default subnet) assigns one anyway. With more
// than one, EC2 accepts no AssociatePublicIpAddress at all and assigns none, so
// it is left unset (the multi-card EFA layout never sets it).
func applyNoPublicIP(input *ec2.RunInstancesInput, lc LaunchConfig) {
	if !lc.NoPublicIP {
		return
	}
	if len(input.NetworkInterfaces) == 1 {
		input.NetworkInterfaces[0].AssociatePublicIpAddress = aws.Bool(false)
		return
	}
	for i := range input.NetworkInterfaces {
		input.NetworkInterfaces[i].AssociatePublicIpAddress = nil
	}
}

// reportPreflightOnce writes a PreflightNoPublicIP line to out unless this
// client has already written the same line (#1443).
func (c *Client) reportPreflightOnce(out io.Writer, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if _, seen := c.privateNetReported.LoadOrStore(line, true); seen {
		return
	}
	_, _ = io.WriteString(out, line)
}

// PreflightNoPublicIP settles a private-only launch's network before anything
// is created (#1433): with only an AZ, the subnet SubnetInAZ picks is pinned
// on cfg (and printed to out), so the instance launches in the subnet that was
// checked; then the subnet is checked. It refuses, listing what is missing,
// unless cfg.AllowMissingEndpoints, which prints the list instead. internet
// are needs only NAT meets beyond those cfg names. A no-op without NoPublicIP.
// A line this client has already written is not written again, so the check
// Launch repeats after the CLI's, or once per job array member, adds nothing
// to out (#1443).
func (c *Client) PreflightNoPublicIP(ctx context.Context, cfg *LaunchConfig, internet []string, out io.Writer) (PrivateCheck, error) {
	if !cfg.NoPublicIP {
		return PrivateCheck{}, nil
	}
	if err := cfg.ValidateNoPublicIP(); err != nil {
		return PrivateCheck{}, err
	}
	if cfg.SubnetID == "" {
		choice, err := c.SubnetInAZ(ctx, cfg.Region, cfg.AvailabilityZone, cfg.VPCID)
		if err != nil {
			return PrivateCheck{}, fmt.Errorf("--no-public-ip: subnet in %s: %w", cfg.AvailabilityZone, err)
		}
		c.reportPreflightOnce(out, "using subnet %s (%s, %d free IPs) in %s\n", choice.SubnetID, choice.AvailabilityZone, choice.FreeIPs, choice.VPCID)
		cfg.SubnetID = choice.SubnetID
	}
	n, err := c.DescribePrivateNetwork(ctx, cfg.Region, cfg.SubnetID)
	if err != nil {
		if cfg.AllowMissingEndpoints {
			c.reportPreflightOnce(out, "⚠️  --no-public-ip: could not check subnet %s (%v); launching anyway (--allow-missing-endpoints)\n", cfg.SubnetID, err)
			return PrivateCheck{}, nil
		}
		return PrivateCheck{}, fmt.Errorf("--no-public-ip: could not check that subnet %s can serve a private-only instance "+
			"(it reads ec2:DescribeSubnets, ec2:DescribeRouteTables, ec2:DescribeVpcEndpoints and, for a route to an instance, "+
			"ec2:DescribeInstances): %w. "+
			"Pass --allow-missing-endpoints to launch without the check", cfg.SubnetID, err)
	}
	check := CheckPrivateNetwork(n, *cfg, internet)
	for _, w := range check.Warnings {
		c.reportPreflightOnce(out, "⚠️  --no-public-ip: %s\n", w)
	}
	if check.OK() {
		return check, nil
	}
	list := "  - " + strings.Join(check.Missing, "\n  - ")
	if cfg.AllowMissingEndpoints {
		c.reportPreflightOnce(out, "⚠️  --no-public-ip: subnet %s (%s, %s) cannot reach:\n%s\nLaunching anyway (--allow-missing-endpoints): "+
			"what needs these will fail on the instance, and spored's TTL and cost limit may not be enforced; the TTL reaper is the backstop.\n",
			n.SubnetID, n.VPCID, n.AvailabilityZone, list)
		return check, nil
	}
	return check, fmt.Errorf("%w: subnet %s (%s, %s) has no way to reach:\n%s\n"+
		"Without these spored cannot enforce the TTL or cost limit, and `spawn connect` has nothing to reach. Add the VPC "+
		"endpoints or a route to a NAT gateway or a working NAT instance, or pass --allow-missing-endpoints to launch anyway",
		ErrPrivateNetworkIncomplete, n.SubnetID, n.VPCID, n.AvailabilityZone, list)
}
