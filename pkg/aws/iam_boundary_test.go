package aws

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/testutil"
)

// Through the real createOrGetInstanceProfile pipeline (against the IAM
// emulator): a launch provisions the account boundary, and re-using a role
// created before the boundary existed retrofits it. The emulator does not
// persist a boundary passed to CreateRole, so the new-role CreateRole case is
// proven live; the retrofit (PutRolePermissionsBoundary) it does model.
func TestCreateOrGetInstanceProfile_ProvisionsAndRetrofitsTheBoundary(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	cfg := IAMRoleConfig{InlinePolicyJSON: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`}

	name, err := c.CreateOrGetInstanceProfile(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.GetAccountID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ic := iam.NewFromConfig(env.AWSConfig)
	if _, err := ic.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(spawnInstanceBoundaryARN(acct))}); err != nil {
		t.Fatalf("the launch did not provision the boundary: %v", err)
	}

	// A second launch on the same role retrofits the boundary (the emulator
	// surfaces a PutRolePermissionsBoundary, unlike a CreateRole one).
	if _, err := c.CreateOrGetInstanceProfile(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := ic.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Role.PermissionsBoundary == nil ||
		aws.ToString(r.Role.PermissionsBoundary.PermissionsBoundaryArn) != spawnInstanceBoundaryARN(acct) {
		t.Errorf("the role was not retrofitted onto the boundary: %+v", r.Role.PermissionsBoundary)
	}
}

// boundaryFake is an IAM account's one spawn-instance boundary policy and one
// role, for the boundary provisioning and retrofit paths. It records calls and
// enforces IAM's five-version cap.
type boundaryFake struct {
	calls []string

	policyExists bool
	versions     []types.PolicyVersion // oldest first; default is marked
	docs         map[string]string     // version id -> document

	roleExists   bool
	roleBoundary string

	createPolicyErr error // returned by CreatePolicy (e.g. AccessDenied)
	getPolicyErr    error // returned by GetPolicy other than NoSuchEntity
}

func newBoundaryFake() *boundaryFake {
	return &boundaryFake{docs: map[string]string{}, roleExists: true}
}

func noSuchEntity(msg string) error {
	return &types.NoSuchEntityException{Message: aws.String(msg)}
}

func (f *boundaryFake) defaultVersion() string {
	for _, v := range f.versions {
		if v.IsDefaultVersion {
			return aws.ToString(v.VersionId)
		}
	}
	if len(f.versions) > 0 {
		return aws.ToString(f.versions[len(f.versions)-1].VersionId)
	}
	return "v1"
}

func (f *boundaryFake) GetPolicy(_ context.Context, _ *iam.GetPolicyInput, _ ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	f.calls = append(f.calls, "GetPolicy")
	if f.getPolicyErr != nil {
		return nil, f.getPolicyErr
	}
	if !f.policyExists {
		return nil, noSuchEntity("policy not found")
	}
	return &iam.GetPolicyOutput{Policy: &types.Policy{DefaultVersionId: aws.String(f.defaultVersion())}}, nil
}

func (f *boundaryFake) GetPolicyVersion(_ context.Context, in *iam.GetPolicyVersionInput, _ ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	f.calls = append(f.calls, "GetPolicyVersion "+aws.ToString(in.VersionId))
	doc, ok := f.docs[aws.ToString(in.VersionId)]
	if !ok {
		return nil, noSuchEntity("version not found")
	}
	return &iam.GetPolicyVersionOutput{PolicyVersion: &types.PolicyVersion{Document: aws.String(doc)}}, nil
}

func (f *boundaryFake) CreatePolicy(_ context.Context, in *iam.CreatePolicyInput, _ ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	f.calls = append(f.calls, "CreatePolicy")
	if f.createPolicyErr != nil {
		return nil, f.createPolicyErr
	}
	if f.policyExists {
		return nil, &smithy.GenericAPIError{Code: "EntityAlreadyExists", Message: "exists"}
	}
	f.policyExists = true
	f.versions = []types.PolicyVersion{{VersionId: aws.String("v1"), IsDefaultVersion: true}}
	f.docs["v1"] = aws.ToString(in.PolicyDocument)
	return &iam.CreatePolicyOutput{Policy: &types.Policy{Arn: aws.String("arn:aws:iam::111122223333:policy" + sporedPolicyPath + spawnInstanceBoundaryName)}}, nil
}

func (f *boundaryFake) CreatePolicyVersion(_ context.Context, in *iam.CreatePolicyVersionInput, _ ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error) {
	f.calls = append(f.calls, "CreatePolicyVersion")
	if len(f.versions) >= boundaryVersionCap {
		return nil, &smithy.GenericAPIError{Code: "LimitExceeded", Message: "five versions"}
	}
	id := "v" + itoa(policyVersionNum(f.highestVersion())+1)
	for i := range f.versions {
		f.versions[i].IsDefaultVersion = false
	}
	f.versions = append(f.versions, types.PolicyVersion{VersionId: aws.String(id), IsDefaultVersion: in.SetAsDefault})
	f.docs[id] = aws.ToString(in.PolicyDocument)
	return &iam.CreatePolicyVersionOutput{}, nil
}

func (f *boundaryFake) highestVersion() string {
	hi := "v0"
	for _, v := range f.versions {
		if policyVersionNum(aws.ToString(v.VersionId)) > policyVersionNum(hi) {
			hi = aws.ToString(v.VersionId)
		}
	}
	return hi
}

func (f *boundaryFake) ListPolicyVersions(_ context.Context, _ *iam.ListPolicyVersionsInput, _ ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error) {
	f.calls = append(f.calls, "ListPolicyVersions")
	return &iam.ListPolicyVersionsOutput{Versions: append([]types.PolicyVersion(nil), f.versions...)}, nil
}

func (f *boundaryFake) DeletePolicyVersion(_ context.Context, in *iam.DeletePolicyVersionInput, _ ...func(*iam.Options)) (*iam.DeletePolicyVersionOutput, error) {
	f.calls = append(f.calls, "DeletePolicyVersion "+aws.ToString(in.VersionId))
	var rest []types.PolicyVersion
	for _, v := range f.versions {
		if aws.ToString(v.VersionId) != aws.ToString(in.VersionId) {
			rest = append(rest, v)
		}
	}
	f.versions = rest
	delete(f.docs, aws.ToString(in.VersionId))
	return &iam.DeletePolicyVersionOutput{}, nil
}

func (f *boundaryFake) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.calls = append(f.calls, "GetRole")
	if !f.roleExists {
		return nil, noSuchEntity("role not found")
	}
	r := &types.Role{RoleName: in.RoleName}
	if f.roleBoundary != "" {
		r.PermissionsBoundary = &types.AttachedPermissionsBoundary{PermissionsBoundaryArn: aws.String(f.roleBoundary)}
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (f *boundaryFake) PutRolePermissionsBoundary(_ context.Context, in *iam.PutRolePermissionsBoundaryInput, _ ...func(*iam.Options)) (*iam.PutRolePermissionsBoundaryOutput, error) {
	f.calls = append(f.calls, "PutRolePermissionsBoundary")
	f.roleBoundary = aws.ToString(in.PermissionsBoundary)
	return &iam.PutRolePermissionsBoundaryOutput{}, nil
}

func noWarnB(t *testing.T) func(string) {
	return func(s string) { t.Logf("warn: %s", s) }
}

// A new account: the launch path creates the boundary (admin's first launch).
func TestEnsureBoundaryForLaunch_NewAccountCreates(t *testing.T) {
	f := newBoundaryFake()
	if err := ensureBoundaryForLaunch(context.Background(), f, testAccount, noWarnB(t)); err != nil {
		t.Fatal(err)
	}
	if !f.policyExists {
		t.Error("the boundary was not created")
	}
	if got := f.docs["v1"]; got != spawnInstanceBoundaryDocument {
		t.Error("the created boundary document is not spawn's")
	}
}

// An existing boundary is used as-is, with no write.
func TestEnsureBoundaryForLaunch_ExistingIsUsed(t *testing.T) {
	f := newBoundaryFake()
	f.policyExists = true
	if err := ensureBoundaryForLaunch(context.Background(), f, testAccount, noWarnB(t)); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c == "CreatePolicy" {
			t.Errorf("an existing boundary was re-created; calls %v", f.calls)
		}
	}
}

// A non-admin whose create is refused gets a BoundaryMissingError naming the
// admin command, and the account id.
func TestEnsureBoundaryForLaunch_NonAdminIsTold(t *testing.T) {
	f := newBoundaryFake()
	f.createPolicyErr = accessDenied("iam:CreatePolicy")
	err := ensureBoundaryForLaunch(context.Background(), f, testAccount, noWarnB(t))
	var missing *BoundaryMissingError
	if err == nil || !asBoundaryMissing(err, &missing) {
		t.Fatalf("err = %v, want a BoundaryMissingError", err)
	}
	if !strings.Contains(err.Error(), "spawn setup-account") {
		t.Errorf("the error does not name the admin command: %v", err)
	}
	if !strings.Contains(err.Error(), testAccount) {
		t.Errorf("the error does not name the account: %v", err)
	}
}

func asBoundaryMissing(err error, target **BoundaryMissingError) bool {
	for e := err; e != nil; {
		if m, ok := e.(*BoundaryMissingError); ok {
			*target = m
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// Retrofit: an existing role without the boundary gets it set, before any
// policy write; a role that already has it is left alone.
func TestEnsureRoleBoundary_Retrofit(t *testing.T) {
	f := newBoundaryFake()
	if err := ensureRoleBoundary(context.Background(), f, "spored-instance-role", testAccount); err != nil {
		t.Fatal(err)
	}
	if f.roleBoundary != spawnInstanceBoundaryARN(testAccount) {
		t.Errorf("the boundary was not set on the role (got %q)", f.roleBoundary)
	}
	puts := 0
	for _, c := range f.calls {
		if c == "PutRolePermissionsBoundary" {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("PutRolePermissionsBoundary called %d times, want 1", puts)
	}

	// Already set: no second write.
	f.calls = nil
	if err := ensureRoleBoundary(context.Background(), f, "spored-instance-role", testAccount); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c == "PutRolePermissionsBoundary" {
			t.Errorf("a role already on the boundary was written again: %v", f.calls)
		}
	}
}

// The admin path: a new account creates the policy; an already-current one is
// unchanged.
func TestEnsureSpawnInstanceBoundary_CreateAndNoop(t *testing.T) {
	f := newBoundaryFake()
	out, err := EnsureSpawnInstanceBoundary(context.Background(), f, testAccount, noWarnB(t))
	if err != nil || !out.Created {
		t.Fatalf("new account: out %+v, err %v; want Created", out, err)
	}
	out, err = EnsureSpawnInstanceBoundary(context.Background(), f, testAccount, noWarnB(t))
	if err != nil || !out.Unchanged {
		t.Fatalf("already current: out %+v, err %v; want Unchanged", out, err)
	}
}

// A schema bump (the deployed document differs) writes a new default version.
func TestEnsureSpawnInstanceBoundary_SchemaBump(t *testing.T) {
	f := newBoundaryFake()
	f.policyExists = true
	f.versions = []types.PolicyVersion{{VersionId: aws.String("v1"), IsDefaultVersion: true}}
	f.docs["v1"] = `{"Version":"2012-10-17","Statement":[{"Sid":"Old","Effect":"Allow","Action":"*","Resource":"*"}]}`
	out, err := EnsureSpawnInstanceBoundary(context.Background(), f, testAccount, noWarnB(t))
	if err != nil || !out.Updated {
		t.Fatalf("drifted document: out %+v, err %v; want Updated", out, err)
	}
	if f.docs[f.defaultVersion()] != spawnInstanceBoundaryDocument {
		t.Error("the new default version is not spawn's document")
	}
}

// The five-version cap: the oldest non-default version is pruned before a new
// one is written.
func TestEnsureSpawnInstanceBoundary_FiveVersionCap(t *testing.T) {
	f := newBoundaryFake()
	f.policyExists = true
	f.versions = []types.PolicyVersion{
		{VersionId: aws.String("v1")},
		{VersionId: aws.String("v2")},
		{VersionId: aws.String("v3")},
		{VersionId: aws.String("v4")},
		{VersionId: aws.String("v5"), IsDefaultVersion: true},
	}
	for _, id := range []string{"v1", "v2", "v3", "v4", "v5"} {
		f.docs[id] = `{"Version":"2012-10-17","Statement":[{"Sid":"Old","Effect":"Allow","Action":"*","Resource":"*"}]}`
	}
	out, err := EnsureSpawnInstanceBoundary(context.Background(), f, testAccount, noWarnB(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Updated || out.Pruned != 1 {
		t.Fatalf("out %+v; want Updated with one pruned", out)
	}
	deleted := false
	for _, c := range f.calls {
		if c == "DeletePolicyVersion v1" {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the oldest non-default version (v1) was not pruned; calls %v", f.calls)
	}
	if len(f.versions) > boundaryVersionCap {
		t.Errorf("after the write there are %d versions, over the cap", len(f.versions))
	}
	if f.docs[f.defaultVersion()] != spawnInstanceBoundaryDocument {
		t.Error("the new default version is not spawn's document")
	}
}

// The boundary document is well-formed and safe to put with a real
// PutRolePolicy/CreatePolicy: valid JSON, no policy variable in any Resource
// (#1069), every Condition key belongs to its action's service or is global,
// and it both allows a broad ceiling and denies the escalation surface. The
// stored golden guards against silent drift (bump spawnInstanceBoundarySchema
// and the golden together).
func TestSpawnInstanceBoundaryDocument_Golden(t *testing.T) {
	golden := filepath.Join("testdata", "spawn-instance-boundary.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(spawnInstanceBoundaryDocument+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (set UPDATE_GOLDEN=1 to write it): %v", err)
	}
	if strings.TrimRight(string(want), "\n") != spawnInstanceBoundaryDocument {
		t.Errorf("the boundary document drifted from %s; bump spawnInstanceBoundarySchema and re-run with UPDATE_GOLDEN=1", golden)
	}

	var pol struct {
		Version   string
		Statement []struct {
			Sid       string
			Effect    string
			Action    interface{}
			Resource  interface{}
			Condition map[string]map[string]interface{}
		}
	}
	if err := json.Unmarshal([]byte(spawnInstanceBoundaryDocument), &pol); err != nil {
		t.Fatalf("the boundary is not valid JSON: %v", err)
	}
	if pol.Version != "2012-10-17" {
		t.Errorf("Version %q", pol.Version)
	}
	allowStar, denyIAM, denyOrg, denyAccount, denyAssume := false, false, false, false, false
	for _, st := range pol.Statement {
		for _, r := range asStrings(st.Resource) {
			if strings.Contains(r, "${") {
				t.Errorf("statement %s: a policy variable in a Resource fails PutRolePolicy (#1069): %q", st.Sid, r)
			}
		}
		switch st.Sid {
		case "AllowAllServicesAsTheCeiling":
			allowStar = st.Effect == "Allow" && asStrings(st.Action)[0] == "*" && asStrings(st.Resource)[0] == "*"
		case "DenyIAMAdministration":
			denyIAM = st.Effect == "Deny"
			// PassRole and CreateServiceLinkedRole must NOT be denied: the
			// nested and sweep roles need them.
			for _, a := range asStrings(st.Action) {
				if a == "iam:PassRole" || a == "iam:CreateServiceLinkedRole" {
					t.Errorf("the boundary denies %s, which the nested/sweep roles need", a)
				}
			}
		case "DenyOrganizations":
			denyOrg = st.Effect == "Deny"
		case "DenyAccountManagement":
			denyAccount = st.Effect == "Deny"
		case "DenyCrossAccountAssumeRole":
			denyAssume = st.Effect == "Deny"
			// aws:ResourceAccount is a global key, valid on sts:AssumeRole, and
			// it is in a Condition (not a Resource), so ${aws:PrincipalAccount}
			// is accepted there.
			cond := st.Condition["StringNotEquals"]
			if v, ok := cond["aws:ResourceAccount"]; !ok || v != "${aws:PrincipalAccount}" {
				t.Errorf("DenyCrossAccountAssumeRole condition is %v, want aws:ResourceAccount != ${aws:PrincipalAccount}", cond)
			}
			if asStrings(st.Action)[0] != "sts:AssumeRole" {
				t.Errorf("DenyCrossAccountAssumeRole action is %v, want sts:AssumeRole", st.Action)
			}
		}
	}
	if !allowStar {
		t.Error("the boundary has no broad Allow ceiling; it would break --iam-policy-file workloads")
	}
	if !denyIAM || !denyOrg || !denyAccount || !denyAssume {
		t.Errorf("a Deny is missing (iam %v, org %v, account %v, assume %v)", denyIAM, denyOrg, denyAccount, denyAssume)
	}
}
