package aws

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// maximalConfig populates every optional LaunchConfig section that contributes
// tags. This is the "sophisticated user" shape from #477 — FSx + EFS + both
// webhooks + job array + sweep + every lifecycle flag — which is exactly the
// configuration least likely to suspect tag arithmetic when EC2 rejects it.
func maximalConfig() LaunchConfig {
	return LaunchConfig{
		InstanceType: "c6i.large", Region: "us-east-1", AMI: "ami-x", KeyName: "k",
		Name: "n", DNSName: "d", TTL: "4h", IdleTimeout: "1h", HibernateOnIdle: true,
		CostLimit: 10, PricePerHour: 0.085, SessionTimeout: "8h",
		PreStop: "sync", PreStopTimeout: "5m", Username: "ec2-user",
		SpotInterruptionWebhookURL: "https://spot", WebhookCorrelation: "corr",
		WebhookTimeout: "2s", CompletionWebhookURL: "https://done",
		NotifyURL: "https://n", NotifyCommand: "/spore", SlackWorkspaceID: "T1",
		NotifyPlatform: "slack",
		JobArrayID:     "a", JobArrayName: "an", JobArraySize: 4, JobArrayIndex: 1,
		SweepID: "s", SweepName: "sn", SweepSize: 4, SweepIndex: 1,
		EFSID: "fs-1", EFSMountPoint: "/efs",
		FSxLustreID: "fs-2", FSxMountPoint: "/fsx",
		OnComplete: "terminate", CompletionFile: "/tmp/C", CompletionDelay: "1m",
		Spot: true, EFAEnabled: true, Hibernate: true,
	}
}

// tagPair mirrors what buildTags emits, flattened for assertions.
type tagPair struct{ Key, Value string }

func builtTags(t *testing.T, cfg LaunchConfig) []tagPair {
	t.Helper()
	raw := mustBuildTags(t, cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	out := make([]tagPair, 0, len(raw))
	for _, tg := range raw {
		out = append(out, tagPair{Key: *tg.Key, Value: *tg.Value})
	}
	return out
}

// TestTagsNeverExceedAWSLimit is spawn#477.
//
// AWS caps a resource at 50 tags and FAILS RunInstances outright when exceeded —
// it does not truncate. The old cap was `paramCount >= 35`, with a comment
// claiming that "stays under AWS 50-tag limit"; it counted only the sweep
// parameters and ignored the ~45 tags the other sections had already appended.
// A maximal launch with 60 sweep parameters produced 81 tags.
func TestTagsNeverExceedAWSLimit(t *testing.T) {
	for _, nParams := range []int{0, 1, 10, 35, 60, 200} {
		cfg := maximalConfig()
		if nParams > 0 {
			cfg.Parameters = map[string]string{}
			for i := 0; i < nParams; i++ {
				cfg.Parameters[fmt.Sprintf("param%03d", i)] = "v"
			}
		}
		tags := builtTags(t, cfg)
		if len(tags) > awsMaxTagsPerResource {
			t.Errorf("%d sweep params → %d tags, over AWS's %d-tag limit by %d. "+
				"RunInstances rejects the call outright, so the launch fails.",
				nParams, len(tags), awsMaxTagsPerResource, len(tags)-awsMaxTagsPerResource)
		}
	}
}

// TestTagsHaveNoDuplicateKeys is a launch-breaking bug found while fixing #477,
// and it needs only TWO flags rather than a maximal config.
//
// Both the spot-interruption webhook and the completion webhook emitted
// spawn:webhook-correlation and spawn:webhook-timeout, so setting both URLs
// produced duplicate keys. Verified against real EC2:
//
//	InvalidParameterValue: Duplicate tag key 'spawn:webhook-correlation' specified.
//
// So the launch failed immediately, nothing to do with the 50-tag limit.
func TestTagsHaveNoDuplicateKeys(t *testing.T) {
	cfg := maximalConfig() // sets BOTH webhook URLs plus correlation and timeout
	tags := builtTags(t, cfg)

	seen := map[string]int{}
	for _, tg := range tags {
		seen[tg.Key]++
	}
	var dupes []string
	for k, n := range seen {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s (x%d)", k, n))
		}
	}
	sort.Strings(dupes)
	if len(dupes) > 0 {
		t.Errorf("duplicate tag keys: %s\n\nEC2 rejects the whole RunInstances call with "+
			"\"Duplicate tag key ... specified\", so this fails the launch outright.",
			strings.Join(dupes, ", "))
	}
}

