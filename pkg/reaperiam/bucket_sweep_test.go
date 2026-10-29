package reaperiam

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestS3GrantsAreScopedToSpawnsTwoBucketFamilies: the empty-bucket sweep
// (#1301) can delete a bucket, so every s3: action the reaper holds is granted
// on spawn's staging and results bucket prefixes and nothing else — never "*",
// never another name. The object-level grants are s3:DeleteObject on the pools'
// staged specs under the results bucket (#1351) and on the launch config
// objects in the staging bucket (#1359), and nothing more: no s3:GetObject on
// either. The staging bucket is listed only under launch-config/ (#1359), by
// condition, so its task bootstrap objects stay unlistable (#1274). The
// prefixes are derived from the names spawn creates, not typed here. The SAM
// and cross-account copies are held to these statements by the template drift
// gates.
func TestS3GrantsAreScopedToSpawnsTwoBucketFamilies(t *testing.T) {
	resultsARN := "arn:aws:s3:::" + strings.TrimSuffix(taskproto.DefaultResultsBucket("", ""), "-") + "*"
	stagingARN := "arn:aws:s3:::" + taskproto.StagingBucketPrefix + "*"
	// #1351: the one object grant, delete-only, on the pools' staged specs.
	specsARN := resultsARN + "/" + poolqueue.SpecsRoot() + "*"
	// #1359: delete-only, on the launch config objects.
	configsARN := stagingARN + "/" + launchconfig.KeyPrefix + "config-*.json"
	want := map[string]bool{stagingARN: true, resultsARN: true, specsARN: true, configsARN: true}
	listCond := canonicalJSON(t, map[string]map[string]string{"StringLike": {"s3:prefix": launchconfig.KeyPrefix + "*"}})

	seen := map[string]bool{}
	stagingList := false
	for _, st := range ScanSelfStatements() {
		s3 := false
		for _, a := range st.Action {
			if strings.HasPrefix(a, "s3:") {
				s3 = true
			}
		}
		if !s3 {
			continue
		}
		if !want[st.Resource] {
			t.Errorf("s3 actions %v are granted on %q; only %v are allowed", st.Action, st.Resource, want)
			continue
		}
		seen[st.Resource] = true
		for _, a := range st.Action {
			if !strings.HasPrefix(a, "s3:") {
				t.Errorf("%s shares a statement scoped to %s", a, st.Resource)
			}
			if strings.HasPrefix(a, "s3:GetObject") || a == "s3:*" {
				t.Errorf("%s is granted on %s: the reaper reads no object", a, st.Resource)
			}
		}
		wantActions := ResultsBucketSweepActions()
		switch {
		case st.Resource == specsARN || st.Resource == configsARN:
			wantActions = []string{"s3:DeleteObject"}
		case st.Resource == stagingARN && strings.Join(st.Action, ",") == "s3:ListBucket":
			// The one list of the staging bucket, and only under
			// launch-config/.
			stagingList = true
			wantActions = []string{"s3:ListBucket"}
			if st.Condition == nil || canonicalJSON(t, st.Condition) != listCond {
				t.Errorf("s3:ListBucket on the staging bucket is not limited to %s*: condition %v", launchconfig.KeyPrefix, st.Condition)
			}
		case st.Resource == stagingARN:
			wantActions = StagingBucketSweepActions()
			for _, a := range st.Action {
				if strings.HasPrefix(a, "s3:List") {
					t.Errorf("%s is granted on the staging bucket unconditioned; no spawn role may list its bootstrap objects", a)
				}
			}
		}
		if strings.Join(st.Action, ",") != strings.Join(wantActions, ",") {
			t.Errorf("%s grants %v, want %v", st.Resource, st.Action, wantActions)
		}
	}
	for r := range want {
		if !seen[r] {
			t.Errorf("no statement grants the sweep on %s", r)
		}
	}
	if !stagingList {
		t.Error("the launch config sweep cannot list launch-config/ in the staging bucket")
	}
	// The ARN the reaper deletes matches a config object, and not a task's
	// bootstrap object beside it.
	if !iamGlobMatches(configsARN, "arn:aws:s3:::spawn-staging-111122223333-us-east-1/"+launchconfig.Key(strings.Repeat("a", 64))) {
		t.Errorf("%s does not match a launch config object", configsARN)
	}
	if iamGlobMatches(configsARN, "arn:aws:s3:::spawn-staging-111122223333-us-east-1/bootstrap/t1/bootstrap-"+strings.Repeat("a", 64)+".sh") {
		t.Errorf("%s matches a task bootstrap object", configsARN)
	}
	if LaunchConfigObjectARN != configsARN || StagingBucketARN != stagingARN {
		t.Errorf("LaunchConfigObjectARN/StagingBucketARN = %s/%s, derived %s/%s", LaunchConfigObjectARN, StagingBucketARN, configsARN, stagingARN)
	}
}
