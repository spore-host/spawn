// Package reaperconfig is the ttl-reaper's environment contract: the names of
// the settings a deployment may set and the values the reaper accepts for them.
//
// It exists so the reaper and `spawn reaper deploy` share one definition
// (spawn#1154). The reaper (lambda/ttl-reaper, a separate Go module) parses its
// environment with these functions, and the CLI validates its flags with the
// same ones, so a value the CLI accepts is by construction a value the reaper
// understands. Before this, the CLI could not set these at all, and a value
// typed by hand was checked by nothing until the reaper silently fell back to
// its default.
//
// It deliberately imports nothing but pkg/ttl: the reaper links it, and a Lambda
// should not pull in a deploy tool's AWS clients to parse four variables.
package reaperconfig

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/ttl"
)

// The settings `spawn reaper deploy` can set.
const (
	EnvSpotRequests = "REAPER_SPOT_REQUESTS"
	EnvNetResources = "REAPER_NET_RESOURCES"
	EnvElasticIPs   = "REAPER_ELASTIC_IPS"
	EnvMaxAge       = "REAPER_MAX_AGE"
	EnvNotifyURL    = "REAPER_NOTIFY_URL"
	EnvTTLGrace     = "REAPER_TTL_GRACE"
	// EnvEmptyBuckets is the empty spawn-staging / spawn-results bucket sweep
	// (#1301): off|report|reap, default report.
	EnvEmptyBuckets = "REAPER_EMPTY_BUCKETS"
	// EnvInstanceRoles is the per-task spawn-instance-* IAM role sweep
	// (#1300). Unset means report, not reap: it is new deletion behaviour.
	EnvInstanceRoles = "REAPER_INSTANCE_ROLES"
	// EnvPoolQueues is the abandoned pool queue and staged-spec sweep
	// (#1351). Unset means report, not reap: it is new deletion behaviour.
	EnvPoolQueues = "REAPER_POOL_QUEUES"
	// EnvScheduleParams is the orphaned schedule params sweep (#1398).
	// Unset means report, not reap: it is new deletion behaviour.
	EnvScheduleParams = "REAPER_SCHEDULE_PARAMS"
	// EnvLaunchConfigs is the unreferenced launch config object sweep
	// (#1359). Unset means report, not reap: it is new deletion behaviour.
	EnvLaunchConfigs = "REAPER_LAUNCH_CONFIGS"
)

// The three states of a sweep that can be previewed before it is allowed to
// delete: REAPER_SPOT_REQUESTS, REAPER_NET_RESOURCES, REAPER_ELASTIC_IPS,
// REAPER_EMPTY_BUCKETS, REAPER_INSTANCE_ROLES, REAPER_POOL_QUEUES,
// REAPER_SCHEDULE_PARAMS and REAPER_LAUNCH_CONFIGS.
const (
	// ModeOff skips the sweep entirely.
	ModeOff = "off"
	// ModeReport logs what the sweep WOULD reclaim and changes nothing. The
	// reaper's default, and what any unrecognised value becomes.
	ModeReport = "report"
	// ModeReap lets the sweep act. Explicit opt-in.
	ModeReap = "reap"
)

// Modes lists the accepted sweep modes, for help text and error messages.
func Modes() []string { return []string{ModeOff, ModeReport, ModeReap} }

// ParseMode maps a sweep-mode value onto its canonical form. ok is false for
// anything unrecognised, including the empty string; the reaper treats that as
// ModeReport (a typo must neither arm a sweep nor silently disable it), and the
// CLI rejects it, because a value typed at a prompt can be corrected and one in
// a Lambda environment cannot.
func ParseMode(v string) (mode string, ok bool) {
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case ModeOff, ModeReport, ModeReap:
		return m, true
	}
	return "", false
}

// ParseMaxAge parses REAPER_MAX_AGE: a pkg/ttl duration ("72h", "3d") that must
// be positive.
func ParseMaxAge(v string) (time.Duration, error) {
	d, err := ttl.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("max age must be positive, got %s", d)
	}
	return d, nil
}

