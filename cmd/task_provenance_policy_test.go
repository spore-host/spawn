package cmd

import (
	"encoding/json"
	"testing"
)

type stagingPolicyDoc struct {
	Statement []struct {
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	} `json:"Statement"`
}

func grantsOn(t *testing.T, pol, action string) map[string]bool {
	t.Helper()
	var d stagingPolicyDoc
	if err := json.Unmarshal([]byte(pol), &d); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, pol)
	}
	out := map[string]bool{}
	for _, st := range d.Statement {
		for _, a := range st.Action {
			if a == action {
				for _, r := range st.Resource {
					out[r] = true
				}
			}
		}
	}
	return out
}

// #1076: `spawn task run` uploads outputs with tags and annotates them, so its
// write buckets need PutObjectTagging and PutObjectAnnotation. Its READ buckets
// must not get them, and the shared policy, used by `spawn launch --s3-write`
// and sweeps, must not change.
func TestTaskProvenanceGrantsOnlyTheWriteBuckets(t *testing.T) {
	pol := taskStagingPolicyFor([]string{"in-bucket"}, []string{"out-bucket"}, "spawn-results-1-us-east-1", nil, true)
	for _, action := range []string{"s3:PutObject", "s3:PutObjectTagging", "s3:PutObjectAnnotation"} {
		on := grantsOn(t, pol, action)
		for _, want := range []string{"arn:aws:s3:::out-bucket/*", "arn:aws:s3:::spawn-results-1-us-east-1/*"} {
			if !on[want] {
				t.Errorf("%s not granted on %s:\n%s", action, want, pol)
			}
		}
		for r := range on {
			if r == "arn:aws:s3:::in-bucket/*" || r == "*" {
				t.Errorf("%s granted on %s, which the task only reads:\n%s", action, r, pol)
			}
		}
	}

	shared := taskStagingPolicy([]string{"in-bucket"}, []string{"out-bucket"}, "spawn-results-1-us-east-1", nil)
	for _, action := range []string{"s3:PutObjectTagging", "s3:PutObjectAnnotation"} {
		if len(grantsOn(t, shared, action)) != 0 {
			t.Errorf("the shared staging policy grants %s, which only provenance uploads use:\n%s", action, shared)
		}
	}
}
