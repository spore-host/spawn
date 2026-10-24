package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func extendInstance(launch time.Time, tags map[string]string) *aws.InstanceInfo {
	return &aws.InstanceInfo{InstanceID: "i-test", LaunchTime: launch, Tags: tags}
}

// TestExtendKeepsTheCostLimitByDefault is spawn#1262 with the reporter's
// numbers: an on-demand c8gn.48xlarge at $11.376/hr, launched with --ttl 2h
// --cost-limit 25, extended by 1h at 47 minutes. #639's default raised the cap
// to cover the new deadline, so it never fired and the instances ran to about
// $29 each. Now the cap is kept, and the decision says when it fires (about
// 132 minutes after launch) and what raising it would take.
func TestExtendKeepsTheCostLimitByDefault(t *testing.T) {
	launch := time.Date(2026, 10, 20, 21, 14, 0, 0, time.UTC)
	now := launch.Add(47 * time.Minute)
	inst := extendInstance(launch, map[string]string{
		"spawn:cost-limit":      "25.0000",
		"spawn:price-per-hour":  "11.376000",
		"spawn:compute-seconds": "2820", // 47 minutes
	})
	d := decideCostLimit(inst, launch.Add(3*time.Hour), now, 0, false)

	if d.Changed() {
		t.Fatalf("the cap was moved from $%.2f to $%.2f without being asked", d.Current, d.NewValue)
	}
	if !d.Binds {
		t.Fatal("a $25 cap at $11.376/hr fires about 132m in, before the 3h deadline: Binds must be set")
	}
	// spored counts compute-seconds plus up to computeTagLag, so the estimate may
	// come up to 5 minutes early; it must never come late.
	fires := d.FiresAt.Sub(launch)
	if fires < 126*time.Minute || fires > 132*time.Minute {
		t.Errorf("FiresAt = launch+%v, want about launch+132m (never later)", fires)
	}
	out := renderCostLimitDecision(d)
	for _, want := range []string{"⚠️", "25.0000", "is kept", "will terminate this instance", "spawn extend i-test 1m --cost-limit 34.", "--raise-cost-limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning is missing %q:\n%s", want, out)
		}
	}
}

// TestDecideCostLimitRaisesWhenAsked is spawn#639's case with --raise-cost-limit:
// a 60m TTL capped at $0.65 on a $0.63808/hr instance, extended by 45m. The cap
// is raised to what 105 minutes costs, and the reason names the flag.
func TestDecideCostLimitRaisesWhenAsked(t *testing.T) {
	launch := time.Date(2026, 10, 3, 3, 14, 0, 0, time.UTC)
	newDeadline := launch.Add(105 * time.Minute)

	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit":     "0.6500",
		"spawn:price-per-hour": "0.638080",
	}), newDeadline, launch.Add(60*time.Minute), 0, true)

	if !d.Changed() {
		t.Fatalf("--raise-cost-limit must raise the cap: $%.4f covers only ~61m but the new deadline is 105m away", d.Current)
	}
	// 105 minutes at $0.63808/hr = $1.1166.
	if d.NewValue < 1.10 || d.NewValue > 1.13 {
		t.Errorf("NewValue = %.4f, want ~1.1166 (105m at $0.63808/hr)", d.NewValue)
	}
	if !strings.Contains(d.Reason, "--raise-cost-limit") {
		t.Errorf("Reason must name the flag that raised it: %q", d.Reason)
	}
}

// TestDecideCostLimitIncludesEBS: the cap counts EBS as well as compute (#616), so a
// cap covering only compute would bind before the new deadline — the same bug one
// layer down.
func TestDecideCostLimitIncludesEBS(t *testing.T) {
	launch := time.Now().Add(-30 * time.Minute)
	newDeadline := launch.Add(2 * time.Hour)
	tags := map[string]string{"spawn:cost-limit": "0.10", "spawn:price-per-hour": "1.00"}

	withoutEBS := decideCostLimit(extendInstance(launch, tags), newDeadline, time.Now(), 0, true)

	tagsEBS := map[string]string{}
	for k, v := range tags {
		tagsEBS[k] = v
	}
	tagsEBS["spawn:ebs-hourly-cost"] = "0.50"
	withEBS := decideCostLimit(extendInstance(launch, tagsEBS), newDeadline, time.Now(), 0, true)

	if withEBS.NewValue <= withoutEBS.NewValue {
		t.Errorf("the EBS rate must raise the required cap: without=%.4f with=%.4f",
			withoutEBS.NewValue, withEBS.NewValue)
	}
	// 2h at $1.50/hr combined = $3.00.
	if withEBS.NewValue < 2.9 || withEBS.NewValue > 3.1 {
		t.Errorf("with EBS, NewValue = %.4f, want ~3.00 (2h at $1.00 + $0.50)", withEBS.NewValue)
	}
}

