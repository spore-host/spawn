package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// setupAccountFake is a minimal spawnaws.BoundaryAdminAPI: the boundary does
// not exist, so EnsureSpawnInstanceBoundary creates it.
type setupAccountFake struct{ created bool }

func (f *setupAccountFake) GetPolicy(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	return nil, &types.NoSuchEntityException{Message: aws.String("not found")}
}
func (f *setupAccountFake) GetPolicyVersion(context.Context, *iam.GetPolicyVersionInput, ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	return nil, &types.NoSuchEntityException{Message: aws.String("not found")}
}
func (f *setupAccountFake) CreatePolicy(context.Context, *iam.CreatePolicyInput, ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	f.created = true
	return &iam.CreatePolicyOutput{Policy: &types.Policy{Arn: aws.String("arn:aws:iam::123456789012:policy/spore-host/spawn-instance-boundary")}}, nil
}
func (f *setupAccountFake) CreatePolicyVersion(context.Context, *iam.CreatePolicyVersionInput, ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error) {
	return &iam.CreatePolicyVersionOutput{}, nil
}
func (f *setupAccountFake) ListPolicyVersions(context.Context, *iam.ListPolicyVersionsInput, ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error) {
	return &iam.ListPolicyVersionsOutput{}, nil
}
func (f *setupAccountFake) DeletePolicyVersion(context.Context, *iam.DeletePolicyVersionInput, ...func(*iam.Options)) (*iam.DeletePolicyVersionOutput, error) {
	return &iam.DeletePolicyVersionOutput{}, nil
}

var _ spawnaws.BoundaryAdminAPI = (*setupAccountFake)(nil)

// `spawn setup-account` provisions the boundary and reports what it did, with
// no AWS call beyond the seams.
func TestSetupAccountCreatesTheBoundary(t *testing.T) {
	savedCaller, savedClient := setupAccountCaller, setupAccountIAMClient
	t.Cleanup(func() { setupAccountCaller, setupAccountIAMClient = savedCaller, savedClient })

	f := &setupAccountFake{}
	setupAccountCaller = func(context.Context, aws.Config) (string, string, error) {
		return "123456789012", "arn:aws:iam::123456789012:user/admin", nil
	}
	setupAccountIAMClient = func(aws.Config) spawnaws.BoundaryAdminAPI { return f }

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	if err := runSetupAccount(c, nil); err != nil {
		t.Fatal(err)
	}
	if !f.created {
		t.Error("the boundary was not created")
	}
	if !strings.Contains(out.String(), "Created the spawn permissions boundary") {
		t.Errorf("output does not report the creation:\n%s", out.String())
	}
}

// Without admin rights the creation is refused, and a launch path (not the
// admin command) tells the user the exact command to run — asserted via the
// error type used there.
func TestSetupAccountReportsAccessDenied(t *testing.T) {
	// The admin command surfaces the raw error; the BoundaryMissingError (with
	// the command text) is the launch path's, exercised in pkg/aws. Here we
	// only confirm the command reports a create failure rather than claiming
	// success.
	savedCaller, savedClient := setupAccountCaller, setupAccountIAMClient
	t.Cleanup(func() { setupAccountCaller, setupAccountIAMClient = savedCaller, savedClient })
	setupAccountCaller = func(context.Context, aws.Config) (string, string, error) {
		return "123456789012", "arn:aws:iam::123456789012:user/plain", nil
	}
	setupAccountIAMClient = func(aws.Config) spawnaws.BoundaryAdminAPI { return &deniedBoundaryAPI{} }

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	c := &cobra.Command{}
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := runSetupAccount(c, nil); err == nil {
		t.Fatal("setup-account reported success though the create was denied")
	}
}

type deniedBoundaryAPI struct{ setupAccountFake }

func (deniedBoundaryAPI) CreatePolicy(context.Context, *iam.CreatePolicyInput, ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to iam:CreatePolicy"}
}
