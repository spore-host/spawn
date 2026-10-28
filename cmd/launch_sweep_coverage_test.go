package cmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/pkg/aws"
)

// The launch-flag gate for parameter sweeps (#697, #1366).
//
// sweepLaunchFlags (cmd/launch_sweep_flags.go) gives every `spawn launch` flag
// a rule: it reaches the members, steers the sweep or the CLI, or is refused.
// These tests discover the flag set from the cobra command, so a new flag fails
// until it is decided, and they check each claim against the code: a flag said
// to reach the members must change what a foreground member is launched with,
// through a field that is carried, and one said to act only on the CLI must
// change nothing a member is launched with.
//
// This replaces a hand-kept manifest whose "honoured" check only asked whether
// the flag's variable was mentioned in launch_config.go. It called --fsx-create,
// --fsx-recall and --vpc honoured, and they were dropped (#1366).

func TestEveryLaunchFlagHasASweepRule(t *testing.T) {
	names := launchFlagNames(t)
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
		rule, ok := sweepLaunchFlags[n]
		if !ok {
			t.Errorf("--%s has no entry in sweepLaunchFlags: decide whether a parameter sweep carries it to "+
				"its members, reads it itself, or refuses it. Accepting and dropping it is #1366", n)
			continue
		}
		if strings.TrimSpace(rule.why) == "" {
			t.Errorf("--%s: the rule records no reason", n)
		}
		if len(rule.unlessSet) > 0 && rule.kind != sweepRefused {
			t.Errorf("--%s: unlessSet only qualifies a refusal", n)
		}
	}
	for n := range sweepLaunchFlags {
		if !known[n] {
			t.Errorf("sweepLaunchFlags has an entry for --%s, which `spawn launch` no longer has", n)
		}
	}
}

func TestSweepFlagRulesAreEnforced(t *testing.T) {
	for _, n := range launchFlagNames(t) {
		rule, ok := sweepLaunchFlags[n]
		if !ok {
			continue // TestEveryLaunchFlagHasASweepRule reports it
		}
		err := sweepUnsupportedFlags(changedOnly(n))
		switch {
		case rule.kind == sweepRefused && err == nil:
			t.Errorf("--%s is marked refused on a sweep but the guard accepted it", n)
		case rule.kind == sweepRefused && !strings.Contains(err.Error(), "--"+n+": "+rule.why):
			t.Errorf("--%s: the refusal does not name the flag and its reason: %v", n, err)
		case rule.kind != sweepRefused && err != nil:
			t.Errorf("--%s is not marked refused on a sweep but the guard refused it: %v", n, err)
		}
		for _, other := range rule.unlessSet {
			if err := sweepUnsupportedFlags(changedOnly(n, other)); err != nil {
				t.Errorf("--%s with --%s was refused: %v", n, other, err)
			}
		}
	}
	if err := sweepUnsupportedFlags(changedOnly()); err != nil {
		t.Errorf("a sweep with no flags set was refused: %v", err)
	}
}

// flagPrereqs are the flags a flag needs to take effect (or to pass
// buildLaunchConfig's validation), set before the baseline is taken.
var flagPrereqs = map[string][]string{
	"capacity-block": {"reservation-id"}, "efa-interfaces": {"efa"},
	"webhook-correlation": {"spot-webhook-url"}, "webhook-timeout": {"spot-webhook-url"},
	"metrics-port": {"metrics"}, "metrics-bind": {"metrics"}, "metrics-path": {"metrics"},
	"tracing-exporter": {"tracing"}, "tracing-sampling": {"tracing"},
	"pre-stop-timeout": {"pre-stop"}, "completion-file": {"on-complete"}, "notify-platform": {"slack-workspace"},
	"efs-mount-point": {"efs-id"}, "efs-mount-options": {"efs-id"}, "efs-profile": {"efs-id"},
	"fsx-mount-point": {"fsx-id"}, "fsx-skip-validate": {"fsx-id"}, "no-dns": {"dns"},
	"spot-max-price": {"spot"},
}

