package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// --no-public-ip (#1433), CLI side.

// stubNoPublicIPPreflight replaces the subnet check: it records each config it
// is asked about, pins a subnet as the real one does for --az, and returns
// err.
func stubNoPublicIPPreflight(t *testing.T, err error) *[]aws.LaunchConfig {
	t.Helper()
	var seen []aws.LaunchConfig
	prev := preflightNoPublicIPFor
	preflightNoPublicIPFor = func(_ context.Context, _ *aws.Client, cfg *aws.LaunchConfig, _ []string, _ io.Writer) (aws.PrivateCheck, error) {
		seen = append(seen, *cfg)
		if cfg.SubnetID == "" {
			cfg.SubnetID = "subnet-0private"
		}
		return aws.PrivateCheck{}, err
	}
	t.Cleanup(func() { preflightNoPublicIPFor = prev })
	return &seen
}

// Gate: on every launch path that takes the flag (single launch, its dry-run,
// a job array and an MPI cohort), the subnet check runs and a refusal stops
// the launch before a single mutating AWS call: no key pair, no role, no
// security group, no instance.
func TestNoPublicIPSubnetCheckRunsBeforeAnyMutatingCall(t *testing.T) {
	refused := fmt.Errorf("%w: subnet-0private cannot reach s3", aws.ErrPrivateNetworkIncomplete)
	for name, extra := range map[string]map[string]string{
		"single launch": {},
		"dry-run":       {"dry-run": "true"},
		"job array":     {"count": "2", "job-array-name": "arr"},
		"mpi cohort":    {"count": "2", "job-array-name": "arr", "mpi": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			seen := stubNoPublicIPPreflight(t, refused)
			setEFALaunchFlags(t, extra)
			setLaunchFlag(t, "no-public-ip", "true")
			setLaunchFlag(t, "az", "us-east-1a")
			err := runLaunch(launchCmd, nil)
			if !errors.Is(err, aws.ErrPrivateNetworkIncomplete) {
				t.Fatalf("runLaunch = %v, want the subnet check's refusal", err)
			}
			if len(*seen) != 1 || !(*seen)[0].NoPublicIP {
				t.Errorf("subnet check asked about %d configs (%+v), want one with NoPublicIP", len(*seen), *seen)
			}
			if m := f.mutating(); len(m) != 0 {
				t.Errorf("mutating AWS call(s) before the refusal: %v", m)
			}
		})
	}
}

// The MPI bootstrap downloads the EFA installer from the internet, which no
// VPC endpoint serves: the CLI adds it to what the check must find a way to.
func TestNoPublicIPMPIWithEFANeedsTheInternet(t *testing.T) {
	keepLaunchGlobals(t)
	mpiEnabled, efaEnabled = true, true
	if needs := noPublicIPInternetNeeds(); len(needs) != 1 || !strings.Contains(needs[0], "efa-installer.amazonaws.com") {
		t.Errorf("needs = %q, want the EFA installer", needs)
	}
	mpiEnabled = false
	if needs := noPublicIPInternetNeeds(); len(needs) != 0 {
		t.Errorf("--efa without --mpi installs nothing, needs = %q", needs)
	}
}

// --no-public-ip allocates no Elastic IP, so the region's Elastic IP quota is
// not consulted, and the dry-run says so: Public IP none, no Elastic IP.
func TestNoPublicIPDryRunShowsNoPublicIPAndSkipsTheElasticIPQuota(t *testing.T) {
	f := &efaSubnetAWS{subnets: []efaSubnetFixture{{"subnet-0big", "vpc-default", "us-east-1a", 400}}}
	serveEFASubnetAWS(t, f)
	quota := stubElasticIPQuota(t, aws.ElasticIPQuota{InUse: 5, Limit: 5}, nil) // full: would refuse
	stubNoPublicIPPreflight(t, nil)
	setEFALaunchFlags(t, map[string]string{"az": "us-east-1a", "no-public-ip": "true"})
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
	if plan.PublicIP != "none (private only)" || plan.ElasticIP || plan.EFAInterfaces != 2 {
		t.Errorf("plan public IP %q, Elastic IP %t, %d interfaces; want none (private only), false, 2", plan.PublicIP, plan.ElasticIP, plan.EFAInterfaces)
	}
	if plan.SubnetID != "subnet-0private" {
		t.Errorf("plan subnet %q, want the one the check pinned", plan.SubnetID)
	}
	if *quota != 0 {
		t.Errorf("the Elastic IP quota was read %d time(s) for a launch that allocates none", *quota)
	}

	var text bytes.Buffer
	renderLaunchPlanTable(&text, plan)
	if !strings.Contains(text.String(), "Public IP:    none (private only)") || strings.Contains(text.String(), "Elastic IP allocated") {
		t.Errorf("table:\n%s", text.String())
	}
}

