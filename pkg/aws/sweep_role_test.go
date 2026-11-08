package aws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"

	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/sweep"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1537: spawn provisions SpawnSweepCrossAccountRole. These tests drive the
// provisioning against a fake IAM that records every call, and once against
// the emulator.

var updateSweepGolden = flag.Bool("update-sweep-golden", false, "rewrite testdata/sweep-cross-account-policy.golden.json")

const (
	sweepCompute      = "111122223333"
	sweepOrchestrator = "444455556666"
	forbidden         = "942542972736"
)

// fakeSweepIAM is one account's IAM: at most the one role, its trust, its
// inline policies (fakeRolePolicies, #1558's fake, which enforces the size
// cap) and its attached managed policies.
type fakeSweepIAM struct {
	*fakeRolePolicies
	mu       sync.Mutex
	exists   bool
	trust    string
	tags     []types.Tag
	attached []string
	getErr   error
	create   error
	update   error
	log      []string
}

func newFakeSweepIAM() *fakeSweepIAM { return &fakeSweepIAM{fakeRolePolicies: newFakeRole()} }

// scriptRole is the role scripts/setup-sweep-cross-account-role.sh made: its
// trust, and its policy under the unversioned name, as of v0.161.0.
func scriptRole(t *testing.T) *fakeSweepIAM {
	t.Helper()
	f := newFakeSweepIAM()
	f.exists = true
	f.trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + sweepOrchestrator +
		`:role/SpawnSweepOrchestratorRole"},"Action":"sts:AssumeRole"}]}`
	f.policies[sweepRolePolicyBase] = scriptPolicyV0161(sweepCompute)
	return f
}

// scriptPolicyV0161 is the document the script wrote at v0.161.0, without
// the Sids spawn adds and with the five Describe grants nothing calls.
func scriptPolicyV0161(account string) string {
	doc := `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["ec2:RunInstances","ec2:DescribeInstances","ec2:DescribeInstanceStatus","ec2:DescribeImages","ec2:DescribeKeyPairs","ec2:DescribeSecurityGroups","ec2:DescribeSubnets","ec2:DescribeVpcs","ec2:DescribeAvailabilityZones","ec2:CreateTags","ec2:TerminateInstances","ec2:CancelSpotInstanceRequests"],"Resource":"*"},
 {"Effect":"Allow","Action":["ec2:DeletePlacementGroup"],"Resource":"*","Condition":{"StringEquals":{"ec2:ResourceTag/spawn:managed":"true"}}},
 {"Effect":"Allow","Action":["iam:PassRole"],"Resource":["arn:aws:iam::ACCT:role/spored-instance-role","arn:aws:iam::ACCT:role/spawnd-role","arn:aws:iam::ACCT:role/spawn-instance-*"],"Condition":{"StringEquals":{"iam:PassedToService":"ec2.amazonaws.com"}}},
 {"Sid":"DenyPassNestedSpawnRoles","Effect":"Deny","Action":["iam:PassRole"],"Resource":["arn:aws:iam::ACCT:role/spawn-instance-nested-*"]}
]}`
	return strings.ReplaceAll(doc, "ACCT", account)
}

func (f *fakeSweepIAM) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func noSuchRole() error {
	return &types.NoSuchEntityException{Message: aws.String("The role with name SpawnSweepCrossAccountRole cannot be found.")}
}

func (f *fakeSweepIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.record("GetRole " + aws.ToString(in.RoleName))
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.exists {
		return nil, noSuchRole()
	}
	// IAM returns the trust URL-encoded.
	return &iam.GetRoleOutput{Role: &types.Role{
		RoleName:                 in.RoleName,
		AssumeRolePolicyDocument: aws.String(url.QueryEscape(f.trust)),
	}}, nil
}

func (f *fakeSweepIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.record("CreateRole " + aws.ToString(in.RoleName))
	if f.create != nil {
		return nil, f.create
	}
	if f.exists {
		return nil, &smithy.GenericAPIError{Code: "EntityAlreadyExists", Message: "Role with name SpawnSweepCrossAccountRole already exists."}
	}
	f.exists = true
	f.trust = aws.ToString(in.AssumeRolePolicyDocument)
	f.tags = in.Tags
	return &iam.CreateRoleOutput{Role: &types.Role{RoleName: in.RoleName}}, nil
}

