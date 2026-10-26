package reaperiam

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestS3GrantsAreScopedToSpawnsTwoBucketFamilies: the empty-bucket sweep
// (#1301) can delete a bucket, so every s3: action the reaper holds is granted
// on spawn's staging and results bucket prefixes and nothing else — never "*",
// never another name. The prefixes are derived from the names spawn creates,
// not typed here. The SAM and cross-account copies are held to these
// statements by the template drift gates.
func TestS3GrantsAreScopedToSpawnsTwoBucketFamilies(t *testing.T) {
	want := map[string]bool{
		"arn:aws:s3:::" + taskproto.StagingBucketPrefix + "*":                                   true,
		"arn:aws:s3:::" + strings.TrimSuffix(taskproto.DefaultResultsBucket("", ""), "-") + "*": true,
	}
	seen := map[string]bool{}
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
		}
		wantActions := ResultsBucketSweepActions()
		if strings.Contains(st.Resource, taskproto.StagingBucketPrefix) {
			wantActions = StagingBucketSweepActions()
			// #1274: no role spawn makes may list the staging bucket or read
			// an object in it.
			for _, a := range st.Action {
				if strings.HasPrefix(a, "s3:List") || strings.HasPrefix(a, "s3:GetObject") || a == "s3:*" {
					t.Errorf("%s is granted on the staging bucket, which no spawn role may list or read", a)
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
}