// DefaultTTLGrace is how long past spawn:ttl-deadline the reaper waits before
// reaping a running instance whose spored is still checking in (#1186).
//
// spored enforces the deadline itself and runs the task flush hook on the way
// out, which takes up to 45 s plus a warning delay. A reaper run that lands in
// that window used to terminate the instance under spored's feet, and the
// task's command.log and completion record were lost with it. A fresh
// spawn:last-heartbeat is the evidence that spored is alive to do the job.
const DefaultTTLGrace = 10 * time.Minute

// MaxTTLGrace caps REAPER_TTL_GRACE. The grace only applies while the
// instance's own heartbeat is fresh, and that tag is instance-writable, so a
// large grace would let an instance extend its life past its deadline by
// asserting it is alive. An hour bounds that.
const MaxTTLGrace = time.Hour

// ParseTTLGrace parses REAPER_TTL_GRACE: a pkg/ttl duration from 0 (no grace:
// reap at the deadline, as before #1186) to MaxTTLGrace.
func ParseTTLGrace(v string) (time.Duration, error) {
	d, err := ttl.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, err
	}
	if d < 0 || d > MaxTTLGrace {
		return 0, fmt.Errorf("TTL grace must be between 0 and %s, got %s", MaxTTLGrace, d)
	}
	return d, nil
}

// ValidateNotifyURL checks REAPER_NOTIFY_URL, which the reaper POSTs its
// notifications to. The URL is a secret (it is usually a webhook whose path is
// the credential), so no error here ever contains it.
func ValidateNotifyURL(v string) error {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil {
		return errors.New("notify URL does not parse")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("notify URL must be http:// or https://")
	}
	if u.Host == "" {
		return errors.New("notify URL has no host")
	}
	return nil
}

// Validate checks a value for one of the settable variables. An empty value is
// valid for every one of them: it means "remove the variable", which returns
// the reaper to its own default.
func Validate(name, value string) error {
	if strings.TrimSpace(value) == "" {
		if _, ok := settable[name]; ok {
			return nil
		}
	}
	switch name {
	case EnvSpotRequests, EnvNetResources, EnvElasticIPs, EnvEmptyBuckets, EnvInstanceRoles, EnvPoolQueues, EnvScheduleParams, EnvLaunchConfigs:
		if _, ok := ParseMode(value); !ok {
			return fmt.Errorf("%s: %q is not one of %s", name, value, strings.Join(Modes(), ", "))
		}
		return nil
	case EnvMaxAge:
		if _, err := ParseMaxAge(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	case EnvNotifyURL:
		if err := ValidateNotifyURL(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	case EnvTTLGrace:
		if _, err := ParseTTLGrace(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("%s is not a setting spawn reaper deploy can set", name)
}

var settable = map[string]struct{}{
	EnvSpotRequests: {}, EnvNetResources: {}, EnvElasticIPs: {}, EnvMaxAge: {}, EnvNotifyURL: {},
	EnvTTLGrace: {}, EnvEmptyBuckets: {}, EnvInstanceRoles: {}, EnvPoolQueues: {},
	EnvScheduleParams: {}, EnvLaunchConfigs: {},
}

// Settable lists the variables Validate accepts, sorted. Derived from the same
// set Validate consults, so the two cannot disagree: a variable added to one
// copy and not the other was settable by `spawn reaper deploy` and missing from
// its flags and its census tests, or the reverse.
func Settable() []string {
	out := make([]string, 0, len(settable))
	for k := range settable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Secret reports whether a variable's value must never be printed.
func Secret(name string) bool { return name == EnvNotifyURL }

// EnvRegions is the comma-separated list of regions the reaper scans.
const EnvRegions = "REAPER_REGIONS"

// DefaultRegions is the set the reaper scans when REAPER_REGIONS is unset. It
// mirrors the 11 regions the release workflow publishes spored to
// (spawn-binaries-<region>), i.e. where spawn actually operates.
//
// It lives here, not in the reaper, so the CLI's coverage check (#1157) reads
// the same answer the reaper acts on: a launch in a region the reaper does not
// scan is not covered by it, wherever the function happens to live.
func DefaultRegions() []string {
	return []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"ca-central-1",
		"eu-west-1", "eu-west-2", "eu-central-1",
		"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
	}
}

// ParseRegions is how the reaper reads REAPER_REGIONS: comma-separated and
// trimmed, with an unset or empty value meaning DefaultRegions.
func ParseRegions(env string) []string {
	var out []string
	for _, p := range strings.Split(env, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return DefaultRegions()
	}
	return out
}
