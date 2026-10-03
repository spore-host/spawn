package reaperdeploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// --- fakes: record calls, return plausible shapes, touch no network -------------

type fakeIAM struct {
	roleExists bool
	calls      []string
	policyDoc  string
}

func (f *fakeIAM) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.calls = append(f.calls, "GetRole")
	if !f.roleExists {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: awssdk.String(RoleName)}}, nil
}
func (f *fakeIAM) CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.calls = append(f.calls, "CreateRole")
	return &iam.CreateRoleOutput{}, nil
}
func (f *fakeIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.calls = append(f.calls, "PutRolePolicy")
	f.policyDoc = awssdk.ToString(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, nil
}
func (f *fakeIAM) AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.calls = append(f.calls, "AttachRolePolicy")
	return &iam.AttachRolePolicyOutput{}, nil
}
func (f *fakeIAM) DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.calls = append(f.calls, "DeleteRolePolicy")
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (f *fakeIAM) DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.calls = append(f.calls, "DetachRolePolicy")
	return &iam.DetachRolePolicyOutput{}, nil
}
func (f *fakeIAM) DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.calls = append(f.calls, "DeleteRole")
	return &iam.DeleteRoleOutput{}, nil
}

type fakeLambda struct {
	exists  bool
	dryRun  string // REAPER_DRY_RUN on the existing function
	calls   []string
	lastEnv map[string]string
}

func (f *fakeLambda) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	f.calls = append(f.calls, "GetFunction")
	if !f.exists {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	return &lambda.GetFunctionOutput{Configuration: &lambdatypes.FunctionConfiguration{
		FunctionArn: awssdk.String("arn:aws:lambda:us-east-1:1:function/" + FunctionName),
		Environment: &lambdatypes.EnvironmentResponse{Variables: map[string]string{"REAPER_DRY_RUN": f.dryRun}},
	}}, nil
}
func (f *fakeLambda) CreateFunction(_ context.Context, in *lambda.CreateFunctionInput, _ ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error) {
	f.calls = append(f.calls, "CreateFunction")
	if in.Environment != nil {
		f.lastEnv = in.Environment.Variables
	}
	return &lambda.CreateFunctionOutput{FunctionArn: awssdk.String("arn:aws:lambda:us-east-1:1:function/" + FunctionName)}, nil
}
func (f *fakeLambda) UpdateFunctionCode(context.Context, *lambda.UpdateFunctionCodeInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error) {
	f.calls = append(f.calls, "UpdateFunctionCode")
	return &lambda.UpdateFunctionCodeOutput{}, nil
}
func (f *fakeLambda) UpdateFunctionConfiguration(_ context.Context, in *lambda.UpdateFunctionConfigurationInput, _ ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error) {
	f.calls = append(f.calls, "UpdateFunctionConfiguration")
	if in.Environment != nil {
		f.lastEnv = in.Environment.Variables
	}
	return &lambda.UpdateFunctionConfigurationOutput{}, nil
}
func (f *fakeLambda) AddPermission(context.Context, *lambda.AddPermissionInput, ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error) {
	f.calls = append(f.calls, "AddPermission")
	return &lambda.AddPermissionOutput{}, nil
}
func (f *fakeLambda) RemovePermission(context.Context, *lambda.RemovePermissionInput, ...func(*lambda.Options)) (*lambda.RemovePermissionOutput, error) {
	f.calls = append(f.calls, "RemovePermission")
	return &lambda.RemovePermissionOutput{}, nil
}
func (f *fakeLambda) DeleteFunction(context.Context, *lambda.DeleteFunctionInput, ...func(*lambda.Options)) (*lambda.DeleteFunctionOutput, error) {
	f.calls = append(f.calls, "DeleteFunction")
	return &lambda.DeleteFunctionOutput{}, nil
}

type fakeEvents struct{ calls []string }

func (f *fakeEvents) PutRule(context.Context, *eventbridge.PutRuleInput, ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error) {
	f.calls = append(f.calls, "PutRule")
	return &eventbridge.PutRuleOutput{}, nil
}
func (f *fakeEvents) PutTargets(context.Context, *eventbridge.PutTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error) {
	f.calls = append(f.calls, "PutTargets")
	return &eventbridge.PutTargetsOutput{}, nil
}
func (f *fakeEvents) DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	f.calls = append(f.calls, "DescribeRule")
	return &eventbridge.DescribeRuleOutput{}, nil
}
func (f *fakeEvents) RemoveTargets(context.Context, *eventbridge.RemoveTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	f.calls = append(f.calls, "RemoveTargets")
	return &eventbridge.RemoveTargetsOutput{}, nil
}
func (f *fakeEvents) DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	f.calls = append(f.calls, "DeleteRule")
	return &eventbridge.DeleteRuleOutput{}, nil
}

type fakeS3 struct {
	bucketExists bool
	calls        []string
}

