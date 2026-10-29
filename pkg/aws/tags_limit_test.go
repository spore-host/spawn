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
	// A sweep member, not also a job-array member: the #477 shape with both
	// leaves no room for --tag values beside the slots spored needs (#1355).
	cfg.JobArrayID = ""
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
	launch, reserve, _ := tagCounts(t, withPrice)
	n := launch + reserve
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
// then; a failed write deletes the filesystem and terminates the instance. Once
// it is available spored writes spawn:fsx-id before it deletes
// spawn:fsx-pending, and its association status (#1355). So the launch must
// leave room for all of them: the three keys the launch does not write already
// (spawn:fsx-mount-point is one key whoever writes it) and spored's three.
func TestTagLimitLeavesRoomForAnEphemeralFSx(t *testing.T) {
	cfg := LaunchConfig{Name: "n", TTL: "1h", InstanceType: "t3.micro", Region: "us-east-1", PricePerHour: 0.01}
	launch, reserve, _ := tagCounts(t, cfg)
	cfg.Tags = map[string]string{}
	for i := 0; launch+reserve+len(cfg.Tags) < awsMaxTagsPerResource; i++ {
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
	}
	if _, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct"); err != nil {
		t.Fatalf("a launch leaving exactly its reserve free was refused: %v", err)
	}
	cfg.FSxLustreCreate, cfg.FSxLifecycle = true, "ephemeral"
	_, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	var limit *TagLimitError
	if !errors.As(err, &limit) || limit.Count != awsMaxTagsPerResource+7 || !strings.Contains(err.Error(), "spawn:fsx-pending") ||
		!strings.Contains(err.Error(), "spawn:fsx-id") {
		t.Fatalf("the same launch with an ephemeral FSx to create after it: err=%v, want a refusal counting its 7 tags", err)
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
// bound beyond localhost) plus an idle CPU threshold (#1308). Since #1359 only
// --metrics takes a tag, the one Prometheus' EC2 discovery filters on; the
// bind address and the threshold are in the launch config object.
//
// With the slots kept for spored's tags (#1355) it fits on the #477 maximal
// shape as a sweep member, with the version, OS and price source a real
// launch tags: 38 at launch + 10 kept = 48. The same shape as a job-array
// member too is 53, 3 over, and is refused naming the slots spored needs.
func TestMonitoringSettingsFitOnAMaximalLaunch(t *testing.T) {
	cfg := maximalConfig()
	cfg.IdleCPUPercent = 10
	cfg.Observability = SporedObservability{MetricsEnabled: true, MetricsBind: "0.0.0.0"}
	cfg.PricePerHour, cfg.SpawnVersion, cfg.TargetOS = 0, "0.150.0", "linux" // the pre-flight's stand-ins
	withArray := cfg
	cfg.JobArrayID, cfg.JobArrayName, cfg.JobArraySize, cfg.JobArrayIndex = "", "", 0, 0
	if err := cfg.ValidateTagCount(); err != nil {
		t.Fatalf("the pre-flight refuses the documented monitoring setup on a maximal sweep member: %v", err)
	}
	priced := cfg
	priced.PricePerHour, priced.PriceSource = 0.085, "live"
	tags := builtTags(t, priced) // fails the test if buildTags refuses it
	launch, reserve, _ := tagCounts(t, priced)
	t.Logf("maximal sweep member + --metrics --metrics-bind --idle-cpu-threshold: %d + %d kept = %d", launch, reserve, launch+reserve)
	if launch+reserve > awsMaxTagsPerResource {
		t.Errorf("maximal sweep member + --metrics --metrics-bind --idle-cpu-threshold = %d tags with its reserve, over AWS's %d", launch+reserve, awsMaxTagsPerResource)
	}
	have := map[string]string{}
	for _, tg := range tags {
		have[tg.Key] = tg.Value
	}
	if have["spawn:metrics-enabled"] != "true" {
		t.Errorf("spawn:metrics-enabled = %q, want true: Prometheus' EC2 discovery filters on it", have["spawn:metrics-enabled"])
	}
	obj := objectSettings(t, cfg)
	for k, v := range map[string]string{"metrics-bind": "0.0.0.0", "idle-cpu": "10"} {
		if obj[k] != v {
			t.Errorf("object %s = %q, want %q", k, obj[k], v)
		}
	}

	var limit *TagLimitError
	if err := withArray.ValidateTagCount(); !errors.As(err, &limit) || !strings.Contains(err.Error(), "slot(s) spored needs") {
		t.Errorf("the same launch as a job-array member too: ValidateTagCount() = %v, want a refusal naming spored's slots", err)
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

// tagCounts is how many tags a launch with cfg gives RunInstances, before
// sweep parameters, and how many slots it keeps free for tags written after
// it (tagReserve), whether or not buildTags refuses it.
func tagCounts(t *testing.T, cfg LaunchConfig) (launch, reserve int, refused *TagLimitError) {
	t.Helper()
	cfg.Parameters = nil
	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	if errors.As(err, &refused) {
		reserve = len(refused.Spored) + len(refused.PostLaunch)
		return refused.Count - reserve, reserve, refused
	}
	if err != nil {
		t.Fatalf("buildTags: %v", err)
	}
	return len(tags), tagReserveSize(cfg, tags), nil
}

// A launch must leave a slot free for each tag spored writes by itself once it
// runs (#1355). #1336 allowed a launch at exactly 50: there every one of those
// writes fails whole, and with spawn:compute-seconds unwritable a stop/start
// resets the cost clock. Now that launch is refused before any AWS call
// (ValidateTagCount is the pre-flight), saying how many slots spored needs;
// with exactly the reserve free it goes ahead.
func TestALaunchLeavesSporedItsSlots(t *testing.T) {
	cfg := LaunchConfig{Name: "n", TTL: "1h", InstanceType: "t3.micro", Region: "us-east-1", PricePerHour: 0.01}
	launch, reserve, _ := tagCounts(t, cfg)
	cfg.Tags = map[string]string{}
	for i := 0; launch+len(cfg.Tags) < awsMaxTagsPerResource; i++ {
		cfg.Tags[fmt.Sprintf("user%02d", i)] = "v"
	}
	err := cfg.ValidateTagCount()
	var limit *TagLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("a launch writing %d tags, leaving none of its %d reserved slots: ValidateTagCount() = %v, want a *TagLimitError",
			awsMaxTagsPerResource, reserve, err)
	}
	if limit.Count != awsMaxTagsPerResource+reserve || len(limit.Spored) != reserve {
		t.Errorf("refusal counts %d tags with %d for spored, want %d with %d", limit.Count, len(limit.Spored), awsMaxTagsPerResource+reserve, reserve)
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprintf("%d slot(s) spored needs", reserve), "spawn:compute-seconds", "spawn:last-heartbeat"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, msg)
		}
	}

	for i := 0; i < reserve; i++ {
		delete(cfg.Tags, fmt.Sprintf("user%02d", i))
	}
	if err := cfg.ValidateTagCount(); err != nil {
		t.Errorf("a launch leaving exactly its %d reserved slots free was refused: %v", reserve, err)
	}
}