// TestSweepParamTagsAreDeterministic: the old loop ranged over the Parameters
// map directly, so WHICH parameters survived truncation depended on Go's
// randomised map iteration order. Two members of the same sweep could therefore
// record different parameter subsets — and spawn:param:* is how a member records
// which point in the space it is, so the surviving set has to be stable.
func TestSweepParamTagsAreDeterministic(t *testing.T) {
	cfg := maximalConfig()
	cfg.Parameters = map[string]string{}
	for i := 0; i < 60; i++ {
		cfg.Parameters[fmt.Sprintf("param%03d", i)] = "v"
	}

	first := paramKeysOf(builtTags(t, cfg))
	if len(first) == 0 {
		t.Fatal("no spawn:param:* tags survived at all; the sweep record is empty")
	}
	for i := 0; i < 12; i++ {
		got := paramKeysOf(builtTags(t, cfg))
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("surviving parameter set changed between calls:\n  %v\n  %v\n\n"+
				"Go map iteration is randomised, so two members of one sweep would record "+
				"different points in the parameter space.", first, got)
		}
	}

	// Deterministic AND sorted, so the surviving subset is predictable rather
	// than merely stable within a process.
	sorted := append([]string{}, first...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != strings.Join(first, ",") {
		t.Errorf("surviving parameters are not in sorted order: %v", first)
	}
}

// TestUserSuppliedTagsSurvive: --tag is an explicit request, so it must not be
// the thing silently dropped to make room. Section 7 previously appended these
// with no cap at all, which is also a user-reachable way to blow the limit.
func TestUserSuppliedTagsSurvive(t *testing.T) {
	cfg := maximalConfig()
	cfg.Tags = map[string]string{"project": "gchp", "owner": "me"}
	cfg.Parameters = map[string]string{}
	for i := 0; i < 60; i++ {
		cfg.Parameters[fmt.Sprintf("param%03d", i)] = "v"
	}

	tags := builtTags(t, cfg)
	if len(tags) > awsMaxTagsPerResource {
		t.Fatalf("%d tags, over the limit", len(tags))
	}
	for _, want := range []string{"project", "owner"} {
		found := false
		for _, tg := range tags {
			if tg.Key == want {
				found = true
			}
		}
		if !found {
			t.Errorf("user tag %q was dropped to make room; an explicitly requested tag "+
				"outranks an informational spawn:param:* entry", want)
		}
	}
}

// TestFunctionalTagsAreNeverDropped is the fail-safe that matters most.
//
// spored reads its entire lifecycle contract from tags. Dropping spawn:ttl to
// fit under the limit would produce an instance with NO TTL enforcement — a
// silent, open-ended bill, which is far worse than a launch that fails loudly.
//
// So under pressure from --tag values the launch is refused rather than
// trimmed (#1336): this used to build 134 tags, and RunInstances was the first
// thing to object, after the launch's security groups, IAM and FSx existed.
// Under pressure from sweep parameters, the one droppable class, the launch
// goes ahead with every functional tag kept.
func TestFunctionalTagsAreNeverDropped(t *testing.T) {
	cfg := maximalConfig()
	cfg.Tags = map[string]string{}
	for i := 0; i < 80; i++ { // absurd, to force maximum pressure
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
	}
	cfg.Parameters = map[string]string{}
	for i := 0; i < 80; i++ {
		cfg.Parameters[fmt.Sprintf("param%03d", i)] = "v"
	}

	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	var limit *TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("80 --tag values on a maximal launch built %d tags with err=%v; want a *TagLimitError. "+
			"Trimming would drop either an explicit --tag or a tag spored enforces from, and "+
			"RunInstances refuses the untrimmed set", len(tags), err)
	}
	if limit.CustomTags != 80 || !strings.Contains(err.Error(), "80 custom --tag") {
		t.Errorf("the refusal does not say the 80 --tag values are what to drop: %v", err)
	}

	cfg.Tags = nil
	have := map[string]bool{}
	for _, tg := range builtTags(t, cfg) {
		have[tg.Key] = true
	}
	for _, must := range []string{
		"spawn:managed", "spawn:ttl", "spawn:cost-limit", "spawn:idle-timeout",
		"spawn:on-complete", "spawn:local-username", "spawn:price-per-hour",
	} {
		if !have[must] {
			t.Errorf("%s was dropped under tag pressure. spored reads its lifecycle "+
				"contract from tags, so losing this disables enforcement and bills "+
				"open-endedly — strictly worse than failing the launch.", must)
		}
	}
}

