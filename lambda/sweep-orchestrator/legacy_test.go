package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sporedinstall"
	"github.com/spore-host/spawn/pkg/sweep"
	"gopkg.in/yaml.v3"
)

// v0146Params is a member's params exactly as a v0.146.0 CLI uploads them: its
// detachedParamKeys (`git show v0.146.0:cmd/launch_sweep_detached_keys.go`),
// ami, iam_role, instance_type, key_name, spot and ttl, and nothing this Lambda
// now reads besides. A frozen list on purpose: it is a released CLI's contract,
// which no later change can alter.
func v0146Params(spot bool, ttl string) map[string]interface{} {
	m := map[string]interface{}{
		"ami":           "ami-0123456789abcdef0",
		"iam_role":      "spawn-instance-abc123",
		"instance_type": "t3.micro",
		"key_name":      "alice-key",
		"spot":          spot,
	}
	if ttl != "" {
		m["ttl"] = ttl
	}
	return m
}

// releaseOrchestrator makes this test's Lambda the release v, as
// scripts/deploy-sweep-orchestrator.sh builds it (-X main.Version=v), and
// re-runs the init that hands the version to pkg/launcher.
func releaseOrchestrator(t *testing.T, v string) {
	t.Helper()
	prevV, prevC := Version, launcher.CallerVersion
	t.Cleanup(func() { Version, launcher.CallerVersion = prevV, prevC })
	Version = v
	applyVersion()
}

func legacyState() *SweepRecord {
	return &SweepRecord{
		SweepID: "sw-20261026", SweepName: "sw", TotalParams: 4,
		AWSAccountID: "123456789012", UserID: "arn:aws:iam::123456789012:user/alice",
	}
}

