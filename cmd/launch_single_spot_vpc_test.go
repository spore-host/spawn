package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/progress"
)

// singleLaunchRunInstances is the RunInstances input a single launch built
// from the current launch flags sends, through the same settings
// aws.Client.Launch applies (applyLaunchSettings via LaunchSettings.Apply).
func singleLaunchRunInstances(t *testing.T, truffle *input.TruffleInput) *ec2.RunInstancesInput {
	t.Helper()
	cfg, err := buildLaunchConfig(truffle)
	if err != nil {
		t.Fatalf("buildLaunchConfig: %v", err)
	}
	in := &ec2.RunInstancesInput{}
	if err := aws.NewLaunchSettings(*cfg, "", 0).Apply(in); err != nil {
		t.Fatalf("apply the launch settings: %v", err)
	}
	return in
}

func spotMaxPriceOf(in *ec2.RunInstancesInput) string {
	if in.InstanceMarketOptions == nil || in.InstanceMarketOptions.SpotOptions == nil ||
		in.InstanceMarketOptions.SpotOptions.MaxPrice == nil {
		return ""
	}
	return *in.InstanceMarketOptions.SpotOptions.MaxPrice
}

// A single `spawn launch --spot --spot-max-price` sends the cap to EC2
// (#1388). buildLaunchConfig used to read the flag nowhere, so the instance
// launched with no max price (EC2 caps at the on-demand rate). Every single
// launch path builds its config here: the flags path, pipe mode, and the job
// array and MPI cohorts, which launch copies of it.
func TestSingleSpotLaunchCarriesSpotMaxPrice(t *testing.T) {
	keepLaunchGlobals(t)
	isolateAWSEnv(t)
	setLaunchFlag(t, "instance-type", "m7i.large")
	setLaunchFlag(t, "region", "us-east-1")
	setLaunchFlag(t, "ttl", "1h")
	setLaunchFlag(t, "spot", "true")
	setLaunchFlag(t, "spot-max-price", "0.10")

	if got := spotMaxPriceOf(singleLaunchRunInstances(t, nil)); got != "0.10" {
		t.Errorf("a single Spot launch with --spot-max-price 0.10 sends MaxPrice %q; want \"0.10\"", got)
	}

	// The explicit flag beats the price truffle piped in, as every other flag
	// overrides its truffle field.
	piped := &input.TruffleInput{InstanceType: "m7i.large", Region: "us-east-1", Spot: true, SpotPrice: 0.5}
	if got := spotMaxPriceOf(singleLaunchRunInstances(t, piped)); got != "0.10" {
		t.Errorf("with a piped spot price, --spot-max-price 0.10 sends MaxPrice %q; want \"0.10\"", got)
	}
}

// Without the flag nothing changes: no cap on a plain Spot launch, truffle's
// piped price on a piped one.
func TestSingleSpotLaunchWithoutSpotMaxPrice(t *testing.T) {
	keepLaunchGlobals(t)
	isolateAWSEnv(t)
	setLaunchFlag(t, "instance-type", "m7i.large")
	setLaunchFlag(t, "region", "us-east-1")
	setLaunchFlag(t, "ttl", "1h")
	setLaunchFlag(t, "spot", "true")

	if got := spotMaxPriceOf(singleLaunchRunInstances(t, nil)); got != "" {
		t.Errorf("a Spot launch with no --spot-max-price sends MaxPrice %q; want none", got)
	}
	piped := &input.TruffleInput{InstanceType: "m7i.large", Region: "us-east-1", Spot: true, SpotPrice: 0.5}
	if got := spotMaxPriceOf(singleLaunchRunInstances(t, piped)); got != "0.5000" {
		t.Errorf("a piped Spot launch sends MaxPrice %q; want truffle's \"0.5000\"", got)
	}
}

