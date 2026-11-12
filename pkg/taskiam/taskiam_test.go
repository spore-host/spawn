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
// on the staged objects' key shape in the staging bucket only (#1274).
func TestTaskRunPolicyFor(t *testing.T) {
	in, out, rw := []string{"in"}, []string{"out"}, []string{"rw"}
	const staging = "spawn-staging-123456789012-us-east-1"
	base := parse(t, StagingPolicyFor(in, out, "results", rw, true))
	got := parse(t, TaskRunPolicyFor(in, out, "results", rw, staging))
	if len(got.Statement) != len(base.Statement)+1 || !reflect.DeepEqual(got.Statement[:len(base.Statement)], base.Statement) {
		t.Fatalf("TaskRunPolicyFor is not StagingPolicyFor plus one statement:\n%+v\n%+v", base, got)
	}
	extra := got.Statement[len(got.Statement)-1]
	if extra.Effect != "Allow" || !reflect.DeepEqual(extra.Action, []string{"s3:GetObject"}) {
		t.Errorf("staged-files statement = %+v, want Allow s3:GetObject only", extra)
	}
	want := []string{"arn:aws:s3:::" + staging + "/*/bootstrap-" + strings.Repeat("?", 64) + ".sh"}
	if !reflect.DeepEqual(extra.Resource, want) {
		t.Errorf("staged-files resources = %q, want %q", extra.Resource, want)
	}
	if strings.Contains(StagingPolicyFor(in, out, "results", rw, true), "bootstrap-") {
		t.Error("StagingPolicyFor (launch, sweep and pool paths) gained the task-only grant")
	}
	if p := TaskRunPolicyFor(in, out, "results", rw, ""); p != StagingPolicyFor(in, out, "results", rw, true) {
		t.Error("an empty staging bucket must add nothing")
	}
}

func TestStagedFilesKey(t *testing.T) {
	sha := strings.Repeat("a", 64)
	if got, want := StagedFilesKey("t1", sha), "bootstrap/t1/bootstrap-"+sha+".sh"; got != want {
		t.Errorf("StagedFilesKey = %q, want %q", got, want)
	}
	if !strings.HasPrefix(StagedFilesKey("t1", sha), StagedFilesKeyPrefix("t1")) {
		t.Error("the key does not start with StagedFilesKeyPrefix")
	}
	// The instance-role grant still covers the key under the bootstrap/ root
	// the one-day lifecycle rule is scoped to (#1359): '*' in an S3 ARN spans
	// '/', so bucket/*/bootstrap-<sha>.sh matches bootstrap/t1/bootstrap-<sha>.sh.
	pat := StagedFilesResources("b")[0]
	if !strings.HasPrefix(pat, "arn:aws:s3:::b/*/bootstrap-") {
		t.Errorf("staged-files resource %s no longer matches keys under %s", pat, StagedFilesRoot)
	}
}

// A task's role writes the results bucket only under the results base prefix
// (tenant-11): never the bucket's other keys, such as the pool specs under
// staging/pool-specs/ that pool workers run.
func TestTaskResultsWriteIsScopedToTheResultsPrefix(t *testing.T) {
	const bucket = "spawn-results-123456789012-us-east-1"
	cases := []struct {
		key, task, want string
	}{
		{"tasks/t-1", "t-1", bucket + "/tasks"},         // the default layout
		{"runs/r9/t-1", "t-1", bucket + "/runs/r9"},     // a spec's own results_prefix
		{"t-1", "t-1", bucket},                          // results at the bucket root
		{"/tasks/t-1/", "t-1", bucket + "/tasks"},       // slashes trimmed
		{"tasks/other", "t-1", bucket + "/tasks/other"}, // not a task-id suffix: kept whole
		{"", "t-1", bucket},                             // no key at all
	}
	for _, c := range cases {
		if got := ResultsWriteTarget(bucket, c.key, c.task); got != c.want {
			t.Errorf("ResultsWriteTarget(%q, %q) = %q, want %q", c.key, c.task, got, c.want)
		}
	}
	if got := ResultsWriteTarget("", "tasks/t-1", "t-1"); got != "" {
		t.Errorf("no bucket gave %q, want \"\"", got)
	}

	pol := parse(t, TaskRunPolicyFor(nil, nil, ResultsWriteTarget(bucket, "tasks/t-1", "t-1"), nil, ""))
	var writes []string
	for _, st := range pol.Statement {
		for _, a := range st.Action {
			if a == "s3:PutObject" {
				writes = append(writes, st.Resource...)
			}
		}
	}
	if want := []string{"arn:aws:s3:::" + bucket + "/tasks/*"}; !reflect.DeepEqual(writes, want) {
		t.Errorf("results PutObject on %q, want exactly %q", writes, want)
	}
}
