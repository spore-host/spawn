package cmd

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// The reported instance (#896): m8a.4xlarge --spot in us-west-2, on-demand
// $0.9738/hr, spot about $0.36/hr.
func spotStatusInstance(tags map[string]string) *aws.InstanceInfo {
	base := map[string]string{
		"spawn:managed":         "true",
		"spawn:price-per-hour":  "0.973800",
		"spawn:ebs-hourly-cost": "0.002200",
	}
	for k, v := range tags {
		base[k] = v
	}
	launch := time.Now().Add(-time.Minute)
	base["spawn:ttl-deadline"] = launch.Add(time.Hour).UTC().Format(time.RFC3339)
	return &aws.InstanceInfo{
		InstanceID: "i-028c23f816d2eb68e", InstanceType: "m8a.4xlarge", State: "running",
		Region: "us-west-2", SpotInstance: true, LaunchTime: launch, Tags: base,
	}
}

func noLookup(t *testing.T) spotPriceLookup {
	return func(context.Context, string, string) (float64, error) {
		t.Error("looked up a spot price that should have come from the tag or not been needed")
		return 0, errors.New("unexpected")
	}
}

// A spot instance's billable row and max cost use its spot price, not the
// on-demand rate it used to be costed at under a "(spot)" label (#896).
func TestStatusRate_SpotInstanceUsesItsSpotPrice(t *testing.T) {
	inst := spotStatusInstance(map[string]string{aws.SpotPriceTag: "0.360000"})
	rate := resolveStatusRate(context.Background(), inst, noLookup(t))
	if rate.PerHour != 0.36 || !rate.SpotPriced {
		t.Fatalf("rate = %+v, want the recorded spot price $0.36", rate)
	}

	res := billableResourcesAt(inst, rate)
	if len(res) == 0 || res[0].Kind != "instance" {
		t.Fatalf("no instance row: %+v", res)
	}
	if res[0].USD != "~$0.3600/hr" || !strings.Contains(res[0].Note, "spot") {
		t.Errorf("instance row = %q / %q, want ~$0.3600/hr labelled spot", res[0].USD, res[0].Note)
	}

	block := lifecycleProtectionBlockAt(inst, rate)
	// One hour from launch to deadline: ~$0.36 at spot, ~$0.97 at on-demand.
	if !strings.Contains(block, "~$0.36 by deadline at the spot price at launch") {
		t.Errorf("max cost is not at the spot price:\n%s", block)
	}
	if !strings.Contains(block, "~$0.97 at on-demand") {
		t.Errorf("max cost dropped the on-demand bound:\n%s", block)
	}
}

// An instance launched before spawn recorded the spot price is priced at the
// price now in effect; if that cannot be had, it falls back to on-demand and the
// row says the figure overstates — never an unlabelled on-demand number.
func TestStatusRate_SpotWithoutTagLooksUpThenLabelsTheFallback(t *testing.T) {
	inst := spotStatusInstance(nil)

	found := resolveStatusRate(context.Background(), inst, func(_ context.Context, region, id string) (float64, error) {
		if region != "us-west-2" || id != "i-028c23f816d2eb68e" {
			t.Errorf("lookup(%q, %q), want the instance's own region and id", region, id)
		}
		return 0.3712, nil
	})
	if found.PerHour != 0.3712 || found.Basis != rateBasisSpotCurrent {
		t.Errorf("rate = %+v, want the looked-up current spot price", found)
	}

	failed := resolveStatusRate(context.Background(), inst, func(context.Context, string, string) (float64, error) {
		return 0, errors.New("UnauthorizedOperation")
	})
	if failed.PerHour != 0.9738 || failed.SpotPriced {
		t.Fatalf("fallback rate = %+v, want on-demand", failed)
	}
	note := billableResourcesAt(inst, failed)[0].Note
	if !strings.Contains(note, "spot price unknown") || !strings.Contains(note, "overstates") {
		t.Errorf("on-demand fallback for a spot instance is not labelled: %q", note)
	}
}

