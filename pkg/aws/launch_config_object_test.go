package aws

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/launchconfig"
)

// objectSettings is the launch config object a launch of cfg writes, as its
// settings, verified against the spawn:config tag the same launch writes.
func objectSettings(t *testing.T, cfg LaunchConfig) map[string]string {
	t.Helper()
	body, sha, err := LaunchConfigObject(cfg)
	if err != nil {
		t.Fatalf("LaunchConfigObject: %v", err)
	}
	tags := mustBuildTags(t, cfg, "123456789012", "arn:aws:iam::123456789012:user/test", "")
	if got := findTagValue(tags, launchconfig.Tag); got != sha {
		t.Fatalf("%s tag = %q, the object hashes to %q", launchconfig.Tag, got, sha)
	}
	if body == nil {
		return map[string]string{}
	}
	doc, unknown, err := launchconfig.Decode(body, sha)
	if err != nil || len(unknown) > 0 {
		t.Fatalf("Decode: %v (unknown %v)", err, unknown)
	}
	return doc.Settings
}

// goldenTags is testdata/every-setting-tags.v0.149.0.json: the tags v0.149.0's
// buildTags wrote for everyLaunchConfigSetting, before the object existed.
func goldenTags(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("testdata/every-setting-tags.v0.149.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) < 40 {
		t.Fatalf("the golden has %d tags; it is the wrong file", len(m))
	}
	return m
}

func atFixedClock(t *testing.T) {
	t.Helper()
	old := tagClock
	tagClock = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { tagClock = old })
}

// The round trip, launcher side (#1359): for a launch setting every
// launch-fixed spored setting, today's tags plus today's object hold exactly
// what v0.149.0's tags held, value for value. Each moved setting is in the
// object and not a tag; every other tag is unchanged; nothing is lost and
// nothing is added but spawn:config. pkg/provider's
// TestLaunchConfigObjectIsEquivalentToTheTagPath is the spored half.
func TestLaunchConfigObjectHoldsWhatTheTagsHeld(t *testing.T) {
	atFixedClock(t)
	cfg := everyLaunchConfigSetting()
	golden := goldenTags(t)

	tags := map[string]string{}
	for _, tg := range mustBuildTags(t, cfg, "123456789012", "arn:aws:iam::123456789012:user/u", "acct") {
		tags[*tg.Key] = *tg.Value
	}
	settings := objectSettings(t, cfg)
	if len(settings) != len(launchconfig.Settings) {
		t.Errorf("the object holds %d settings, want all %d: everyLaunchConfigSetting no longer sets every one, or the object drops some",
			len(settings), len(launchconfig.Settings))
	}

	got := map[string]string{}
	for k, v := range tags {
		if k == launchconfig.Tag {
			continue
		}
		if launchconfig.IsSetting(k) {
			t.Errorf("%s=%q is still a tag; it moved to the launch config object", k, v)
		}
		got[k] = v
	}
	for k, v := range settings {
		if _, dup := got["spawn:"+k]; dup {
			t.Errorf("spawn:%s is in both the tags and the object", k)
		}
		got["spawn:"+k] = v
	}
	for _, k := range unionKeys(got, golden) {
		if got[k] != golden[k] {
			t.Errorf("%s: v0.149.0 wrote %q, now tags+object hold %q", k, golden[k], got[k])
		}
	}
}

func unionKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// A launch with none of the launch-fixed settings writes no object and no
// spawn:config tag, so it needs no S3 at all.
func TestALaunchWithNoLaunchFixedSettingHasNoObject(t *testing.T) {
	cfg := LaunchConfig{Name: "plain", TTL: "8h", Region: "us-east-1", InstanceType: "t3.micro"}
	body, sha, err := LaunchConfigObject(cfg)
	if err != nil || body != nil || sha != "" {
		t.Fatalf("LaunchConfigObject = %q, %q, %v; want no object", body, sha, err)
	}
	if v := findTagValue(mustBuildTags(t, cfg, "123456789012", "arn:aws:iam::1:user/u", ""), launchconfig.Tag); v != "" {
		t.Errorf("%s = %q on a launch with no object", launchconfig.Tag, v)
	}
}

