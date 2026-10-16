package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spore-host/spawn/pkg/testutil"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// #1058: spored enforces --cost-limit as PricePerHour × compute hours, and the
// only rate truffle has is Linux's — m7i.xlarge us-east-1 $0.2016/hr against a
// Windows $0.3856/hr — so a Windows cap fired after about twice the spend it
// promised. The intent, from resolvePricePerHour's own contract: "a cost limit
// that cannot be priced cannot be enforced, and silently leaving it unenforced is
// worse than refusing to launch" — and a known-low price is not a price.

func TestWindowsCostLimitIsRefusedNotUnderpriced(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", CostLimit: 10, TargetOS: "windows"}
	err := resolvePricePerHour(context.Background(), fakePricer{price: 0.2016, source: truffleaws.PriceSourceLive}, cfg, true)
	if err == nil {
		t.Fatalf("a Windows --cost-limit was accepted at PricePerHour=%v — spored would let it spend ~1.9x the cap", cfg.PricePerHour)
	}
	if cfg.PricePerHour != 0 {
		t.Errorf("PricePerHour = %v on a refused Windows launch, want 0", cfg.PricePerHour)
	}
	for _, want := range []string{"10.00", "Windows", "--ttl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestWindowsWithoutCostLimitRecordsNoLinuxRate(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", TargetOS: "windows"}
	if err := resolvePricePerHour(context.Background(), fakePricer{price: 0.2016, source: truffleaws.PriceSourceLive}, cfg, true); err != nil {
		t.Fatalf("a Windows launch with no cost limit was refused: %v", err)
	}
	if cfg.PricePerHour != 0 {
		t.Errorf("PricePerHour = %v: the Linux rate would be written to spawn:price-per-hour and read as the "+
			"Windows rate by everything downstream", cfg.PricePerHour)
	}
}

// errNotCalled would surface if the pricer were consulted when it must not be.
var errNotCalled = errors.New("pricer must not be consulted when a rate is supplied")

// A caller that knows the real rate supplies it, and the cap is then honest.
func TestWindowsCostLimitWithASuppliedRateIsHonoured(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", CostLimit: 10, TargetOS: "windows", PricePerHour: 0.3856}
	if err := resolvePricePerHour(context.Background(), fakePricer{err: errNotCalled}, cfg, true); err != nil {
		t.Fatalf("a Windows cap with a caller-supplied rate was refused: %v", err)
	}
	if cfg.PricePerHour != 0.3856 {
		t.Errorf("PricePerHour = %v, want the supplied 0.3856", cfg.PricePerHour)
	}
}

func TestLinuxCostLimitIsUnchanged(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", CostLimit: 10}
	if err := resolvePricePerHour(context.Background(), fakePricer{price: 0.2016, source: truffleaws.PriceSourceLive}, cfg, false); err != nil {
		t.Fatal(err)
	}
	if cfg.PricePerHour != 0.2016 {
		t.Errorf("PricePerHour = %v, want 0.2016", cfg.PricePerHour)
	}
}

// Sweeps, burst, `spawn app` and other repos call Launch without TargetOS, and
// "" means Linux — so the AMI must decide when a cost limit is at stake.
func TestWindowsIsDetectedFromTheAMIWhenTargetOSIsUnset(t *testing.T) {
	ctx := context.Background()
	calls := 0
	windowsAMI := func(context.Context, string, string) bool { calls++; return true }

	cases := []struct {
		name      string
		cfg       LaunchConfig
		want      bool
		wantCalls int
	}{
		{"TargetOS windows", LaunchConfig{TargetOS: "Windows"}, true, 0},
		{"TargetOS linux wins over the AMI", LaunchConfig{TargetOS: "linux", CostLimit: 5}, false, 0},
		{"unset, cost limit: ask the AMI", LaunchConfig{AMI: "ami-1", CostLimit: 5}, true, 1},
		{"unset, no cost limit: no lookup", LaunchConfig{AMI: "ami-1"}, false, 0},
		{"unset, supplied rate: no lookup", LaunchConfig{AMI: "ami-1", CostLimit: 5, PricePerHour: 1}, false, 0},
	}
	for _, tc := range cases {
		calls = 0
		if got := launchIsWindowsForPricing(ctx, tc.cfg, windowsAMI); got != tc.want || calls != tc.wantCalls {
			t.Errorf("%s: got %v with %d AMI lookups, want %v with %d", tc.name, got, calls, tc.want, tc.wantCalls)
		}
	}
}

// End to end through Launch, the way a sweep, burst or another repo calls it:
// TargetOS unset, a Windows AMI, a cost limit. Substrate bundles the public
// Windows images behind their SSM parameters with Platform set, so this is the
// real AMI check, not a stand-in. The launch must be refused BEFORE RunInstances.
func TestLaunchRefusesAWindowsCostLimitWhenTargetOSIsUnset(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	p, err := ssm.NewFromConfig(env.AWSConfig).GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String("/aws/service/ami-windows-latest/Windows_Server-2025-English-Full-Base"),
	})
	if err != nil {
		t.Fatalf("resolve the bundled Windows AMI: %v", err)
	}
	ami := aws.ToString(p.Parameter.Value)
	c := NewClientFromConfig(env.AWSConfig)
	if !c.IsWindowsAMI(ctx, "us-east-1", ami) {
		t.Fatalf("%s is not reported as Windows, so this test proves nothing", ami)
	}

	_, err = c.Launch(ctx, LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", AMI: ami, CostLimit: 10, TTL: "1h"})
	if err == nil || !strings.Contains(err.Error(), "cost-limit") {
		t.Fatalf("Launch with TargetOS unset, a Windows AMI and --cost-limit = %v; want the #1058 refusal", err)
	}
	out, derr := ec2.NewFromConfig(env.AWSConfig).DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
	if derr != nil {
		t.Fatal(derr)
	}
	for _, r := range out.Reservations {
		if len(r.Instances) > 0 {
			t.Errorf("an instance was created despite the refusal: %s", aws.ToString(r.Instances[0].InstanceId))
		}
	}
}