func (f *fakeSweepIAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.record("UpdateAssumeRolePolicy")
	if f.update != nil {
		return nil, f.update
	}
	f.trust = aws.ToString(in.PolicyDocument)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (f *fakeSweepIAM) ListAttachedRolePolicies(_ context.Context, _ *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.record("ListAttachedRolePolicies")
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, a := range f.attached {
		out.AttachedPolicies = append(out.AttachedPolicies, types.AttachedPolicy{PolicyArn: aws.String(a)})
	}
	return out, nil
}

func (f *fakeSweepIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.record("DetachRolePolicy " + aws.ToString(in.PolicyArn))
	var rest []string
	for _, a := range f.attached {
		if a != aws.ToString(in.PolicyArn) {
			rest = append(rest, a)
		}
	}
	f.attached = rest
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakeSweepIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.record("DeleteRole " + aws.ToString(in.RoleName))
	if len(f.policies) > 0 || len(f.attached) > 0 {
		return nil, &smithy.GenericAPIError{Code: "DeleteConflict", Message: "Cannot delete entity, must delete policies first."}
	}
	f.exists = false
	return &iam.DeleteRoleOutput{}, nil
}

// iamCalls is every call that reached the fake, inline policies included.
func (f *fakeSweepIAM) iamCalls() []string {
	return append(append([]string(nil), f.log...), f.calls...)
}

// --- the account boundary ---------------------------------------------------

// The Amazon-internal management account is refused as the compute account
// and as the trusted account, before Organizations is even asked, and
// EnsureSweepCrossAccountRole refuses it on its own, making no IAM call.
func TestSweepRoleRefusesTheForbiddenAccount(t *testing.T) {
	asked := false
	mgmt := func(context.Context) (string, error) { asked = true; return "", nil }
	for _, c := range []struct{ compute, orch string }{{forbidden, sweepOrchestrator}, {sweepCompute, forbidden}} {
		err := CheckSweepRoleAccounts(t.Context(), c.compute, c.orch, mgmt, noWarn(t))
		var refused *SweepRoleRefusedError
		if !errors.As(err, &refused) || refused.Account != forbidden {
			t.Fatalf("compute %s, orchestrator %s: err = %v, want a refusal of %s", c.compute, c.orch, err, forbidden)
		}
		for _, want := range []string{"Amazon-internal", "o-pygqyjjoym", "ExternalId", forbidden} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}

		f := newFakeSweepIAM()
		if _, err := EnsureSweepCrossAccountRole(t.Context(), f, c.compute, c.orch, noWarn(t)); !errors.As(err, &refused) {
			t.Errorf("EnsureSweepCrossAccountRole(%s, %s): err = %v, want the refusal", c.compute, c.orch, err)
		}
		if calls := f.iamCalls(); len(calls) != 0 {
			t.Errorf("the refused ensure made IAM calls: %v", calls)
		}
	}
	if asked {
		t.Error("Organizations was asked about an account refused by name")
	}
}

// Any Organizations management account is refused, as the compute account or
// as the orchestrator's; a member account, an account in no organization and
// an organization that cannot be read are not (the last with a warning).
func TestSweepRoleRefusesAManagementAccount(t *testing.T) {
	cases := []struct {
		name    string
		mgmt    string
		err     error
		refused string
		warns   int
	}{
		{"compute is the management account", sweepCompute, nil, sweepCompute, 0},
		{"orchestrator is the management account", sweepOrchestrator, nil, sweepOrchestrator, 0},
		{"member of another's organization", "777788889999", nil, "", 0},
		{"no organization", "", nil, "", 0},
		{"organization unreadable", "", errors.New("AccessDeniedException: not authorized"), "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var w warnings
			err := CheckSweepRoleAccounts(t.Context(), sweepCompute, sweepOrchestrator,
				func(context.Context) (string, error) { return c.mgmt, c.err }, w.add)
			if c.refused == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
			} else {
				var refused *SweepRoleRefusedError
				if !errors.As(err, &refused) || refused.Account != c.refused || !strings.Contains(err.Error(), "management account") {
					t.Fatalf("err = %v, want a management-account refusal of %s", err, c.refused)
				}
			}
			if len(w) != c.warns {
				t.Errorf("warnings %v, want %d", w, c.warns)
			}
		})
	}
}