// Without the flag the dry-run keeps the Elastic IP line and no Public IP
// override.
func TestWithoutNoPublicIPTheDryRunPlanIsUnchanged(t *testing.T) {
	cfg := aws.LaunchConfig{EFAEnabled: true, EFAInterfaces: 2, SubnetID: "subnet-1"}
	plan := describeLaunchPlan(&cfg)
	if err := applyEFAPlan(&plan, cfg, cardsCapacity("c8gn.48xlarge", 2)); err != nil {
		t.Fatal(err)
	}
	if plan.PublicIP != "" || !plan.ElasticIP {
		t.Errorf("plan public IP %q, Elastic IP %t; want empty, true", plan.PublicIP, plan.ElasticIP)
	}
}

// The combinations --no-public-ip refuses, each before any AWS work.
func TestNoPublicIPRefusesWhatNeedsAPublicIP(t *testing.T) {
	for name, tc := range map[string]struct {
		flags map[string]string
		want  string
	}{
		"--dns":                         {map[string]string{"no-public-ip": "true", "dns": "other", "no-dns": "false"}, "--dns other cannot be used with --no-public-ip"},
		"--dns-domain":                  {map[string]string{"no-public-ip": "true", "dns-domain": "example.org"}, "--dns-domain"},
		"--wait-for-ssh":                {map[string]string{"no-public-ip": "true", "wait-for-ssh": "true"}, "--wait-for-ssh cannot be used with --no-public-ip"},
		"--allow-missing-endpoints":     {map[string]string{"allow-missing-endpoints": "true"}, "applies only with --no-public-ip"},
		"--batch-queue":                 {map[string]string{"no-public-ip": "true", "batch-queue": "q.json"}, "not supported with --batch-queue"},
		"neither --subnet-id nor --az":  {map[string]string{"no-public-ip": "true"}, "pass --subnet-id, or --az"},
		"--no-public-ip --dns-function": {map[string]string{"no-public-ip": "true", "dns-function-arn": "arn:aws:lambda:us-east-1:1:function:f"}, "--dns-function-arn"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			setEFALaunchFlags(t, tc.flags)
			err := runLaunch(launchCmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runLaunch = %v, want %q", err, tc.want)
			}
			if m := f.mutating(); len(m) != 0 {
				t.Errorf("mutating AWS call(s) before the refusal: %v", m)
			}
			if n := f.count("DescribeVpcEndpoints"); n != 0 {
				t.Errorf("refused combination still checked the subnet (%d DescribeVpcEndpoints)", n)
			}
		})
	}
}

// The wizard reads no launch flags, so it refuses the flag rather than drop it.
func TestNoPublicIPRefusedByTheWizard(t *testing.T) {
	keepLaunchGlobals(t)
	noPublicIP = true
	if err := validateNoPublicIPPath(false, true); err == nil || !strings.Contains(err.Error(), "wizard") {
		t.Errorf("wizard: err = %v", err)
	}
	if err := validateNoPublicIPPath(false, false); err != nil {
		t.Errorf("flags path refused: %v", err)
	}
}