// The reserve is the list less the keys the launch writes itself: an update
// to a key the instance already carries takes no slot. A launch tagged with a
// filesystem still being created writes spawn:fsx-pending and
// spawn:fsx-mount-point itself, so only spored's other FSx tags are kept.
func TestTheReserveSkipsKeysTheLaunchWrites(t *testing.T) {
	cfg := LaunchConfig{Name: "n", TTL: "1h", InstanceType: "t3.micro", Region: "us-east-1", PricePerHour: 0.01,
		FSxPending: "fs-3", FSxMountPoint: "/fsx"}
	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::1:user/u", "acct")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tg := range tags {
		have[*tg.Key] = true
	}
	if !have["spawn:fsx-pending"] || !have["spawn:fsx-mount-point"] {
		t.Fatalf("the launch does not write its pending FSx's tags: %v", have)
	}
	spored, spawn := tagReserve(cfg, tags)
	got := strings.Join(append(spored, spawn...), ",")
	for _, k := range []string{"spawn:fsx-id", "spawn:fsx-dra-status", "spawn:fsx-dra-error"} {
		if !strings.Contains(got, k) {
			t.Errorf("reserve %s keeps no slot for %s", got, k)
		}
	}
	for _, k := range []string{"spawn:fsx-pending", "spawn:fsx-mount-point"} {
		if strings.Contains(got, k) {
			t.Errorf("reserve %s keeps a slot for %s, which the launch already writes", got, k)
		}
	}
}
