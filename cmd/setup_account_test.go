package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	smithy "github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/testutil"
)

const setupTestAccount = "123456789012"

// runSetup runs `spawn setup-account` with the given flags against f in
// account, with no AWS call beyond the seams, and returns its output.
func runSetup(t *testing.T, f *testutil.IAMAccount, account string, remove, offboard, yes bool) (string, error) {
	t.Helper()
	savedCaller, savedClient := setupAccountCaller, setupAccountIAMClient
	savedRemove, savedOffboard, savedYes := setupAccountRemove, setupAccountOffboard, setupAccountYes
	t.Cleanup(func() {
		setupAccountCaller, setupAccountIAMClient = savedCaller, savedClient
		setupAccountRemove, setupAccountOffboard, setupAccountYes = savedRemove, savedOffboard, savedYes
	})
	setupAccountCaller = func(context.Context, aws.Config) (string, string, error) {
		return account, "arn:aws:iam::" + account + ":user/admin", nil
	}
	setupAccountIAMClient = func(aws.Config) spawnaws.AccountSetupAPI { return f }
	setupAccountRemove, setupAccountOffboard, setupAccountYes = remove, offboard, yes

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	err := runSetupAccount(c, nil)
	return out.String(), err
}

func writes(f *testutil.IAMAccount) []string {
	var out []string
	for _, c := range f.Calls {
		if !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "List") {
			out = append(out, c)
		}
	}
	return out
}

