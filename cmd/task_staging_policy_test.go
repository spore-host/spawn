package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// iamPolicyDoc is enough of an IAM policy to check the shapes IAM rejects.
type iamPolicyDoc struct {
	Version   string `json:"Version"`
	Statement []struct {
		Effect   string          `json:"Effect"`
		Action   json.RawMessage `json:"Action"`
		Resource []string        `json:"Resource"`
	} `json:"Statement"`
}

// assertIAMAcceptable checks the two things IAM rejects a document for that
// json.Valid happily accepts: a statement with an empty Resource list, and a
// document with no statements.
//
// This distinction is the whole of #669. The pre-existing test for the
// read-only case (TestS3FlagsAreAdditiveWithIAMPolicy) asserted only the
// "Version" prefix, so it passed for years while the function returned a
// document IAM would not take.
func assertIAMAcceptable(t *testing.T, policy string) {
	t.Helper()
	if policy == "" {
		t.Fatal("empty policy string; callers skip attaching it, so assert that instead")
	}
	var doc iamPolicyDoc
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, policy)
	}
	if len(doc.Statement) == 0 {
		t.Errorf(`"Statement":[] is rejected by IAM as MalformedPolicyDocument; a policy `+
			"with nothing to say must not be built at all.\n%s", policy)
	}
	for i, st := range doc.Statement {
		if len(st.Resource) == 0 {
			t.Errorf(`Statement[%d] (%s) has "Resource":[], which IAM rejects as `+
				"MalformedPolicyDocument — and it rejects the WHOLE document, so every "+
				"launch using this role fails.\n%s", i, st.Action, policy)
		}
		for _, r := range st.Resource {
			if r == "" || strings.HasSuffix(r, ":::") || strings.HasSuffix(r, ":::/*") {
				t.Errorf("Statement[%d] has a bucket-less ARN %q", i, r)
			}
		}
	}
}

// TestTaskStagingPolicy_ReadOnly is spawn#669.
//
// `--s3-read my-bucket` with no `--s3-write` failed the entire launch with
// MalformedPolicyDocument. taskStagingPolicy appended the s3:PutObject statement
// unconditionally, but on the launch path outputBuckets is empty and
// resultsBucket is "" — and dedupeBuckets drops empty strings — so the statement
// shipped as {"Action":["s3:PutObject"],"Resource":[]}.
//
// IAM rejects the whole document for that, not just the statement, so a
// read-only bucket grant made the instance profile impossible to create and the
// launch could not proceed at all.
func TestTaskStagingPolicy_ReadOnly(t *testing.T) {
	policy := taskStagingPolicy([]string{"my-inputs"}, nil, "", nil)
	assertIAMAcceptable(t, policy)

	if strings.Contains(policy, "s3:PutObject") {
		t.Errorf("a read-only grant must not contain s3:PutObject at all — there is no "+
			"bucket to write to, and an unscoped write would be worse than the bug.\n%s", policy)
	}
	if !strings.Contains(policy, "s3:GetObject") {
		t.Errorf("the read grant is the point of --s3-read and must survive:\n%s", policy)
	}
	if !strings.Contains(policy, `"arn:aws:s3:::my-inputs/*"`) {
		t.Errorf("expected the input bucket's object ARN:\n%s", policy)
	}
}

// TestTaskStagingPolicy_NoBucketsAtAll: guarding the write statement alone would
// turn this case from one invalid document into another, because an empty
// Statement list is equally malformed. The function returns "" instead, and
// every caller already skips attaching an empty InlinePolicyJSON (the
// PutRolePolicy calls in pkg/aws/iam.go are both guarded on != "").
func TestTaskStagingPolicy_NoBucketsAtAll(t *testing.T) {
	if got := taskStagingPolicy(nil, nil, "", nil); got != "" {
		t.Errorf("with no buckets the policy must be empty so callers skip attaching it; "+
			`got %q — note "Statement":[] is MalformedPolicyDocument too`, got)
	}
}

// TestTaskStagingPolicy_WriteStillGranted guards against over-correcting: the
// write statement must still appear whenever there IS something to write to,
// from either --s3-write or the results bucket.
func TestTaskStagingPolicy_WriteStillGranted(t *testing.T) {
	t.Run("from --s3-write", func(t *testing.T) {
		policy := taskStagingPolicy(nil, []string{"out"}, "", nil)
		assertIAMAcceptable(t, policy)
		if !strings.Contains(policy, "s3:PutObject") || !strings.Contains(policy, `"arn:aws:s3:::out/*"`) {
			t.Errorf("write grant lost:\n%s", policy)
		}
	})

	t.Run("from the results bucket", func(t *testing.T) {
		policy := taskStagingPolicy(nil, nil, "spawn-results-1-us-east-1", nil)
		assertIAMAcceptable(t, policy)
		if !strings.Contains(policy, `"arn:aws:s3:::spawn-results-1-us-east-1/*"`) {
			t.Errorf("the results bucket must still be writable:\n%s", policy)
		}
	})

	t.Run("read and write together", func(t *testing.T) {
		policy := taskStagingPolicy([]string{"in"}, []string{"out"}, "res", nil)
		assertIAMAcceptable(t, policy)
		for _, want := range []string{`"arn:aws:s3:::in/*"`, `"arn:aws:s3:::out/*"`, `"arn:aws:s3:::res/*"`} {
			if !strings.Contains(policy, want) {
				t.Errorf("missing %s:\n%s", want, policy)
			}
		}
	})

	t.Run("read-write buckets only", func(t *testing.T) {
		policy := taskStagingPolicy(nil, nil, "", []string{"storage"})
		assertIAMAcceptable(t, policy)
		if !strings.Contains(policy, "s3:DeleteObject") {
			t.Errorf("an --s3-read-write bucket needs delete:\n%s", policy)
		}
	})
}
