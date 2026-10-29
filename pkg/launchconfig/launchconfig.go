// Package launchconfig is the per-launch config object (#1359): the spored
// settings a launch fixes, which used to be instance tags and are now one JSON
// object the launcher writes and spored reads once.
//
// # Why
//
// EC2 tags were spored's configuration file. They share AWS's 50-per-resource
// limit with spored's own runtime tags and the user's --tag values, cap a value
// at 256 characters, and are readable by every principal in the account that can
// call ec2:DescribeInstances or DescribeTags, which is how the webhook and notify
// URLs (often carrying a token) were exposed account-wide.
//
// # What moves
//
// A key moves only if spored alone reads it, at boot, and nothing changes it
// after the launch. [Settings] lists them. Everything else stays a tag:
// whatever `spawn extend` or `spored config set` edits, spored's runtime and
// status tags, and every key the reaper, `spawn list`/`status`/`connect`/
// `cost`, the dashboard, the sweep and job-array machinery, Prometheus' EC2
// discovery or any Lambda reads off the instance. [TagKeys] records why each
// stays. TestEveryLaunchTagIsClassified (pkg/aws) fails on a key in neither.
//
// # The contract
//
// A same-version contract, as spored is pinned to the launching CLI's version
// (#1412): a spawn that writes the object installs a spored that reads it, and
// an older spawn keeps writing tags and installing its older spored, which
// reads them. Nothing is written twice and spored has no tag fallback for a
// moved key, so there is one source for each setting. A development build of
// spawn installs the latest released spored, which reads the object from the
// first release that has it ([FirstSporedVersion]).
//
// # Where
//
// In the account's staging bucket for the launch region,
// spawn-staging-<account>-<region> (#1274/#1293: private, encrypted, never
// listable by an instance role), at [Key]: launch-config/config-<sha256>.json,
// content-addressed. The instance carries one tag, spawn:config=<sha256>
// ([Tag]). spored fetches the object at first boot, refuses it unless it hashes
// to the tag, and caches it on the root volume ([CachePath], mode 0600), so a
// reboot, stop/start or hibernation never fetches again and `spored reload`
// re-reads only the tags. A detached sweep's member, launched by the
// sweep-orchestrator Lambda long after the CLI exited, gets the same bytes
// seeded into that cache by its user data instead (pkg/launcher).
//
// # Lifecycle
//
// The TTL reaper deletes an object once no instance and no open Spot request
// in its region carries spawn:config=<its sha> (Sweep, REAPER_LAUNCH_CONFIGS,
// report by default). A lifecycle rule on the staging bucket expires every
// object under [KeyPrefix] after [ObjectMaxDays] as the backstop, long past
// the reaper's 7-day max-age ceiling, so the object is still there when a
// persistent Spot request's replacement boots with a fresh root volume and
// reads its notification settings from it (#1080).
//
// Kept free of pkg/aws so spored and the TTL reaper (a nested module) can
// import it.
package launchconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// Schema is the version of the object's shape this spawn writes and reads. An
// object with any other schema is refused: under the same-version contract
// spored only ever meets objects its own release wrote.
const Schema = 1

// TagSuffix is the tag naming the object, under spored's tag prefix:
// spawn:config=<sha256>.
const TagSuffix = "config"

// Tag is the tag a launch writes: spawn:config. A literal, not "spawn:" +
// TagSuffix, so the tag-contract scan sees the key (TestTagIsTheSuffix holds
// the two together).
const Tag = "spawn:config"

// KeyPrefix is where every config object lives in the staging bucket. The
// bucket's backstop lifecycle rule, the instance role's s3:GetObject grant and
// the reaper's list and delete grants are all scoped to it.
const KeyPrefix = "launch-config/"

// ObjectMaxDays is how long the staging bucket keeps a config object if the
// reaper never deletes it: past the reaper's default 7-day max-age ceiling,
// which no spawn instance outlives, and the persistent Spot request a launch
// may hold for as long as its instance's TTL. The reaper's sweep is what
// removes an object once nothing references it; this only bounds what an
// unswept region keeps.
const ObjectMaxDays = 30