// An on-demand instance is costed at on-demand and costs no extra API call.
func TestStatusRate_OnDemandInstanceNeedsNoLookup(t *testing.T) {
	inst := spotStatusInstance(nil)
	inst.SpotInstance = false
	rate := resolveStatusRate(context.Background(), inst, noLookup(t))
	if rate.PerHour != 0.9738 || rate.Basis != rateBasisOnDemand {
		t.Errorf("rate = %+v, want on-demand $0.9738", rate)
	}
}

func TestFormatStatusTime(t *testing.T) {
	mst := time.FixedZone("MST", -7*3600)
	at := time.Date(2026, 10, 15, 17, 59, 0, 0, time.UTC)
	cases := []struct {
		loc  *time.Location
		at   time.Time
		want string
	}{
		{mst, at, "2026-10-15 10:59 MST (17:59 UTC)"},
		{time.UTC, at, "2026-10-15 17:59 UTC"},
		// The UTC date is spelled out when it differs from the local one.
		{mst, time.Date(2026, 10, 16, 2, 30, 0, 0, time.UTC), "2026-10-15 19:30 MST (2026-10-16 02:30 UTC)"},
	}
	for _, c := range cases {
		if got := formatStatusTime(c.at, c.loc); got != c.want {
			t.Errorf("formatStatusTime(%s, %s) = %q, want %q", c.at, c.loc, got, c.want)
		}
	}
}

// One screen, one time format (#896): spored's TTL line said "17:59 UTC" two
// lines above spawn's "Termination deadline: 10:59 MST" for the same instant.
// Every timestamp `spawn status` prints — spored's re-rendered ones and spawn's
// own — must be in formatStatusTime's shape, and the same instant must read the
// same everywhere.
func TestStatusScreen_UsesOneTimeFormat(t *testing.T) {
	old := statusLoc
	statusLoc = time.FixedZone("MST", -7*3600)
	defer func() { statusLoc = old }()

	inst := spotStatusInstance(map[string]string{aws.SpotPriceTag: "0.36"})
	deadline, _ := lifecycleDeadline(inst)
	// Both spored formats: this version's and the parenthesised one older
	// agents still print.
	spored := "  Started:          " + inst.LaunchTime.UTC().Format("2006-01-02 15:04 UTC") + "\n" +
		"  TTL:              59m remaining — terminates " + deadline.UTC().Format("2006-01-02 15:04 UTC") + "\n" +
		"  TTL:              59m remaining  (terminates " + deadline.UTC().Format("2006-01-02 15:04 UTC") + ")\n"

	screen := localizeSporedTimes(spored, statusLocation()) +
		lifecycleProtectionBlockAt(inst, tagStatusRate(inst)) +
		ttlReconciliationNotice(inst, "  TTL:              none — instance will not auto-terminate\n")

	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)
	full := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} MST \((\d{4}-\d{2}-\d{2} )?\d{2}:\d{2} UTC\)`)
	// Strip every correctly formatted stamp and require nothing date-like left.
	// Counting raw stamps against formatted ones failed whenever the local and
	// UTC dates differ — 17:00-24:00 MST every day — because a formatted stamp
	// then carries the UTC DATE in its parenthesis, which the raw count sees
	// as a second timestamp.
	if m := len(full.FindAllString(screen, -1)); m == 0 {
		t.Fatalf("no timestamp on screen in the local (UTC) format:\n%s", screen)
	}
	if left := stamp.FindAllString(full.ReplaceAllString(screen, ""), -1); len(left) > 0 {
		t.Errorf("%d timestamp(s) on screen not in the local (UTC) format: %v\n%s", len(left), left, screen)
	}
	if want := formatStatusTime(deadline, statusLocation()); strings.Count(screen, want) < 4 {
		t.Errorf("the deadline does not read %q on every line that shows it:\n%s", want, screen)
	}
}
