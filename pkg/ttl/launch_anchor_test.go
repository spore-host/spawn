package ttl

import (
	"testing"
	"time"
)

func TestLaunchAnchor(t *testing.T) {
	first := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	restarted := first.Add(72 * time.Hour)
	for _, tc := range []struct {
		name string
		tag  string
		ec2  time.Time
		want time.Time
	}{
		{"restarted: the tag is earlier and wins", first.Format(time.RFC3339), restarted, first},
		{"forged later tag: EC2 is earlier and wins (GHSA-c93q)", restarted.Format(time.RFC3339), first, first},
		{"no tag: EC2", "", restarted, restarted},
		{"unparseable tag: EC2", "garbage", restarted, restarted},
		{"tag only", first.Format(time.RFC3339), time.Time{}, first},
		{"neither", "", time.Time{}, time.Time{}},
	} {
		if got := LaunchAnchor(tc.tag, tc.ec2); !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
