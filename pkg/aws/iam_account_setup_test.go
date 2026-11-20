package aws

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spore-host/libs/footprint"
	libtags "github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/testutil"
)

var _ AccountSetupAPI = (*testutil.IAMAccount)(nil)

func nop(string) {}

// The policy's IAM path and name are the ones every message names
// (lifecyclemark.UserPolicy, used by spawn doctor and the mark's diagnostic).
func TestUserLifecyclePolicyIsLifecyclemarksUserPolicy(t *testing.T) {
	if got := strings.TrimPrefix(sporedPolicyPath, "/") + spawnUserLifecyclePolicyName; got != lifecyclemark.UserPolicy {
		t.Fatalf("the policy is %q; lifecyclemark.UserPolicy says %q", got, lifecyclemark.UserPolicy)
	}
	if got, want := SpawnUserLifecyclePolicyARN(testAccount), "arn:aws:iam::"+testAccount+":policy/"+lifecyclemark.UserPolicy; got != want {
		t.Errorf("ARN %q, want %q", got, want)
	}
}

// spawn#1666 names the actions: the seven the mark calls, and GetItem for
// `spawn reaper teardown --if-idle-for`. Written out, so a change to either
// list is a visible change to what spore.host provisions for every user.
func TestUserLifecycleDocumentGrantsExactlyTheLeaseActions(t *testing.T) {
	var doc struct {
		Version   string
		Statement []struct {
			Sid, Effect string
			Action      []string
			Resource    string
			Condition   any
		}
	}
	raw := spawnUserLifecycleDocument(testAccount)
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the document is not JSON: %v\n%s", err, raw)
	}
	if doc.Version != "2012-10-17" || len(doc.Statement) != 1 {
		t.Fatalf("want one statement in a 2012-10-17 policy:\n%s", raw)
	}
	s := doc.Statement[0]
	want := []string{
		"dynamodb:ConditionCheckItem", "dynamodb:CreateTable", "dynamodb:DeleteItem", "dynamodb:DescribeTable",
		"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:TagResource", "dynamodb:UpdateItem",
	}
	got := append([]string(nil), s.Action...)
	sort.Strings(got)
	if s.Effect != "Allow" || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("statement %s %v, want Allow %v", s.Effect, got, want)
	}
	if s.Resource != "arn:aws:dynamodb:*:"+testAccount+":table/spore-host-lifecycle" {
		t.Errorf("resource %q, want the lease table in every region of this account only", s.Resource)
	}
	// No policy variable in a Resource: CreatePolicy rejects it (#1069).
	if strings.Contains(raw, "${") {
		t.Errorf("the document holds a policy variable:\n%s", raw)
	}
}

// Created tagged managed-by=spawn and permanent, never role=service; a second
// run changes nothing.
func TestEnsureUserLifecyclePolicyCreatesThenIsIdempotent(t *testing.T) {
	f := testutil.NewIAMAccount(testAccount)
	ctx := context.Background()
	res, err := EnsureSpawnUserLifecyclePolicy(ctx, f, testAccount, nop)
	if err != nil {
		t.Fatal(err)
	}
	arn := SpawnUserLifecyclePolicyARN(testAccount)
	p := f.Policies[arn]
	if !res.Created || p == nil {
		t.Fatalf("not created: %+v", res)
	}
	if p.Tags[libtags.ManagedBy] != "spawn" || p.Tags["spore-host:permanent"] != "true" {
		t.Errorf("tags %v, want managed-by=spawn and permanent=true", p.Tags)
	}
	if _, ok := p.Tags[libtags.Role]; ok {
		t.Errorf("the user policy carries %s (%v); that marks spore.host's own service", libtags.Role, p.Tags)
	}
	if !samePolicyDocument(p.Docs["v1"], spawnUserLifecycleDocument(testAccount)) {
		t.Errorf("created with the wrong document:\n%s", p.Docs["v1"])
	}

	f.Calls = nil
	res, err = EnsureSpawnUserLifecyclePolicy(ctx, f, testAccount, nop)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged || res.Created || res.Updated || res.Tagged {
		t.Errorf("a re-run reported %+v, want Unchanged only", res)
	}
	for _, c := range f.Calls {
		if !strings.HasPrefix(c, "Get") {
			t.Errorf("a re-run of a current policy wrote: %s", c)
		}
	}
	if len(f.Policies[arn].Versions) != 1 {
		t.Errorf("a re-run added versions: %v", f.Policies[arn].Versions)
	}
}

