package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #578: `spawn cost <single-instance>` must report the instance's own compute
// cost (on-demand rate × runtime, from the spawn:price-per-hour tag — the same
// data `spawn status` uses) rather than leaking a raw DynamoDB
// ResourceNotFoundException from an absent sweep record.
func TestSingleInstanceCostReport(t *testing.T) {
	t.Run("prices from the price-per-hour tag × runtime", func(t *testing.T) {
		inst := &aws.InstanceInfo{
			Name:         "lith-devbox",
			InstanceID:   "i-0abc123",
			InstanceType: "m5.large",
			State:        "running",
			Tags:         map[string]string{"spawn:price-per-hour": "0.0960"},
		}
		out := singleInstanceCostReport(inst, 2*time.Hour)

		// 0.0960/hr × 2h = $0.192 → "$0.19".
		for _, want := range []string{"lith-devbox", "i-0abc123", "m5.large", "$0.0960/hr", "Est. compute:", "$0.19"} {
			if !strings.Contains(out, want) {
				t.Errorf("report missing %q; got:\n%s", want, out)
			}
		}
		if strings.Contains(out, "ResourceNotFound") {
			t.Errorf("report leaked a raw DynamoDB error:\n%s", out)
		}
	})

	t.Run("no price-per-hour tag yields a clear note, not a raw error", func(t *testing.T) {
		inst := &aws.InstanceInfo{
			Name:         "untagged",
			InstanceID:   "i-1",
			InstanceType: "t3.micro",
			State:        "stopped",
			Tags:         map[string]string{},
		}
		out := singleInstanceCostReport(inst, time.Hour)
		if !strings.Contains(out, "cannot be estimated") {
			t.Errorf("expected a clear cannot-be-estimated note; got:\n%s", out)
		}
		if strings.Contains(out, "ResourceNotFound") {
			t.Errorf("report leaked a raw DynamoDB error:\n%s", out)
		}
	})
}
