package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshotBuildPolicyExampleWellFormed guards the ready-to-edit IAM policy
// referenced by 'spawn snapshot create --help' and docs/reference-data-volumes.md
// (#579): it must stay valid JSON and grant exactly the EBS-direct snapshot
// actions a snapshot build needs. A malformed example would break the very
// "--iam-policy-file" path the docs point users at.
func TestSnapshotBuildPolicyExampleWellFormed(t *testing.T) {
	path := filepath.Join("..", "examples", "iam", "snapshot-build-policy.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example policy %s: %v", path, err)
	}

	var doc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect string          `json:"Effect"`
			Action json.RawMessage `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("example policy is not well-formed JSON: %v", err)
	}
	if doc.Version == "" {
		t.Error("example policy missing Version")
	}
	if len(doc.Statement) == 0 {
		t.Fatal("example policy has no statements")
	}

	// Collect every action string across all statements (Action may be a string
	// or an array of strings).
	got := map[string]bool{}
	for _, s := range doc.Statement {
		var one string
		var many []string
		if json.Unmarshal(s.Action, &one) == nil {
			got[one] = true
			continue
		}
		if err := json.Unmarshal(s.Action, &many); err != nil {
			t.Fatalf("statement Action is neither string nor []string: %s", s.Action)
		}
		for _, a := range many {
			got[a] = true
		}
	}

	for _, want := range []string{
		"ebs:StartSnapshot",
		"ebs:PutSnapshotBlock",
		"ebs:CompleteSnapshot",
		"ec2:DescribeSnapshots",
	} {
		if !got[want] {
			t.Errorf("example policy missing required action %q", want)
		}
	}
}
