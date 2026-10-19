package cmd

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// policyActions returns every action a policy document allows.
func policyActions(t *testing.T, pol string) map[string]bool {
	t.Helper()
	var d stagingPolicyDoc
	if err := json.Unmarshal([]byte(pol), &d); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, pol)
	}
	out := map[string]bool{}
	for _, st := range d.Statement {
		for _, a := range st.Action {
			out[a] = true
		}
	}
	return out
}

// taskRunProvenanceActions discovers, from `spawn task run`'s own policy
// builder, the actions it adds for provenance: what the provenance=true policy
// allows that the provenance=false one does not. Discovered rather than
// listed, so an action task run adds later is required of pools too.
func taskRunProvenanceActions(t *testing.T) []string {
	t.Helper()
	with := policyActions(t, taskStagingPolicyFor(nil, []string{"out"}, "results", nil, true))
	without := policyActions(t, taskStagingPolicyFor(nil, []string{"out"}, "results", nil, false))
	var out []string
	for a := range with {
		if !without[a] {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("discovered no provenance actions in task run's policy; the discovery is broken, not the pool clean")
	}
	return out
}

// TestPoolWorkerPolicyGrantsTaskRunsProvenanceActions is #1148: a pooled job
// uploads its outputs with `spored provenance upload`, as `task run` does, so
// every bucket a worker writes needs the same provenance actions. Without them
// pooled outputs land untagged and the results-retention lifecycle (#1076)
// cannot see them.
func TestPoolWorkerPolicyGrantsTaskRunsProvenanceActions(t *testing.T) {
	const results = "spawn-results-123456789012-us-east-1"
	pol := poolWorkerPolicy("123456789012", "us-east-1", results, "run-1", []string{"in-bucket"}, []string{"out-bucket"})
	for _, action := range taskRunProvenanceActions(t) {
		on := grantsOn(t, pol, action)
		for _, want := range []string{"arn:aws:s3:::" + results + "/*", "arn:aws:s3:::out-bucket/*"} {
			if !on[want] {
				t.Errorf("pool worker policy does not grant %s on %s, which task run grants for provenance:\n%s", action, want, pol)
			}
		}
		if on["arn:aws:s3:::in-bucket/*"] {
			t.Errorf("%s granted on a bucket the pool only reads:\n%s", action, pol)
		}
	}
}

// TestPoolECRGrantMatchesTaskRun is #1148: a pool whose tasks run a private-ECR
// image needs the pull grant, and gets it iff `spawn task run` would grant it
// for the same image.
func TestPoolECRGrantMatchesTaskRun(t *testing.T) {
	images := []string{
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/tools:1.0",
		"210987654321.dkr.ecr.eu-west-1.amazonaws.com/ns/img@sha256:abc",
		"public.ecr.aws/docker/library/ubuntu:24.04",
		"docker.io/library/ubuntu:24.04",
		"ubuntu",
		"ghcr.io/org/img:v1",
	}
	var granted, notGranted int
	poolGrants := func(containers ...string) bool {
		return contains(poolWorkerRoleConfig("123456789012", "us-east-1", "b", "r", nil, nil, containers).Policies, "ecr:ReadOnly")
	}
	for _, img := range images {
		taskRun := len(taskExtraPolicies(&taskproto.TaskSpec{Container: img})) > 0
		pool := poolGrants(img)
		if pool != taskRun {
			t.Errorf("image %s: pool ECR grant = %v, task run = %v", img, pool, taskRun)
		}
		if taskRun {
			granted++
		} else {
			notGranted++
		}
	}
	if granted == 0 || notGranted == 0 {
		t.Fatalf("image corpus exercises only one side (granted %d, not %d)", granted, notGranted)
	}
	// Mixed: one private image among public ones still needs the grant; none at all needs nothing.
	if !poolGrants("ubuntu", images[0]) {
		t.Error("a pool with one private-ECR image among several got no pull grant")
	}
	if poolGrants() {
		t.Error("a pool with no --container got an ECR grant")
	}
}

// TestPoolContainerFlagReachesTheRole: the grant is decided from the
// --container flag, so the flag must exist and bind to what the role uses.
func TestPoolContainerFlagReachesTheRole(t *testing.T) {
	old := poolContainers
	t.Cleanup(func() { poolContainers = old })
	f := poolCreateCmd.Flags().Lookup("container")
	if f == nil {
		t.Fatal("pool create has no --container flag")
	}
	if err := f.Value.Set("123456789012.dkr.ecr.us-east-1.amazonaws.com/tools:1.0"); err != nil {
		t.Fatal(err)
	}
	if !contains(poolWorkerRoleConfig("1", "us-east-1", "b", "r", nil, nil, poolContainers).Policies, "ecr:ReadOnly") {
		t.Errorf("--container did not reach the pool's ECR decision (poolContainers=%v)", poolContainers)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