// spore.host's shared control-plane account runs the orchestrator and no
// members: a role there means the profile is aimed at the wrong account.
func TestSweepRoleRefusesTheSharedInfraAccount(t *testing.T) {
	err := CheckSweepRoleAccounts(t.Context(), sharedInfraAccount, sharedInfraAccount, nil, noWarn(t))
	var refused *SweepRoleRefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("err = %v, want the shared infra account refused", err)
	}
	if err := CheckSweepRoleAccounts(t.Context(), sweepCompute, sharedInfraAccount, nil, noWarn(t)); err != nil {
		t.Errorf("a member account trusting the shared orchestrator: %v", err)
	}
}

// --- the trust ----------------------------------------------------------------

// The trust is exactly one Allow of sts:AssumeRole by the orchestrator's
// execution role, with no condition. In particular no sts:ExternalId: the
// orchestrator sends none (TestTheOrchestratorSendsNoExternalID), so a trust
// that required one would refuse every detached sweep.
func TestSweepTrustDocumentShape(t *testing.T) {
	doc, err := SweepCrossAccountTrustDocument(sweepOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Statement":[{"Action":"sts:AssumeRole","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::444455556666:role/SpawnSweepOrchestratorRole"}}],"Version":"2012-10-17"}`
	if doc != want {
		t.Errorf("trust:\n%s\nwant:\n%s", doc, want)
	}
	if strings.Contains(doc, "ExternalId") || strings.Contains(doc, "Condition") {
		t.Errorf("the trust carries a condition the orchestrator cannot meet: %s", doc)
	}
	if _, err := SweepCrossAccountTrustDocument("arn:aws:iam::1:role/x"); err == nil {
		t.Error("a non-account was accepted as the orchestrator's account")
	}

	principal := SweepOrchestratorRoleARN(sweepOrchestrator)
	for name, c := range map[string]struct {
		doc  string
		want bool
	}{
		"spawn's":         {doc, true},
		"list forms":      {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":["` + principal + `"]},"Action":["sts:AssumeRole"]}]}`, true},
		"single object":   {`{"Statement":{"Effect":"Allow","Principal":{"AWS":"` + principal + `"},"Action":"sts:AssumeRole"}}`, true},
		"other principal": {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::942542972736:role/x"},"Action":"sts:AssumeRole"}]}`, false},
		"two principals":  {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":["` + principal + `","arn:aws:iam::777788889999:root"]},"Action":"sts:AssumeRole"}]}`, false},
		"ExternalId": {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"` + principal +
			`"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"sts:ExternalId":"x"}}}]}`, false},
		"second statement": {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"` + principal + `"},"Action":"sts:AssumeRole"},` +
			`{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`, false},
		"service principal": {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"` + principal + `","Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`, false},
		"wider action":      {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"` + principal + `"},"Action":"sts:*"}]}`, false},
		"unique ID":         {`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"AROAEXAMPLEID"},"Action":"sts:AssumeRole"}]}`, false},
		"not JSON":          {`nope`, false},
	} {
		if got := SweepTrustMatches(c.doc, principal); got != c.want {
			t.Errorf("%s: SweepTrustMatches = %v, want %v", name, got, c.want)
		}
	}
}

// The orchestrator assumes the role with no ExternalId, at every site; this is
// what the trust above is held to. If an ExternalId is ever added, it is
// added to the orchestrator, the records and the trust together.
func TestTheOrchestratorSendsNoExternalID(t *testing.T) {
	dir := filepath.Join("..", "..", "lambda", "sweep-orchestrator")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	assumes := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		assumes += strings.Count(string(b), "sts.AssumeRoleInput{")
		if bytes.Contains(b, []byte("ExternalId")) {
			t.Errorf("%s sends an ExternalId: SweepCrossAccountTrustDocument must require it, and the CLI record it", e.Name())
		}
	}
	if assumes == 0 {
		t.Fatal("found no AssumeRole in lambda/sweep-orchestrator; the matcher is stale")
	}
}

// --- the policy ---------------------------------------------------------------

func sweepPolicy(t *testing.T) string {
	t.Helper()
	doc, err := SweepCrossAccountPolicyDocument(sweepCompute)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// The least-privilege policy is pinned to a golden file. A change to it is a
// change to what the orchestrator may do in every compute account: review the
// golden diff, bump sweepRolePolicySchema, and update the pin below
// (go test ./pkg/aws -run TestSweepPolicyGolden -update-sweep-golden).
func TestSweepPolicyGolden(t *testing.T) {
	doc := sweepPolicy(t)
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(doc)); err != nil {
		t.Fatalf("the policy is not JSON: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden := filepath.Join("testdata", "sweep-cross-account-policy.golden.json")
	if *updateSweepGolden {
		if err := os.WriteFile(golden, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Errorf("SpawnSweepCrossAccountRole's policy for %s differs from %s:\n%s", sweepCompute, golden, pretty.String())
	}

	const pinnedSchema = 1
	const pin = "cd81f5748cb58ec1144fbfc31ff2382c8d4d537a2842fd55252968afff8ab3d0"
	if sweepRolePolicySchema != pinnedSchema {
		t.Fatalf("sweepRolePolicySchema is %d and the pin is for %d: update both together", sweepRolePolicySchema, pinnedSchema)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(sweepCrossAccountPolicyTemplate))); got != pin {
		t.Errorf("sweepCrossAccountPolicyTemplate changed (sha256 %s, pinned %s) but sweepRolePolicySchema is still %d.\n"+
			"Bump sweepRolePolicySchema and update the pin: at the same schema an older CLI's copy would replace "+
			"this one (#1558).", got, pin, sweepRolePolicySchema)
	}
}

// The traps the policy must not fall into: a policy variable in a Resource
// fails PutRolePolicy (#1069), a condition key of another service than the
// action's never matches, and every ARN names the compute account.
func TestSweepPolicyHasNoTraps(t *testing.T) {
	for _, st := range parseStatementsLoose(t, "sweep policy", sweepPolicy(t)) {
		res, _ := json.Marshal(st["Resource"])
		if strings.Contains(string(res), "${") {
			t.Errorf("statement %v: a policy variable in Resource fails PutRolePolicy (#1069)", st["Sid"])
		}
		for _, r := range resourceStrings(st["Resource"]) {
			parts := strings.Split(r, ":")
			if r != "*" && len(parts) > 4 && parts[4] != "" && parts[4] != sweepCompute && parts[4] != "*" {
				t.Errorf("statement %v: resource %s names account %s, not the compute account", st["Sid"], r, parts[4])
			}
		}
		services := map[string]bool{}
		for a := range actionSet(st["Action"]) {
			services[strings.SplitN(a, ":", 2)[0]] = true
		}
		cond, _ := st["Condition"].(map[string]interface{})
		for _, op := range cond {
			keys, _ := op.(map[string]interface{})
			for k := range keys {
				if svc := strings.SplitN(k, ":", 2)[0]; svc != "aws" && !services[svc] {
					t.Errorf("statement %v: condition key %s is not of its actions' service %v: it never matches", st["Sid"], k, services)
				}
			}
		}
	}
	if _, err := SweepCrossAccountPolicyDocument("SPAWN-ACCOUNT-ID"); err == nil {
		t.Error("the policy was bound to a non-account")
	}
}

// #1323/#1331/#1372: the role may pass spored's role and the spawn-instance-*
// roles to EC2 only, no other wildcard, and never a nested-spawn role.
func TestSweepPolicyPassesOnlySpawnRolesToEC2(t *testing.T) {
	sporedRole := "arn:aws:iam::" + sweepCompute + ":role/" + SporedRoleName
	perPolicy := "arn:aws:iam::" + sweepCompute + ":role/" + instancerole.NamePrefix + "*"
	nested := "arn:aws:iam::" + sweepCompute + ":role/" + NestedSpawnRolePrefix + "*"
	allows, denies := 0, 0
	for _, st := range parseStatementsLoose(t, "sweep policy", sweepPolicy(t)) {
		if !actionSet(st["Action"])["iam:PassRole"] {
			continue
		}
		res := resourceStrings(st["Resource"])
		switch st["Effect"] {
		case "Deny":
			denies++
			if len(res) != 1 || res[0] != nested || st["Condition"] != nil {
				t.Errorf("the PassRole Deny is %v (condition %v), want exactly %s, unconditioned", res, st["Condition"], nested)
			}
		case "Allow":
			allows++
			has := map[string]bool{}
			for _, r := range res {
				has[r] = true
				if strings.Contains(r, "*") && r != perPolicy {
					t.Errorf("PassRole resource %q is a wildcard other than spawn's %s", r, perPolicy)
				}
			}
			if !has[sporedRole] || !has[perPolicy] {
				t.Errorf("PassRole resources %v must include %s and %s", res, sporedRole, perPolicy)
			}
			cond, _ := st["Condition"].(map[string]interface{})
			se, _ := cond["StringEquals"].(map[string]interface{})
			if se["iam:PassedToService"] != "ec2.amazonaws.com" {
				t.Errorf("PassRole is not limited to EC2: %v", st["Condition"])
			}
		}
	}
	if allows != 1 || denies != 1 {
		t.Errorf("%d PassRole Allows and %d Denies, want one of each", allows, denies)
	}
}

func resourceStrings(v interface{}) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []interface{}:
		var out []string
		for _, e := range x {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// --- provisioning -------------------------------------------------------------

// From nothing: the role is created with spawn's trust and the spawn:managed
// tag, and the policy written under its versioned name.
func TestEnsureSweepRole_CreatesTheRole(t *testing.T) {
	f := newFakeSweepIAM()
	out, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Found || out.TrustUpdated {
		t.Errorf("outcome %+v, want Created", out)
	}
	want, _ := SweepCrossAccountTrustDocument(sweepOrchestrator)
	if f.trust != want {
		t.Errorf("trust %s, want %s", f.trust, want)
	}
	if len(f.tags) != 1 || aws.ToString(f.tags[0].Key) != "spawn:managed" || aws.ToString(f.tags[0].Value) != "true" {
		t.Errorf("tags %v, want spawn:managed=true", f.tags)
	}
	if got := f.names(); len(got) != 1 || got[0] != SweepRolePolicyName() || got[0] != "SweepCrossAccountEC2Policy-v1" {
		t.Errorf("inline policies %v, want [SweepCrossAccountEC2Policy-v1]", got)
	}
	if f.policies[SweepRolePolicyName()] != sweepPolicy(t) {
		t.Error("the policy written is not SweepCrossAccountPolicyDocument")
	}
}

// The script's role is adopted, not re-created: its trust is spawn's already,
// so it is left alone, the policy is written under the versioned name, and
// the script's unversioned copy deleted after it (never leaving the role
// without its grants). A second run changes nothing.
func TestEnsureSweepRole_AdoptsTheScriptRole(t *testing.T) {
	f := scriptRole(t)
	out, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Created || out.TrustUpdated {
		t.Errorf("outcome %+v, want Found and nothing else", out)
	}
	for _, c := range f.log {
		if strings.HasPrefix(c, "CreateRole") || c == "UpdateAssumeRolePolicy" {
			t.Errorf("adopting the script's role called %s", c)
		}
	}
	if got := f.names(); len(got) != 1 || got[0] != SweepRolePolicyName() {
		t.Errorf("inline policies %v, want only %s", got, SweepRolePolicyName())
	}
	put, del := -1, -1
	for i, c := range f.calls {
		switch c {
		case "Put " + SweepRolePolicyName():
			put = i
		case "Delete " + sweepRolePolicyBase:
			del = i
		}
	}
	if put < 0 || del < 0 || del < put {
		t.Errorf("calls %v: want the versioned Put before the Delete of %s", f.calls, sweepRolePolicyBase)
	}

	before := len(f.calls)
	if _, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls[before:] {
		if strings.HasPrefix(c, "Delete") {
			t.Errorf("a second ensure deleted something: %v", f.calls[before:])
		}
	}
}

// A stale role converges: a trust naming another principal (or carrying a
// condition) is replaced with spawn's, the old one reported; a policy from
// before #1515 (no egress-proxy grants) is replaced by the current one.
func TestEnsureSweepRole_ConvergesAStaleRole(t *testing.T) {
	f := scriptRole(t)
	f.trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::` + sweepOrchestrator +
		`:role/SpawnSweepOrchestratorRole","arn:aws:iam::777788889999:root"]},"Action":"sts:AssumeRole"}]}`
	stale := scriptPolicyV0161(sweepCompute) // no EgressProxy* statements
	f.policies[sweepRolePolicyBase] = stale
	out, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.TrustUpdated || !strings.Contains(out.PreviousTrust, "777788889999") {
		t.Errorf("outcome %+v, want the trust replaced and the old one reported", out)
	}
	if want, _ := SweepCrossAccountTrustDocument(sweepOrchestrator); f.trust != want {
		t.Errorf("trust after converging: %s", f.trust)
	}
	granted := grantedActions(t, f.policies)
	for _, a := range []string{"ec2:CreateSecurityGroup", "ssm:SendCommand", "ssm:GetCommandInvocation"} {
		if !granted[a] {
			t.Errorf("after converging the role does not grant %s", a)
		}
	}
	if granted["ec2:DescribeImages"] {
		t.Error("the stale policy's unused grants survived the convergence")
	}

	// A trust IAM rewrote to the orchestrator role's unique ID (the role was
	// deleted and re-created) is replaced too.
	f.trust = `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"AROAEXAMPLEUNIQUEID"},"Action":"sts:AssumeRole"}]}`
	if out, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t)); err != nil || !out.TrustUpdated {
		t.Errorf("unique-ID trust: outcome %+v, err %v; want it replaced", out, err)
	}
}

