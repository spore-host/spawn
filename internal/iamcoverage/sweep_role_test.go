package iamcoverage

import (
	"encoding/json"
	"sort"
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