// FirstSporedVersion is the first spored release that reads the object. An
// older spored reads these settings from tags, so `spawn upgrade-spored`
// refuses to cross this version in either direction on an instance whose
// settings are in the other place.
const FirstSporedVersion = "0.150.0"

// sha256HexLen is the length of a lowercase hex sha256.
const sha256HexLen = 64

// The settings, by the name each had as a tag (without the spawn: prefix).
// Values are the strings the tag carried, so spored parses them exactly as it
// parsed the tag.
const (
	ActivePorts          = "active-ports"
	ActiveProcesses      = "active-processes"
	AppMode              = "app-mode"
	ReadyPort            = "ready-port"
	ReadyPath            = "ready-path"
	WebAuth              = "web-auth"
	SpotWebhookURL       = "spot-webhook-url"
	CompletionWebhookURL = "completion-webhook-url"
	WebhookCorrelation   = "webhook-correlation"
	WebhookTimeout       = "webhook-timeout"
	NotifyURL            = "notify-url"
	NotifyCommand        = "notify-command"
	NotifyPlatform       = "notify-platform"
	SlackWorkspaceID     = "slack-workspace-id"
	IdleCPU              = "idle-cpu"
	MetricsPort          = "metrics-port"
	MetricsBind          = "metrics-bind"
	MetricsPath          = "metrics-path"
	TracingEnabled       = "tracing-enabled"
	TracingExporter      = "tracing-exporter"
	TracingSampling      = "tracing-sampling"
)

// Settings are the keys that live in the object and nowhere else: each read
// only by spored, only at boot, and never changed after the launch. The reason
// is the evidence from the reader audit (#1359).
var Settings = map[string]string{
	ActivePorts:          "spored's idle detector only (pkg/provider)",
	ActiveProcesses:      "spored's idle detector only",
	AppMode:              "spored's web-app proxy only (#590)",
	ReadyPort:            "spored's web-app readiness probe only",
	ReadyPath:            "spored's web-app readiness probe only",
	WebAuth:              "spored's web-app proxy only",
	SpotWebhookURL:       "spored's Spot-interruption webhook only (#228); URL-bearing",
	CompletionWebhookURL: "spored's completion webhook only (#497); URL-bearing",
	WebhookCorrelation:   "spored's webhooks only",
	WebhookTimeout:       "spored's webhooks only",
	NotifyURL:            "spored's notifier, and a Spot replacement's (from its request's tags); URL-bearing",
	NotifyCommand:        "spored's notifier only",
	NotifyPlatform:       "spored's notifier only",
	SlackWorkspaceID:     "spored's notifier only",
	IdleCPU:              "spored's idle detector only (#1308)",
	MetricsPort:          "spored's metrics server only (#1308); Prometheus' documented EC2 discovery scrapes a fixed port",
	MetricsBind:          "spored's metrics server only (#1308)",
	MetricsPath:          "spored's metrics server only (#1308)",
	TracingEnabled:       "spored's tracer only (#1308)",
	TracingExporter:      "spored's tracer only (#1308)",
	TracingSampling:      "spored's tracer only (#1308)",
}