// allSporedSettings sets every optional spored setting #1308 added, each of
// which writes its own tag.
func allSporedSettings(cfg *LaunchConfig) {
	sampling := 0.5
	cfg.IdleCPUPercent = 10
	cfg.Observability = SporedObservability{
		MetricsEnabled: true, MetricsPort: 9100, MetricsBind: "0.0.0.0", MetricsPath: "/m",
		TracingEnabled: true, TracingSampling: &sampling,
	}
}

// A maximal launch with every #1308 spored setting set is 54 tags, plus the
// Spot price spawn writes after launch (#1336). It is refused by buildTags, so
// in the shell, with an error that gives the count and the limit and names
// each setting the user can drop.
func TestMaximalSporedSettingsLaunchIsRefused(t *testing.T) {
	cfg := maximalConfig()
	allSporedSettings(&cfg)

	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	var limit *TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("maximal launch + every spored setting built %d tags with err=%v; want a *TagLimitError, "+
			"since RunInstances rejects more than %d", len(tags), err, awsMaxTagsPerResource)
	}
	if limit.Count != 55 || limit.Limit != awsMaxTagsPerResource {
		t.Errorf("Count=%d Limit=%d, want 55 (54 tags + spawn:spot-price-per-hour) and %d", limit.Count, limit.Limit, awsMaxTagsPerResource)
	}
	msg := err.Error()
	for _, want := range []string{
		"55 tags", "allows 50", "Drop at least 5",
		"--idle-cpu-threshold (spawn:idle-cpu)", "--metrics (spawn:metrics-enabled)",
		"--metrics-port (spawn:metrics-port)", "--metrics-bind (spawn:metrics-bind)",
		"--metrics-path (spawn:metrics-path)", "--tracing (spawn:tracing-enabled, spawn:tracing-exporter)",
		"--tracing-sampling (spawn:tracing-sampling)", SpotPriceTag,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "custom --tag") {
		t.Errorf("refusal names --tag values the launch does not have:\n%s", msg)
	}

	// The pre-flight, which callers run before creating anything, agrees.
	if err := cfg.ValidateTagCount(); !errors.As(err, &limit) {
		t.Errorf("ValidateTagCount() = %v; want the same *TagLimitError", err)
	}
}

// ValidateTagCount runs before Launch has resolved the price and account name,
// so it counts them as present: a launch that would only go over once Launch
// tagged its price must be refused by the pre-flight too.
func TestValidateTagCountCountsWhatLaunchResolves(t *testing.T) {
	cfg := maximalConfig()
	cfg.PricePerHour = 0 // Launch looks it up, and tags it with its source
	cfg.Tags = map[string]string{}
	// Fill to exactly the limit once the price, its source and the account
	// name Launch resolves are in.
	withPrice := cfg
	withPrice.PricePerHour, withPrice.PriceSource = 1, "live"
	n := len(builtTags(t, withPrice)) + len(postLaunchTags(cfg))
	for i := 0; n < awsMaxTagsPerResource; i++ {
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
		n++
	}
	if err := cfg.ValidateTagCount(); err != nil {
		t.Fatalf("a launch at exactly %d tags was refused: %v", awsMaxTagsPerResource, err)
	}
	cfg.Tags["one-more"] = "v"
	var limit *TagLimitError
	if err := cfg.ValidateTagCount(); !errors.As(err, &limit) || limit.Count != awsMaxTagsPerResource+1 {
		t.Fatalf("one tag over, with the price still unresolved: ValidateTagCount() = %v, want a refusal at %d",
			err, awsMaxTagsPerResource+1)
	}
}

