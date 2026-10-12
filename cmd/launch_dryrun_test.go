package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestDescribeLaunchPlan_ReflectsResolvedConfig checks that the plan the
// dry-run prints accurately reflects a representative set of resolved
// LaunchConfig fields, mirroring what buildLaunchConfig would have set from
// flags (spawn#569). Pure — no AWS.
func TestDescribeLaunchPlan_ReflectsResolvedConfig(t *testing.T) {
	prev := struct {
		instanceProfile, iamRole      string
		iamPolicy, iamManagedPolicies []string
		iamPolicyFile                 string
		mpiEnabled                    bool
		jobArrayName                  string
		count                         int
	}{instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile, mpiEnabled, jobArrayName, count}
	t.Cleanup(func() {
		instanceProfile, iamRole = prev.instanceProfile, prev.iamRole
		iamPolicy, iamManagedPolicies = prev.iamPolicy, prev.iamManagedPolicies
		iamPolicyFile = prev.iamPolicyFile
		mpiEnabled, jobArrayName, count = prev.mpiEnabled, prev.jobArrayName, prev.count
	})
	instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "", "", nil, nil, ""
	mpiEnabled, jobArrayName, count = false, "", 1

	cfg := &spawnaws.LaunchConfig{
		Name:              "my-worker",
		InstanceType:      "m7i.large",
		Region:            "us-east-1",
		AMI:               "ami-0abc123",
		TTL:               "8h",
		OnComplete:        "terminate",
		Spot:              true,
		SpotMaxPrice:      "0.50",
		RootVolumeSizeGiB: 80,
		Tags:              map[string]string{"spawn:team-id": "t-1"},
	}
	plan := describeLaunchPlan(cfg)

	if plan.Name != "my-worker" {
		t.Errorf("Name = %q, want my-worker", plan.Name)
	}
	if plan.InstanceType != "m7i.large" {
		t.Errorf("InstanceType = %q, want m7i.large", plan.InstanceType)
	}
	if plan.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", plan.Region)
	}
	if plan.AMI != "ami-0abc123" {
		t.Errorf("AMI = %q, want ami-0abc123", plan.AMI)
	}
	if !plan.Spot || plan.SpotMaxPrice != "0.50" {
		t.Errorf("Spot/SpotMaxPrice = %v/%q, want true/0.50", plan.Spot, plan.SpotMaxPrice)
	}
	if plan.TTL != "8h" || plan.OnComplete != "terminate" {
		t.Errorf("TTL/OnComplete = %q/%q, want 8h/terminate", plan.TTL, plan.OnComplete)
	}
	if plan.RootVolumeSizeGiB != 80 {
		t.Errorf("RootVolumeSizeGiB = %d, want 80", plan.RootVolumeSizeGiB)
	}
	if plan.Tags["spawn:team-id"] != "t-1" {
		t.Errorf("Tags[spawn:team-id] = %q, want t-1", plan.Tags["spawn:team-id"])
	}
	// No IAM flags and no config.IamInstanceProfile → the shared default, not a
	// prediction (SetupSporedIAMRole's own deterministic name, no hashing).
	if plan.IamInstanceProfile != "spored-instance-profile" {
		t.Errorf("IamInstanceProfile = %q, want spored-instance-profile", plan.IamInstanceProfile)
	}
	if plan.IamInstanceProfileIsPreview {
		t.Error("IamInstanceProfileIsPreview should be false for the shared default")
	}
}

