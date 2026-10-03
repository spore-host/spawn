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

// TestDecideCostLimitReproducesTheReportedCase is spawn#639 with the reporter's own
// numbers: a 60m TTL capped at $0.65 on a $0.63808/hr instance (61 minutes of spend),
// extended by 45m. Before this, the TTL moved to 105m and the cap stayed at $0.65, so
// the instance still died at ~61m — one minute after the ORIGINAL deadline — and the
// output said nothing about why.
func TestDecideCostLimitReproducesTheReportedCase(t *testing.T) {
	launch := time.Date(2026, 10, 3, 3, 14, 0, 0, time.UTC)
	newDeadline := launch.Add(105 * time.Minute)

	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit":     "0.6500",
		"spawn:price-per-hour": "0.638080",
	}), newDeadline, 0, false)

	if !d.Changed() {
		t.Fatalf("the cap must be raised: $%.4f covers only ~61m but the new deadline is 105m away", d.Current)
	}
	// 105 minutes at $0.63808/hr = $1.1166.
	if d.NewValue < 1.10 || d.NewValue > 1.13 {
		t.Errorf("NewValue = %.4f, want ~1.1166 (105m at $0.63808/hr)", d.NewValue)
	}
	// It must explain itself: the reporter's complaint was as much about silence as
	// about the no-op.
	if !strings.Contains(d.Reason, "raised") || !strings.Contains(d.Reason, "unreachable") {
		t.Errorf("Reason must say what was raised and why: %q", d.Reason)
	}
}

// TestDecideCostLimitIncludesEBS: the cap counts EBS as well as compute (#616), so a
// cap covering only compute would bind before the new deadline — the same bug one
// layer down.
func TestDecideCostLimitIncludesEBS(t *testing.T) {
	launch := time.Now().Add(-30 * time.Minute)
	newDeadline := launch.Add(2 * time.Hour)
	tags := map[string]string{"spawn:cost-limit": "0.10", "spawn:price-per-hour": "1.00"}

	withoutEBS := decideCostLimit(extendInstance(launch, tags), newDeadline, 0, false)

	tagsEBS := map[string]string{}
	for k, v := range tags {
		tagsEBS[k] = v
	}
	tagsEBS["spawn:ebs-hourly-cost"] = "0.50"
	withEBS := decideCostLimit(extendInstance(launch, tagsEBS), newDeadline, 0, false)

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
	}), newDeadline, 0, false)

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
	}), launch.Add(105*time.Minute), 5.00, false)

	if d.NewValue != 5.00 {
		t.Errorf("--cost-limit must win, got %.4f", d.NewValue)
	}
	if !strings.Contains(d.Reason, "explicitly") {
		t.Errorf("Reason should attribute the value to the flag: %q", d.Reason)
	}
}

// TestDecideCostLimitKeepFlagIsRespectedAndExplained: opting out is allowed, but the
// output must still say the cap was left alone — otherwise the user is back to an
// instance dying early for an unstated reason, which is the original bug.
func TestDecideCostLimitKeepFlagIsRespectedAndExplained(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:cost-limit":     "0.65",
		"spawn:price-per-hour": "0.638080",
	}), launch.Add(105*time.Minute), 0, true)

	if d.Changed() {
		t.Error("--keep-cost-limit must leave the tag alone")
	}
	out := renderCostLimitDecision(d)
	if !strings.Contains(out, "0.6500") || !strings.Contains(out, "unchanged") {
		t.Errorf("the output must state that the cap was left as-is:\n%s", out)
	}
}

// TestDecideCostLimitNoCapIsNotAProblem: with no cap set, nothing binds early and
// there is nothing to raise.
func TestDecideCostLimitNoCapIsNotAProblem(t *testing.T) {
	launch := time.Now().Add(-10 * time.Minute)
	d := decideCostLimit(extendInstance(launch, map[string]string{
		"spawn:price-per-hour": "1.00",
	}), launch.Add(2*time.Hour), 0, false)

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
	}), launch.Add(2*time.Hour), 0, false)

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