// A policy with another document (an older spawn's) gets a new default
// version, and its missing spore.host tags.
func TestEnsureUserLifecyclePolicyUpdatesADriftedOne(t *testing.T) {
	f := testutil.NewIAMAccount(testAccount)
	arn := SpawnUserLifecyclePolicyARN(testAccount)
	f.AddPolicy(arn, 1, nil)
	res, err := EnsureSpawnUserLifecyclePolicy(context.Background(), f, testAccount, nop)
	if err != nil {
		t.Fatal(err)
	}
	p := f.Policies[arn]
	if !res.Updated || !res.Tagged || p.Default != "v2" {
		t.Fatalf("result %+v, default %s; want updated to v2 and tagged", res, p.Default)
	}
	if !samePolicyDocument(p.Docs["v2"], spawnUserLifecycleDocument(testAccount)) {
		t.Errorf("v2 is not this spawn's document")
	}
}

// --- --remove ------------------------------------------------------------

// account is a setup account: the boundary with three versions and the user
// policy with two, attached to a group and a role.
func setupAccount(t *testing.T) *testutil.IAMAccount {
	t.Helper()
	f := testutil.NewIAMAccount(testAccount)
	f.AddPolicy(spawnInstanceBoundaryARN(testAccount), 3, boundaryTags())
	up := f.AddPolicy(SpawnUserLifecyclePolicyARN(testAccount), 2, userLifecycleTags())
	up.Groups = []string{"spawn-users"}
	up.Roles = []string{"Developer"}
	return f
}

func deletes(f *testutil.IAMAccount) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Delete") || strings.HasPrefix(c, "Detach") {
			out = append(out, c)
		}
	}
	return out
}

// The refusal the whole command exists to make: while any role has the
// boundary as its permissions boundary, nothing is deleted — not the boundary,
// and not the user policy either — and the plan names the role.
func TestRemoveRefusesWhileTheBoundaryIsInUse(t *testing.T) {
	f := setupAccount(t)
	f.Policies[spawnInstanceBoundaryARN(testAccount)].BoundaryOf = []string{"spawn-instance-abc"}
	ctx := context.Background()

	plan, err := PlanAccountRemoval(ctx, f, testAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Refusals) == 0 || !strings.Contains(plan.Refusals[0], "role/spawn-instance-abc") {
		t.Fatalf("the plan does not refuse naming the role: %v", plan.Refusals)
	}
	if d := deletes(f); len(d) > 0 {
		t.Fatalf("planning deleted: %v", d)
	}
	if err := ExecuteAccountRemoval(ctx, f, plan, nop); err == nil {
		t.Fatal("a refused plan executed")
	}
	if d := deletes(f); len(d) > 0 {
		t.Errorf("a refused plan deleted: %v", d)
	}
}

// Execute's own re-check, independent of the plan's: a plan made before a
// role was given the boundary (or one whose refusals were dropped) still does
// not delete the boundary.
func TestRemoveRechecksTheBoundaryBeforeDeletingIt(t *testing.T) {
	f := setupAccount(t)
	ctx := context.Background()
	plan, err := PlanAccountRemoval(ctx, f, testAccount, false)
	if err != nil || len(plan.Refusals) > 0 {
		t.Fatalf("plan: %v %v", err, plan.Refusals)
	}
	b := spawnInstanceBoundaryARN(testAccount)
	f.Policies[b].BoundaryOf = []string{"spored-instance-role"} // a launch since the plan
	err = ExecuteAccountRemoval(ctx, f, plan, nop)
	if err == nil || !strings.Contains(err.Error(), "role/spored-instance-role") {
		t.Fatalf("execute: %v, want a refusal naming the role", err)
	}
	if f.Called("DeletePolicy", b) || f.Policies[b] == nil {
		t.Errorf("the boundary was deleted while a role used it")
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "DeletePolicyVersion "+b) {
			t.Errorf("a version of the in-use boundary was deleted: %s", c)
		}
	}
}