// sampleFlagValues are values that pass a flag's validation; any other flag
// gets a generic value of its type (sampleFlagValue).
var sampleFlagValues = map[string]string{
	"instance-type": "m7i.xlarge", "region": "us-west-2", "az": "us-east-1c", "ami": "ami-0fedcba9876543210",
	"volume-size": "40", "attach-volume": "snap-0123:/refdata", "key-name": testSweepKeyName, "key-pair": testSweepKeyName,
	"security-group": "sg-1", "security-group-ids": "sg-1", "subnet": "subnet-1", "subnet-id": "subnet-1", "vpc": "vpc-1",
	"efa-interfaces": "2", "placement-group": "pg-1", "spot-max-price": "0.25", "reservation-id": "cr-1",
	"tag": "project=x", "team": "team-1", "name": "box", "dns": "box",
	"ttl": "2h", "idle-timeout": "30m", "on-idle": "hibernate", "pre-stop": "sync", "pre-stop-timeout": "5m",
	"spot-webhook-url": "https://hooks.example.org/spot", "completion-webhook-url": "https://hooks.example.org/done",
	"webhook-correlation": "corr", "webhook-timeout": "2s", "on-complete": "terminate", "completion-file": "/tmp/DONE",
	"completion-delay": "45s", "session-timeout": "10m", "cost-limit": "5", "command": "run.sh",
	"active-ports": "8888", "active-processes": "python", "idle-cpu-threshold": "12.5",
	"metrics-port": "9200", "metrics-bind": "0.0.0.0", "metrics-path": "/m", "tracing-exporter": "stdout", "tracing-sampling": "0.5",
	"slack-workspace": "T1", "notify-platform": "teams", "user-data": "echo sweep-user-data",
	"plugin": "docker", "efs-id": "fs-0123456789abcdef0", "efs-mount-point": "/shared", "efs-mount-options": "timeo=150",
	"efs-profile": "max-io", "fsx-id": "fs-0fsx", "fsx-mount-point": "/lustre",
	"nist-800-53": "low", "template-var": "a=b", "count": "2", "fsx-recall": "my-stack",
}

// sampleFlagValue is a value for --name that its validation accepts.
func sampleFlagValue(t *testing.T, name string) string {
	t.Helper()
	if v, ok := sampleFlagValues[name]; ok {
		return v
	}
	f := launchCmd.Flags().Lookup(name)
	if f == nil {
		f = launchCmd.InheritedFlags().Lookup(name)
	}
	if f == nil {
		t.Fatalf("no flag --%s", name)
	}
	switch f.Value.Type() {
	case "bool":
		return "true"
	case "int", "int32", "int64", "float64":
		return "3"
	case "stringToString":
		return "a=b"
	}
	return "sample-" + name
}

// setLaunchFlag sets a launch flag as the command line would, for a test
// under keepLaunchGlobals. A slice or map flag is set with Replace rather than
// Set: pflag's own Set on one marks an internal "changed" that Replace never
// clears, so every later Set appends to it, and keepLaunchGlobals cannot put
// that back.
func setLaunchFlag(t *testing.T, name, value string) {
	t.Helper()
	f := launchCmd.Flags().Lookup(name)
	if f == nil {
		t.Fatalf("no flag --%s", name)
	}
	if sv, ok := f.Value.(pflag.SliceValue); ok {
		if err := sv.Replace([]string{value}); err != nil {
			t.Fatalf("set --%s=%s: %v", name, value, err)
		}
		f.Changed = true
		return
	}
	if f.Value.Type() == "stringToString" {
		t.Fatalf("--%s: setLaunchFlag cannot set a map flag and put it back; mark it Changed instead", name)
	}
	if err := launchCmd.Flags().Set(name, value); err != nil {
		t.Fatalf("set --%s=%s: %v", name, value, err)
	}
}

// stubTeamName keeps --team from asking DynamoDB for the team's name.
func stubTeamName(t *testing.T) {
	t.Helper()
	prev := resolveTeamNameFn
	t.Cleanup(func() { resolveTeamNameFn = prev })
	resolveTeamNameFn = func(context.Context, string) (string, error) { return "", nil }
}

// sweepFlagMember is the first member of a one-row foreground sweep launched
// with the launch flags as they are now set, and its user-data script.
func sweepFlagMember(t *testing.T) (aws.LaunchConfig, string) {
	t.Helper()
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatalf("sweepBaseConfig: %v", err)
	}
	pf := &ParamFileFormat{Params: []map[string]interface{}{{"trial": "a"}}}
	cfgs, err := foregroundMembers(t, testSweepPlat(t), *base, pf, &fakeStorageLookups{})
	if err != nil {
		t.Fatalf("build the member: %v", err)
	}
	cfg := *cfgs[0]
	script := string(decodeUserDataOnce(t, cfg.UserData))
	cfg.UserData = ""
	return cfg, script
}

// changedConfigFields names the LaunchConfig fields that differ.
func changedConfigFields(a, b aws.LaunchConfig) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	var out []string
	for i := 0; i < va.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

