//go:build e2e_tier0

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tier 0 coverage for `spawn egress-proxy` (#1515 phase 5).
//
// Against a fresh account there is no proxy and no proxy security group, so
// `status` must say so in words (not print an empty table that reads as
// "nothing was checked"), in both output formats, and `delete --dry-run` must
// find nothing to do and write nothing. Creating, upgrading and deleting a real
// proxy needs a booted instance whose spored answers a readiness probe over
// SSM, which Substrate does not serve; that path was live-tested on #1525.

func TestTier0_EgressProxy_StatusOnAFreshAccount(t *testing.T) {
	env := startSpawnSubstrate(t)

	out := env.runOK("--region", "us-east-1", "egress-proxy", "status")
	if !strings.Contains(flatten(out), "No spawn egress proxy or proxy security group in us-east-1") {
		t.Errorf("status on a fresh account must say there is no proxy and no proxy group in the region it checked:\n%s", out)
	}

	js := env.runOK("--region", "us-east-1", "-o", "json", "egress-proxy", "status")
	var doc struct {
		Regions []string          `json:"regions"`
		VPCs    []json.RawMessage `json:"vpcs"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatalf("status -o json is not JSON: %v\n%s", err, js)
	}
	if len(doc.Regions) != 1 || doc.Regions[0] != "us-east-1" || len(doc.VPCs) != 0 {
		t.Errorf("status -o json on a fresh account: regions %v, %d VPCs; want [us-east-1] and none\n%s", doc.Regions, len(doc.VPCs), js)
	}
}

func TestTier0_EgressProxy_DeleteDryRunOnAFreshAccount(t *testing.T) {
	env := startSpawnSubstrate(t)

	out := env.runOK("--region", "us-east-1", "egress-proxy", "delete", "--all", "--dry-run")
	if strings.Contains(out, "terminate ") || strings.Contains(out, "REFUSE") {
		t.Errorf("delete --dry-run on a fresh account planned work:\n%s", out)
	}
}
