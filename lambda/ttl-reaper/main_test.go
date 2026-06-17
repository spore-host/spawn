package main

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

func init() { tagprefix.Init() }

func tag(k, v string) ec2types.Tag { return ec2types.Tag{Key: aws.String(k), Value: aws.String(v)} }

func newReaper() *reaper { return &reaper{maxAge: 7 * 24 * time.Hour} }

func TestEvaluate_PastDeadline_Reaped(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-past"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("Name", "old-job"),
			tag("spawn:ttl-deadline", now.Add(-30*time.Minute).Format(time.RFC3339)),
		},
	}
	c, expired := newReaper().evaluate(inst, "us-east-1", now)
	if !expired {
		t.Fatal("instance past its ttl-deadline must be reaped")
	}
	if c.reason != "ttl-deadline" {
		t.Errorf("reason = %q, want ttl-deadline", c.reason)
	}
	if c.name != "old-job" {
		t.Errorf("name = %q, want old-job", c.name)
	}
}

func TestEvaluate_WithinDeadline_Spared(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-fresh"),
		// launch-time far in the past, but deadline is in the FUTURE: the
		// honored deadline must win and max-age must NOT also fire.
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl-deadline", now.Add(2*time.Hour).Format(time.RFC3339)),
			tag("spawn:launch-time", now.Add(-30*24*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, expired := newReaper().evaluate(inst, "us-east-1", now); expired {
		t.Fatal("instance within its ttl-deadline must be spared even if launch-time is old")
	}
}

func TestEvaluate_NoDeadline_MaxAgeCeiling(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	// --no-timeout style: no ttl-deadline tag, but launch-time exceeds max-age.
	inst := ec2types.Instance{
		InstanceId: aws.String("i-nodeadline"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:launch-time", now.Add(-8*24*time.Hour).Format(time.RFC3339)),
		},
	}
	c, expired := newReaper().evaluate(inst, "us-east-1", now)
	if !expired {
		t.Fatal("instance older than max-age must be reaped even with no deadline")
	}
	if c.reason != "max-age" {
		t.Errorf("reason = %q, want max-age", c.reason)
	}
}

func TestEvaluate_NoDeadline_YoungerThanMaxAge_Spared(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-young"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:launch-time", now.Add(-1*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, expired := newReaper().evaluate(inst, "us-east-1", now); expired {
		t.Fatal("young instance with no deadline must be spared")
	}
}

func TestEvaluate_UnparseableDeadline_FallsBackToMaxAge(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-garbage"),
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl-deadline", "not-a-timestamp"),
			tag("spawn:launch-time", now.Add(-10*24*time.Hour).Format(time.RFC3339)),
		},
	}
	c, expired := newReaper().evaluate(inst, "us-east-1", now)
	if !expired {
		t.Fatal("unparseable deadline must fall back to max-age ceiling")
	}
	if c.reason != "max-age" {
		t.Errorf("reason = %q, want max-age", c.reason)
	}
}

func TestEvaluate_FallsBackToAPILaunchTimeWhenTagMissing(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	apiLaunch := now.Add(-9 * 24 * time.Hour)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-notag"),
		LaunchTime: &apiLaunch,
		Tags:       []ec2types.Tag{tag("spawn:managed", "true")},
	}
	c, expired := newReaper().evaluate(inst, "us-east-1", now)
	if !expired || c.reason != "max-age" {
		t.Fatalf("must reap via API LaunchTime fallback; got expired=%t reason=%q", expired, c.reason)
	}
}

func TestParseRegions(t *testing.T) {
	if got := parseRegions(""); len(got) != len(defaultRegions) {
		t.Errorf("empty should yield defaults (%d), got %d", len(defaultRegions), len(got))
	}
	got := parseRegions("us-east-1, eu-west-1 ,")
	if len(got) != 2 || got[0] != "us-east-1" || got[1] != "eu-west-1" {
		t.Errorf("parseRegions trimming failed: %v", got)
	}
}

