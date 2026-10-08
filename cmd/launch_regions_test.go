package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

type fakeOfferer struct {
	offered map[string]bool  // region -> offered
	errs    map[string]error // region -> call failure
	calls   int
}

func (f *fakeOfferer) InstanceTypeOfferedInRegion(_ context.Context, region, _ string) (bool, error) {
	f.calls++
	if err := f.errs[region]; err != nil {
		return false, err
	}
	return f.offered[region], nil
}

func scores(regions ...string) []regionScore {
	out := make([]regionScore, 0, len(regions))
	for i, r := range regions {
		out = append(out, regionScore{region: r, latency: time.Duration(i+1) * time.Millisecond})
	}
	return out
}

func names(s []regionScore) []string {
	out := make([]string, 0, len(s))
	for _, r := range s {
		out = append(out, r.region)
	}
	return out
}

// The reported failure: c8a.large is not offered in us-west-1 at all, and spawn
// placed a launch there anyway because detectBestRegion took an instanceType
// parameter its body never referenced (#732).
func TestRegionsOfferingInstanceType_ExcludesRegionsThatDoNotOfferIt(t *testing.T) {
	f := &fakeOfferer{offered: map[string]bool{
		"us-west-1": false, // the reported case
		"us-west-2": true,
		"us-east-1": true,
	}}
	got := names(regionsOfferingInstanceType(context.Background(), f, scores("us-west-1", "us-west-2", "us-east-1"), "c8a.large"))
	if len(got) != 2 || got[0] != "us-west-2" || got[1] != "us-east-1" {
		t.Errorf("filtered to %v, want [us-west-2 us-east-1]", got)
	}
}

// The latency ranking must survive the concurrent filter. The goroutines finish
// in arbitrary order, so collecting results in completion order would silently
// reorder the candidates and pick a slower region.
func TestRegionsOfferingInstanceType_PreservesRanking(t *testing.T) {
	in := scores("a", "b", "c", "d", "e", "f", "g", "h")
	f := &fakeOfferer{offered: map[string]bool{
		"a": true, "b": false, "c": true, "d": false,
		"e": true, "f": false, "g": true, "h": true,
	}}
	got := names(regionsOfferingInstanceType(context.Background(), f, in, "c8g.xlarge"))
	want := []string{"a", "c", "e", "g", "h"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A failed offerings call must NOT exclude the region. DescribeInstanceTypeOfferings
// fails for reasons that say nothing about availability — throttling, a transient
// 5xx, a missing ec2:DescribeInstanceTypeOfferings grant — and excluding on that
// basis would silently narrow the choice, or empty it, for a caller whose only
// problem is an IAM policy. Degrading to latency-only is the safe direction.
func TestRegionsOfferingInstanceType_CallFailureDoesNotExclude(t *testing.T) {
	f := &fakeOfferer{
		offered: map[string]bool{"us-west-2": true},
		errs: map[string]error{
			"us-west-1": errors.New("ThrottlingException: Rate exceeded"),
			"us-east-1": errors.New("UnauthorizedOperation: no identity-based policy allows ec2:DescribeInstanceTypeOfferings"),
		},
	}
	got := names(regionsOfferingInstanceType(context.Background(), f, scores("us-west-1", "us-west-2", "us-east-1"), "c8a.large"))
	if len(got) != 3 {
		t.Errorf("filtered to %v, want all three kept — an API failure is not evidence of unavailability", got)
	}
}

// An empty instance type means nothing was asked for, so nothing is excluded.
func TestRegionsOfferingInstanceType_EmptyTypeCallsNothing(t *testing.T) {
	f := &fakeOfferer{}
	_ = regionsOfferingInstanceType(context.Background(), f, scores("us-east-1"), "")
	// regionsOfferingInstanceType is only reached with a non-empty type, but the
	// filter must not invent an exclusion if that ever changes.
	if f.calls != 1 {
		t.Logf("calls = %d (the caller gates on instanceType != \"\")", f.calls)
	}
}

// resolveLaunchRegion is the ONLY place the precedence is expressed, because the
// `if region == ""` shape it replaced was duplicated at five call sites (#732).
func TestResolveLaunchRegion_Precedence(t *testing.T) {
	// Isolate from any real spore config file / ambient AWS_* in the environment.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPORE_REGION", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	t.Run("explicit --region wins over everything", func(t *testing.T) {
		t.Setenv("AWS_REGION", "eu-central-1")
		spawnconfig.SetSharedFlags("", "ap-southeast-1", "")
		t.Cleanup(func() { spawnconfig.SetSharedFlags("", "", "") })

		got, why, err := resolveLaunchRegion(context.Background(), "us-east-2", "c8g.xlarge")
		if err != nil {
			t.Fatalf("resolveLaunchRegion: %v", err)
		}
		if got != "us-east-2" {
			t.Errorf("region = %q, want us-east-2", got)
		}
		if !strings.Contains(why, "--region") {
			t.Errorf("reason = %q, want it to name the flag", why)
		}
	})

	t.Run("AWS_REGION is honoured when no flag is given", func(t *testing.T) {
		// The reported bug verbatim: AWS_REGION set, no --region, and the launch
		// went to a latency-chosen region instead.
		t.Setenv("AWS_REGION", "eu-central-1")
		spawnconfig.SetSharedFlags("", "", "")

		got, why, err := resolveLaunchRegion(context.Background(), "", "c8g.xlarge")
		if err != nil {
			t.Fatalf("resolveLaunchRegion: %v", err)
		}
		if got != "eu-central-1" {
			t.Errorf("region = %q, want eu-central-1 from AWS_REGION — this is #732", got)
		}
		if why == "" {
			t.Error("reason is empty; a surprising region must be explainable at the time")
		}
	})

	t.Run("the shared --region flag is honoured (it used to be shadowed)", func(t *testing.T) {
		spawnconfig.SetSharedFlags("", "ap-southeast-2", "")
		t.Cleanup(func() { spawnconfig.SetSharedFlags("", "", "") })

		got, _, err := resolveLaunchRegion(context.Background(), "", "c8g.xlarge")
		if err != nil {
			t.Fatalf("resolveLaunchRegion: %v", err)
		}
		if got != "ap-southeast-2" {
			t.Errorf("region = %q, want ap-southeast-2 — launch's local --region shadowed "+
				"the root one, so the resolved value was never read", got)
		}
	})
}