// TestPreviewIAMInstanceProfile covers previewIAMInstanceProfile's precedence
// against ensureIAMProfile's real one (spawn#569): --instance-profile wins
// outright, then --iam-role by name, then the deterministic hash prediction
// for --iam-policy/--iam-policy-file/--iam-managed-policies, else the shared
// default. Only the hash-prediction branch is marked "preview" — the others
// are already-known names, same as a real launch would show.
func TestPreviewIAMInstanceProfile(t *testing.T) {
	prev := struct {
		instanceProfile, iamRole      string
		iamPolicy, iamManagedPolicies []string
		iamPolicyFile                 string
	}{instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile}
	t.Cleanup(func() {
		instanceProfile, iamRole = prev.instanceProfile, prev.iamRole
		iamPolicy, iamManagedPolicies = prev.iamPolicy, prev.iamManagedPolicies
		iamPolicyFile = prev.iamPolicyFile
	})

	t.Run("instance-profile wins outright", func(t *testing.T) {
		instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "my-existing-profile", "some-role", []string{"s3:ReadOnly"}, nil, ""
		name, isPreview := previewIAMInstanceProfile(&spawnaws.LaunchConfig{})
		if name != "my-existing-profile" || isPreview {
			t.Errorf("got (%q, %v), want (my-existing-profile, false)", name, isPreview)
		}
	})

	t.Run("iam-role names a specific role", func(t *testing.T) {
		instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "", "my-role", nil, nil, ""
		name, isPreview := previewIAMInstanceProfile(&spawnaws.LaunchConfig{})
		if name != "my-role" || isPreview {
			t.Errorf("got (%q, %v), want (my-role, false)", name, isPreview)
		}
	})

	t.Run("iam-policy predicts the deterministic hash name", func(t *testing.T) {
		instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "", "", []string{"s3:ReadOnly"}, nil, ""
		name, isPreview := previewIAMInstanceProfile(&spawnaws.LaunchConfig{})
		want := spawnaws.PredictedInstanceRoleName(spawnaws.IAMRoleConfig{Policies: []string{"s3:ReadOnly"}})
		if name != want || !isPreview {
			t.Errorf("got (%q, %v), want (%q, true)", name, isPreview, want)
		}
	})

	t.Run("no IAM flags → shared default, not a prediction", func(t *testing.T) {
		instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "", "", nil, nil, ""
		name, isPreview := previewIAMInstanceProfile(&spawnaws.LaunchConfig{})
		if name != "spored-instance-profile" || isPreview {
			t.Errorf("got (%q, %v), want (spored-instance-profile, false)", name, isPreview)
		}
	})

	t.Run("config.IamInstanceProfile already resolved wins over everything", func(t *testing.T) {
		instanceProfile, iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "flag-profile", "", nil, nil, ""
		name, isPreview := previewIAMInstanceProfile(&spawnaws.LaunchConfig{IamInstanceProfile: "already-resolved"})
		if name != "already-resolved" || isPreview {
			t.Errorf("got (%q, %v), want (already-resolved, false)", name, isPreview)
		}
	})
}

// TestPreviewSecurityGroupPlan covers previewSecurityGroupPlan's branches
// against ensureSecurityGroup's real ones.
func TestPreviewSecurityGroupPlan(t *testing.T) {
	prevMPI, prevJobArray := mpiEnabled, jobArrayName
	t.Cleanup(func() { mpiEnabled, jobArrayName = prevMPI, prevJobArray })

	t.Run("mpi creates an mpi security group", func(t *testing.T) {
		mpiEnabled, jobArrayName = true, "compute"
		got := previewSecurityGroupPlan(&spawnaws.LaunchConfig{})
		if !strings.Contains(got, "spawn-mpi-compute") {
			t.Errorf("got %q, want it to mention spawn-mpi-compute", got)
		}
	})

	t.Run("windows with no explicit SG creates a windows security group", func(t *testing.T) {
		mpiEnabled = false
		got := previewSecurityGroupPlan(&spawnaws.LaunchConfig{TargetOS: "windows", Name: "winbox"})
		if !strings.Contains(got, "spawn-windows-winbox") {
			t.Errorf("got %q, want it to mention spawn-windows-winbox", got)
		}
	})

	t.Run("explicit security groups are used as-is", func(t *testing.T) {
		mpiEnabled = false
		got := previewSecurityGroupPlan(&spawnaws.LaunchConfig{SecurityGroupIDs: []string{"sg-123"}})
		if !strings.Contains(got, "explicitly given") {
			t.Errorf("got %q, want it to mention the explicit security group", got)
		}
	})

	t.Run("plain linux with nothing set uses the default SG", func(t *testing.T) {
		mpiEnabled = false
		got := previewSecurityGroupPlan(&spawnaws.LaunchConfig{})
		if !strings.Contains(got, "default security group") {
			t.Errorf("got %q, want it to mention the default security group", got)
		}
	})
}

