package main

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// #990: an instance launched with `--ttl 1d` by a spawn that predates the fix
// carries spawn:ttl=1d and no spawn:ttl-deadline. spored enforces launch + 1d
// there (pkg/agent synthesizes the deadline). The reaper must agree, or a
// stopped instance — no spored running — lives until the 7-day ceiling.
func TestEvaluate_TTLWithoutDeadline_UsesLaunchPlusTTL(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, ttl := range []string{"1d", "24h", "1d0h", "24h0m0s"} {
		inst := ec2types.Instance{
			InstanceId: aws.String("i-1d"),
			Tags: []ec2types.Tag{
				tag("spawn:managed", "true"),
				tag("spawn:ttl", ttl),
				tag("spawn:launch-time", now.Add(-25*time.Hour).Format(time.RFC3339)),
			},
		}
		c, expired := newReaper().evaluate(inst, "us-east-1", now)
		if !expired {
			t.Errorf("spawn:ttl=%s, launched 25h ago, no deadline tag: spared; spored would have terminated it 1h ago", ttl)
			continue
		}
		if c.reason != "ttl" {
			t.Errorf("spawn:ttl=%s: reason %q, want ttl", ttl, c.reason)
		}
		if want := now.Add(-time.Hour).Format(time.RFC3339); c.deadline != want {
			t.Errorf("spawn:ttl=%s: deadline %s, want %s", ttl, c.deadline, want)
		}
	}
}

func TestEvaluate_TTLWithoutDeadline_SparedBeforeIt(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-2d"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl", "2d"),
			tag("spawn:launch-time", now.Add(-25*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, expired := newReaper().evaluate(inst, "us-east-1", now); expired {
		t.Fatal("spawn:ttl=2d, launched 25h ago: reaped a day early")
	}
}

// spawn:ttl=0 with the deadline removed is `spored config set ttl 0`: TTL
// disabled. Only the ceiling applies, exactly as before.
func TestEvaluate_TTLZeroIsDisabled(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-zero"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl", "0"),
			tag("spawn:launch-time", now.Add(-25*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, expired := newReaper().evaluate(inst, "us-east-1", now); expired {
		t.Fatal("spawn:ttl=0 (disabled) was reaped before the max-age ceiling")
	}
}

// A valid deadline stays authoritative: spawn:ttl does not override it, in
// either direction.
func TestEvaluate_DeadlineBeatsTTLTag(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-extended"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl", "1h"), // stale label
			tag("spawn:ttl-deadline", now.Add(time.Hour).Format(time.RFC3339)),
			tag("spawn:launch-time", now.Add(-5*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, expired := newReaper().evaluate(inst, "us-east-1", now); expired {
		t.Fatal("a future spawn:ttl-deadline was overridden by spawn:ttl")
	}
}

func TestParseMaxAgeAcceptsDays(t *testing.T) {
	if got := parseMaxAge("3d"); got != 72*time.Hour {
		t.Errorf("parseMaxAge(3d) = %v, want 72h", got)
	}
	if got := parseMaxAge("1w"); got != defaultMaxAge {
		t.Errorf("parseMaxAge(1w) = %v, want the default", got)
	}
}
