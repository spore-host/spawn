package reaperiam

import (
	"strings"
	"testing"

	"github.com/spore-host/libs/lifecycle"

	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/sweep"
)

// The account-idle sweep's IAM (#1654) names the resources the code uses:
// the lifecycle table libs/lifecycle creates, and the orchestration tables
// pkg/sweep and pkg/pipeline write, in pkg/scheduler.TableRegion.
func TestAccountIdleGrantsNameTheTablesTheCodeUses(t *testing.T) {
	if want := "arn:aws:dynamodb:*:*:table/" + lifecycle.TableName; LifecycleTableARN != want {
		t.Errorf("LifecycleTableARN = %s, but libs/lifecycle's table makes it %s", LifecycleTableARN, want)
	}
	if want := "arn:aws:dynamodb:" + scheduler.TableRegion + ":*:table/" + sweep.DefaultResourceNames().DynamoTableName; SweepsTableARN != want {
		t.Errorf("SweepsTableARN = %s, but pkg/sweep's table makes it %s", SweepsTableARN, want)
	}
	if want := "arn:aws:dynamodb:" + scheduler.TableRegion + ":*:table/" + pipeline.DefaultTableName; PipelinesTableARN != want {
		t.Errorf("PipelinesTableARN = %s, but pkg/pipeline's table makes it %s", PipelinesTableARN, want)
	}
}

// Every DynamoDB write the member role holds is on the lifecycle table: the
// account-idle sweep may write its lease and nothing else in a member
// account. The writes are discovered (any dynamodb: action that is not a
// Describe, List, Get or Scan), not listed.
func TestMemberDynamoDBWritesAreOnlyTheLease(t *testing.T) {
	sources := map[string]map[grant]bool{
		"pkg/reaperiam":                 reaperIAMGrants(t),
		samTemplatePath + " (ScanSelf)": explode(t, samTemplatePath, samScanSelfStatements(t)),
		crossAccountRolePath:            explode(t, crossAccountRolePath, crossAccountRoleStatements(t)),
	}
	writes := 0
	for where, grants := range sources {
		for g := range grants {
			verb, ok := strings.CutPrefix(g.Action, "dynamodb:")
			if !ok {
				continue
			}
			if verb == "ListTables" || verb == "ListTagsOfResource" {
				continue // the footprint's listing of tables and their tags
			}
			if g.Resources != LifecycleTableARN {
				t.Errorf("%s: %s is granted on %s; in a member account the reaper touches only %s", where, g.Action, g.Resources, LifecycleTableARN)
			}
			if verb != "GetItem" && verb != "DescribeTable" {
				writes++
			}
		}
	}
	if writes == 0 {
		t.Fatal("found no lease-table write in any copy: the lease cannot be observed, and this gate checks nothing")
	}
}

// spawn#1664: the reaper's roles can never delete a managed policy, and so
// never the permissions boundary spore-host/spawn-instance-boundary, the one
// spore.host resource that outlives every teardown. No copy of its policy
// grants iam:DeletePolicy, iam:DeletePolicyVersion or an iam: wildcard; were
// one to, it would need an explicit Deny on the boundary first.
func TestReaperCannotDeleteAPolicy(t *testing.T) {
	sources := map[string]map[grant]bool{
		"pkg/reaperiam":                 reaperIAMGrants(t),
		samTemplatePath + " (ScanSelf)": explode(t, samTemplatePath, samScanSelfStatements(t)),
		crossAccountRolePath:            explode(t, crossAccountRolePath, crossAccountRoleStatements(t)),
	}
	for where, grants := range sources {
		for g := range grants {
			if g.Effect != "Allow" {
				continue
			}
			switch {
			case g.Action == "iam:DeletePolicy", g.Action == "iam:DeletePolicyVersion",
				g.Action == "*", strings.HasPrefix(g.Action, "iam:*"), strings.HasPrefix(g.Action, "iam:Delete*"):
				t.Errorf("%s grants %s on %s: the reaper must never be able to delete the spawn-instance boundary (#1664)", where, g.Action, g.Resources)
			}
		}
	}
}