// TestRunLaunchDryRun_InvalidFlagCombinationFailsClosed is the "doubles as a
// linter" requirement (spawn#569): --count > 1 without --job-array-name is
// rejected with the SAME error a real launch would give (validateJobArrayFlags,
// shared with launchWithProgress — not a second copy of the check). Deliberately
// avoids --mpi here: that path also calls preflightInstanceConstraints (a real
// DescribeInstanceTypes lookup), which would couple this test to Substrate's
// seeded instance-type catalog; --job-array-name's own check needs no AWS call
// at all (mpiEnabled/efaEnabled/hibernate all false short-circuits it), so it
// isolates the flag-validation behavior this test is actually about.
func TestRunLaunchDryRun_InvalidFlagCombinationFailsClosed(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)

	prevCount, prevJobArray, prevOS := count, jobArrayName, osFlag
	t.Cleanup(func() { count, jobArrayName, osFlag = prevCount, prevJobArray, prevOS })
	count, jobArrayName, osFlag = 3, "", "linux"

	cfg := &spawnaws.LaunchConfig{InstanceType: "c5.large", Region: "us-east-1", AMI: "auto"}
	var out bytes.Buffer
	err := runLaunchDryRun(context.Background(), &out, client, cfg)
	if err == nil {
		t.Fatal("expected an error for --count > 1 with no --job-array-name, got nil")
	}
	if !strings.Contains(err.Error(), "--job-array-name is required when --count > 1") {
		t.Errorf("error = %q, want it to mention --job-array-name is required (the real launch's own error)", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("expected no output on a rejected dry-run, got: %s", out.String())
	}
}

// TestRunLaunchDryRun_JSONOutputIsValidAndParseable confirms -o json produces
// parseable JSON reflecting the resolved config (spawn#569's third test
// requirement).
func TestRunLaunchDryRun_JSONOutputIsValidAndParseable(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)

	prevOut := spawnOutputFormat
	t.Cleanup(func() { spawnOutputFormat = prevOut })
	spawnOutputFormat = "json"

	prevAMI, prevOS := ami, osFlag
	t.Cleanup(func() { ami, osFlag = prevAMI, prevOS })
	ami, osFlag = "ami-0abc123", "linux" // pinned, avoids IsWindowsAMI/DescribeImages

	cfg := &spawnaws.LaunchConfig{
		Name:         "json-test",
		InstanceType: "m7i.large",
		Region:       "us-east-1",
		AMI:          "ami-0abc123",
		TTL:          "1h",
	}
	var out bytes.Buffer
	if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
		t.Fatalf("runLaunchDryRun: %v", err)
	}

	var decoded launchPlan
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out.String())
	}
	if decoded.Name != "json-test" {
		t.Errorf("decoded Name = %q, want json-test", decoded.Name)
	}
	if decoded.InstanceType != "m7i.large" {
		t.Errorf("decoded InstanceType = %q, want m7i.large", decoded.InstanceType)
	}
	if decoded.TTL != "1h" {
		t.Errorf("decoded TTL = %q, want 1h", decoded.TTL)
	}
}