func TestAccountIDFromRoleARN(t *testing.T) {
	got := accountIDFromRoleARN("arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2")
	if got != "435415984226" {
		t.Errorf("account id = %q, want 435415984226", got)
	}
	// Non-ARN input returns the input unchanged (best-effort label).
	if got := accountIDFromRoleARN("weird"); got != "weird" {
		t.Errorf("non-ARN should pass through, got %q", got)
	}
}

func TestParseListAndDedup(t *testing.T) {
	got := parseList("a, b ,, a ")
	if len(got) != 3 { // parseList keeps dupes; dedup removes them
		t.Fatalf("parseList = %v, want 3 items", got)
	}
	d := dedup(got)
	if len(d) != 2 || d[0] != "a" || d[1] != "b" {
		t.Errorf("dedup = %v, want [a b]", d)
	}
}

func TestParseMaxAge(t *testing.T) {
	if got := parseMaxAge(""); got != defaultMaxAge {
		t.Errorf("empty should be default %s, got %s", defaultMaxAge, got)
	}
	if got := parseMaxAge("2h"); got != 2*time.Hour {
		t.Errorf("2h parse failed: %s", got)
	}
	if got := parseMaxAge("garbage"); got != defaultMaxAge {
		t.Errorf("garbage should fall back to default, got %s", got)
	}
}

func TestEvaluate_PopulatesGracefulFields(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-graceful"),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl-deadline", now.Add(-1*time.Minute).Format(time.RFC3339)),
			tag("spawn:pre-stop", "aws s3 sync ~/out s3://b/"),
			tag("spawn:pre-stop-timeout", "3m"),
			tag("spawn:local-username", "ec2-user"),
		},
	}
	c, expired := newReaper().evaluate(inst, "us-east-1", now)
	if !expired {
		t.Fatal("expired instance should be a candidate")
	}
	if !c.running {
		t.Error("running flag not set from InstanceState")
	}
	if c.preStop != "aws s3 sync ~/out s3://b/" {
		t.Errorf("preStop = %q", c.preStop)
	}
	if c.preStopTimeout != 3*time.Minute {
		t.Errorf("preStopTimeout = %v, want 3m", c.preStopTimeout)
	}
	if c.localUsername != "ec2-user" {
		t.Errorf("localUsername = %q, want ec2-user", c.localUsername)
	}
}

