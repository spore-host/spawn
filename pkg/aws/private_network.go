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
	// serves it. GatewayOnly: an interface endpoint does not.
	Gateway, GatewayOnly bool
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
		Endpoint: "s3", IAMPrefixes: []string{"s3"}, Gateway: true, GatewayOnly: true,
		Why: "the bootstrap downloads spored from https://spawn-binaries-<region>.s3.amazonaws.com, and spored reads its " +
			"launch config object and stages data. That download uses the global-style S3 name, which an S3 interface " +
			"endpoint's private DNS does not serve, so only a gateway endpoint (or NAT) counts",
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
	// DefaultRoute describes a 0.0.0.0/0 route to anything else (an internet
	// gateway, a transit gateway, an instance), which spawn cannot count on.
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
	noNAT := "no NAT route in " + orNone(n.RouteTableID)
	if n.DefaultRoute != "" {
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
			if s.GatewayOnly {
				reasons = append(reasons, fmt.Sprintf("interface endpoint %s does not count (only a gateway endpoint does)", e.ID))
				continue
			}
			if !e.PrivateDNS {
				reasons = append(reasons, fmt.Sprintf("interface endpoint %s has private DNS disabled, so the default %s.%s.amazonaws.com "+
					"name still resolves to the public endpoint; enable private DNS on it", e.ID, s.Endpoint, n.Region))
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
	if s.GatewayOnly {
		kind = "gateway endpoint"
	} else if s.Gateway {
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
}

// DescribePrivateNetwork reads what [CheckPrivateNetwork] decides on for a
// subnet: read-only (ec2:DescribeSubnets, ec2:DescribeRouteTables,
// ec2:DescribeVpcEndpoints), and cached on the client per region and subnet.
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
	n.NATGatewayID, n.DefaultRoute = defaultRoute(*rt)

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

// defaultRoute is the route table's 0.0.0.0/0 target: a NAT gateway's ID, or a
// description of any other target. A blackholed route is no route.
func defaultRoute(rt types.RouteTable) (nat, other string) {
	for _, r := range rt.Routes {
		if aws.ToString(r.DestinationCidrBlock) != "0.0.0.0/0" || r.State == types.RouteStateBlackhole {
			continue
		}
		switch {
		case aws.ToString(r.NatGatewayId) != "":
			return aws.ToString(r.NatGatewayId), ""
		case aws.ToString(r.GatewayId) != "":
			other = "internet gateway " + aws.ToString(r.GatewayId)
		case aws.ToString(r.TransitGatewayId) != "":
			other = "transit gateway " + aws.ToString(r.TransitGatewayId)
		case aws.ToString(r.InstanceId) != "":
			other = "instance " + aws.ToString(r.InstanceId)
		case aws.ToString(r.NetworkInterfaceId) != "":
			other = "network interface " + aws.ToString(r.NetworkInterfaceId)
		default:
			other = "an unrecognised target"
		}
	}
	return "", other
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

// PreflightNoPublicIP settles a private-only launch's network before anything
// is created (#1433): with only an AZ, the subnet SubnetInAZ picks is pinned
// on cfg (and printed to out), so the instance launches in the subnet that was
// checked; then the subnet is checked. It refuses, listing what is missing,
// unless cfg.AllowMissingEndpoints, which prints the list instead. internet
// are needs only NAT meets beyond those cfg names. A no-op without NoPublicIP.
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
		fmt.Fprintf(out, "using subnet %s (%s, %d free IPs) in %s\n", choice.SubnetID, choice.AvailabilityZone, choice.FreeIPs, choice.VPCID)
		cfg.SubnetID = choice.SubnetID
	}
	n, err := c.DescribePrivateNetwork(ctx, cfg.Region, cfg.SubnetID)
	if err != nil {
		if cfg.AllowMissingEndpoints {
			fmt.Fprintf(out, "⚠️  --no-public-ip: could not check subnet %s (%v); launching anyway (--allow-missing-endpoints)\n", cfg.SubnetID, err)
			return PrivateCheck{}, nil
		}
		return PrivateCheck{}, fmt.Errorf("--no-public-ip: could not check that subnet %s can serve a private-only instance "+
			"(it reads ec2:DescribeSubnets, ec2:DescribeRouteTables and ec2:DescribeVpcEndpoints): %w. "+
			"Pass --allow-missing-endpoints to launch without the check", cfg.SubnetID, err)
	}
	check := CheckPrivateNetwork(n, *cfg, internet)
	for _, w := range check.Warnings {
		fmt.Fprintf(out, "⚠️  --no-public-ip: %s\n", w)
	}
	if check.OK() {
		return check, nil
	}
	list := "  - " + strings.Join(check.Missing, "\n  - ")
	if cfg.AllowMissingEndpoints {
		fmt.Fprintf(out, "⚠️  --no-public-ip: subnet %s (%s, %s) cannot reach:\n%s\nLaunching anyway (--allow-missing-endpoints): "+
			"what needs these will fail on the instance, and spored's TTL and cost limit may not be enforced; the TTL reaper is the backstop.\n",
			n.SubnetID, n.VPCID, n.AvailabilityZone, list)
		return check, nil
	}
	return check, fmt.Errorf("%w: subnet %s (%s, %s) has no way to reach:\n%s\n"+
		"Without these spored cannot enforce the TTL or cost limit, and `spawn connect` has nothing to reach. Add the VPC "+
		"endpoints or a NAT gateway route, or pass --allow-missing-endpoints to launch anyway",
		ErrPrivateNetworkIncomplete, n.SubnetID, n.VPCID, n.AvailabilityZone, list)
}