// TestRunLaunchDryRun_UserDataReflectsFlags checks --user-data's resolved
// content size is reflected in the plan (spawn#569's "a representative set of
// flags" requirement), and that a --user-data-file pointing at a missing file
// fails closed with the same error a real launch would give (resolveCustomUserData
// is the exact function buildUserData calls) — "doubles as a linter".
func TestRunLaunchDryRun_UserDataReflectsFlags(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)

	prevUserData, prevUserDataFile, prevAMI, prevOS := userData, userDataFile, ami, osFlag
	t.Cleanup(func() {
		userData, userDataFile, ami, osFlag = prevUserData, prevUserDataFile, prevAMI, prevOS
	})
	ami, osFlag = "ami-0abc123", "linux"

	t.Run("inline --user-data is sized in the plan", func(t *testing.T) {
		userData, userDataFile = "#!/bin/bash\necho hello\n", ""
		cfg := &spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-0abc123", TTL: "1h"}
		var out bytes.Buffer
		if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
			t.Fatalf("runLaunchDryRun: %v", err)
		}
		want := fmt.Sprintf("%d bytes", len("#!/bin/bash\necho hello\n"))
		if !strings.Contains(out.String(), want) {
			t.Errorf("expected the plan to report %s of custom user data, got:\n%s", want, out.String())
		}
	})

	t.Run("a missing --user-data-file fails closed", func(t *testing.T) {
		userData, userDataFile = "", "/nonexistent/path/does-not-exist.sh"
		cfg := &spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-0abc123", TTL: "1h"}
		var out bytes.Buffer
		err := runLaunchDryRun(context.Background(), &out, client, cfg)
		if err == nil {
			t.Fatal("expected an error for a missing --user-data-file, got nil")
		}
		if out.Len() != 0 {
			t.Errorf("expected no output on a rejected dry-run, got: %s", out.String())
		}
	})
}

// TestRunLaunchDryRun_LaunchesNothing is the Tier-0 "assert on the
// observable, not the intermediate value" e2e for spawn#569's hard safety
// requirement: running the dry-run against a real (Substrate-emulated) AWS
// backend must create ZERO EC2 instances, regardless of what the resolved
// config looks like. It asserts via DescribeInstances — the actual AWS
// inventory — not by inspecting whether some internal function was "called".
func TestRunLaunchDryRun_LaunchesNothing(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	ec2Client := ec2.NewFromConfig(env.AWSConfig)

	prevAMI, prevOS := ami, osFlag
	t.Cleanup(func() { ami, osFlag = prevAMI, prevOS })
	ami, osFlag = "auto", "linux"

	cfg := &spawnaws.LaunchConfig{
		Name:         "nothing-launched",
		InstanceType: "m7i.large",
		Region:       "us-east-1",
		TTL:          "1h",
	}
	var out bytes.Buffer
	if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
		t.Fatalf("runLaunchDryRun: %v", err)
	}
	if out.Len() == 0 {
		t.Error("expected dry-run output, got none")
	}

	result, err := ec2Client.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	for _, res := range result.Reservations {
		if len(res.Instances) > 0 {
			t.Errorf("expected zero EC2 instances after --dry-run, found %d", len(res.Instances))
		}
	}
}

// seedAL2023AMIParams pre-populates the SSM parameters GetRecommendedAMI reads
// for AL2023 auto-detection, mirroring pkg/aws/ami_test.go's own seeding.
// Needed because Substrate does not auto-seed these paths (they are AWS
// public parameters, not something the emulator invents on its own).
func seedAL2023AMIParams(t *testing.T, env *testutil.TestEnv) {
	t.Helper()
	ssmClient := ssm.NewFromConfig(env.AWSConfig)
	params := map[string]string{
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64": "ami-x86-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64":  "ami-arm-standard",
	}
	for name, val := range params {
		if _, err := ssmClient.PutParameter(context.Background(), &ssm.PutParameterInput{
			Name:  aws.String(name),
			Value: aws.String(val),
			Type:  ssmtypes.ParameterTypeString,
		}); err != nil {
			t.Fatalf("PutParameter %s: %v", name, err)
		}
	}
}

