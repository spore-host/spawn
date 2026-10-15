package pipeline

import (
	"encoding/json"
	"path"
	"testing"
)

// iamMatch is IAM's ARN wildcard match for the patterns StageDataPolicy uses
// ('*' only, never spanning nothing it should not): path.Match is close enough
// once '/' is not special, so it is replaced on both sides.
func iamMatch(pattern, arn string) bool {
	const sep = "\x00"
	r := func(s string) string {
		out := []byte(s)
		for i := range out {
			if out[i] == '/' {
				out[i] = sep[0]
			}
		}
		return string(out)
	}
	ok, _ := path.Match(r(pattern), r(arn))
	return ok
}

type policyDoc struct {
	Statement []struct {
		Effect    string
		Action    []string
		Resource  string
		Condition map[string]map[string]string
	}
}

// The stage profile must cover every key the stage runner touches (the stage
// config, upstream outputs, its own outputs and both markers) and nothing in
// another pipeline's prefix. The default spored profile grants none of them,
// which is why the stages could not hand data on (#911).
func TestStageDataPolicyCoversTheStageKeysAndNoOthers(t *testing.T) {
	for _, prefix := range []string{"pipelines/ml", ""} {
		var doc policyDoc
		if err := json.Unmarshal([]byte(StageDataPolicy("bkt", prefix)), &doc); err != nil {
			t.Fatalf("prefix %q: policy is not JSON: %v", prefix, err)
		}
		objectGrant := func(action, key string) bool {
			for _, st := range doc.Statement {
				for _, a := range st.Action {
					if st.Effect == "Allow" && a == action && iamMatch(st.Resource, "arn:aws:s3:::bkt/"+key) {
						return true
					}
				}
			}
			return false
		}
		for _, key := range []string{
			StageConfigKey(prefix, "pl-1"),
			StageOutputPrefix(prefix, "up") + "data.csv",
		} {
			if !objectGrant("s3:GetObject", key) {
				t.Errorf("prefix %q: no s3:GetObject on %s", prefix, key)
			}
		}
		for _, key := range []string{
			StageOutputPrefix(prefix, "me") + "result.txt",
			StageMarkerKey(prefix, "me", MarkerComplete),
			StageMarkerKey(prefix, "me", MarkerFailed),
			StageInstanceMarkerKey(prefix, "me", 3, MarkerComplete),
			StageInstanceMarkerKey(prefix, "me", 3, MarkerFailed),
		} {
			if !objectGrant("s3:PutObject", key) {
				t.Errorf("prefix %q: no s3:PutObject on %s", prefix, key)
			}
		}
		if prefix != "" && objectGrant("s3:GetObject", "other/stages/x/output/secret") {
			t.Errorf("prefix %q: the grant reaches another prefix", prefix)
		}

		listed := false
		for _, st := range doc.Statement {
			if len(st.Action) == 1 && st.Action[0] == "s3:ListBucket" && st.Resource == "arn:aws:s3:::bkt" {
				cond := st.Condition["StringLike"]["s3:prefix"]
				if iamMatch(cond, StageOutputPrefix(prefix, "up")) {
					listed = true
				}
				if prefix != "" && iamMatch(cond, "other/") {
					t.Errorf("prefix %q: ListBucket reaches another prefix (%q)", prefix, cond)
				}
			}
		}
		if !listed {
			t.Errorf("prefix %q: no ListBucket on the upstream output prefix, so data_input cannot list it", prefix)
		}
	}
}
