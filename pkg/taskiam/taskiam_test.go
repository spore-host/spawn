package taskiam

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type policyDoc struct {
	Version   string
	Statement []struct {
		Effect   string
		Action   []string
		Resource []string
	}
}

func parse(t *testing.T, s string) policyDoc {
	t.Helper()
	var d policyDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("not a JSON policy: %v\n%s", err, s)
	}
	return d
}

// TestTaskRunPolicyFor: a task's policy is StagingPolicyFor with provenance,
// plus exactly one statement more, which allows s3:GetObject and nothing else
// on the staged objects' name shape in the results bucket only (#1274).
func TestTaskRunPolicyFor(t *testing.T) {
	in, out, rw := []string{"in"}, []string{"out"}, []string{"rw"}
	base := parse(t, StagingPolicyFor(in, out, "results", rw, true))
	got := parse(t, TaskRunPolicyFor(in, out, "results", rw))
	if len(got.Statement) != len(base.Statement)+1 || !reflect.DeepEqual(got.Statement[:len(base.Statement)], base.Statement) {
		t.Fatalf("TaskRunPolicyFor is not StagingPolicyFor plus one statement:\n%+v\n%+v", base, got)
	}
	extra := got.Statement[len(got.Statement)-1]
	if extra.Effect != "Allow" || !reflect.DeepEqual(extra.Action, []string{"s3:GetObject"}) {
		t.Errorf("staged-files statement = %+v, want Allow s3:GetObject only", extra)
	}
	q := strings.Repeat("?", 64)
	want := []string{"arn:aws:s3:::results/bootstrap-" + q + ".sh", "arn:aws:s3:::results/*/bootstrap-" + q + ".sh"}
	if !reflect.DeepEqual(extra.Resource, want) {
		t.Errorf("staged-files resources = %q, want %q", extra.Resource, want)
	}
	if strings.Contains(StagingPolicyFor(in, out, "results", rw, true), "bootstrap-") {
		t.Error("StagingPolicyFor (launch, sweep and pool paths) gained the task-only grant")
	}
	if TaskRunPolicyFor(nil, nil, "", nil) != "" {
		t.Error("no buckets at all must still be no policy")
	}
}

func TestStagedFilesKey(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for prefix, want := range map[string]string{"tasks/t1": "tasks/t1/bootstrap-" + sha + ".sh", "": "bootstrap-" + sha + ".sh"} {
		if got := StagedFilesKey(prefix, sha); got != want {
			t.Errorf("StagedFilesKey(%q) = %q, want %q", prefix, got, want)
		}
		if !strings.HasPrefix(StagedFilesKey(prefix, sha), StagedFilesKeyPrefix(prefix)) {
			t.Errorf("the key under %q does not start with StagedFilesKeyPrefix", prefix)
		}
	}
}