// An ephemeral FSx is created after RunInstances and its four tags are written
// then; a failed write deletes the filesystem and terminates the instance. So
// the launch must leave room for them.
func TestTagLimitLeavesRoomForAnEphemeralFSx(t *testing.T) {
	cfg := LaunchConfig{Name: "n", TTL: "1h", InstanceType: "t3.micro", Region: "us-east-1", PricePerHour: 0.01}
	base := len(builtTags(t, cfg))
	cfg.Tags = map[string]string{}
	for i := 0; base+len(cfg.Tags) < awsMaxTagsPerResource; i++ {
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
	}
	if _, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct"); err != nil {
		t.Fatalf("a launch at exactly %d tags was refused: %v", awsMaxTagsPerResource, err)
	}
	cfg.FSxLustreCreate, cfg.FSxLifecycle = true, "ephemeral"
	_, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	var limit *TagLimitError
	if !errors.As(err, &limit) || limit.Count != awsMaxTagsPerResource+4 || !strings.Contains(err.Error(), "spawn:fsx-pending") {
		t.Fatalf("the same launch with an ephemeral FSx to create after it: err=%v, want a refusal counting its 4 tags", err)
	}
}

func paramKeysOf(tags []tagPair) []string {
	var out []string
	for _, tg := range tags {
		if strings.HasPrefix(tg.Key, "spawn:param:") {
			out = append(out, strings.TrimPrefix(tg.Key, "spawn:param:"))
		}
	}
	return out
}

// The monitoring setup docs/self-hosted-monitoring.md documents (metrics on,
// bound beyond localhost) plus an idle CPU threshold still fits on a maximal
// launch (#1308): 49 tags, and the Spot price spawn writes after launch makes 50.
// Every spored setting at once does not fit, and is refused
// (TestMaximalSporedSettingsLaunchIsRefused).
func TestMonitoringSettingsFitOnAMaximalLaunch(t *testing.T) {
	cfg := maximalConfig()
	cfg.IdleCPUPercent = 10
	cfg.Observability = SporedObservability{MetricsEnabled: true, MetricsBind: "0.0.0.0"}
	if err := cfg.ValidateTagCount(); err != nil {
		t.Fatalf("the pre-flight refuses the documented monitoring setup on a maximal launch: %v", err)
	}
	tags := builtTags(t, cfg) // fails the test if buildTags refuses it
	if n := len(tags) + len(postLaunchTags(cfg)); n > awsMaxTagsPerResource {
		t.Errorf("maximal launch + --metrics --metrics-bind --idle-cpu-threshold = %d tags with the ones spawn writes after launch, over AWS's %d", n, awsMaxTagsPerResource)
	}
	have := map[string]string{}
	for _, tg := range tags {
		have[tg.Key] = tg.Value
	}
	for k, v := range map[string]string{"spawn:metrics-enabled": "true", "spawn:metrics-bind": "0.0.0.0", "spawn:idle-cpu": "10"} {
		if have[k] != v {
			t.Errorf("%s = %q, want %q", k, have[k], v)
		}
	}
}

// A volume restored from a snapshot already carries the launch's tag set,
// which RunInstances puts on every volume; the snapshot's tags copied onto it
// afterwards share the same 50-tag limit, and CreateTags fails the whole call
// past it (#1336). Only what fits is added, provenance first, and a key the
// volume already has takes no new slot.
func TestFitVolumeTagsStaysWithinTheLimit(t *testing.T) {
	tag := func(k string) ec2types.Tag { return ec2types.Tag{Key: &k, Value: &k} }
	var existing []ec2types.Tag
	for i := 0; i < 48; i++ {
		existing = append(existing, tag(fmt.Sprintf("launch-%02d", i)))
	}
	add := []ec2types.Tag{tag("spawn:from-snapshot"), tag("launch-00"), tag("db"), tag("owner"), tag("team")}
	fit, dropped := fitVolumeTags(existing, add)
	var keys []string
	for _, t := range fit {
		keys = append(keys, *t.Key)
	}
	if got := strings.Join(keys, ","); got != "spawn:from-snapshot,launch-00,db" {
		t.Errorf("fit = %s, want spawn:from-snapshot,launch-00,db (48 + 2 new = 50)", got)
	}
	if got := strings.Join(dropped, ","); got != "owner,team" {
		t.Errorf("dropped = %s, want owner,team", got)
	}
}