// TestDecideCostLimitNeverLowers. An extension is a decision to allow MORE spend;
// silently tightening a cap someone set deliberately would be its own surprise.
func TestDecideCostLimitNeverLowers(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	newDeadline := launch.Add(20 * time.Minute)

	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit":     "100.00", // far more than 20m needs
		"spawn:price-per-hour": "1.00",
	}), newDeadline, time.Now(), 0, false)

	if d.Changed() {
		t.Errorf("a cap that already covers the new deadline must be left alone, got $%.4f -> $%.4f",
			d.Current, d.NewValue)
	}
}

func TestDecideCostLimitExplicitWins(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit":     "0.65",
		"spawn:price-per-hour": "0.638080",
	}), launch.Add(105*time.Minute), time.Now(), 5.00, false)

	if d.NewValue != 5.00 {
		t.Errorf("--cost-limit must win, got %.4f", d.NewValue)
	}
	if !strings.Contains(d.Reason, "explicitly") {
		t.Errorf("Reason should attribute the value to the flag: %q", d.Reason)
	}
}

// The flags cannot ask for two different things.
func TestCheckExtendCostLimitFlags(t *testing.T) {
	cases := []struct {
		explicit    float64
		raise, keep bool
		ok          bool
	}{
		{0, false, false, true},
		{5, false, false, true},
		{0, true, false, true},
		{0, false, true, true}, // the old opt-out, now the default
		{5, true, false, false},
		{5, false, true, false},
		{0, true, true, false},
		{-1, false, false, false},
	}
	for _, tc := range cases {
		err := checkExtendCostLimitFlags(tc.explicit, tc.raise, tc.keep)
		if (err == nil) != tc.ok {
			t.Errorf("checkExtendCostLimitFlags(%v, %v, %v) = %v, want ok=%v", tc.explicit, tc.raise, tc.keep, err, tc.ok)
		}
	}
}

// TestDecideCostLimitNoCapIsNotAProblem: with no cap set, nothing binds early and
// there is nothing to raise.
func TestDecideCostLimitNoCapIsNotAProblem(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:price-per-hour": "1.00",
	}), launch.Add(2*time.Hour), time.Now(), 0, false)

	if d.Changed() {
		t.Error("no cost limit means nothing to change")
	}
	if renderCostLimitDecision(d) != "" {
		t.Errorf("nothing to say when there is no cap, got: %q", renderCostLimitDecision(d))
	}
}

// TestDecideCostLimitWithoutARateSaysSo. Guessing a cap is how #533's cap stopped
// being enforceable; an unknown rate must produce an explanation, not a number.
func TestDecideCostLimitWithoutARateSaysSo(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit": "0.65", // capped, but no price tag
	}), launch.Add(2*time.Hour), time.Now(), 0, false)

	if d.Changed() {
		t.Error("without a rate the required cap is unknowable; it must not be invented")
	}
	if !strings.Contains(d.Reason, "price-per-hour") {
		t.Errorf("Reason must name the missing tag: %q", d.Reason)
	}
	if !strings.Contains(renderCostLimitDecision(d), "0.6500") {
		t.Error("the user should still see the cap that will bind")
	}
}

func TestCostLimitTagValueMatchesLaunchFormat(t *testing.T) {
	// spawn writes spawn:cost-limit with 4 decimals at launch; spored parses that.
	if got := costLimitTagValue(1.1166666); got != "1.1167" {
		t.Errorf("costLimitTagValue = %q, want 4-decimal form", got)
	}
}