// TestSweepFlagRulesHoldAgainstTheCode checks each flag's claim on a real
// foreground member: set the flag (after its prerequisites), build the member
// the way launchParameterSweep does, and compare it with the member built
// without the flag.
//
//   - a flag said to reach the members through their config must change it,
//     and every field it changes must be one a foreground member carries
//     (foregroundDispositions), never one it drops or refuses;
//   - one said to reach them through the user-data builder must change the
//     member's user data;
//   - one said to act on the CLI alone must change neither;
//   - one said to steer the sweep must be read by the sweep path's own code.
func TestSweepFlagRulesHoldAgainstTheCode(t *testing.T) {
	steering := sweepSteeringSource(t)
	vars := launchCmdFlagVars(t)
	for _, n := range launchFlagNames(t) {
		rule, ok := sweepLaunchFlags[n]
		if !ok || rule.kind == sweepRefused {
			continue
		}
		if rule.kind == sweepSteers {
			v, ok := vars[n]
			if !ok {
				t.Errorf("--%s steers the sweep, but binds no variable the sweep path could read", n)
			} else if !regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\b`).MatchString(steering) {
				t.Errorf("--%s (var %s) is said to steer the sweep, but the sweep path never reads it", n, v)
			}
			continue
		}
		t.Run(n, func(t *testing.T) {
			keepLaunchGlobals(t)
			dir := isolateAWSEnv(t)
			t.Setenv("HOME", dir)
			stubTeamName(t)
			set := func(name string) {
				t.Helper()
				v := sampleFlagValue(t, name)
				if name == "user-data-file" || name == "config" {
					v = filepath.Join(dir, name+".txt")
					body := "echo from-a-file\n"
					if name == "config" {
						body = "plugins:\n  - ref: rstudio-server\n"
					}
					if err := os.WriteFile(v, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				setLaunchFlag(t, name, v)
			}
			for _, k := range []string{"instance-type", "region"} {
				if err := launchCmd.Flags().Set(k, map[string]string{"instance-type": "m7i.large", "region": "us-east-1"}[k]); err != nil {
					t.Fatal(err)
				}
			}
			if err := launchCmd.Flags().Set("ttl", "1h"); err != nil {
				t.Fatal(err)
			}
			for _, p := range flagPrereqs[n] {
				set(p)
			}
			before, beforeUD := sweepFlagMember(t)
			set(n)
			after, afterUD := sweepFlagMember(t)

			fields := changedConfigFields(before, after)
			userData := beforeUD != afterUD
			switch rule.kind {
			case sweepViaConfig:
				if len(fields) == 0 {
					t.Fatalf("--%s is said to reach the members through their config, but it changes no field", n)
				}
				for _, f := range fields {
					d := foregroundDispositions[f]
					if d.refused || !(d.settings || d.tags || d.userData || d.direct != "") {
						t.Errorf("--%s sets LaunchConfig.%s, which a foreground member does not carry (%q)", n, f, d.why)
					}
				}
			case sweepViaUserData:
				if !userData {
					t.Errorf("--%s is said to reach the members through the user-data builder, but their user data "+
						"is the same with and without it", n)
				}
			case sweepCLIOnly:
				if len(fields) > 0 || userData {
					t.Errorf("--%s is said to act on the CLI alone, but it changes the member: fields %v, user data %t",
						n, fields, userData)
				}
			}
		})
	}
}

// --- helpers ---

var flagBindRE = regexp.MustCompile(`launchCmd\.(?:Persistent)?Flags\(\)\.\w+Var(?:P)?\(&(\w+),\s*"([a-z0-9-]+)"`)

// launchCmdFlagVars maps each launch flag to the variable it is bound to.
func launchCmdFlagVars(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("launch_flags.go")
	if err != nil {
		t.Fatalf("read launch_flags.go: %v", err)
	}
	out := map[string]string{}
	for _, m := range flagBindRE.FindAllStringSubmatch(string(b), -1) {
		if _, dup := out[m[2]]; !dup {
			out[m[2]] = m[1]
		}
	}
	if len(out) < 100 {
		t.Fatalf("found only %d flag bindings in launch_flags.go; the matcher is broken", len(out))
	}
	return out
}

// sweepSteeringSource is the sweep path's own code: the dispatch and
// orchestration, not the config builder every launch shares (a flag read only
// there reaches the members through their config, and is checked as such).
func sweepSteeringSource(t *testing.T) string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"launch_sweep*.go", "sweep*.go", "launch_fleet.go", "launch_regions.go"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	var sb strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
	}
	if sb.Len() == 0 {
		t.Fatal("read no sweep-path source; the gate would pass vacuously")
	}
	return sb.String()
}