func TestEvaluate_StoppedInstanceNotRunning(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId: aws.String("i-stopped"),
		State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		Tags: []ec2types.Tag{
			tag("spawn:managed", "true"),
			tag("spawn:ttl-deadline", now.Add(-1*time.Minute).Format(time.RFC3339)),
			tag("spawn:pre-stop", "echo hi"),
		},
	}
	c, _ := newReaper().evaluate(inst, "us-east-1", now)
	if c.running {
		t.Error("stopped instance must not be marked running (graceful tier skips it — SSM RunCommand needs a running instance)")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"aws s3 sync ~/o s3://b/": `'aws s3 sync ~/o s3://b/'`,
		"it's a test":             `'it'\''s a test'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func fsxTag(k, v string) fsxtypes.Tag { return fsxtypes.Tag{Key: aws.String(k), Value: aws.String(v)} }

func TestEvaluateFSx_PastDeadline_Expired(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-past"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:ttl-deadline", now.Add(-1*time.Hour).Format(time.RFC3339)),
		},
	}
	id, reason, _, expired := newReaper().evaluateFSx(fsItem, now)
	if !expired || reason != "ttl-deadline" || id != "fs-past" {
		t.Errorf("got id=%q reason=%q expired=%v, want fs-past/ttl-deadline/true", id, reason, expired)
	}
}

func TestEvaluateFSx_WithinDeadline_Spared(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-future"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:ttl-deadline", now.Add(24*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, _, _, expired := newReaper().evaluateFSx(fsItem, now); expired {
		t.Error("filesystem within its deadline must not be expired")
	}
}

func TestEvaluateFSx_NotManaged_Ignored(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-foreign"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags:         []fsxtypes.Tag{fsxTag("spawn:ttl-deadline", now.Add(-1*time.Hour).Format(time.RFC3339))},
	}
	if _, _, _, expired := newReaper().evaluateFSx(fsItem, now); expired {
		t.Error("non-spawn-managed filesystem must never be reaped, even past a deadline tag")
	}
}

func TestEvaluateFSx_MaxAgeFallback(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-old"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			// no ttl-deadline → max-age (default 7d) from spawn:fsx-created
			fsxTag("spawn:fsx-created", now.Add(-8*24*time.Hour).Format(time.RFC3339)),
		},
	}
	_, reason, _, expired := newReaper().evaluateFSx(fsItem, now)
	if !expired || reason != "max-age" {
		t.Errorf("8-day-old FS with no deadline should expire via max-age, got reason=%q expired=%v", reason, expired)
	}
}

func TestEvaluateFSx_NotAvailable_Skipped(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-creating"),
		Lifecycle:    fsxtypes.FileSystemLifecycleCreating, // still provisioning
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:ttl-deadline", now.Add(-1*time.Hour).Format(time.RFC3339)),
		},
	}
	if _, _, _, expired := newReaper().evaluateFSx(fsItem, now); expired {
		t.Error("a CREATING filesystem must not be reaped (only AVAILABLE)")
	}
}

// TestEvaluateFSx_EphemeralOrphan_PastGrace is the #210 safety net: an ephemeral
// FSx (no ttl-deadline) older than the orphan grace is eligible — covers the
// "launch never succeeded, no instance ever owned it" case. The refcount check in
// reapFSxRegion still gates the actual delete.
func TestEvaluateFSx_EphemeralOrphan_PastGrace(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-orphan"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:fsx-lifecycle", "ephemeral"),
			fsxTag("spawn:fsx-created", now.Add(-(ephemeralOrphanGrace + time.Minute)).Format(time.RFC3339)),
		},
	}
	_, reason, _, expired := newReaper().evaluateFSx(fsItem, now)
	if !expired || reason != "ephemeral-orphan" {
		t.Errorf("ephemeral FS past orphan grace should expire as ephemeral-orphan, got reason=%q expired=%v", reason, expired)
	}
}

// TestEvaluateFSx_EphemeralOrphan_WithinGrace spares a freshly-created ephemeral
// FS (its instance may still be launching / spored still mounting). The default
// 7d max-age is far away, so it must NOT be expired within the grace window.
func TestEvaluateFSx_EphemeralOrphan_WithinGrace(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-fresh"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:fsx-lifecycle", "ephemeral"),
			fsxTag("spawn:fsx-created", now.Add(-2*time.Minute).Format(time.RFC3339)),
		},
	}
	if _, _, _, expired := newReaper().evaluateFSx(fsItem, now); expired {
		t.Error("an ephemeral FS within the orphan grace must not be reaped (instance may still be launching/mounting)")
	}
}

// TestEvaluateFSx_DurableNoOrphanReap confirms the orphan net is ephemeral-only:
// a durable FS with no deadline relies on max-age, not the short orphan grace.
func TestEvaluateFSx_DurableNoOrphanReap(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	fsItem := fsxtypes.FileSystem{
		FileSystemId: aws.String("fs-durable"),
		Lifecycle:    fsxtypes.FileSystemLifecycleAvailable,
		Tags: []fsxtypes.Tag{
			fsxTag("spawn:managed", "true"),
			fsxTag("spawn:fsx-lifecycle", "durable"),
			fsxTag("spawn:fsx-created", now.Add(-(ephemeralOrphanGrace + time.Hour)).Format(time.RFC3339)),
		},
	}
	if _, _, _, expired := newReaper().evaluateFSx(fsItem, now); expired {
		t.Error("a durable FS must not be reaped via the ephemeral orphan grace (only max-age/deadline applies)")
	}
}
