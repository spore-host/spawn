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

// TestBothSporedPolicyPathsGrantTheFSxServiceLinkedRole closes the gap #622's
// report did not mention.
//
// spawn resolves an instance profile through TWO divergent paths (the #550
// contrast): CreateOrGetInstanceProfile builds the dynamic baseline, while
// SetupSporedIAMRole writes the static sporedDCVRolePolicy and is what a launch
// with NO --iam-* flags gets — the simplest invocation there is. That static policy
// already had the #221 fsx: actions but not the service-linked-role grant, so it
// had the identical silent-empty-filesystem bug. Fixing only the path the reporter
// happened to use would have left the more common one broken.
func TestBothSporedPolicyPathsGrantTheFSxServiceLinkedRole(t *testing.T) {
	client := &Client{}
	for name, doc := range map[string]string{
		"dynamic baseline (CreateOrGetInstanceProfile)": client.sporedBaselinePolicyDoc(),
		"static DCV/no-flags role (SetupSporedIAMRole)": sporedDCVRolePolicy,
	} {
		t.Run(name, func(t *testing.T) {
			if !json.Valid([]byte(doc)) {
				t.Fatalf("policy is not valid JSON:\n%s", doc)
			}
			if !strings.Contains(doc, "fsx:CreateDataRepositoryAssociation") {
				t.Fatalf("policy lacks fsx:CreateDataRepositoryAssociation, so the SLR grant would be pointless here")
			}
			if !strings.Contains(doc, "iam:CreateServiceLinkedRole") {
				t.Errorf("policy grants fsx:CreateDataRepositoryAssociation but not "+
					"iam:CreateServiceLinkedRole — the association will fail and --fsx-import-path "+
					"will mount an EMPTY filesystem (#622):\n%s", doc)
			}
			if !strings.Contains(doc, "s3.data-source.lustre.fsx.amazonaws.com") {
				t.Errorf("the SLR grant must be conditioned to the FSx-S3 service principal:\n%s", doc)
			}
		})
	}
}

// TestSporedDCVPolicyIsWellFormed guards the specific mistake of annotating a
// statement: IAM rejects unknown statement elements with MalformedPolicyDocument,
// which would break every launch on this path rather than degrade it.
func TestSporedDCVPolicyIsWellFormed(t *testing.T) {
	var parsed struct {
		Version   string                   `json:"Version"`
		Statement []map[string]interface{} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(sporedDCVRolePolicy), &parsed); err != nil {
		t.Fatalf("sporedDCVRolePolicy is not valid JSON: %v", err)
	}
	if parsed.Version == "" {
		t.Error("policy has no Version")
	}
	allowed := map[string]bool{
		"Sid": true, "Effect": true, "Action": true, "NotAction": true,
		"Resource": true, "NotResource": true, "Condition": true,
		"Principal": true, "NotPrincipal": true,
	}
	for i, st := range parsed.Statement {
		for key := range st {
			if !allowed[key] {
				t.Errorf("statement %d has non-IAM element %q — IAM rejects this with "+
					"MalformedPolicyDocument (no comments allowed in a policy document)", i, key)
			}
		}
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