func decodeUserData(t *testing.T, in *ec2.RunInstancesInput) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(awssdk.ToString(in.UserData))
	if err != nil {
		t.Fatalf("user data is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("user data is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func tagMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	return m
}

// assertLegacyMemberRunsSpored holds a legacy member's RunInstances to #1368:
// user data that is the stub for this orchestrator's release, fetching the
// body this build would inline, and the tags a CLI-built member carries.
func assertLegacyMemberRunsSpored(t *testing.T, in *ec2.RunInstancesInput, now time.Time, ttl time.Duration) {
	t.Helper()
	script := decodeUserData(t, in)
	sum := sha256.Sum256([]byte(launcher.BootstrapBody()))
	if !strings.Contains(script, "SPAWN_BS_VERSION='0.147.0'") ||
		!strings.Contains(script, "SPAWN_BS_SHA256="+hex.EncodeToString(sum[:])) {
		t.Error("the user data is not the stub fetching spawn 0.147.0's published body, verified against the body this build would inline")
	}
	if strings.Contains(script, "systemctl start spored") {
		t.Error("a release orchestrator inlined the bootstrap body instead of fetching the published one")
	}
	if !strings.Contains(script, "LOCAL_USERNAME='ec2-user'") {
		t.Error("the bootstrap creates no ec2-user")
	}

	var instance []ec2types.Tag
	for _, s := range in.TagSpecifications {
		if s.ResourceType == ec2types.ResourceTypeInstance {
			instance = s.Tags
		}
	}
	tags := tagMap(instance)
	want := map[string]string{
		"spawn:managed":        "true",
		"spawn:root":           "true",
		"spawn:created-by":     "spawn",
		"spawn:iam-user":       "arn:aws:iam::123456789012:user/alice",
		"spawn:account-id":     "123456789012",
		"spawn:version":        "0.147.0",
		"spawn:launch-time":    now.UTC().Format(time.RFC3339),
		"spawn:sweep-id":       "sw-20261026",
		"spawn:sweep-name":     "sw",
		"spawn:sweep-index":    "2",
		"spawn:sweep-size":     "4",
		"spawn:local-username": "ec2-user",
		"spawn:os":             "linux",
		"Name":                 "sw-2",
	}
	if ttl > 0 {
		want["spawn:ttl-deadline"] = now.Add(ttl).UTC().Format(time.RFC3339)
	}
	for k, v := range want {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
	if tags["spawn:account-base36"] == "" {
		t.Error("no spawn:account-base36: the DNS updater cannot name the member")
	}
	if len(instance) > maxInstanceTags {
		t.Errorf("%d tags, over AWS's %d", len(instance), maxInstanceTags)
	}
}

// TestV0146ParamsMemberLaunchesWithSpored is #1368: a member whose params are
// a v0.146.0 CLI's launches, with the spored bootstrap and full tag set this
// Lambda rendered from them, rather than being refused.
func TestV0146ParamsMemberLaunchesWithSpored(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	in, err := buildRunInstancesInput(legacyState(), v0146Params(true, "4h"), 2, "t3.micro", now)
	if err != nil {
		t.Fatalf("a v0.146.0 member was refused: %v", err)
	}
	assertLegacyMemberRunsSpored(t, in, now, 4*time.Hour)
	tags := tagMap(in.TagSpecifications[0].Tags)
	if tags["spawn:ttl"] != "4h" {
		t.Errorf("spawn:ttl = %q, want 4h", tags["spawn:ttl"])
	}
	if tags["spawn:spot-request-type"] != "one-time" {
		t.Errorf("spawn:spot-request-type = %q, want one-time: this Lambda makes one-time requests", tags["spawn:spot-request-type"])
	}
	if awssdk.ToString(in.ImageId) != "ami-0123456789abcdef0" || awssdk.ToString(in.KeyName) != "alice-key" ||
		awssdk.ToString(in.IamInstanceProfile.Name) != "spawn-instance-abc123" {
		t.Errorf("the member lost its ami, key or profile: %q %q %q", awssdk.ToString(in.ImageId),
			awssdk.ToString(in.KeyName), awssdk.ToString(in.IamInstanceProfile.Name))
	}
	if in.InstanceMarketOptions == nil || in.InstanceMarketOptions.MarketType != ec2types.MarketTypeSpot {
		t.Error("the member's bare spot flag was dropped (#1367's legacy path)")
	}
}

// TestScheduleReplayedV0146ParamsMemberLaunchesWithSpored: a schedule a
// v0.146.0 CLI made replays its params on every firing, through
// scheduler-handler's YAML decode and sweep.EncodeParams, so it stays legacy
// for its whole life. Its members launch with spored too.
func TestScheduleReplayedV0146ParamsMemberLaunchesWithSpored(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	row := v0146Params(false, "90m")
	delete(row, "instance_type")
	stored, err := json.Marshal(map[string]interface{}{
		"defaults": map[string]interface{}{"instance_type": "t3.micro"},
		"params":   []map[string]interface{}{{}, {}, row},
	})
	if err != nil {
		t.Fatal(err)
	}
	var handler struct {
		Defaults map[string]interface{}   `yaml:"defaults"`
		Params   []map[string]interface{} `yaml:"params"`
	}
	if err := yaml.Unmarshal(stored, &handler); err != nil {
		t.Fatal(err)
	}
	body, err := sweep.EncodeParams(&sweep.ParamFileFormat{Defaults: handler.Defaults, Params: handler.Params})
	if err != nil {
		t.Fatal(err)
	}
	var params ParamFileFormat
	if err := json.Unmarshal(body, &params); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	in, err := buildRunInstancesInput(legacyState(), mergeParams(params.Defaults, params.Params[2]), 2, "t3.micro", now)
	if err != nil {
		t.Fatalf("a schedule-replayed v0.146.0 member was refused: %v", err)
	}
	assertLegacyMemberRunsSpored(t, in, now, 90*time.Minute)
}

// A legacy member with no ttl gets a CLI sweep member's 1h idle timeout, so
// nothing runs unbounded; on Spot its idle action is terminate, since EC2
// cannot stop an instance from the one-time request this Lambda makes. With no
// iam_role it gets spored-instance-profile, whose role may write spored's
// heartbeat tag, not v0.146.0's spawnd-role, whose policy has no
// ec2:CreateTags: under spawnd-role spored would run and #1313 would still
// terminate the member five minutes in.
func TestV0146ParamsMemberWithoutTTLOrRole(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	for _, spot := range []bool{false, true} {
		p := v0146Params(spot, "")
		delete(p, "iam_role")
		in, err := buildRunInstancesInput(legacyState(), p, 2, "t3.micro", time.Now())
		if err != nil {
			t.Fatalf("spot=%t: %v", spot, err)
		}
		tags := tagMap(in.TagSpecifications[0].Tags)
		if tags["spawn:idle-timeout"] != "1h" {
			t.Errorf("spot=%t: spawn:idle-timeout = %q, want 1h", spot, tags["spawn:idle-timeout"])
		}
		if _, deadline := tags["spawn:ttl-deadline"]; deadline {
			t.Errorf("spot=%t: a member with no ttl got a deadline", spot)
		}
		if got := tags["spawn:terminate-on-idle"] == "true"; got != spot {
			t.Errorf("spot=%t: terminate-on-idle = %t", spot, got)
		}
		if spot && tags["spawn:spot-request-type"] != "one-time" {
			t.Errorf("spawn:spot-request-type = %q, want one-time", tags["spawn:spot-request-type"])
		}
		if got := awssdk.ToString(in.IamInstanceProfile.Name); got != spawnaws.SporedInstanceProfileName {
			t.Errorf("spot=%t: profile = %q, want %s", spot, got, spawnaws.SporedInstanceProfileName)
		}
	}
}

// #1313 terminates a running member with no spawn:last-heartbeat, which only
// spored writes. A legacy member is not exempt from that check; it passes it
// by running spored: the stub runs only the body whose sha256 it carries,
// which is BootstrapBody, which installs and starts spored.
func TestLegacyMemberBootsSpored(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	in, err := buildRunInstancesInput(legacyState(), v0146Params(false, "1h"), 2, "t3.micro", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`SPAWN_BS_SHA256=([0-9a-f]{64})`).FindStringSubmatch(decodeUserData(t, in))
	if m == nil {
		t.Fatal("the legacy member's user data verifies no body")
	}
	body := launcher.BootstrapBody()
	if sum := sha256.Sum256([]byte(body)); hex.EncodeToString(sum[:]) != m[1] {
		t.Fatal("the body the stub runs is not BootstrapBody")
	}
	if !regexp.MustCompile(`(?m)^systemctl start spored$`).MatchString(body) {
		t.Error("the body the legacy member runs does not start spored")
	}
}

// A plain `go build`, with no version injected, names no release, so the body
// is inline, as on any dev build. scripts/deploy-sweep-orchestrator.sh never
// deploys one.
func TestDevOrchestratorInlinesTheBody(t *testing.T) {
	releaseOrchestrator(t, "")
	in, err := buildRunInstancesInput(legacyState(), v0146Params(false, "1h"), 2, "t3.micro", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s := decodeUserData(t, in); strings.Contains(s, "SPAWN_BS_VERSION=") || !strings.Contains(s, "systemctl start spored") {
		t.Error("a dev orchestrator's legacy member does not carry the body inline")
	}
}

// #1412: a legacy member installs the spored of this orchestrator's release,
// the one its bootstrap body is pinned to, and a dev orchestrator the latest.
// The spored the orchestrator's spawn:version tag names is the one it runs.
func TestLegacyMemberInstallsTheOrchestratorsSpored(t *testing.T) {
	pin := regexp.MustCompile(`(?m)^` + sporedinstall.PinVar + `='([^']*)'$`)
	for version, want := range map[string]string{"0.147.0": "0.147.0", "": ""} {
		releaseOrchestrator(t, version)
		in, err := buildRunInstancesInput(legacyState(), v0146Params(false, "1h"), 2, "t3.micro", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		m := pin.FindAllStringSubmatch(decodeUserData(t, in), -1)
		if len(m) != 1 || m[0][1] != want {
			t.Errorf("a %q orchestrator's legacy member pins spored to %v, want exactly one pin to %q", version, m, want)
		}
	}
}

// Params carrying the CLI's bootstrap and tags launch with exactly those:
// nothing rendered here replaces or joins them.
func TestStrictParamsAreUnchanged(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	shipped := cliTags(map[string]string{"spawn:version": "0.147.3", "spawn:ttl": "2h"})
	config := map[string]interface{}{
		"ami": "ami-1", "instance_type": "t3.micro", userDataParam: "H4sIAAAAAAAA/cli",
		tagsParam: shipped, launchSettingsParam: cliSettings(t, nil),
	}
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	in, err := buildRunInstancesInput(&SweepRecord{SweepID: "sw-20261026", SweepName: "sw"}, config, 3, "t3.micro", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := awssdk.ToString(in.UserData); got != "H4sIAAAAAAAA/cli" {
		t.Errorf("UserData = %q, want the CLI's verbatim", got)
	}
	want := mustMemberTags(t, config, now)
	if got := in.TagSpecifications[0].Tags; len(got) != len(want) || tagMap(got)["spawn:version"] != "0.147.3" {
		t.Errorf("tags = %v, want the CLI's as memberTags anchors them: %v", tagMap(got), tagMap(want))
	}
	for k, v := range tagMap(want) {
		if tagMap(in.TagSpecifications[0].Tags)[k] != v {
			t.Errorf("tag %s changed from %q", k, v)
		}
	}
}

// Params carrying only one of the two keys are refused, whichever it is: no
// spawn writes one without the other, and pairing a CLI's bootstrap with tags
// rendered here (or the reverse) boots a member against tags it was not
// rendered for.
func TestPartialParamsAreRefused(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	for name, extra := range map[string]map[string]interface{}{
		"bootstrap only": {userDataParam: "H4sIAAAAAAAA/cli"},
		"tags only":      {tagsParam: cliTags(nil)},
	} {
		config := v0146Params(false, "1h")
		for k, v := range extra {
			config[k] = v
		}
		in, err := buildRunInstancesInput(legacyState(), config, 1, "t3.micro", time.Now())
		if err == nil || in != nil {
			t.Errorf("%s: launched", name)
			continue
		}
		if !strings.Contains(err.Error(), userDataParam) || !strings.Contains(err.Error(), tagsParam) || !strings.Contains(err.Error(), "param set 1") {
			t.Errorf("%s: error %q does not name the member and both keys", name, err)
		}
		if capacity, _ := isCapacityError(err); capacity {
			t.Errorf("%s: refusal reads as a capacity error", name)
		}
	}
}

// The release the deploy script injects reaches pkg/launcher: an init calls
// applyVersion, which hands it over. Without that, a release orchestrator
// would inline the body (a replaced spawn names no release) and its stub would
// name no version.
func TestInitHandsTheVersionToTheLauncher(t *testing.T) {
	src := readMainForTest(t)
	inits := regexp.MustCompile(`(?ms)^func init\(\) \{\n(.*?)^\}`).FindAllStringSubmatch(src, -1)
	if len(inits) == 0 {
		t.Fatal("found no init; the matcher is stale")
	}
	called := 0
	for _, m := range inits {
		called += len(regexp.MustCompile(`(?m)^\s*applyVersion\(\)$`).FindAllString(m[1], -1))
	}
	if called != 1 {
		t.Errorf("%d inits call applyVersion(), want 1", called)
	}
	if !regexp.MustCompile(`(?m)^func applyVersion\(\) \{\n\s*launcher\.CallerVersion = Version\n\}`).MatchString(src) {
		t.Error("applyVersion does not assign launcher.CallerVersion = Version")
	}
}
