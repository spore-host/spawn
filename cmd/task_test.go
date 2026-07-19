package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestPricePerHour(t *testing.T) {
	if got := pricePerHour(&aws.InstanceInfo{Tags: map[string]string{"spawn:price-per-hour": "0.7680"}}); got != 0.768 {
		t.Errorf("pricePerHour = %v, want 0.768", got)
	}
	if got := pricePerHour(&aws.InstanceInfo{Tags: map[string]string{}}); got != 0 {
		t.Errorf("pricePerHour with no tag = %v, want 0", got)
	}
	if got := pricePerHour(&aws.InstanceInfo{Tags: map[string]string{"spawn:price-per-hour": "junk"}}); got != 0 {
		t.Errorf("pricePerHour with bad tag = %v, want 0", got)
	}
}

func TestEstimateInstanceCost(t *testing.T) {
	inst := &aws.InstanceInfo{Tags: map[string]string{"spawn:price-per-hour": "1.00"}}
	if got := estimateInstanceCost(inst, 2*time.Hour); got != 2.0 {
		t.Errorf("estimateInstanceCost(2h @ $1) = %v, want 2.0", got)
	}
	// Unknown rate → 0 (never fabricate a number).
	if got := estimateInstanceCost(&aws.InstanceInfo{Tags: map[string]string{}}, 5*time.Hour); got != 0 {
		t.Errorf("estimateInstanceCost with no rate = %v, want 0", got)
	}
	// Non-positive age → 0.
	if got := estimateInstanceCost(inst, -time.Hour); got != 0 {
		t.Errorf("estimateInstanceCost with negative age = %v, want 0", got)
	}
}

func TestLikelyCause(t *testing.T) {
	cases := []struct {
		state string
		spot  bool
		want  string // substring, or "" for no cause
	}{
		{"running", false, ""},
		{"terminated", false, "TTL elapsed"},
		{"terminated", true, "Spot interruption"},
		{"stopped", false, "idle-timeout"},
		{"pending", false, ""},
	}
	for _, tc := range cases {
		got := likelyCause(&aws.InstanceInfo{State: tc.state, SpotInstance: tc.spot})
		if tc.want == "" {
			if got != "" {
				t.Errorf("likelyCause(%s, spot=%v) = %q, want empty", tc.state, tc.spot, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("likelyCause(%s, spot=%v) = %q, want substring %q", tc.state, tc.spot, got, tc.want)
		}
	}
}

func TestCliName(t *testing.T) {
	if got := cliName(&aws.InstanceInfo{Name: "my-job", InstanceID: "i-123"}); got != "my-job" {
		t.Errorf("cliName preferring name = %q, want my-job", got)
	}
	if got := cliName(&aws.InstanceInfo{InstanceID: "i-123"}); got != "i-123" {
		t.Errorf("cliName falling back to id = %q, want i-123", got)
	}
}
