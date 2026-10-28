package cmd

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/input"
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

// `--vpc` without `--subnet-id` is refused on every non-sweep launch before a
// single AWS call (#1389): RunInstances takes a subnet, not a VPC, so it used
// to land an ordinary Linux instance in the region's default VPC. The cases
// are the single launch (Linux, Windows, --dry-run), the job array, MPI and
// the batch queue, all of which go through runLaunch's flag checks.
func TestVPCWithoutSubnetIsRefusedBeforeAnyAWSCall(t *testing.T) {
	cases := map[string]map[string]string{
		"linux":       {},
		"windows":     {"os": "windows", "ami": "ami-0123456789abcdef0"},
		"dry-run":     {"dry-run": "true"},
		"job array":   {"count": "2", "job-array-name": "arr"},
		"mpi":         {"count": "2", "job-array-name": "arr", "mpi": "true"},
		"batch queue": {"batch-queue": "queue.json"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			f := serveTagLimitAWS(t)
			for k, v := range map[string]string{
				"vpc": "vpc-0123456789abcdef0", "instance-type": "m7i.large",
				"region": "us-east-1", "ttl": "1h", "name": "box", "yes": "true",
			} {
				setLaunchFlag(t, k, v)
			}
			for k, v := range extra {
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
				t.Errorf("runLaunch = %v; want a refusal naming --vpc and telling the user to pass --subnet-id", err)
			}
		})
	}
}

// --vpc with a subnet (either spelling) passes the check; ValidateSubnetInVPC
// then checks that the two agree (#673).
func TestVPCWithSubnetIsAccepted(t *testing.T) {
	for _, alias := range []string{"subnet-id", "subnet"} {
		t.Run(alias, func(t *testing.T) {
			keepLaunchGlobals(t)
			setLaunchFlag(t, "vpc", "vpc-0123456789abcdef0")
			setLaunchFlag(t, alias, "subnet-0123456789abcdef0")
			if err := validateVPCHasSubnet(); err != nil {
				t.Errorf("--vpc with --%s was refused: %v", alias, err)
			}
		})
	}
	keepLaunchGlobals(t)
	if err := validateVPCHasSubnet(); err != nil {
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