func (f *fakeS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.calls = append(f.calls, "HeadBucket")
	if !f.bucketExists {
		return nil, errors.New("not found")
	}
	return &s3.HeadBucketOutput{}, nil
}
func (f *fakeS3) CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.calls = append(f.calls, "CreateBucket")
	return &s3.CreateBucketOutput{}, nil
}
func (f *fakeS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.calls = append(f.calls, "PutObject")
	return &s3.PutObjectOutput{}, nil
}

func zipBytes() []byte { return []byte("PK\x03\x04 pretend this is a lambda") }

func newTestDeployer(iamF *fakeIAM, lamF *fakeLambda, evF *fakeEvents, s3F *fakeS3) *Deployer {
	return &Deployer{
		IAM: iamF, Lambda: lamF, Events: evF, S3: s3F,
		Fetch: func(context.Context, string) ([]byte, error) { return zipBytes(), nil },
		// Negative skips the IAM-propagation wait: without this the unit suite spends
		// 10s per role-creating test (40s total) sleeping.
		PropagationDelay: -1,
	}
}

// --- tests ----------------------------------------------------------------------

// TestDeployCreatesEverythingUnarmed: the end-to-end shape, and the critical
// assertion that a fresh deploy lands in dry-run.
func TestDeployCreatesEverythingUnarmed(t *testing.T) {
	iamF, lamF, evF, s3F := &fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{}
	d := newTestDeployer(iamF, lamF, evF, s3F)

	res, err := d.Deploy(context.Background(), Options{
		AccountID: "123456789012", Region: "us-east-1", Version: "0.114.0", Artifact: "/tmp/x.zip",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if !res.DryRun {
		t.Error("a fresh deploy must report dry-run")
	}
	if lamF.lastEnv["REAPER_DRY_RUN"] != "true" {
		t.Errorf("the created function must be unarmed, got REAPER_DRY_RUN=%q", lamF.lastEnv["REAPER_DRY_RUN"])
	}
	if lamF.lastEnv["REAPER_SCAN_SELF"] != "true" {
		t.Error("the created function must be in scan-self mode")
	}

	// The policy written must be reaperiam's, including the fsx grants whose absence
	// was the #625 silent gap.
	for _, want := range []string{"fsx:DeleteFileSystem", "ec2:TerminateInstances", "spawn:managed"} {
		if !strings.Contains(iamF.policyDoc, want) {
			t.Errorf("role policy missing %q:\n%s", want, iamF.policyDoc)
		}
	}
	if strings.Contains(iamF.policyDoc, "sts:AssumeRole") {
		t.Error("the execution policy must not grant sts:AssumeRole")
	}

	// Permission before target, or EventBridge accepts the target and then fails
	// every invocation with AccessDenied, visible only in a metric nobody watches.
	joined := strings.Join(append(lamF.calls, evF.calls...), ",")
	permAt := strings.Index(strings.Join(lamF.calls, ","), "AddPermission")
	if permAt < 0 {
		t.Error("AddPermission was never called")
	}
	if !strings.Contains(joined, "PutTargets") {
		t.Error("PutTargets was never called")
	}
}

// TestRedeployDoesNotDisarm is the dangerous case. An operator arms the reaper, then
// re-deploys to pick up a new version; silently reverting it to dry-run would quietly
// remove the protection they deliberately enabled.
func TestRedeployDoesNotDisarm(t *testing.T) {
	iamF := &fakeIAM{roleExists: true}
	lamF := &fakeLambda{exists: true, dryRun: "false"} // already armed
	d := newTestDeployer(iamF, lamF, &fakeEvents{}, &fakeS3{bucketExists: true})

	if _, err := d.Deploy(context.Background(), Options{
		AccountID: "1", Region: "us-east-1", Version: "0.115.0", Artifact: "/tmp/x.zip",
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if lamF.lastEnv["REAPER_DRY_RUN"] != "false" {
		t.Errorf("re-deploying an ARMED reaper must not disarm it; REAPER_DRY_RUN=%q", lamF.lastEnv["REAPER_DRY_RUN"])
	}
}

// TestDeployRejectsANonZipArtifact: a 404 page or an LFS pointer downloads fine and
// is not a zip. Catching it here beats a successful deploy that fails with
// Runtime.InvalidEntrypoint at invoke time, hours later, in a log nobody is reading.
func TestDeployRejectsANonZipArtifact(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	d.Fetch = func(context.Context, string) ([]byte, error) {
		return []byte("<!DOCTYPE html><title>404 Not Found</title>"), nil
	}

	_, err := d.Deploy(context.Background(), Options{
		AccountID: "1", Region: "us-east-1", Version: "0.114.0", Artifact: "https://example/x.zip",
	})
	if err == nil {
		t.Fatal("a non-zip artifact must be refused before anything is created")
	}
	if !strings.Contains(err.Error(), "not a zip") {
		t.Errorf("the error should say the artifact is not a zip, got: %v", err)
	}
}

func TestDeployRejectsAnEmptyArtifact(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	d.Fetch = func(context.Context, string) ([]byte, error) { return nil, nil }
	if _, err := d.Deploy(context.Background(), Options{
		AccountID: "1", Region: "us-east-1", Version: "0.114.0", Artifact: "/tmp/x",
	}); err == nil {
		t.Fatal("an empty artifact must be refused")
	}
}

func TestDeployRequiresVersionOrArtifact(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	if _, err := d.Deploy(context.Background(), Options{AccountID: "1", Region: "us-east-1"}); err == nil {
		t.Fatal("with neither --version nor --artifact there is nothing to deploy")
	}
}

func TestDeployRequiresAccountAndRegion(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	for _, o := range []Options{{Region: "us-east-1"}, {AccountID: "1"}} {
		if _, err := d.Deploy(context.Background(), o); err == nil {
			t.Errorf("Deploy(%+v) should require both account and region", o)
		}
	}
}

func TestArmFlipsDryRunAndKeepsScanSelf(t *testing.T) {
	lamF := &fakeLambda{exists: true, dryRun: "true"}
	d := newTestDeployer(&fakeIAM{roleExists: true}, lamF, &fakeEvents{}, &fakeS3{bucketExists: true})

	if err := d.Arm(context.Background()); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if lamF.lastEnv["REAPER_DRY_RUN"] != "false" {
		t.Errorf("REAPER_DRY_RUN = %q, want \"false\"", lamF.lastEnv["REAPER_DRY_RUN"])
	}
	if lamF.lastEnv["REAPER_SCAN_SELF"] != "true" {
		t.Error("arming must not drop scan-self, or the reaper would scan nothing")
	}
}

func TestArmOnANonDeployedReaperSaysSo(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{exists: false}, &fakeEvents{}, &fakeS3{})
	err := d.Arm(context.Background())
	if err == nil {
		t.Fatal("arming a reaper that isn't deployed must error")
	}
	if !strings.Contains(err.Error(), "spawn reaper deploy") {
		t.Errorf("the error should name the command that fixes it, got: %v", err)
	}
}

func TestInspectNotDeployedIsNotAnError(t *testing.T) {
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{exists: false}, &fakeEvents{}, &fakeS3{})
	info, err := d.Inspect(context.Background())
	if err != nil {
		t.Fatalf("absence is a finding, not an error: %v", err)
	}
	if info.Deployed {
		t.Error("Deployed should be false")
	}
}

func TestInspectReportsArmedState(t *testing.T) {
	for dryRun, wantArmed := range map[string]bool{"true": false, "false": true, "": true} {
		lamF := &fakeLambda{exists: true, dryRun: dryRun}
		d := newTestDeployer(&fakeIAM{}, lamF, &fakeEvents{}, &fakeS3{})
		info, err := d.Inspect(context.Background())
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if info.Armed != wantArmed {
			t.Errorf("REAPER_DRY_RUN=%q => Armed=%v, want %v (an unset flag means not dry-run, i.e. armed)",
				dryRun, info.Armed, wantArmed)
		}
	}
}

// TestTeardownIsIdempotent: a re-run after a partial teardown must converge, not error
// on the pieces already gone.
func TestTeardownIsIdempotent(t *testing.T) {
	iamF, lamF, evF := &fakeIAM{roleExists: true}, &fakeLambda{exists: true}, &fakeEvents{}
	d := newTestDeployer(iamF, lamF, evF, &fakeS3{bucketExists: true})

	removed, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("expected a teardown to report what it removed")
	}
	// Most-dependent first: the rule target and rule before the function, the
	// policies before the role — otherwise IAM refuses to delete a role with
	// policies still attached.
	order := strings.Join(removed, ",")
	if strings.Index(order, "function") > strings.Index(order, "role "+RoleName) {
		t.Errorf("the function must be removed before the role it uses: %s", order)
	}
}

// fakeEventsNoRule models EventBridge faithfully for the already-torn-down case:
// DescribeRule reports absence, but DeleteRule returns SUCCESS anyway, because
// EventBridge's DeleteRule is idempotent.
type fakeEventsNoRule struct {
	fakeEvents
	deleteCalled bool
}

func (f *fakeEventsNoRule) DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	return nil, &ebtypes.ResourceNotFoundException{}
}
func (f *fakeEventsNoRule) DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	f.deleteCalled = true
	return &eventbridge.DeleteRuleOutput{}, nil
}

// TestTeardownDoesNotClaimToRemoveAnAbsentRule. Observed on real AWS: a second
// teardown printed "removed rule spawn-ttl-reaper-selfscan-schedule" when the rule
// was already gone, because DeleteRule succeeds for a missing rule and err==nil was
// taken as proof something was deleted. Claiming work that did not happen is the same
// defect class as the rest of this session's fixes, just smaller.
func TestTeardownDoesNotClaimToRemoveAnAbsentRule(t *testing.T) {
	ev := &fakeEventsNoRule{}
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{exists: false}, &fakeEvents{}, &fakeS3{})
	d.Events = ev // the faithful fake: DescribeRule says absent, DeleteRule still succeeds

	removed, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, r := range removed {
		if strings.Contains(r, "rule "+RuleName) {
			t.Errorf("claimed to remove a rule that was not there: %q", r)
		}
	}
	if ev.deleteCalled {
		t.Error("DeleteRule should not even be attempted when DescribeRule says the rule is absent")
	}
}
