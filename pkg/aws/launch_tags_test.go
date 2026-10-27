package aws

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// anchorConfigs are launches whose tags carry each kind of launch instant: a
// TTL deadline, a persistent Spot request's end date with and without a TTL,
// and a job array's creation time.
func anchorConfigs() map[string]LaunchConfig {
	sweep := LaunchConfig{
		Name: "sw-1", InstanceType: "t3.micro", Region: "us-east-1", TargetOS: "linux",
		SweepID: "sw", SweepName: "sw", SweepIndex: 1, SweepSize: 2,
		Parameters: map[string]string{"alpha": "0.1"}, JobArrayCommand: "python train.py",
	}
	withTTL := sweep
	withTTL.TTL = "1d"
	idle := sweep
	idle.IdleTimeout = "1h"
	spotIdle := idle
	spotIdle.Spot = true
	spotTTL := spotIdle
	spotTTL.TTL = "3h"
	array := withTTL
	array.JobArrayID, array.JobArrayName, array.JobArraySize = "ja", "ja", 2
	return map[string]LaunchConfig{
		"ttl": withTTL, "idle only": idle, "persistent spot, no ttl": spotIdle,
		"persistent spot, ttl": spotTTL, "job array": array,
	}
}

func renderAt(t *testing.T, lc LaunchConfig, at time.Time) []types.Tag {
	t.Helper()
	prev := tagClock
	tagClock = func() time.Time { return at }
	defer func() { tagClock = prev }()
	tags, err := BuildLaunchTags(lc, "111122223333", "arn:aws:iam::111122223333:user/u", "")
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

// TestAnchorLaunchTagsMovesEveryInstant is #1330's anchoring gate. A detached
// sweep member's tags are rendered by the CLI and launched later by the sweep
// orchestrator, which re-anchors them with AnchorLaunchTags. Rendered at T0 and
// anchored at T1, they must be exactly what buildTags writes for a launch at T1:
// otherwise the member's TTL counts from when the CLI ran, not from when it
// started. Which tags depend on the launch instant is discovered by rendering
// at two instants, not listed, so a new one buildTags adds fails here until
// AnchorLaunchTags moves it too.
func TestAnchorLaunchTagsMovesEveryInstant(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(50 * time.Hour)
	moved := map[string]bool{}
	for name, lc := range anchorConfigs() {
		at0, at1 := renderAt(t, lc, t0), renderAt(t, lc, t1)
		m0, m1 := tagMap(at0), tagMap(at1)
		for k := range m1 {
			if m0[k] != m1[k] {
				moved[k] = true
			}
		}
		got, err := AnchorLaunchTags(at0, t1)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(tagMap(got), m1) {
			t.Errorf("%s: rendered at %s and anchored at %s:\n  %v\nwant buildTags at %s:\n  %v", name, t0, t1, tagMap(got), t1, m1)
		}
		if len(got) != len(at1) {
			t.Errorf("%s: %d tags, want %d (a repeated key fails RunInstances)", name, len(got), len(at1))
		}
		// Anchoring at the instant they were rendered changes nothing.
		same, err := AnchorLaunchTags(at0, t0)
		if err != nil || !reflect.DeepEqual(same, at0) {
			t.Errorf("%s: anchoring at the render instant changed the tags: %v (err %v)", name, tagMap(same), err)
		}
	}
	if len(moved) == 0 {
		t.Fatal("no tag depends on the launch instant; the discovery is broken")
	}
	for _, k := range []string{"spawn:launch-time", "spawn:ttl-deadline"} {
		if !moved[k] {
			t.Errorf("%s did not move between the two renders; the fixture no longer covers it", k)
		}
	}
}

// A spawn:ttl the deadline cannot be derived from is an error, not a launch
// with no deadline (#990).
func TestAnchorLaunchTagsRefusesAnUnparseableTTL(t *testing.T) {
	_, err := AnchorLaunchTags([]types.Tag{{Key: aws.String("spawn:ttl"), Value: aws.String("tomorrow")}}, time.Now())
	if err == nil {
		t.Fatal("anchored a spawn:ttl that has no deadline")
	}
}

// countingPricer prices every type at rate (0: no price available) and counts
// the lookups.
type countingPricer struct {
	rate  float64
	calls int
}

func (p *countingPricer) OnDemandPriceWithSource(_ context.Context, _, _ string) (float64, truffleaws.PriceSource, error) {
	p.calls++
	if p.rate == 0 {
		return 0, "", errors.New("no price")
	}
	return p.rate, truffleaws.PriceSourceLive, nil
}

// A LaunchTagger prices each (region, type, OS) once across a sweep's members,
// and writes the same spawn:price-per-hour a lookup per member would. A missing
// price is reused only where Launch would accept one: a member with a cost limit
// is looked up again and refused, never tagged with no price to enforce it by.
func TestLaunchTaggerPricesEachShapeOnce(t *testing.T) {
	ctx := context.Background()
	p := &countingPricer{rate: 0.0416}
	tg := &LaunchTagger{c: &Client{}, accountID: "111122223333", userARN: "u", pricer: p, prices: map[string]resolvedPrice{}}
	lc := LaunchConfig{InstanceType: "t3.medium", Region: "us-east-1", TargetOS: "linux", TTL: "1h", CostLimit: 5}
	for i := 0; i < 3; i++ {
		tags, err := tg.Tags(ctx, lc)
		if err != nil {
			t.Fatal(err)
		}
		if got := tagMap(tags)["spawn:price-per-hour"]; got != "0.041600" {
			t.Errorf("member %d: spawn:price-per-hour = %q, want 0.041600", i, got)
		}
	}
	other := lc
	other.InstanceType = "t3.large"
	if _, err := tg.Tags(ctx, other); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Errorf("%d price lookups for 4 members of 2 shapes, want 2", p.calls)
	}

	none := &countingPricer{}
	tg = &LaunchTagger{c: &Client{}, pricer: none, prices: map[string]resolvedPrice{}}
	free := LaunchConfig{InstanceType: "x9.huge", Region: "us-east-1", TargetOS: "linux", TTL: "1h"}
	for i := 0; i < 2; i++ {
		if _, err := tg.Tags(ctx, free); err != nil {
			t.Fatalf("no price and no cost limit is a warning, not an error: %v", err)
		}
	}
	if none.calls != 1 {
		t.Errorf("%d lookups of an unpriceable shape for 2 members with no cost limit, want 1", none.calls)
	}
	capped := free
	capped.CostLimit = 5
	if _, err := tg.Tags(ctx, capped); err == nil {
		t.Error("a member with a cost limit was tagged with no price to enforce it by")
	}
}