// SettingKeys is [Settings]' keys, sorted.
func SettingKeys() []string {
	out := make([]string, 0, len(Settings))
	for k := range Settings {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsSetting reports whether key, with or without the spawn: prefix, is one of
// [Settings].
func IsSetting(key string) bool {
	_, ok := Settings[strings.TrimPrefix(key, "spawn:")]
	return ok
}

// Why a key a launch writes stays a tag.
const (
	StaysEdited    = "edited after launch (spawn extend / spored config set)"
	StaysFiltered  = "filtered or read off the instance by the reaper, a Lambda, list/status/cost or the dashboard"
	StaysCLI       = "read off the instance by a CLI command (connect, status, cleanup, sweep, array, fsx)"
	StaysMachinery = "sweep, job-array or MPI machinery, or spored's runtime state"
	StaysBootstrap = "read by the boot script or a user's instance script, not by spored"
	StaysObject    = "names the launch config object itself"
)

// TagKeys are the keys a launch writes that stay tags, by name without the
// spawn: prefix, and the reason from the reader audit (#1359). spawn:param:*
// is the sweep parameters' prefix.
var TagKeys = map[string]string{
	"managed":            StaysFiltered, // reaper, IAM conditions, every inventory
	"root":               StaysFiltered,
	"created-by":         StaysFiltered,
	"account-id":         StaysCLI,
	"account-base36":     StaysFiltered, // reaper, dns-updater, connect
	"account-name":       StaysFiltered, // dns-updater, reaper
	"iam-user":           StaysFiltered, // dashboard, cost-history-collector, cleanup
	"version":            StaysFiltered, // reaper skew report, list-amis
	"launch-time":        StaysFiltered, // reaper, extend, status, orchestrators
	"os":                 StaysCLI,      // connect, ssh, plugin, create-ami
	"ttl":                StaysEdited,   // extend, spored config set; reaper, list, dashboard
	"ttl-deadline":       StaysEdited,
	"cost-limit":         StaysEdited,
	"idle-timeout":       StaysEdited,
	"on-complete":        StaysEdited,
	"completion-file":    StaysEdited,
	"completion-delay":   StaysEdited,
	"hibernate-on-idle":  StaysEdited,
	"terminate-on-idle":  StaysCLI,      // status idle
	"dns-name":           StaysFiltered, // dashboard, reaper, dns-updater, connect
	"dns-domain":         StaysFiltered, // reaper, connect, dns
	"dns-endpoint":       StaysCLI,      // spawn dns
	"command":            StaysMachinery,
	"dcv-session-id":     StaysCLI,      // connect
	"app-name":           StaysCLI,      // connect
	"fsx-id":             StaysFiltered, // reaper FSx refcount, status
	"fsx-pending":        StaysFiltered, // reaper, written after launch
	"fsx-mount-point":    StaysCLI,      // fsx lease, written after launch
	"fsx-mount-name":     StaysCLI,
	"fsx-s3-import-path": StaysCLI, // spawn fsx, written after launch
	"fsx-s3-export-path": StaysCLI,
	"efs-id":             StaysCLI,       // status billable
	"efs-mount-point":    StaysBootstrap, // instance scripts (#314)
	"session-timeout":    StaysBootstrap, // the boot script's describe-tags, and the dashboard
	"spot-request-type":  StaysFiltered,  // dashboard, stop, status, reaper
	"spot-valid-until":   StaysCLI,       // extend, start
	"pre-stop":           StaysFiltered,  // the reaper runs it before an external terminate
	"pre-stop-timeout":   StaysFiltered,
	"local-username":     StaysCLI,      // connect, ssh, queue, plugin, reaper
	"price-per-hour":     StaysFiltered, // cost, status, sweep-orchestrator
	"price-source":       StaysCLI,      // provenance
	"metrics-enabled":    StaysFiltered, // Prometheus' EC2 service discovery filter (deployment/prometheus)
	"job-array-id":       StaysMachinery,
	"job-array-name":     StaysMachinery,
	"job-array-size":     StaysMachinery,
	"job-array-index":    StaysMachinery,
	"job-array-created":  StaysMachinery,
	"sweep-id":           StaysMachinery,
	"sweep-name":         StaysMachinery,
	"sweep-size":         StaysMachinery,
	"sweep-index":        StaysMachinery,
	"param:":             StaysMachinery, // spawn:param:<name>
	TagSuffix:            StaysObject,
}

// Classify says where a key a launch writes lives: "object" for one of
// [Settings], or the reason it stays a tag. ok is false for a key in neither,
// which is a key nobody has decided about.
func Classify(key string) (where string, ok bool) {
	k := strings.TrimPrefix(key, "spawn:")
	if why, ok := Settings[k]; ok {
		_ = why
		return "object", true
	}
	if strings.HasPrefix(k, "param:") {
		return TagKeys["param:"], true
	}
	why, ok := TagKeys[k]
	return why, ok
}

// Document is the object's JSON.
type Document struct {
	// Schema is [Schema].
	Schema int `json:"schema"`
	// Settings are [Settings] keys to the values their tags carried.
	Settings map[string]string `json:"settings"`
}

// Encode returns the object for settings, its canonical bytes and their
// sha256. Empty values are dropped; nil and an empty map have no object
// (body nil, sha ""), and a launch writes no tag for them. A key that is not
// one of [Settings] is an error: it would be a setting no spored reads.
//
// The bytes are canonical (encoding/json sorts map keys), so the same settings
// always hash the same and two launches with the same settings share one
// object.
func Encode(settings map[string]string) (body []byte, sha string, err error) {
	clean := make(map[string]string, len(settings))
	for k, v := range settings {
		if _, ok := Settings[k]; !ok {
			return nil, "", fmt.Errorf("launch config object: %q is not a launch-fixed spored setting", k)
		}
		if v != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return nil, "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Document{Schema: Schema, Settings: clean}); err != nil {
		return nil, "", fmt.Errorf("launch config object: %w", err)
	}
	body = bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return body, SHA256Hex(body), nil
}

// ErrDigest is a body whose sha256 is not the one the instance's tag names: a
// tampered or corrupted cache, or the wrong object.
var ErrDigest = errors.New("launch config object does not hash to the instance's spawn:config tag")

// Decode verifies that body hashes to wantSHA and parses it. A body of any
// other hash is [ErrDigest], and nothing in it is read. A schema other than
// [Schema] is refused. Keys this release does not know are returned in
// unknown and left out of the document, rather than failing it: they can only
// come from a newer development build, and the settings this spored does know
// are still the launch's.
func Decode(body []byte, wantSHA string) (doc *Document, unknown []string, err error) {
	if !ValidSHA(wantSHA) {
		return nil, nil, fmt.Errorf("launch config object: %q is not a sha256", wantSHA)
	}
	if got := SHA256Hex(body); got != wantSHA {
		return nil, nil, fmt.Errorf("%w (sha256 %s, want %s)", ErrDigest, got, wantSHA)
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, nil, fmt.Errorf("launch config object: %w", err)
	}
	if d.Schema != Schema {
		return nil, nil, fmt.Errorf("launch config object has schema %d; this spored reads schema %d", d.Schema, Schema)
	}
	for k := range d.Settings {
		if _, ok := Settings[k]; !ok {
			unknown = append(unknown, k)
			delete(d.Settings, k)
		}
	}
	sort.Strings(unknown)
	if d.Settings == nil {
		d.Settings = map[string]string{}
	}
	return &d, unknown, nil
}

// SHA256Hex is the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidSHA reports whether s is a lowercase hex sha256, the only thing a
// spawn:config tag may hold.
func ValidSHA(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Key is the object's key in the staging bucket.
func Key(sha string) string {
	return KeyPrefix + "config-" + sha + ".json"
}

// SHAOfKey is the sha256 a config object's key names, and false for any other
// key.
func SHAOfKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, KeyPrefix+"config-")
	if !ok {
		return "", false
	}
	sha, ok := strings.CutSuffix(rest, ".json")
	if !ok || !ValidSHA(sha) {
		return "", false
	}
	return sha, true
}

// Bucket is the staging bucket the object for a launch in account/region is
// in.
func Bucket(account, region string) string {
	return taskproto.StagingBucket(account, region)
}

// ObjectARNPattern is the IAM resource every spored role may s3:GetObject: a
// config object in any of account's staging buckets, and nothing else in them.
// Each '?' is one hex digit of the sha256. account may be a placeholder bound
// later (pkg/aws.bindPolicyAccount).
func ObjectARNPattern(account string) string {
	return "arn:aws:s3:::" + taskproto.StagingBucketPrefix + account + "-*/" + KeyPrefix + "config-" + strings.Repeat("?", sha256HexLen) + ".json"
}