// The --cost-limit cap must appear in the preview, in both output formats (#811).
//
// The mechanism being gated is that the preview PRINTS the cap, not that
// costLimitScopeNote returns a string. That distinction is the whole bug: the
// function was correct, its test was green, and the note never reached a user
// because nothing called it. A test of the helper alone would have passed
// throughout — it did, for months.
func TestLaunchPreviewStatesTheCostLimit(t *testing.T) {
	plan := launchPlan{
		Name:                 "p",
		InstanceType:         "c7g.large",
		Region:               "us-east-1",
		OnDemandPricePerHour: 0.0725,
		MaxCostUSD:           0.58,
		CostLimitUSD:         50,
	}

	var buf bytes.Buffer
	renderLaunchPlanTable(&buf, plan)
	out := buf.String()
	if !strings.Contains(out, "Cost limit:") {
		t.Errorf("the launch preview does not mention --cost-limit. It prints "+
			"`Max cost: ~$%.2f`, which is rate × ttl and compute only, so omitting the cap "+
			"invites the reader to assume that line IS the cap.\n--- preview ---\n%s",
			plan.MaxCostUSD, out)
	}
	// The scope, not just the number: a bare "$50.00" next to a compute-only
	// estimate says nothing about storage being outside what spored can reclaim.
	for _, want := range []string{"compute + storage", "reaper"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview states the cap but not its scope (missing %q)", want)
		}
	}
	// And the compute-only nature of Max cost is now said out loud, since the two
	// lines sit together and mean different things.
	if !strings.Contains(out, "compute only") {
		t.Error("`Max cost:` does not say it covers compute only, so it reads as the total " +
			"beside a cap that is not compute-only")
	}

	// No cap set: nothing to say, and nothing printed.
	buf.Reset()
	plan.CostLimitUSD = 0
	renderLaunchPlanTable(&buf, plan)
	if strings.Contains(buf.String(), "Cost limit:") {
		t.Error("the preview states a cost limit when none was set")
	}
}

// The machine-readable preview must carry it too, or a wrapper reading -o json
// cannot see the cap that governs the launch it is about to make.
func TestLaunchPlanJSONCarriesTheCostLimit(t *testing.T) {
	b, err := json.Marshal(launchPlan{CostLimitUSD: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cost_limit_usd":50`) {
		t.Errorf("launchPlan JSON has no cost_limit_usd: %s", b)
	}
	// omitempty: an unset cap must not render as a 0 that reads like "capped at $0".
	b, err = json.Marshal(launchPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "cost_limit_usd") {
		t.Errorf("an unset cap is serialized as 0, which reads as a $0 cap: %s", b)
	}
}

// The cap must be carried from the resolved config into the plan.
//
// Separate from TestLaunchPreviewStatesTheCostLimit because the two halves fail
// independently and the symptom is identical: with the render call present but the
// field never populated, the preview is silent about a cap that is in force. That
// revert passed the render gate, which is why this one exists.
func TestDescribeLaunchPlanCarriesTheCostLimit(t *testing.T) {
	prevCount := count
	t.Cleanup(func() { count = prevCount })
	count = 1

	plan := describeLaunchPlan(&spawnaws.LaunchConfig{
		Name: "x", InstanceType: "c7g.large", Region: "us-east-1", CostLimit: 42.5,
	})
	if plan.CostLimitUSD != 42.5 {
		t.Errorf("CostLimitUSD = %v, want 42.5 — the cap never reaches the preview, so a "+
			"launch bounded at $42.50 previews as unbounded", plan.CostLimitUSD)
	}

	plan = describeLaunchPlan(&spawnaws.LaunchConfig{
		Name: "x", InstanceType: "c7g.large", Region: "us-east-1",
	})
	if plan.CostLimitUSD != 0 {
		t.Errorf("CostLimitUSD = %v with no cap configured", plan.CostLimitUSD)
	}
}
