package aws

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestCreateOrGetInstanceProfile_PolicyFileGetsSporedBaseline is the
// regression guard for spawn#502: a role created via IAMRoleConfig.PolicyFile
// (the path reachable from `spawn launch --iam-policy-file`) must still carry
// the spored-baseline-policy grants. Before the fix, this was the one of
// three policy-source branches in createIAMRole that never attached it —
// spored could not read its own tags, so TTL/on-complete/pre-stop silently
// never fired for hours on a real fleet.
func TestCreateOrGetInstanceProfile_PolicyFileGetsSporedBaseline(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	f, err := os.CreateTemp(t.TempDir(), "policy-*.json")
	if err != nil {
		t.Fatalf("create temp policy file: %v", err)
	}
	if _, err := f.WriteString(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject"],"Resource":"arn:aws:s3:::some-bucket/prefix/*"}]}`); err != nil {
		t.Fatalf("write temp policy file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp policy file: %v", err)
	}

	cfg := IAMRoleConfig{
		RoleName:      "spawn-502-policyfile-test",
		TrustServices: []string{"ec2.amazonaws.com"},
		PolicyFile:    f.Name(),
	}
	if _, err := c.CreateOrGetInstanceProfile(context.Background(), cfg); err != nil {
		t.Fatalf("CreateOrGetInstanceProfile error: %v", err)
	}

	iamClient := iam.NewFromConfig(env.AWSConfig)
	roleName := "spawn-502-policyfile-test"
	out, err := iamClient.GetRolePolicy(context.Background(), &iam.GetRolePolicyInput{
		RoleName:   &roleName,
		PolicyName: strPtr("spored-baseline-policy"),
	})
	if err != nil {
		t.Fatalf("role %q missing spored-baseline-policy entirely (spawn#502 regression): %v", roleName, err)
	}
	for _, want := range []string{"ec2:DescribeTags", "ec2:TerminateInstances"} {
		if !strings.Contains(*out.PolicyDocument, want) {
			t.Errorf("spored-baseline-policy on a PolicyFile-created role missing %q:\n%s", want, *out.PolicyDocument)
		}
	}

	// The caller's own custom policy must still be attached unchanged.
	custom, err := iamClient.GetRolePolicy(context.Background(), &iam.GetRolePolicyInput{
		RoleName:   &roleName,
		PolicyName: strPtr("spawn-custom-policy"),
	})
	if err != nil {
		t.Fatalf("role %q missing spawn-custom-policy: %v", roleName, err)
	}
	if !strings.Contains(*custom.PolicyDocument, "s3:PutObject") {
		t.Errorf("spawn-custom-policy missing the caller's own statement:\n%s", *custom.PolicyDocument)
	}
}

func strPtr(s string) *string { return &s }

// TestCreateOrGetInstanceProfile_Concurrent verifies that many concurrent
// launches ensuring the SAME shared role/profile all succeed, rather than
// racing on check-then-act and failing with EntityAlreadyExists (#64).
func TestCreateOrGetInstanceProfile_Concurrent(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)

	const n = 8
	cfg := IAMRoleConfig{
		RoleName:      "spawn-instance-concurrent-test",
		TrustServices: []string{"ec2.amazonaws.com"},
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	profiles := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			profiles[i], errs[i] = c.CreateOrGetInstanceProfile(context.Background(), cfg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent CreateOrGetInstanceProfile[%d] failed: %v", i, err)
		}
		if profiles[i] != cfg.RoleName {
			t.Errorf("profile[%d] = %q, want %q", i, profiles[i], cfg.RoleName)
		}
	}
}

// TestIsAlreadyExists covers the benign-error classifier used by retryIAM.
func TestIsAlreadyExists(t *testing.T) {
	if !isAlreadyExists(&types.EntityAlreadyExistsException{}) {
		t.Error("EntityAlreadyExistsException should be treated as already-exists")
	}
	if !isAlreadyExists(&types.LimitExceededException{}) {
		t.Error("LimitExceededException (already-attached role) should be treated as already-exists")
	}
	if isAlreadyExists(nil) {
		t.Error("nil must not be already-exists")
	}
	if isAlreadyExists(errors.New("some other error")) {
		t.Error("unrelated error must not be already-exists")
	}
}

// TestRetryIAM_AlreadyExistsIsSuccess verifies retryIAM treats already-exists as
// success and returns a genuine error unchanged.
func TestRetryIAM_AlreadyExistsIsSuccess(t *testing.T) {
	if err := retryIAM(func() error { return &types.EntityAlreadyExistsException{} }); err != nil {
		t.Errorf("already-exists should be success, got %v", err)
	}
	sentinel := errors.New("boom")
	if err := retryIAM(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("non-retryable error should pass through, got %v", err)
	}
}