// The DNS record --name implies is skipped, not refused, so a named private
// launch needs no --no-dns; the spawn:dns-name tag spored registers from is
// not written.
func TestNoPublicIPSkipsTheNameImpliedDNSRecord(t *testing.T) {
	keepLaunchGlobals(t)
	isolateAWSEnv(t)
	stubTeamName(t)
	for k, v := range map[string]string{"instance-type": "m7i.large", "region": "us-east-1", "ttl": "1h", "name": "box", "no-public-ip": "true", "subnet-id": "subnet-1"} {
		setLaunchFlag(t, k, v)
	}
	var config *aws.LaunchConfig
	var err error
	stderr := captureStderr(t, func() { config, err = buildLaunchConfig(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if config.DNSName != "" || !config.NoPublicIP {
		t.Errorf("DNSName = %q, NoPublicIP = %t; want no DNS name and NoPublicIP", config.DNSName, config.NoPublicIP)
	}
	if !strings.Contains(stderr, "no DNS record for box") {
		t.Errorf("no notice that the DNS record is skipped: %q", stderr)
	}
}

// A foreground sweep (and its resume) checks every --no-public-ip member, in
// its own subnet, and names the parameter set it refuses.
func TestPreflightSweepNoPublicIPChecksEveryMember(t *testing.T) {
	seen := stubNoPublicIPPreflight(t, nil)
	cfgs := []*aws.LaunchConfig{
		{NoPublicIP: true, AvailabilityZone: "us-east-1a"},
		{},
		{NoPublicIP: true, SubnetID: "subnet-2"},
	}
	if err := preflightSweepNoPublicIP(context.Background(), nil, cfgs, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 || cfgs[0].SubnetID != "subnet-0private" || cfgs[2].SubnetID != "subnet-2" {
		t.Errorf("checked %d members; subnets %q, %q", len(*seen), cfgs[0].SubnetID, cfgs[2].SubnetID)
	}
	stubNoPublicIPPreflight(t, aws.ErrPrivateNetworkIncomplete)
	err := preflightSweepNoPublicIP(context.Background(), nil, cfgs, []int{4, 5, 6}, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "parameter set 4:") {
		t.Errorf("err = %v, want it to name parameter set 4", err)
	}
}

// Both foreground sweep paths run the member check after the EFA count is
// resolved and before the EFA subnets and the SSH key, the first thing they
// create. Matched on the invocation, not the name.
func TestSweepPathsCheckNoPublicIPMembersBeforeCreatingAnything(t *testing.T) {
	for file, idx := range map[string]string{"launch_sweep.go": "nil", "resume.go": "pending"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		efa := strings.Index(s, "resolveSweepEFA(launchConfigs, ")
		at := strings.Index(s, "preflightSweepNoPublicIP(ctx, awsClient, launchConfigs, "+idx+", os.Stderr)")
		subnets := strings.Index(s, "resolveSweepEFASubnets(ctx, awsClient, launchConfigs, ")
		key := strings.Index(s, "setupSSHKey(ctx, ")
		if efa < 0 || at < 0 || subnets < 0 || key < 0 || !(efa < at && at < subnets && subnets < key) {
			t.Errorf("%s: want preflightSweepNoPublicIP after resolveSweepEFA and before resolveSweepEFASubnets and setupSSHKey (offsets %d, %d, %d, %d)",
				file, efa, at, subnets, key)
		}
	}
}

// `spawn connect` to an instance with no public IP says it is using SSM, and
// answers an --ssh it cannot honour.
func TestConnectToAPrivateInstanceSaysItUsesSSM(t *testing.T) {
	never := func(context.Context, int32) (aws.PortIngress, error) {
		t.Fatal("read the security groups of an instance with no public IP")
		return aws.PortIngress{}, nil
	}
	for _, forceSSH := range []bool{false, true} {
		tr, note, err := linuxConnectTransport(context.Background(), false, forceSSH, false, "", 22, never)
		if err != nil || tr != connectTransportSSM {
			t.Fatalf("--ssh=%t: transport %v, err %v; want SSM", forceSSH, tr, err)
		}
		want := "no public IP (private only), so connect uses SSM"
		if forceSSH {
			want = "--ssh needs a public IP"
		}
		if !strings.Contains(note, want) {
			t.Errorf("--ssh=%t: note %q, want it to say %q", forceSSH, note, want)
		}
	}
	if _, note, _ := linuxConnectTransport(context.Background(), true, false, false, "", 22, never); note != "" {
		t.Errorf("--session-manager asked for SSM; note %q, want none", note)
	}
}
