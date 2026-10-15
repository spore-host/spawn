package aws

import (
	"testing"
	"time"
)

// TestBuildTags_BurstMemberIsReaperVisibleAndBounded is the pkg/aws half of
// #917. `spawn burst` now builds each member as a LaunchConfig like this one and
// launches it through Client.Launch, so these are the tags a burst member
// carries. They must put it in scope for every cost backstop: spawn:managed=true
// (the TTL reaper's scan filter, `spawn list`, and the condition on the spored
// role's terminate grant), a TTL with a computed deadline (what the reaper
// enforces), its own job-array index, and the burst-only spawn:burst tag that
// burst supplies as a custom tag (#904).
func TestBuildTags_BurstMemberIsReaperVisibleAndBounded(t *testing.T) {
	cfg := LaunchConfig{
		Name:          "spawn-burst-genomics-2",
		TTL:           "8h",
		OnComplete:    "terminate",
		JobArrayID:    "genomics",
		JobArrayName:  "genomics",
		JobArraySize:  4,
		JobArrayIndex: 2,
		Tags:          map[string]string{"spawn:burst": "true"},
	}
	before := time.Now()
	tags := buildTags(cfg, "123456789012", "arn:aws:iam::123456789012:user/test", "")

	for key, want := range map[string]string{
		"spawn:managed":         "true",
		"spawn:ttl":             "8h",
		"spawn:on-complete":     "terminate",
		"spawn:job-array-id":    "genomics",
		"spawn:job-array-name":  "genomics",
		"spawn:job-array-size":  "4",
		"spawn:job-array-index": "2",
		"spawn:burst":           "true",
	} {
		if got := findTagValue(tags, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	dl, err := time.Parse(time.RFC3339, findTagValue(tags, "spawn:ttl-deadline"))
	if err != nil {
		t.Fatalf("spawn:ttl-deadline missing or unparseable — the reaper has no deadline to enforce: %v", err)
	}
	if lo, hi := before.Add(8*time.Hour-time.Minute), time.Now().Add(8*time.Hour+time.Minute); dl.Before(lo) || dl.After(hi) {
		t.Errorf("spawn:ttl-deadline = %s, want about 8h from launch", dl)
	}
	if findTagValue(tags, "spawn:launch-time") == "" {
		t.Error("spawn:launch-time missing — the reaper's max-age ceiling anchors on it")
	}
}
