package aws

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSporedBaselineGrantsFSxServiceLinkedRole is the spawn#622 regression.
//
// FSx creates a per-filesystem service-linked role on the FIRST
// CreateDataRepositoryAssociation in an account, which requires the CALLING
// principal to hold iam:CreateServiceLinkedRole. #221 granted the fsx:* actions but
// not this, so every DRA on a fresh filesystem failed with "Amazon FSx is unable to
// create Service-Linked-Role to access the S3 bucket" — and because spored mounts
// anyway by design, --fsx-import-path produced a mounted but EMPTY 1200 GiB
// filesystem (~$174/month) while the CLI reported success.
//
// It looked account-specific only because the role persists once created.
func TestSporedBaselineGrantsFSxServiceLinkedRole(t *testing.T) {
	client := &Client{}
	doc := client.sporedBaselinePolicyDoc()
	if !json.Valid([]byte(doc)) {
		t.Fatalf("baseline policy is not valid JSON:\n%s", doc)
	}

	if !strings.Contains(doc, "iam:CreateServiceLinkedRole") {
		t.Errorf("baseline policy lacks iam:CreateServiceLinkedRole — a DRA on a fresh "+
			"filesystem will fail and --fsx-import-path will mount an EMPTY filesystem (#622):\n%s", doc)
	}
	// The grant is useless without the fsx actions it enables, so pin them together.
	for _, want := range []string{"fsx:CreateDataRepositoryAssociation", "fsx:DescribeDataRepositoryAssociations"} {
		if !strings.Contains(doc, want) {
			t.Errorf("baseline policy missing %q\n%s", want, doc)
		}
	}
}

// TestFSxServiceLinkedRoleGrantIsScopedToTheOneService: iam:CreateServiceLinkedRole
// unconditioned would let a spored instance create a service-linked role for ANY
// AWS service. It must be conditioned on the FSx-S3 service principal.
func TestFSxServiceLinkedRoleGrantIsScopedToTheOneService(t *testing.T) {
	client := &Client{}

	var parsed struct {
		Statement []struct {
			Action    interface{}            `json:"Action"`
			Condition map[string]interface{} `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(client.sporedBaselinePolicyDoc()), &parsed); err != nil {
		t.Fatalf("unmarshal baseline policy: %v", err)
	}

	found := false
	for _, st := range parsed.Statement {
		if !actionsContain(st.Action, "iam:CreateServiceLinkedRole") {
			continue
		}
		found = true

		if st.Condition == nil {
			t.Fatal("iam:CreateServiceLinkedRole must be conditioned, not granted outright — " +
				"unconditioned it permits creating a service-linked role for any AWS service")
		}
		raw, err := json.Marshal(st.Condition)
		if err != nil {
			t.Fatalf("marshal condition: %v", err)
		}
		cond := string(raw)
		if !strings.Contains(cond, "iam:AWSServiceName") {
			t.Errorf("condition must key on iam:AWSServiceName, got %s", cond)
		}
		if !strings.Contains(cond, "s3.data-source.lustre.fsx.amazonaws.com") {
			t.Errorf("condition must name the FSx-S3 service principal, got %s", cond)
		}
	}
	if !found {
		t.Fatal("no statement grants iam:CreateServiceLinkedRole (#622)")
	}
}

// actionsContain handles Action being either a string or a list, which is how IAM
// permits it and how these statements are built.
func actionsContain(action interface{}, want string) bool {
	switch v := action.(type) {
	case string:
		return v == want
	case []interface{}:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