// A role a newer spawn converged (a higher schema) is left as it is, with a
// warning: its policy includes this one's grants (#1558).
func TestEnsureSweepRole_DefersToANewerSpawn(t *testing.T) {
	f := scriptRole(t)
	delete(f.policies, sweepRolePolicyBase)
	newer := versionedPolicyName(sweepRolePolicyBase, sweepRolePolicySchema+1)
	f.policies[newer] = sweepPolicy(t)
	var w warnings
	if _, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, w.add); err != nil {
		t.Fatal(err)
	}
	if got := f.names(); len(got) != 1 || got[0] != newer {
		t.Errorf("inline policies %v, want the newer spawn's %s alone", got, newer)
	}
	if len(w) != 1 || !strings.Contains(w[0], "newer spawn") {
		t.Errorf("warnings %v, want one about the newer spawn", w)
	}
}

// The failure classes the CLI tells apart.
func TestEnsureSweepRole_Failures(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized"}

	f := newFakeSweepIAM()
	f.getErr = denied
	out, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if !errors.Is(err, ErrSweepRoleUnknown) || out.Found || out.Created {
		t.Errorf("GetRole denied: outcome %+v, err %v; want ErrSweepRoleUnknown", out, err)
	}

	f = newFakeSweepIAM()
	f.create = denied
	out, err = EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if err == nil || errors.Is(err, ErrSweepRoleUnknown) || out.Found || out.Created || len(f.policies) != 0 {
		t.Errorf("CreateRole denied: outcome %+v, err %v, policies %v; want an error with nothing found or made", out, err, f.names())
	}

	f = scriptRole(t)
	f.trust = `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::777788889999:root"},"Action":"sts:AssumeRole"}]}`
	f.update = denied
	out, err = EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t))
	if err == nil || !out.Found {
		t.Errorf("UpdateAssumeRolePolicy denied: outcome %+v, err %v; want an error with Found", out, err)
	}
}

