package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestJobArraySummaryPrintsOneConnectHint is spawn#897 (3).
//
// The summary used to print a raw `ssh -i <key> ec2-user@<ip>` line per member:
// eight lines for --count 8, the local key path in every one, and a path that
// bypasses `spawn connect` (key lookup, SSM fallback). The issue asks for a
// single `spawn connect arr-<index>` line. resolveInstance matches the Name tag,
// so the hint must name members the way formatInstanceName names them.
func TestJobArraySummaryPrintsOneConnectHint(t *testing.T) {
	var members []*aws.LaunchResult
	for i := 0; i < 8; i++ {
		members = append(members, &aws.LaunchResult{
			InstanceID: fmt.Sprintf("i-%017d", i),
			Name:       formatInstanceName("", "arr", i),
			PublicIP:   fmt.Sprintf("54.191.92.%d", 80+i),
		})
	}
	mp := memberParams{name: "arr", size: 8}
	out := captureStderr(t, func() {
		if err := renderJobArrayResult(members, &aws.LaunchConfig{Region: "us-west-2"}, mp, "arr-id", time.Now(), nil); err != nil {
			t.Errorf("renderJobArrayResult: %v", err)
		}
	})

	if strings.Contains(out, "ssh -i") || strings.Contains(out, "ec2-user@") {
		t.Errorf("summary still prints raw ssh commands (#897):\n%s", out)
	}
	if n := strings.Count(out, "spawn connect "); n != 2 { // the pattern and its one example, same line
		t.Errorf("want exactly one connect line (pattern + example), found %d occurrences of `spawn connect`:\n%s", n, out)
	}
	if !strings.Contains(out, "spawn connect arr-<index>") {
		t.Errorf("connect hint does not name members as arr-<index>:\n%s", out)
	}
	if !strings.Contains(out, "spawn connect arr-0") {
		t.Errorf("connect hint has no concrete example:\n%s", out)
	}
}

// The hint must follow a custom --instance-names template, and must not
// suggest a name that resolves to several members.
func TestJobArrayConnectHintFollowsTemplate(t *testing.T) {
	ex := &aws.LaunchResult{InstanceID: "i-0abc", Name: formatInstanceName("worker-{index}", "arr", 0)}
	if got := jobArrayConnectHint("worker-{index}", "arr", ex); !strings.HasPrefix(got, "spawn connect worker-<index>") ||
		!strings.Contains(got, "spawn connect worker-0") {
		t.Errorf("custom template: got %q", got)
	}

	// No {index}: every member carries the same Name, which resolveInstance
	// cannot disambiguate — point at the instance ID instead.
	same := &aws.LaunchResult{InstanceID: "i-0abc", Name: formatInstanceName("{job-array-name}", "arr", 0)}
	got := jobArrayConnectHint("{job-array-name}", "arr", same)
	if strings.Contains(got, "spawn connect arr ") || !strings.Contains(got, "i-0abc") {
		t.Errorf("index-less template should fall back to the instance ID: got %q", got)
	}
}