// The object and the tag are content-addressed: the tag is the sha256 of the
// object's bytes, the same settings always give the same bytes, and different
// settings different ones.
func TestLaunchConfigObjectIsContentAddressed(t *testing.T) {
	cfg := everyLaunchConfigSetting()
	body, sha, err := LaunchConfigObject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sha != launchconfig.SHA256Hex(body) {
		t.Fatalf("sha %s is not the sha256 of the body", sha)
	}
	for i := 0; i < 5; i++ {
		b2, s2, _ := LaunchConfigObject(cfg)
		if s2 != sha || string(b2) != string(body) {
			t.Fatalf("the same config rendered %s then %s", sha, s2)
		}
	}
	cfg.NotifyPlatform = "slack"
	if _, s3, _ := LaunchConfigObject(cfg); s3 == sha {
		t.Error("a different setting hashed the same")
	}
	if strings.Contains(string(body), `\u0026`) {
		t.Errorf("the body HTML-escapes URLs: %s", body)
	}
}

// maximalLaunchTagCount is the most tags a launch can carry, counting the ones
// spawn writes after RunInstances (#1359): every lifecycle, storage, DNS and
// spored setting, a sweep member's and a job-array member's tags at once, and
// the Spot price. The same launch was 66 tags before the launch config object
// (and refused, #1336); the narrower maximalConfig plus every #1308 setting,
// 55 then, is 41 now. A key added to a launch that pushes this up has been
// classified as staying a tag (pkg/launchconfig.TagKeys); raise this with that
// reason, or move the key into the object.
const maximalLaunchTagCount = 46

// The tag count (#1359): a maximal launch, with every spored setting and
// every launch-fixed setting set, comes in well under AWS's 50, and leaves
// room for the tags spored writes at runtime (#1355).
func TestMaximalLaunchTagCount(t *testing.T) {
	cfg := maximalConfig()
	allSporedSettings(&cfg)
	every := everyLaunchConfigSetting()
	cfg.ActivePortsRaw, cfg.ActiveProcessesRaw = every.ActivePortsRaw, every.ActiveProcessesRaw
	cfg.AppMode, cfg.ReadyPort, cfg.ReadyHealthPath, cfg.WebAuth = every.AppMode, every.ReadyPort, every.ReadyHealthPath, every.WebAuth
	cfg.DNSDomain, cfg.DNSEndpoint = "example.org", "https://dns.example.org/update"
	cfg.PriceSource, cfg.SpawnVersion, cfg.TargetOS = "live", "0.150.0", "linux"

	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	if err != nil {
		t.Fatalf("a maximal launch is refused: %v", err)
	}
	n := len(tags) + len(postLaunchTags(cfg))
	t.Logf("maximal launch: %d tags at launch + %d after = %d of %d", len(tags), len(postLaunchTags(cfg)), n, awsMaxTagsPerResource)
	if n > maximalLaunchTagCount {
		var keys []string
		for _, tg := range tags {
			keys = append(keys, *tg.Key)
		}
		t.Errorf("a maximal launch carries %d tags, more than %d:\n%s", n, maximalLaunchTagCount, strings.Join(keys, "\n"))
	}
	if objectSettings(t, cfg)[launchconfig.NotifyURL] == "" {
		t.Error("the maximal launch's object lost its notify URL")
	}
}

// The tag-limit refusal (#1336) still holds once custom tags fill the rest, and
// no longer names settings that take no tag.
func TestTagLimitRefusalNamesOnlySettingsThatAreTags(t *testing.T) {
	cfg := maximalConfig()
	allSporedSettings(&cfg)
	cfg.Tags = map[string]string{}
	for i := 0; i < 20; i++ {
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
	}
	_, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	var limit *TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("a maximal launch with 20 custom tags: err = %v, want a *TagLimitError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--metrics (spawn:metrics-enabled)") || !strings.Contains(msg, "20 custom --tag") {
		t.Errorf("refusal does not name what takes a tag:\n%s", msg)
	}
	for _, gone := range []string{"spawn:idle-cpu", "spawn:metrics-port", "spawn:tracing-enabled", "spawn:tracing-sampling"} {
		if strings.Contains(msg, gone) {
			t.Errorf("refusal names %s, which is in the launch config object and takes no tag:\n%s", gone, msg)
		}
	}
}