// Two spawns creating the role at once: the loser adopts the winner's role.
func TestEnsureSweepRole_ConcurrentCreate(t *testing.T) {
	f := scriptRole(t)
	f.exists = false // GetRole says no role
	calls := 0
	f.getErr = nil
	g := &raceIAM{fakeSweepIAM: f, gets: &calls}
	out, err := EnsureSweepCrossAccountRole(t.Context(), g, sweepCompute, sweepOrchestrator, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Created {
		t.Errorf("outcome %+v, want the concurrent role adopted", out)
	}
	if got := f.names(); len(got) != 1 || got[0] != SweepRolePolicyName() {
		t.Errorf("inline policies %v", got)
	}
}

// raceIAM's role appears between the GetRole and the CreateRole.
type raceIAM struct {
	*fakeSweepIAM
	gets *int
}

func (r *raceIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, o ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	*r.gets++
	if *r.gets == 1 {
		return nil, noSuchRole()
	}
	r.exists = true
	return r.fakeSweepIAM.GetRole(ctx, in, o...)
}

func (r *raceIAM) CreateRole(ctx context.Context, in *iam.CreateRoleInput, o ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	r.exists = true
	return r.fakeSweepIAM.CreateRole(ctx, in, o...)
}

// --- inspection and teardown ------------------------------------------------

func TestInspectSweepRole(t *testing.T) {
	st, err := InspectSweepCrossAccountRole(t.Context(), newFakeSweepIAM())
	if err != nil || st.Exists {
		t.Fatalf("no role: %+v, %v", st, err)
	}
	f := scriptRole(t)
	st, err = InspectSweepCrossAccountRole(t.Context(), f)
	if err != nil || !st.Exists || !st.Legacy || st.Schema != 0 {
		t.Fatalf("script role: %+v, %v", st, err)
	}
	if len(st.Principals) != 1 || st.Principals[0] != SweepOrchestratorRoleARN(sweepOrchestrator) {
		t.Errorf("principals %v", st.Principals)
	}
	if _, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if st, _ = InspectSweepCrossAccountRole(t.Context(), f); st.Legacy || st.Schema != sweepRolePolicySchema {
		t.Errorf("after ensure: %+v", st)
	}
}

// Teardown deletes every inline policy (spawn's versions, the script's copy,
// anything else), detaches every managed policy, and then the role. With no
// role it is a no-op.
func TestTeardownSweepRole(t *testing.T) {
	f := scriptRole(t)
	if _, err := EnsureSweepCrossAccountRole(t.Context(), f, sweepCompute, sweepOrchestrator, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	f.policies[sweepRolePolicyBase] = scriptPolicyV0161(sweepCompute) // an old CLI wrote its copy back
	f.policies["someone-elses"] = `{"Version":"2012-10-17","Statement":[]}`
	f.attached = []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}
	existed, err := TeardownSweepCrossAccountRole(t.Context(), f)
	if err != nil || !existed {
		t.Fatalf("teardown: %v, %v", existed, err)
	}
	if f.exists || len(f.policies) != 0 || len(f.attached) != 0 {
		t.Errorf("after teardown: exists %v, policies %v, attached %v", f.exists, f.names(), f.attached)
	}
	last := f.log[len(f.log)-1]
	if last != "DeleteRole "+sweep.CrossAccountRoleName {
		t.Errorf("the last call was %s, want the DeleteRole", last)
	}

	existed, err = TeardownSweepCrossAccountRole(t.Context(), f)
	if err != nil || existed {
		t.Errorf("second teardown: %v, %v; want nothing to do", existed, err)
	}
}

// The whole lifecycle against the emulator: create, adopt a script-made role
// with its policy, converge, tear down.
func TestSweepRole_AgainstIAM(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	ic := env.IAMClient()
	acct, _, err := NewClientFromConfig(env.AWSConfig).GetCallerIdentityInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		t.Helper()
		out, err := ic.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(sweep.CrossAccountRoleName)})
		if err != nil {
			t.Fatal(err)
		}
		n := append([]string(nil), out.PolicyNames...)
		sort.Strings(n)
		return n
	}

	out, err := EnsureSweepCrossAccountRole(ctx, ic, acct, sweepOrchestrator, noWarn(t))
	if err != nil || !out.Created {
		t.Fatalf("create: %+v, %v", out, err)
	}
	if got := names(); len(got) != 1 || got[0] != SweepRolePolicyName() {
		t.Errorf("after create: %v", got)
	}
	// The script writes its copy again; spawn converges.
	if _, err := ic.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String(sweep.CrossAccountRoleName), PolicyName: aws.String(sweepRolePolicyBase),
		PolicyDocument: aws.String(scriptPolicyV0161(acct)),
	}); err != nil {
		t.Fatal(err)
	}
	// substrate v0.124.0 returns the trust with its {"AWS": arn} principal
	// flattened to a string (scttfrdmn/substrate#1452), which real IAM does not,
	// so the trust is rewritten here and TrustUpdated is not asserted.
	out, err = EnsureSweepCrossAccountRole(ctx, ic, acct, sweepOrchestrator, noWarn(t))
	if err != nil || !out.Found {
		t.Fatalf("adopt: %+v, %v", out, err)
	}
	if got := names(); len(got) != 1 || got[0] != SweepRolePolicyName() {
		t.Errorf("after adopt: %v", got)
	}
	st, err := InspectSweepCrossAccountRole(ctx, ic)
	if err != nil || !st.Exists || st.Schema != sweepRolePolicySchema {
		t.Errorf("inspect: %+v, %v", st, err)
	}
	if existed, err := TeardownSweepCrossAccountRole(ctx, ic); err != nil || !existed {
		t.Fatalf("teardown: %v, %v", existed, err)
	}
	if _, err := ic.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(sweep.CrossAccountRoleName)}); !isNoSuchEntity(err) {
		t.Errorf("the role is still there after teardown: %v", err)
	}
}