// `--vpc` with neither `--subnet-id` nor `--az` is refused on every non-sweep
// launch before a single AWS call (#1389): RunInstances takes a subnet, not a
// VPC, so it used to land an ordinary Linux instance in the region's default
// VPC. The cases are the single launch (Linux, Windows, --dry-run), the job
// array, MPI and the batch queue, all of which go through runLaunch's flag
// checks. The batch queue resolves no subnet, so it is refused even with --az.
func TestVPCWithoutSubnetIsRefusedBeforeAnyAWSCall(t *testing.T) {
	cases := map[string]struct {
		extra   map[string]string
		offerAZ bool
	}{
		"linux":               {map[string]string{}, true},
		"windows":             {map[string]string{"os": "windows", "ami": "ami-0123456789abcdef0"}, true},
		"dry-run":             {map[string]string{"dry-run": "true"}, true},
		"job array":           {map[string]string{"count": "2", "job-array-name": "arr"}, true},
		"mpi":                 {map[string]string{"count": "2", "job-array-name": "arr", "mpi": "true"}, true},
		"batch queue":         {map[string]string{"batch-queue": "queue.json"}, false},
		"batch queue with az": {map[string]string{"batch-queue": "queue.json", "az": "us-east-1a"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := serveTagLimitAWS(t)
			for k, v := range map[string]string{
				"vpc": "vpc-0123456789abcdef0", "instance-type": "m7i.large",
				"region": "us-east-1", "ttl": "1h", "name": "box", "yes": "true",
			} {
				setLaunchFlag(t, k, v)
			}
			for k, v := range tc.extra {
				setLaunchFlag(t, k, v)
			}
			stubTeamName(t)
			err := runLaunch(launchCmd, nil)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.actions) != 0 {
				t.Errorf("--vpc without --subnet-id made %d AWS call(s) before it was refused: %v", len(f.actions), f.actions)
			}
			if err == nil || !strings.Contains(err.Error(), "--vpc") || !strings.Contains(err.Error(), "pass --subnet-id") {
				t.Fatalf("runLaunch = %v; want a refusal naming --vpc and telling the user to pass --subnet-id", err)
			}
			if got := strings.Contains(err.Error(), "or pass --az to let spawn pick a subnet in that VPC"); got != tc.offerAZ {
				t.Errorf("the refusal offers --az: %t, want %t: %v", got, tc.offerAZ, err)
			}
		})
	}
}

// --vpc with a subnet (either spelling) passes the check, and so does --vpc
// --az outside the batch queue; ValidateSubnetInVPC then checks that a given
// subnet and VPC agree (#673), and resolveVPCSubnet picks one for --az.
func TestVPCWithSubnetOrAZIsAccepted(t *testing.T) {
	for _, alias := range []string{"subnet-id", "subnet"} {
		t.Run(alias, func(t *testing.T) {
			keepLaunchGlobals(t)
			setLaunchFlag(t, "vpc", "vpc-0123456789abcdef0")
			setLaunchFlag(t, alias, "subnet-0123456789abcdef0")
			for _, batch := range []bool{false, true} {
				if err := validateVPCHasSubnet(batch); err != nil {
					t.Errorf("--vpc with --%s (batch queue %t) was refused: %v", alias, batch, err)
				}
			}
		})
	}
	t.Run("az", func(t *testing.T) {
		keepLaunchGlobals(t)
		isolateAWSEnv(t)
		setLaunchFlag(t, "vpc", "vpc-0123456789abcdef0")
		setLaunchFlag(t, "az", "us-east-1a")
		setLaunchFlag(t, "instance-type", "m7i.large")
		if err := validateVPCHasSubnet(false); err != nil {
			t.Errorf("--vpc --az was refused: %v", err)
		}
		if _, err := buildLaunchConfig(nil); err != nil {
			t.Errorf("buildLaunchConfig refused --vpc --az: %v", err)
		}
	})
	keepLaunchGlobals(t)
	if err := validateVPCHasSubnet(false); err != nil {
		t.Errorf("a launch with neither --vpc nor --subnet-id was refused: %v", err)
	}
}

// The backstop in buildLaunchConfig, for a caller that did not come through
// runLaunch's flag checks.
func TestBuildLaunchConfigRefusesVPCWithoutSubnet(t *testing.T) {
	keepLaunchGlobals(t)
	isolateAWSEnv(t)
	setLaunchFlag(t, "instance-type", "m7i.large")
	setLaunchFlag(t, "vpc", "vpc-0123456789abcdef0")
	if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), "pass --subnet-id") {
		t.Errorf("buildLaunchConfig with --vpc and no subnet = %v; want the #1389 refusal", err)
	}
}

type fakeSubnetLookup struct {
	choice aws.SubnetChoice
	calls  []string
}

func (f *fakeSubnetLookup) SubnetInAZ(_ context.Context, region, az, vpc string) (aws.SubnetChoice, error) {
	f.calls = append(f.calls, region+"/"+az+"/"+vpc)
	return f.choice, nil
}

