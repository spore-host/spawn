package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// efaSubnetAWS answers the read-only calls a multi-card EFA launch's pre-flight
// makes, and records every action. Everything else (every mutating call: a key
// pair, a role, a security group, RunInstances) is refused and recorded.
type efaSubnetAWS struct {
	mu      sync.Mutex
	actions []string
	subnets []efaSubnetFixture
	// azLookups counts DescribeSubnets by VPC and AZ filter: SubnetInAZ's
	// lookup, not ValidateSubnetInVPC's by subnet ID.
	azLookups int
}

type efaSubnetFixture struct {
	id, vpc, az string
	free        int
}

func (f *efaSubnetAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	action := r.Header.Get("X-Amz-Target")
	if action == "" {
		action = form.Get("Action")
	}
	if action == "" {
		action = r.Method + " " + r.URL.Path
	}
	f.mu.Lock()
	f.actions = append(f.actions, action)
	f.mu.Unlock()
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	w.Header().Set("Content-Type", "text/xml")
	var b bytes.Buffer
	switch action {
	case "GetCallerIdentity":
		b.WriteString(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>t</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
	case "DescribeInstanceTypes":
		// nosemgrep
		b.WriteString(`<DescribeInstanceTypesResponse ` + ns + `><requestId>r</requestId><instanceTypeSet><item><instanceType>` + form.Get("InstanceType.1") +
			`</instanceType><networkInfo><efaSupported>true</efaSupported><efaInfo><maximumEfaInterfaces>2</maximumEfaInterfaces></efaInfo></networkInfo>` +
			`<placementGroupInfo><supportedStrategies><item>cluster</item></supportedStrategies></placementGroupInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`)
	case "DescribeKeyPairs":
		// No key yet, so a launch that gets this far goes on to import one.
		b.WriteString(`<DescribeKeyPairsResponse ` + ns + `><requestId>r</requestId><keySet/></DescribeKeyPairsResponse>`)
	case "DescribeVpcs":
		b.WriteString(`<DescribeVpcsResponse ` + ns + `><requestId>r</requestId><vpcSet><item><vpcId>vpc-default</vpcId><isDefault>true</isDefault></item></vpcSet></DescribeVpcsResponse>`)
	case "DescribeSubnets":
		want := map[string]string{}
		for i := 1; form.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
			want[form.Get(fmt.Sprintf("Filter.%d.Name", i))] = form.Get(fmt.Sprintf("Filter.%d.Value.1", i))
		}
		if _, ok := want["availability-zone"]; ok {
			f.mu.Lock()
			f.azLookups++
			f.mu.Unlock()
		}
		b.WriteString(`<DescribeSubnetsResponse ` + ns + `><requestId>r</requestId><subnetSet>`)
		for _, sn := range f.subnets {
			if sn.vpc != want["vpc-id"] || sn.az != want["availability-zone"] {
				continue
			}
			fmt.Fprintf(&b, `<item><subnetId>%s</subnetId><vpcId>%s</vpcId><availabilityZone>%s</availabilityZone><availableIpAddressCount>%d</availableIpAddressCount></item>`,
				sn.id, sn.vpc, sn.az, sn.free)
		}
		b.WriteString(`</subnetSet></DescribeSubnetsResponse>`)
	default:
		w.WriteHeader(http.StatusForbidden)
		b.WriteString(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not in this fake</Message></Error></Errors><RequestID>t</RequestID></Response>`)
	}
	_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake AWS Query API answering the SDK in XML, not HTML served to a browser
}

// isReadOnlyAction reports whether an AWS action only reads: Describe*, Get*
// or List*, for a Query action ("DescribeSubnets") or a JSON target
// ("AmazonSSM.GetParameter"), and GET or HEAD for a REST call.
func isReadOnlyAction(action string) bool {
	if strings.HasPrefix(action, "GET ") || strings.HasPrefix(action, "HEAD ") {
		return true
	}
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		action = action[i+1:]
	}
	for _, p := range []string{"Describe", "Get", "List"} {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
}

func (f *efaSubnetAWS) mutating() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.actions {
		if !isReadOnlyAction(a) {
			out = append(out, a)
		}
	}
	return out
}

func (f *efaSubnetAWS) count(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.actions {
		if a == action {
			n++
		}
	}
	return n
}

// serveEFASubnetAWS points the AWS SDK at f, with a two-card EFA type stubbed
// into the EFA capacity and Elastic IP quota seams.
func serveEFASubnetAWS(t *testing.T, f *efaSubnetAWS) {
	t.Helper()
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	stubEFACapacity(t, cardsCapacity("c8gn.48xlarge", 2))
	stubElasticIPQuota(t, aws.ElasticIPQuota{InUse: 0, Limit: 100}, nil)
	stubTeamName(t)
}

func setEFALaunchFlags(t *testing.T, extra map[string]string) {
	t.Helper()
	for k, v := range map[string]string{
		"instance-type": "c8gn.48xlarge", "region": "us-east-1", "ttl": "1h", "name": "box",
		"yes": "true", "no-dns": "true", "efa": "true", "ami": "ami-0123456789abcdef0", "os": "linux",
	} {
		setLaunchFlag(t, k, v)
	}
	for k, v := range extra {
		setLaunchFlag(t, k, v)
	}
}

// Gate (#1410): a launch that resolves to more than one EFA interface with
// neither --subnet-id nor --az is refused in the pre-flight, before a single
// mutating AWS call. It used to get as far as RunInstances, after the key pair
// and the spored role had been set up, and fail there. Covers the single
// launch, its --dry-run, and a job array with a fixed placement group (which
// buildAZChain keeps in one rung, so nothing would name an AZ for it).
func TestMultiCardEFAWithNeitherSubnetNorAZIsRefusedBeforeAnyMutatingCall(t *testing.T) {
	cases := map[string]map[string]string{
		"single launch": {},
		"dry-run":       {"dry-run": "true"},
		"job array with a placement group": {
			"count": "2", "job-array-name": "arr", "placement-group": "pg-fixed",
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			setEFALaunchFlags(t, extra)
			err := runLaunch(launchCmd, nil)
			if m := f.mutating(); len(m) != 0 {
				t.Errorf("the launch made %d mutating AWS call(s): %v", len(m), m)
			}
			if !errors.Is(err, aws.ErrEFANeedsSubnet) {
				t.Fatalf("runLaunch = %v; want the %q refusal", err, aws.ErrEFANeedsSubnet)
			}
			const want = "2 EFA interfaces need one subnet: pass --subnet-id, or --az to let spawn pick one"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q; want it to say %q", err, want)
			}
		})
	}
}

// Gate (#1410): with --az and no subnet, the pre-flight picks the subnet
// (SubnetInAZ: most free IPs in the AZ of the default VPC, or of --vpc) and
// prints it, and every EFA interface is laid out in it.
func TestMultiCardEFAWithAZResolvesTheSubnetForEveryInterface(t *testing.T) {
	for _, tc := range []struct {
		name, vpc, want string
	}{
		{"default VPC", "", "subnet-0big"},
		{"--vpc", "vpc-research", "subnet-0res"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &efaSubnetAWS{subnets: []efaSubnetFixture{
				{"subnet-0small", "vpc-default", "us-east-1a", 10},
				{"subnet-0big", "vpc-default", "us-east-1a", 400},
				{"subnet-0other", "vpc-default", "us-east-1b", 900},
				{"subnet-0res", "vpc-research", "us-east-1a", 50},
			}}
			serveEFASubnetAWS(t, f)
			extra := map[string]string{"az": "us-east-1a"}
			if tc.vpc != "" {
				extra["vpc"] = tc.vpc
			}
			setEFALaunchFlags(t, extra)
			config, err := buildLaunchConfig(nil)
			if err != nil {
				t.Fatal(err)
			}
			client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			stderr := captureStderr(t, func() {
				err = ensureAMIAndPreflight(context.Background(), client, config, progress.NewQuietProgress())
			})
			if err != nil {
				t.Fatalf("ensureAMIAndPreflight: %v", err)
			}
			if config.SubnetID != tc.want {
				t.Fatalf("SubnetID = %q, want %q (SubnetInAZ's choice in us-east-1a)", config.SubnetID, tc.want)
			}
			if !strings.Contains(stderr, "using subnet "+tc.want+" (us-east-1a, ") {
				t.Errorf("the choice was not printed: %q", stderr)
			}
			if f.azLookups != 1 {
				t.Errorf("%d subnet lookups by AZ, want 1", f.azLookups)
			}
			nis, err := aws.EFALaunchInterfaces(*config, cardsCapacity("c8gn.48xlarge", 2))
			if err != nil {
				t.Fatal(err)
			}
			if len(nis) != 2 {
				t.Fatalf("%d interfaces, want 2", len(nis))
			}
			for i, ni := range nis {
				if got := sdkaws.ToString(ni.SubnetId); got != tc.want {
					t.Errorf("interface %d is in subnet %q, want %q", i, got, tc.want)
				}
			}
			if m := f.mutating(); len(m) != 0 {
				t.Errorf("the pre-flight made mutating AWS call(s): %v", m)
			}
		})
	}
}

// Gate (#1410): an explicit --subnet-id is left as it is, with no subnet
// lookup, and so is any launch with one EFA interface or none.
func TestResolveEFASubnetLeavesExplicitSubnetAndSingleEFAAlone(t *testing.T) {
	for name, cfg := range map[string]*aws.LaunchConfig{
		"explicit subnet":          {Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 2, SubnetID: "subnet-0mine", AvailabilityZone: "us-east-1a"},
		"explicit subnet, no --az": {Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 2, SubnetID: "subnet-0mine"},
		"one EFA interface":        {Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1},
		"one EFA interface, --az":  {Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1, AvailabilityZone: "us-east-1a"},
		"no EFA":                   {Region: "us-east-1"},
	} {
		t.Run(name, func(t *testing.T) {
			before := *cfg
			lookup := &fakeSubnetLookup{choice: aws.SubnetChoice{SubnetID: "subnet-0picked"}}
			var out bytes.Buffer
			if err := resolveEFASubnet(context.Background(), lookup, cfg, false, &out); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(lookup.calls) != 0 || cfg.SubnetID != before.SubnetID || out.Len() != 0 {
				t.Errorf("lookups %v, SubnetID %q (was %q), output %q; want none, unchanged, none",
					lookup.calls, cfg.SubnetID, before.SubnetID, out.String())
			}
			if err := cfg.ValidateEFASubnet(); err != nil {
				t.Errorf("ValidateEFASubnet refused it: %v", err)
			}
		})
	}
}

// A single launch with one EFA interface and neither flag still launches: the
// pre-flight neither refuses it nor looks up a subnet (#1410 is about more
// than one interface).
func TestSingleEFALaunchPreflightIsUnaffected(t *testing.T) {
	f := &efaSubnetAWS{}
	serveEFASubnetAWS(t, f)
	setEFALaunchFlags(t, map[string]string{"efa-interfaces": "1"})
	config, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureAMIAndPreflight(context.Background(), client, config, progress.NewQuietProgress()); err != nil {
		t.Fatalf("a one-interface EFA launch was refused: %v", err)
	}
	if config.SubnetID != "" || f.count("DescribeSubnets") != 0 {
		t.Errorf("SubnetID %q, DescribeSubnets %d; want none", config.SubnetID, f.count("DescribeSubnets"))
	}
	if m := f.mutating(); len(m) != 0 {
		t.Errorf("the pre-flight made mutating AWS call(s): %v", m)
	}
}

// A job array or MPI cohort with no fixed placement group gets buildAZChain's
// AZ-fallback chain: every rung names an AZ and Client.Launch picks the subnet
// in it, so the pre-flight neither refuses it nor pins a subnet (which would
// end the fallback).
func TestMultiCardEFACohortWithAZChainKeepsItsFallback(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"job array, neither flag": {"count": "2", "job-array-name": "arr"},
		"job array, --az":         {"count": "2", "job-array-name": "arr", "az": "us-east-1a"},
		"mpi, neither flag":       {"count": "2", "job-array-name": "arr", "mpi": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{subnets: []efaSubnetFixture{{"subnet-0a", "vpc-default", "us-east-1a", 10}}}
			serveEFASubnetAWS(t, f)
			setEFALaunchFlags(t, extra)
			config, err := buildLaunchConfig(nil)
			if err != nil {
				t.Fatal(err)
			}
			client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureAMIAndPreflight(context.Background(), client, config, progress.NewQuietProgress()); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if config.SubnetID != "" {
				t.Errorf("a chained cohort was pinned to subnet %s, ending its AZ fallback", config.SubnetID)
			}
		})
	}
	for _, tc := range []struct {
		cfg   aws.LaunchConfig
		count int
		want  bool
	}{
		{aws.LaunchConfig{}, 2, true},
		{aws.LaunchConfig{}, 1, false},
		{aws.LaunchConfig{PlacementGroup: "pg"}, 2, false},
		{aws.LaunchConfig{SubnetID: "subnet-0a"}, 2, false},
	} {
		if got := cohortHasAZChain(&tc.cfg, tc.count); got != tc.want {
			t.Errorf("cohortHasAZChain(%+v, %d) = %t, want %t", tc.cfg, tc.count, got, tc.want)
		}
	}
}

// countingSubnetLookup answers SubnetInAZ with a subnet named for its
// (region, AZ, VPC) and counts the lookups.
type countingSubnetLookup struct{ calls []string }

func (c *countingSubnetLookup) SubnetInAZ(_ context.Context, region, az, vpc string) (aws.SubnetChoice, error) {
	c.calls = append(c.calls, region+"/"+az+"/"+vpc)
	return aws.SubnetChoice{SubnetID: "subnet-" + region + "-" + az + vpc, AvailabilityZone: az, VPCID: vpc}, nil
}

// Gate (#1410): a foreground sweep resolves each multi-card member's subnet in
// its own (region, AZ), with one SubnetInAZ lookup per distinct pair however
// many rows share it, and leaves single-interface and explicit-subnet rows
// alone.
func TestSweepResolvesMultiCardEFASubnetOncePerRegionAndAZ(t *testing.T) {
	multi := func(region, az string) *aws.LaunchConfig {
		return &aws.LaunchConfig{Region: region, AvailabilityZone: az, EFAEnabled: true, EFAInterfaces: 2}
	}
	cfgs := []*aws.LaunchConfig{
		multi("us-east-1", "us-east-1a"),
		multi("us-east-1", "us-east-1a"),
		multi("us-east-1", "us-east-1b"),
		multi("us-west-2", "us-west-2a"),
		multi("us-east-1", "us-east-1a"),
		{Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1},
		{Region: "us-east-1", AvailabilityZone: "us-east-1c", EFAEnabled: true, EFAInterfaces: 2, SubnetID: "subnet-0mine"},
	}
	lookup := &countingSubnetLookup{}
	var out bytes.Buffer
	if err := resolveSweepEFASubnets(context.Background(), lookup, cfgs, nil, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"us-east-1/us-east-1a/", "us-east-1/us-east-1b/", "us-west-2/us-west-2a/"}
	if strings.Join(lookup.calls, ",") != strings.Join(want, ",") {
		t.Errorf("lookups %v, want one per (region, AZ): %v", lookup.calls, want)
	}
	for i, cfg := range cfgs[:5] {
		if w := "subnet-" + cfg.Region + "-" + cfg.AvailabilityZone; cfg.SubnetID != w {
			t.Errorf("member %d: SubnetID %q, want %q", i, cfg.SubnetID, w)
		}
	}
	if cfgs[5].SubnetID != "" || cfgs[6].SubnetID != "subnet-0mine" {
		t.Errorf("single-interface member got %q, explicit-subnet member got %q", cfgs[5].SubnetID, cfgs[6].SubnetID)
	}
	if n := strings.Count(out.String(), "using subnet "); n != 3 {
		t.Errorf("printed %d choices, want 3: %q", n, out.String())
	}
}

// A multi-card sweep member with neither a subnet nor an AZ is refused, naming
// its parameter set, before any lookup.
func TestSweepRefusesMultiCardEFAMemberWithoutSubnetOrAZ(t *testing.T) {
	cfgs := []*aws.LaunchConfig{
		{Region: "us-east-1", AvailabilityZone: "us-east-1a", EFAEnabled: true, EFAInterfaces: 2},
		{Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 16},
	}
	lookup := &countingSubnetLookup{}
	err := resolveSweepEFASubnets(context.Background(), lookup, cfgs, []int{4, 7}, io.Discard)
	if !errors.Is(err, aws.ErrEFANeedsSubnet) || !strings.Contains(err.Error(), "parameter set 7: 16 EFA interfaces need one subnet") {
		t.Fatalf("err = %v; want parameter set 7's refusal", err)
	}
}

// The sweep resolution is wired into the foreground sweep and its resume, after
// each member's EFA count is resolved and before the first resource (the SSH
// key) is created.
func TestSweepAndResumeResolveMultiCardEFASubnets(t *testing.T) {
	for file, call := range map[string]string{
		"launch_sweep.go": "resolveSweepEFASubnets(ctx, awsClient, launchConfigs, nil, os.Stderr)",
		"resume.go":       "resolveSweepEFASubnets(ctx, awsClient, launchConfigs, pending, os.Stderr)",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		efa := strings.Index(s, "resolveSweepEFA(launchConfigs, ")
		at := strings.Index(s, call)
		key := strings.Index(s, "setupSSHKey(ctx, ")
		if efa < 0 || at < 0 || key < 0 || !(efa < at && at < key) {
			t.Errorf("%s: want %s after resolveSweepEFA and before setupSSHKey (offsets %d, %d, %d)", file, call, efa, at, key)
		}
	}
}

// The --dry-run plan reports the subnet the --az launch resolved.
func TestMultiCardEFADryRunShowsTheResolvedSubnet(t *testing.T) {
	f := &efaSubnetAWS{subnets: []efaSubnetFixture{{"subnet-0big", "vpc-default", "us-east-1a", 400}}}
	serveEFASubnetAWS(t, f)
	setEFALaunchFlags(t, map[string]string{"az": "us-east-1a"})
	config, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	prev := spawnOutputFormat
	t.Cleanup(func() { spawnOutputFormat = prev })
	spawnOutputFormat = "json"
	var out bytes.Buffer
	captureStderr(t, func() { err = runLaunchDryRun(context.Background(), &out, client, config) })
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var plan launchPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("plan: %v\n%s", err, out.String())
	}
	if plan.SubnetID != "subnet-0big" || plan.EFAInterfaces != 2 {
		t.Errorf("plan subnet %q, %d interfaces; want subnet-0big, 2", plan.SubnetID, plan.EFAInterfaces)
	}
	if m := f.mutating(); len(m) != 0 {
		t.Errorf("the dry run made mutating AWS call(s): %v", m)
	}
}
