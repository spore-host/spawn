package aws

import (
	"fmt"
	"sort"
	"strings"
	"testing"
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
	raw := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
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

	tags := builtTags(t, cfg)
	have := map[string]bool{}
	for _, tg := range tags {
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

func paramKeysOf(tags []tagPair) []string {
	var out []string
	for _, tg := range tags {
		if strings.HasPrefix(tg.Key, "spawn:param:") {
			out = append(out, strings.TrimPrefix(tg.Key, "spawn:param:"))
		}
	}
	return out
}