// Report mode is the plan: reads only, in the order Execute deletes.
func TestRemovePlanReadsOnlyAndListsWhatItWouldDelete(t *testing.T) {
	f := setupAccount(t)
	plan, err := PlanAccountRemoval(context.Background(), f, testAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "List") {
			t.Errorf("the plan made a write: %s", c)
		}
	}
	if !plan.Boundary.Exists || strings.Join(plan.Boundary.OlderVersions, ",") != "v1,v2" || plan.Boundary.DefaultVersion != "v3" {
		t.Errorf("boundary %+v, want older v1,v2 and default v3", plan.Boundary)
	}
	if got := entityList(plan.UserPolicy.AttachedTo); got != "group/spawn-users, role/Developer" {
		t.Errorf("user policy attached to %q", got)
	}
	if plan.OnboardRole != nil || f.Called("GetRole", PortalOnboardRoleName) {
		t.Error("without offboard the plan looked at the onboard role")
	}
}

// Execute: the user policy detached, its versions then it; the boundary's
// versions then it, last.
func TestRemoveDeletesVersionsThenThePolicyBoundaryLast(t *testing.T) {
	f := setupAccount(t)
	ctx := context.Background()
	plan, err := PlanAccountRemoval(ctx, f, testAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExecuteAccountRemoval(ctx, f, plan, nop); err != nil {
		t.Fatal(err)
	}
	if len(f.Policies) != 0 {
		t.Fatalf("left behind: %v", f.Policies)
	}
	u, b := SpawnUserLifecyclePolicyARN(testAccount), spawnInstanceBoundaryARN(testAccount)
	want := []string{
		"DetachGroupPolicy " + u + " spawn-users",
		"DetachRolePolicy " + u + " Developer",
		"DeletePolicyVersion " + u + " v1",
		"DeletePolicy " + u,
		"DeletePolicyVersion " + b + " v1",
		"DeletePolicyVersion " + b + " v2",
		"DeletePolicy " + b,
	}
	if got := deletes(f); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("deletions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Nothing set up: an empty plan, and executing it deletes nothing.
func TestRemoveOnAnEmptyAccount(t *testing.T) {
	f := testutil.NewIAMAccount(testAccount)
	plan, err := PlanAccountRemoval(context.Background(), f, testAccount, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || len(plan.Refusals) > 0 {
		t.Fatalf("plan %+v, want empty", plan)
	}
	if err := ExecuteAccountRemoval(context.Background(), f, plan, nop); err != nil || len(deletes(f)) > 0 {
		t.Errorf("execute: %v %v", err, deletes(f))
	}
}

// Never in spore.host's infra account, and never in an account not
// identified: not even a read, and a forged plan is refused too.
func TestRemoveRefusesTheInfraAccount(t *testing.T) {
	for _, acct := range []string{footprintspawn.InfraAccountID, ""} {
		f := testutil.NewIAMAccount(acct)
		f.AddPolicy(spawnInstanceBoundaryARN(acct), 1, nil)
		if _, err := PlanAccountRemoval(context.Background(), f, acct, true); err == nil {
			t.Errorf("%q: planned", acct)
		}
		forged := &AccountRemovalPlan{Account: acct, Boundary: PolicyRemoval{ARN: spawnInstanceBoundaryARN(acct), Exists: true}}
		if err := ExecuteAccountRemoval(context.Background(), f, forged, nop); err == nil {
			t.Errorf("%q: executed a forged plan", acct)
		}
		if len(f.Calls) > 0 {
			t.Errorf("%q: made calls: %v", acct, f.Calls)
		}
	}
}

// --offboard deletes the spore-portal-onboard role: its inline policies, its
// attachments, then the role.
func TestRemoveOffboardDeletesTheOnboardRole(t *testing.T) {
	f := setupAccount(t)
	r := f.AddRole(PortalOnboardRoleName, map[string]string{"Purpose": "spore-portal-onboard", "spawn:managed": "false"})
	r.Inline = []string{"PortalEC2Launch", "PortalPassSporedProfile"}
	r.Attached = []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}
	ctx := context.Background()
	plan, err := PlanAccountRemoval(ctx, f, testAccount, true)
	if err != nil || len(plan.Refusals) > 0 {
		t.Fatalf("plan: %v %v", err, plan.Refusals)
	}
	if err := ExecuteAccountRemoval(ctx, f, plan, nop); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Roles[PortalOnboardRoleName]; ok {
		t.Fatal("the onboard role is still there")
	}
	inl := f.Index("DeleteRolePolicy", PortalOnboardRoleName+" PortalEC2Launch")
	det := f.Index("DetachRolePolicy", "arn:aws:iam::aws:policy/ReadOnlyAccess "+PortalOnboardRoleName)
	del := f.Index("DeleteRole", PortalOnboardRoleName)
	if inl < 0 || det < 0 || del < 0 || inl > del || det > del {
		t.Errorf("order inline=%d detach=%d delete=%d:\n%s", inl, det, del, strings.Join(f.Calls, "\n"))
	}
	// Without --offboard the same account keeps it.
	g := setupAccount(t)
	g.AddRole(PortalOnboardRoleName, map[string]string{"Purpose": "spore-portal-onboard"})
	p2, _ := PlanAccountRemoval(ctx, g, testAccount, false)
	if err := ExecuteAccountRemoval(ctx, g, p2, nop); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Roles[PortalOnboardRoleName]; !ok {
		t.Error("--remove without --offboard deleted the onboard role")
	}
}

// A role of that name spore.host did not make is refused; one the portal's
// CloudFormation quick-create made is refused with the stack to delete.
func TestRemoveOffboardRefusesARoleItShouldNotDelete(t *testing.T) {
	ctx := context.Background()
	f := setupAccount(t)
	f.AddRole(PortalOnboardRoleName, map[string]string{"team": "theirs"})
	if _, err := PlanAccountRemoval(ctx, f, testAccount, true); err == nil || !strings.Contains(err.Error(), "Purpose=spore-portal-onboard") {
		t.Errorf("an untagged role: %v", err)
	}

	g := setupAccount(t)
	g.AddRole(PortalOnboardRoleName, map[string]string{"Purpose": "spore-portal-onboard", "aws:cloudformation:stack-name": "spore-portal-onboard"})
	plan, err := PlanAccountRemoval(ctx, g, testAccount, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Refusals) == 0 || !strings.Contains(plan.Refusals[0], "delete-stack --stack-name spore-portal-onboard") {
		t.Fatalf("refusals %v, want the stack named", plan.Refusals)
	}
	if err := ExecuteAccountRemoval(ctx, g, plan, nop); err == nil || len(deletes(g)) > 0 {
		t.Errorf("a stack-made role's plan executed: %v %v", err, deletes(g))
	}
}

// A boundary or user policy whose usage cannot be read is an error, not an
// empty list that would let the boundary go.
func TestRemovePlanFailsWhenUsageCannotBeRead(t *testing.T) {
	f := setupAccount(t)
	f.Err["ListEntitiesForPolicy"] = &types.ServiceFailureException{}
	if _, err := PlanAccountRemoval(context.Background(), f, testAccount, false); err == nil {
		t.Fatal("planned without knowing what uses the boundary")
	}
}

// No idle teardown deletes the user policy: spawn's footprint classes it
// permanent and not deletable, attached or not, like the boundary it is
// provisioned beside. Only --remove takes it away.
func TestFootprintKeepsTheUserLifecyclePolicy(t *testing.T) {
	for _, inUse := range []string{"", "attached to, or the boundary of, 1 role(s)"} {
		r := footprint.Resource{ARN: SpawnUserLifecyclePolicyARN(testAccount), Type: footprintspawn.TypeManagedPolicy,
			Region: footprintspawn.RegionGlobal, Name: spawnUserLifecyclePolicyName, Tags: userLifecycleTags()}
		footprintspawn.Finish(&r, footprintspawn.Facts{Account: testAccount, InUse: inUse})
		if r.Class != footprint.ClassPermanent || r.Deletable {
			t.Errorf("in use %q: class %s deletable %v (%s); want permanent and kept", inUse, r.Class, r.Deletable, r.Reason)
		}
	}
}