// resolveVPCSubnet sets and prints SubnetInAZ's choice for --vpc --az, and
// looks nothing up when a subnet is given or there is no VPC (#1389).
func TestResolveVPCSubnet(t *testing.T) {
	f := &fakeSubnetLookup{choice: aws.SubnetChoice{SubnetID: "subnet-0bbb", AvailabilityZone: "us-east-1a", VPCID: "vpc-a", FreeIPs: 200}}
	cfg := &aws.LaunchConfig{Region: "us-east-1", VPCID: "vpc-a", AvailabilityZone: "us-east-1a"}
	var out bytes.Buffer
	if err := resolveVPCSubnet(context.Background(), f, cfg, &out); err != nil {
		t.Fatal(err)
	}
	if cfg.SubnetID != "subnet-0bbb" {
		t.Errorf("SubnetID = %q, want subnet-0bbb", cfg.SubnetID)
	}
	if want := "using subnet subnet-0bbb (us-east-1a, 200 free IPs) in vpc-a\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
	if len(f.calls) != 1 || f.calls[0] != "us-east-1/us-east-1a/vpc-a" {
		t.Errorf("lookups = %v, want one for us-east-1/us-east-1a/vpc-a", f.calls)
	}

	for name, c := range map[string]*aws.LaunchConfig{
		"subnet given": {Region: "us-east-1", VPCID: "vpc-a", SubnetID: "subnet-1", AvailabilityZone: "us-east-1a"},
		"no vpc":       {Region: "us-east-1", AvailabilityZone: "us-east-1a"},
	} {
		f := &fakeSubnetLookup{}
		if err := resolveVPCSubnet(context.Background(), f, c, &bytes.Buffer{}); err != nil || len(f.calls) != 0 {
			t.Errorf("%s: err %v, lookups %v; want neither", name, err, f.calls)
		}
	}
	f = &fakeSubnetLookup{}
	err := resolveVPCSubnet(context.Background(), f, &aws.LaunchConfig{Region: "us-east-1", VPCID: "vpc-a"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "pass --subnet-id") || len(f.calls) != 0 {
		t.Errorf("--vpc without --az: err %v, lookups %v; want the refusal and no lookup", err, f.calls)
	}
}

// subnetsFakeEC2 answers DescribeSubnets with three subnets of vpc-a in
// us-east-1a, two tied on the most free IPs.
type subnetsFakeEC2 struct{ actions []string }

func (f *subnetsFakeEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.actions = append(f.actions, r.Form.Get("Action"))
	if r.Form.Get("Action") != "DescribeSubnets" || r.Form.Get("Filter.1.Value.1") != "vpc-a" ||
		r.Form.Get("Filter.2.Value.1") != "us-east-1a" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`))
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	// A fixed XML fixture for the SDK, not HTML served to a browser.
	// nosemgrep
	_, _ = w.Write([]byte(subnetsResponseXML))
}

// subnetsResponseXML: subnet-0ccc and subnet-0bbb tie on the most free IPs.
const subnetsResponseXML = `<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><subnetSet>` +
	`<item><subnetId>subnet-0ccc</subnetId><vpcId>vpc-a</vpcId><availabilityZone>us-east-1a</availabilityZone><availableIpAddressCount>200</availableIpAddressCount></item>` +
	`<item><subnetId>subnet-0zzz</subnetId><vpcId>vpc-a</vpcId><availabilityZone>us-east-1a</availabilityZone><availableIpAddressCount>50</availableIpAddressCount></item>` +
	`<item><subnetId>subnet-0bbb</subnetId><vpcId>vpc-a</vpcId><availabilityZone>us-east-1a</availabilityZone><availableIpAddressCount>200</availableIpAddressCount></item>` +
	`</subnetSet></DescribeSubnetsResponse>`

// The launch's pre-flight (every single launch, Linux or Windows, and every
// --dry-run) resolves --vpc --az to that subnet before anything is created,
// through the real client (#1389). Whatever the rest of the pre-flight does
// against this fake, the subnet is set first.
func TestPreflightResolvesVPCAZToASubnet(t *testing.T) {
	for _, osName := range []string{"linux", "windows"} {
		t.Run(osName, func(t *testing.T) {
			keepLaunchGlobals(t)
			dir := isolateAWSEnv(t)
			f := &subnetsFakeEC2{}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
			t.Setenv("AWS_ENDPOINT_URL", srv.URL)
			t.Setenv("AWS_MAX_ATTEMPTS", "1")
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			setLaunchFlag(t, "os", osName)
			client, err := aws.NewClientWithRegion(context.Background(), "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &aws.LaunchConfig{
				Name: "box", InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-0123456789abcdef0", TTL: "1h",
				VPCID: "vpc-a", AvailabilityZone: "us-east-1a",
			}
			_ = ensureAMIAndPreflight(context.Background(), client, cfg, progress.NewQuietProgress())
			if cfg.SubnetID != "subnet-0bbb" {
				t.Errorf("pre-flight left SubnetID %q, want subnet-0bbb (most free IPs, lowest ID); calls %v", cfg.SubnetID, f.actions)
			}
		})
	}
}