// `spawn setup-account` provisions the boundary and the user lifecycle policy
// (spawn#1666), says how to attach the latter, and a second run changes
// nothing.
func TestSetupAccountCreatesTheBoundaryAndTheUserPolicy(t *testing.T) {
	f := testutil.NewIAMAccount(setupTestAccount)
	out, err := runSetup(t, f, setupTestAccount, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	boundary := spawnaws.SpawnInstanceBoundaryARN(setupTestAccount)
	user := spawnaws.SpawnUserLifecyclePolicyARN(setupTestAccount)
	if f.Policies[boundary] == nil || f.Policies[user] == nil {
		t.Fatalf("policies after setup: %v", f.Policies)
	}
	for _, want := range []string{"Created the spawn permissions boundary", "Created the user lifecycle policy " + user,
		"aws iam attach-group-policy --group-name <group> --policy-arn " + user} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}

	f.Calls = nil
	out, err = runSetup(t, f, setupTestAccount, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if w := writes(f); len(w) > 0 {
		t.Errorf("a second setup-account wrote: %v", w)
	}
	if !strings.Contains(out, "already current") {
		t.Errorf("a re-run does not say it is current:\n%s", out)
	}
}

// The infra account has no lifecycle lease, so no user policy is made there.
func TestSetupAccountMakesNoUserPolicyInTheInfraAccount(t *testing.T) {
	f := testutil.NewIAMAccount(footprintspawn.InfraAccountID)
	if _, err := runSetup(t, f, footprintspawn.InfraAccountID, false, false, false); err != nil {
		t.Fatal(err)
	}
	if f.Policies[spawnaws.SpawnUserLifecyclePolicyARN(footprintspawn.InfraAccountID)] != nil {
		t.Error("created the user lifecycle policy in the infra account")
	}
}

// Without admin rights the creation is refused, and the command reports the
// failure rather than claiming success.
func TestSetupAccountReportsAccessDenied(t *testing.T) {
	f := testutil.NewIAMAccount(setupTestAccount)
	f.Err["CreatePolicy"] = &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to iam:CreatePolicy"}
	if _, err := runSetup(t, f, setupTestAccount, false, false, false); err == nil {
		t.Fatal("setup-account reported success though the create was denied")
	}
}

func provisioned(t *testing.T) *testutil.IAMAccount {
	t.Helper()
	f := testutil.NewIAMAccount(setupTestAccount)
	if _, err := runSetup(t, f, setupTestAccount, false, false, false); err != nil {
		t.Fatal(err)
	}
	f.Policies[spawnaws.SpawnUserLifecyclePolicyARN(setupTestAccount)].Groups = []string{"spawn-users"}
	f.AddRole(spawnaws.PortalOnboardRoleName, map[string]string{"Purpose": "spore-portal-onboard"}).Inline = []string{"PortalEC2Launch"}
	f.Calls = nil
	return f
}

// --remove without --yes is report mode: it prints the plan in order and
// deletes nothing.
func TestSetupAccountRemoveWithoutYesOnlyReports(t *testing.T) {
	f := provisioned(t)
	out, err := runSetup(t, f, setupTestAccount, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if w := writes(f); len(w) > 0 {
		t.Fatalf("report mode wrote: %v", w)
	}
	user, boundary := spawnaws.SpawnUserLifecyclePolicyARN(setupTestAccount), spawnaws.SpawnInstanceBoundaryARN(setupTestAccount)
	iu, ib := strings.Index(out, "1. the user lifecycle policy "+user), strings.Index(out, "2. the permissions boundary "+boundary)
	if iu < 0 || ib < iu || !strings.Contains(out, "detach from group/spawn-users") || !strings.Contains(out, "Report only: nothing was deleted") {
		t.Errorf("the plan is not the expected report:\n%s", out)
	}
	if strings.Contains(out, "spore-portal-onboard") {
		t.Errorf("without --offboard the plan mentions the onboard role:\n%s", out)
	}
}

// --remove --yes deletes both policies and keeps the onboard role; with
// --offboard the role goes too.
func TestSetupAccountRemoveYesDeletes(t *testing.T) {
	f := provisioned(t)
	out, err := runSetup(t, f, setupTestAccount, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Policies) != 0 {
		t.Errorf("policies left: %v", f.Policies)
	}
	if _, ok := f.Roles[spawnaws.PortalOnboardRoleName]; !ok {
		t.Error("--remove without --offboard deleted the onboard role")
	}
	if !strings.Contains(out, "add --offboard") {
		t.Errorf("output does not say how to offboard:\n%s", out)
	}

	g := provisioned(t)
	if _, err := runSetup(t, g, setupTestAccount, true, true, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Roles[spawnaws.PortalOnboardRoleName]; ok || len(g.Policies) != 0 {
		t.Errorf("--offboard left the role (%v) or policies (%v)", g.Roles, g.Policies)
	}
}

// Refused while a role has the boundary: an error, the role named, and
// nothing deleted, even with --yes.
func TestSetupAccountRemoveRefusesWhileTheBoundaryIsInUse(t *testing.T) {
	f := provisioned(t)
	f.Policies[spawnaws.SpawnInstanceBoundaryARN(setupTestAccount)].BoundaryOf = []string{"spored-instance-role"}
	out, err := runSetup(t, f, setupTestAccount, true, true, true)
	if err == nil {
		t.Fatal("removal succeeded while a role used the boundary")
	}
	if !strings.Contains(out, "role/spored-instance-role") {
		t.Errorf("the refusal does not name the role:\n%s", out)
	}
	if w := writes(f); len(w) > 0 {
		t.Errorf("a refused removal wrote: %v", w)
	}
}

// Refused in spore.host's infra account, before any IAM call.
func TestSetupAccountRemoveRefusesTheInfraAccount(t *testing.T) {
	f := testutil.NewIAMAccount(footprintspawn.InfraAccountID)
	if _, err := runSetup(t, f, footprintspawn.InfraAccountID, true, false, true); err == nil || !strings.Contains(err.Error(), "infra account") {
		t.Fatalf("err %v, want the infra-account refusal", err)
	}
	if len(f.Calls) > 0 {
		t.Errorf("made IAM calls: %v", f.Calls)
	}
}

// --offboard and --yes only mean something with --remove.
func TestSetupAccountFlagsNeedRemove(t *testing.T) {
	f := testutil.NewIAMAccount(setupTestAccount)
	if _, err := runSetup(t, f, setupTestAccount, false, true, false); err == nil {
		t.Error("--offboard without --remove ran")
	}
	if _, err := runSetup(t, f, setupTestAccount, false, false, true); err == nil {
		t.Error("--yes without --remove ran")
	}
	if len(f.Calls) > 0 {
		t.Errorf("made IAM calls: %v", f.Calls)
	}
}

// The policy the command creates is the one spawn doctor and the mark's
// diagnostic name.
func TestSetupAccountUserPolicyIsTheOneDoctorNames(t *testing.T) {
	if !strings.HasSuffix(spawnaws.SpawnUserLifecyclePolicyARN(setupTestAccount), ":policy/"+lifecyclemark.UserPolicy) {
		t.Errorf("%s is not %s", spawnaws.SpawnUserLifecyclePolicyARN(setupTestAccount), lifecyclemark.UserPolicy)
	}
}
