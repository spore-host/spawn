package iamcoverage

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// sweepRoleUncalledGrants are the actions SpawnSweepCrossAccountRole grants
// that no call site in the orchestrator names, and why each is needed anyway.
var sweepRoleUncalledGrants = map[string]string{
	"iam:CreateServiceLinkedRole": "RunInstances of a Spot member creates AWSServiceRoleForEC2Spot in an account " +
		"that has none, and IAM authorizes that as the caller's iam:CreateServiceLinkedRole; no SDK call names it",
}

// TestSweepCrossAccountRoleGrantsOnlyWhatTheOrchestratorCalls is the other
// half of TestEveryPrincipalCallIsGranted for the role spawn provisions in
// every compute account (#1537): least privilege, derived from the code. Every
// action the role's Allow statements grant is one the sweep orchestrator calls
// on the member-account credentials, or is in sweepRoleUncalledGrants with the
// reason. The script-era policy granted five Describe actions nothing called
// (DescribeImages, DescribeInstanceStatus, DescribeKeyPairs, DescribeSubnets,
// DescribeVpcs); a grant like that now fails here.
func TestSweepCrossAccountRoleGrantsOnlyWhatTheOrchestratorCalls(t *testing.T) {
	var orch principal
	for _, p := range principals {
		if p.name == "sweep-orchestrator" {
			orch = p
		}
	}
	if orch.name == "" {
		t.Fatal("no sweep-orchestrator principal")
	}
	inv := inventories(t)[orch.name]
	if inv == nil || len(inv.actions) == 0 {
		t.Fatal("discovered no calls for the sweep orchestrator")
	}
	called := map[string]bool{}
	for action, sites := range inv.actions {
		ctxs, _ := orch.contextsFor(action, sites)
		for _, c := range ctxs {
			if c.name == "member account" {
				called[action] = true
			}
		}
	}

	doc, err := spawnaws.SweepCrossAccountPolicyDocument(exAccount)
	if err != nil {
		t.Fatal(err)
	}
	var policy struct{ Statement []map[string]any }
	if err := json.Unmarshal([]byte(doc), &policy); err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, st := range policy.Statement {
		if st["Effect"] != "Allow" {
			continue
		}
		for _, a := range resourceList(st["Action"]) {
			granted[a] = true
		}
	}
	var extra []string
	for a := range granted {
		if !called[a] {
			if _, ok := sweepRoleUncalledGrants[a]; !ok {
				extra = append(extra, a)
			}
		}
	}
	sort.Strings(extra)
	for _, a := range extra {
		t.Errorf("SpawnSweepCrossAccountRole grants %s, which the sweep orchestrator never calls on the member-account "+
			"credentials: remove it (and bump the policy schema), or record why in sweepRoleUncalledGrants", a)
	}
	for a := range sweepRoleUncalledGrants {
		if called[a] {
			t.Errorf("sweepRoleUncalledGrants lists %s, which the orchestrator now calls: remove the entry", a)
		}
		if !granted[a] {
			t.Errorf("sweepRoleUncalledGrants lists %s, which the role no longer grants: remove the entry", a)
		}
	}
}

// TestSweepRoleTagsOnlyAtTheCreatesTheOrchestratorMakes holds the sweep
// role's tag-on-create list (#1586) to the code. SpawnSweepCrossAccountRole
// may call ec2:CreateTags only as part of a create, named in ec2:CreateAction;
// every other CreateTags is Denied. So every create the orchestrator tags
// at, as discovered from its calls, must be in the list (or the create is
// refused), and every entry must be a create it makes (or the list grants a
// tag path nothing uses).
func TestSweepRoleTagsOnlyAtTheCreatesTheOrchestratorMakes(t *testing.T) {
	var orch principal
	for _, p := range principals {
		if p.name == "sweep-orchestrator" {
			orch = p
		}
	}
	inv := inventories(t)[orch.name]
	if inv == nil {
		t.Fatal("no inventory for the sweep orchestrator")
	}
	creates := map[string]bool{}
	sites := inv.actions["ec2:CreateTags"]
	ctxs, _ := orch.contextsFor("ec2:CreateTags", sites)
	member := false
	for _, c := range ctxs {
		if c.name == "member account" {
			member = true
		}
	}
	if !member {
		t.Fatal("discovered no ec2:CreateTags on the member-account credentials: the discovery is broken")
	}
	for _, s := range sites {
		// Op is the operation as written, then what the call set: "ec2.RunInstances [TagSpecifications set]".
		op, _, _ := strings.Cut(s.Op, " ")
		op, ok := strings.CutPrefix(op, "ec2.")
		if !ok {
			t.Errorf("ec2:CreateTags at %s comes from %s, not an EC2 create", s.Pos, s.Op)
			continue
		}
		creates[op] = true
	}
	if len(creates) == 0 {
		t.Fatal("discovered no create the orchestrator tags at")
	}

	doc, err := spawnaws.SweepCrossAccountPolicyDocument(exAccount)
	if err != nil {
		t.Fatal(err)
	}
	var policy struct{ Statement []map[string]any }
	if err := json.Unmarshal([]byte(doc), &policy); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, st := range policy.Statement {
		if st["Effect"] != "Allow" || !contains(resourceList(st["Action"]), "ec2:CreateTags") {
			continue
		}
		cond, _ := st["Condition"].(map[string]any)
		eq, _ := cond["StringEquals"].(map[string]any)
		vals := resourceList(eq["ec2:CreateAction"])
		if len(vals) == 0 {
			t.Errorf("statement %v grants ec2:CreateTags outside a create (no StringEquals ec2:CreateAction)", st["Sid"])
		}
		for _, v := range vals {
			allowed[v] = true
		}
	}
	for op := range creates {
		if !allowed[op] {
			t.Errorf("the orchestrator tags at %s, which the sweep role's ec2:CreateAction list does not name: "+
				"every such launch is refused", op)
		}
	}
	for op := range allowed {
		if !creates[op] {
			t.Errorf("the sweep role may tag at %s, which the orchestrator never calls with tags: remove it", op)
		}
	}
}

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}
