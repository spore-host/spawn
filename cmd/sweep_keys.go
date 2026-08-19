package cmd

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Param-file key validation for the sweep launch path (#526).
//
// buildLaunchConfigFromParams routes any key it does not recognise to
// config.Parameters, which becomes a spawn:param:<key> tag and then a PARAM_<key>
// environment variable on the instance. That passthrough is the feature — it is
// how a sweep hands values to a workload — but it also means the parser could
// never say "that isn't a thing". A key the user *meant* as a spawn setting and a
// key they meant for their own program are indistinguishable, so `ttl_hours: 4`
// launched an instance with no TTL and looked completely healthy doing it.
//
// The fix keeps passthrough and adds three ways for a key to be rejected instead,
// in the order they are checked:
//
//	A. it cannot become an environment variable at all (invalid shell identifier)
//	B. it is a recognised spawn key misspelled — hyphens, or the wrong case
//	C. it is on the curated list of names below: CLI-only flags and settings
//	   people reasonably assume exist
//
// Anything else passes through, and is now listed at launch so an intentional
// parameter is visible and an unintentional one has somewhere to be noticed.

// recognizedRowKeys is every key buildLaunchConfigFromParams acts on, including
// aliases. It exists so the near-miss check has something to compare against.
//
// This list is a duplicate of that function's case labels, which is exactly the
// kind of duplication that rots — so TestRecognizedRowKeysMatchesSwitch parses
// the switch out of the source with go/ast and requires the two to be equal.
// Adding a case without adding it here fails that test.
var recognizedRowKeys = map[string]bool{
	"instance_type":     true,
	"region":            true,
	"az":                true,
	"availability_zone": true,
	"ami":               true,
	"key_pair":          true,
	"key_name":          true,
	"spot":              true,
	"spot_max_price":    true,
	"hibernate":         true,
	"ttl":               true,
	"idle_timeout":      true,
	"cost_limit":        true,
	"hibernate_on_idle": true,
	"session_timeout":   true,
	"on_complete":       true,
	"completion_file":   true,
	"completion_delay":  true,
	"dns":               true,
	"dns_name":          true,
	"step":              true,
	"command":           true,
	"user_command":      true,
	"user_data":         true,
	"iam_role":          true,
	"name":              true,
}

// reservedRowKeys maps a key that must not silently pass through to the guidance
// printed when it appears. Every entry is either a CLI-only flag or a control a
// user could reasonably believe exists; the ones that cost money are the point.
//
// Deliberately NOT on this list: plausible workload parameter names that merely
// resemble a spawn setting — `image` (a container image is a normal sweep
// parameter), `type`, `count`, `steps`, `instance` (an optimisation problem
// instance), `runtime` (a container runtime). Rejecting those would break the
// feature this check is meant to preserve. When in doubt the key passes through: a
// denylist that swallows legitimate parameters is a worse bug than the one it
// fixes.
//
// `timeout` is the load-bearing example, and it is not on this list because
// spawn's own examples/workflow-ci-pipeline.yaml sets `timeout: 10m` per step and
// pkg/queue.JobConfig has a Timeout field — it is documented vocabulary on the
// workflow path, not a near-miss. TestBuildLaunchConfigFromParams_WorkflowStep
// caught the first draft of this list denying it.
//
// The entries that remain are still English words someone could plausibly sweep
// over: `budget` is an optimiser's evaluation budget as often as it is a dollar
// figure, `time_limit` is a standard solver option. They stay because the failure
// they prevent costs money — and because the escape hatch below means a rejection
// is never a dead end: `param:budget: 50` passes through untouched.
var reservedRowKeys = map[string]string{
	// Bounds the user believes they set. These are the expensive ones.
	"ttl_hours":             "use ttl: with a duration, e.g. ttl: 4h",
	"ttl_minutes":           "use ttl: with a duration, e.g. ttl: 90m",
	"ttl_seconds":           "use ttl: with a duration, e.g. ttl: 600s",
	"time_limit":            "use ttl: with a duration, e.g. ttl: 4h",
	"max_runtime":           "use ttl: with a duration, e.g. ttl: 4h",
	"walltime":              "use ttl: with a duration, e.g. ttl: 4h",
	"idle_time":             "use idle_timeout: with a duration, e.g. idle_timeout: 30m",
	"max_cost":              "use cost_limit: with a number in USD, e.g. cost_limit: 8",
	"cost_cap":              "use cost_limit: with a number in USD, e.g. cost_limit: 8",
	"spend_limit":           "use cost_limit: with a number in USD, e.g. cost_limit: 8",
	"budget":                "spawn has no per-sweep budget key; bound each row with ttl: (worst case) and cost_limit: (spend cap)",
	"on_completion":         "use on_complete:, e.g. on_complete: terminate",
	"oncomplete":            "use on_complete:, e.g. on_complete: terminate",
	"terminate_on_done":     "use on_complete: terminate",
	"terminate_on_complete": "use on_complete: terminate",
	"no_timeout":            "--no-timeout is a command-line flag; there is no param-file equivalent (omit ttl:/idle_timeout: instead — but a sweep row with no bound is how zombie instances happen)",

	// Sweep-level settings that are CLI flags, not per-row values.
	"max_concurrent": "--max-concurrent is a command-line flag, not a param-file key (spawn schedule reads max_concurrent from defaults:, but the launch path does not)",
	"launch_delay":   "--launch-delay is a command-line flag, not a param-file key",
	"sweep_name":     "--name is a command-line flag (spawn schedule reads sweep_name from defaults:, but the launch path does not)",
	"detach":         "--detach/--no-detach are command-line flags",
	"no_detach":      "--detach/--no-detach are command-line flags",
	"estimate_only":  "--estimate-only is a command-line flag",
	"dry_run":        "--estimate-only is the command-line flag for pricing a sweep without launching it",

	// Shapes that do not mean what they look like.
	"instance_types":       "one instance_type: per row, or use grid: {instance_type: [...]} to expand a list into rows",
	"image_id":             "use ami:",
	"ami_id":               "use ami:",
	"keypair":              "use key_pair: (or key_name:)",
	"ssh_key":              "use key_pair: (or key_name:)",
	"spot_price":           "use spot_max_price:",
	"iam_profile":          "use iam_role:",
	"instance_profile":     "use iam_role:",
	"iam_instance_profile": "use iam_role:",
	"region_name":          "use region:",
	"availability_zones":   "one az: per row, or use grid: {az: [...]}",
	"disk_size":            "--disk-size is a command-line flag, not a param-file key",
	"volume_size":          "--disk-size is a command-line flag, not a param-file key",
}

// shellIdentifier is the set of names that can survive the trip to the instance.
// pkg/launcher/bootstrap.go writes each parameter into /etc/profile.d as
//
//	export PARAM_<key>="<value>"
//
// so a key that is not a valid shell identifier produces a line the shell
// refuses — `export PARAM_on-complete="terminate"` is a "not a valid identifier"
// error in every login shell, and the parameter reaches the workload as nothing.
var shellIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// normalizeKey folds the two ways a recognised key gets misspelled: CLI-style
// hyphens (`on-complete`) and inherited capitalisation (`TTL`, `Instance_Type`).
func normalizeKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "-", "_"))
}

// explicitParamPrefix opts a key out of the near-miss and reserved-name checks:
// `param:budget: 50` means "I really do want PARAM_budget", and gets it.
//
// Without this, every reserved name is a dead end rather than a redirect — a user
// sweeping a solver's `time_limit` or an optimiser's `budget` would have no way to
// pass it at all, and the check would have broken the feature it is protecting.
// Rule A still applies to what follows the prefix, because that part becomes the
// environment variable name.
const explicitParamPrefix = "param:"

// explicitParamKey returns the parameter name behind an explicit param: prefix.
func explicitParamKey(key string) (string, bool) {
	if !strings.HasPrefix(key, explicitParamPrefix) {
		return "", false
	}
	return strings.TrimPrefix(key, explicitParamPrefix), true
}

// classifyRowKey reports why an unrecognised param-file key must be rejected, or
// returns nil if it is a legitimate passthrough parameter. Callers have already
// established that the key is not in recognizedRowKeys.
func classifyRowKey(key string) error {
	_, err := resolveParamName(key)
	return err
}

// resolveParamName returns the PARAM_* name an unrecognised key becomes, or an
// error explaining why it cannot become one. It is the single place the three
// rejection rules live; classifyRowKey is the boolean-ish wrapper for validation
// passes that do not need the name.
func resolveParamName(key string) (string, error) {
	if name, explicit := explicitParamKey(key); explicit {
		if name == "" {
			return "", fmt.Errorf("%q has nothing after %q — write param:<name> with the "+
				"parameter name you want", key, explicitParamPrefix)
		}
		if !shellIdentifier.MatchString(name) {
			return "", fmt.Errorf("%q cannot become an environment variable: %q is not a valid "+
				"shell identifier (letters, digits and underscores only, not starting with a digit)",
				key, name)
		}
		return name, nil
	}

	norm := normalizeKey(key)

	// B before A: a near-miss gets the specific message even when it is also an
	// invalid identifier, because "did you mean on_complete:" is more use than
	// "that cannot be an environment variable".
	if norm != key && recognizedRowKeys[norm] {
		return "", fmt.Errorf("unknown key %q — did you mean %q? (spawn keys are lowercase with underscores)", key, norm)
	}
	if hint, ok := reservedRowKeys[norm]; ok {
		return "", fmt.Errorf("%q is not a spawn setting: %s. If you really do mean a parameter "+
			"for your workload, write %s%s and it will be passed through as PARAM_%s",
			key, hint, explicitParamPrefix, norm, norm)
	}
	if !shellIdentifier.MatchString(key) {
		return "", fmt.Errorf("unknown key %q cannot be passed to the workload: parameters become "+
			"PARAM_<key> environment variables, and %q is not a valid shell identifier "+
			"(letters, digits and underscores only, not starting with a digit)", key, key)
	}
	return key, nil
}

// valueContainsNewline reports whether a parameter value contains a literal
// newline character (#531). A spawn:param:* tag's value round-trips through
// EC2's tag API and back through `--output text` in pkg/launcher/bootstrap.go,
// whose `read -r key value` loop reads one tag per line — a value containing
// "\n" splits into a stray, malformed extra line rather than surviving as one
// value. There's no encoding trick worth applying here: the tag wire format
// cannot faithfully carry a newline, so the value is rejected outright, at
// launch time, before anything is provisioned, rather than failing silently
// or strangely on the instance.
func valueContainsNewline(val interface{}) bool {
	return strings.Contains(fmt.Sprintf("%v", val), "\n")
}

// passthroughKeys returns the sorted parameter keys a merged param set would send
// to the workload as PARAM_* env vars.
func passthroughKeys(m map[string]interface{}) []string {
	var out []string
	for k := range m {
		if !recognizedRowKeys[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// validateSweepParamKeys checks every key in defaults: and in each row before
// anything is launched, and returns one error naming ALL the bad keys rather than
// failing on the first. A 30-row sweep with three typos should be one round trip,
// not three.
//
// buildLaunchConfigFromParams rejects the same keys on its own (it is reached by
// resume and the quota preflight too), so this is the early, better-worded copy of
// a check that also exists at the seam. Deliberate duplication: this one runs
// before the first AWS call.
func validateSweepParamKeys(paramFormat *ParamFileFormat) error {
	type problem struct {
		where string
		err   error
	}
	var problems []problem

	check := func(where string, m map[string]interface{}) {
		for _, k := range passthroughKeys(m) {
			if err := classifyRowKey(k); err != nil {
				problems = append(problems, problem{where: where, err: err})
				continue
			}
			// The key is fine; also reject a value that cannot survive the trip
			// through an EC2 tag and back (#531). Checked only for passthrough
			// keys — the ones that become spawn:param:* tags and are read back
			// by pkg/launcher/bootstrap.go's one-tag-per-line loop.
			if valueContainsNewline(m[k]) {
				problems = append(problems, problem{where: where, err: fmt.Errorf(
					"%q contains a newline, which cannot survive as an EC2 tag value — "+
						"remove it or replace it with a space/delimiter the workload can parse", k)})
			}
		}
	}

	check("defaults:", paramFormat.Defaults)
	for i, row := range paramFormat.Params {
		label := fmt.Sprintf("row %d", i)
		if it, ok := row["instance_type"].(string); ok && it != "" {
			label += " (" + it + ")"
		}
		check(label, row)
	}
	if len(problems) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d unusable key(s) in %s:", len(problems), paramFile)
	for _, p := range problems {
		fmt.Fprintf(&b, "\n   %s: %v", p.where, p.err)
	}
	b.WriteString("\n\n   Keys spawn does not recognise are passed to the workload as PARAM_* " +
		"environment variables, so a misspelled setting cannot be told apart from one of your " +
		"own parameters. The keys above are rejected because they look like spawn settings that " +
		"would silently do nothing.")
	return fmt.Errorf("%s", b.String())
}

// classifyTopLevelKey reports how an unrecognized TOP-LEVEL param-file key —
// one that is not defaults:, grid:, or params: — should be handled (#530).
//
// pkg/params.ParamFileFormat has exactly three fields, and both
// json.Unmarshal and yaml.Unmarshal silently drop any key that is none of
// them: `ttl: 2h` written one indentation level too high (outside defaults:)
// used to vanish with no trace at all — not even a PARAM_* env var, which at
// least leaves something to grep for. That is a different failure from the
// row-level one #526 fixed (a key inside defaults:/params: that looks like a
// setting), but the same shape one level up, so it reuses the SAME
// recognizedRowKeys/reservedRowKeys registry rather than a second,
// independently-drifting list:
//
//   - a key that IS a recognized row-level setting (ttl, cost_limit,
//     instance_type, ...) is a hard error — the alternative is silently
//     ignoring a setting the user clearly meant to apply, which for
//     ttl/idle_timeout/cost_limit produces an unbounded instance with no
//     warning at all
//   - a key on the reserved (CLI-only/near-miss) list is also a hard error,
//     for the same reason: sweep_name/max_concurrent/launch_delay at the top
//     level is exactly the mistake examples/schedule-params.yaml shipped with
//   - anything else is a warning, not an error: most top-level clutter
//     (description:, version:, author:) is harmless metadata, and a denylist
//     that hard-fails on it would break files that already carry it
func classifyTopLevelKey(key string) (isError bool, message string) {
	norm := normalizeKey(key)
	if recognizedRowKeys[norm] {
		spelling := ""
		if norm != key {
			spelling = fmt.Sprintf(" (correct spelling: %q)", norm)
		}
		return true, fmt.Sprintf("%q at the top level is ignored%s; move it under defaults: to apply it to every row",
			key, spelling)
	}
	if hint, ok := reservedRowKeys[norm]; ok {
		return true, fmt.Sprintf("%q at the top level is ignored: %s", key, hint)
	}
	return false, fmt.Sprintf("unknown top-level key %q is ignored (not defaults:, grid:, or params:)", key)
}

// validateTopLevelParamKeys checks the top-level keys pkg/params.ParseParamFile
// found outside defaults:/grid:/params: (#530) and either errors — before
// anything is launched or priced — or warns, printing to stderr so a warning
// has somewhere to be noticed the same way reportPassthroughParams' passthrough
// list does. filePath names the file in messages; the launch path uses the
// global paramFile flag variable, but this is also called from spawn schedule
// create, which has its own local path variable instead.
func validateTopLevelParamKeys(filePath string, unknownKeys []string) error {
	var errs, warns []string
	for _, k := range unknownKeys {
		if isError, msg := classifyTopLevelKey(k); isError {
			errs = append(errs, msg)
		} else {
			warns = append(warns, msg)
		}
	}

	if len(warns) > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  %s has %d top-level key(s) outside defaults:/grid:/params: — ignored:\n",
			filePath, len(warns))
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "   %s\n", w)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d top-level key(s) in %s look like spawn settings but sit outside defaults:/grid:/params:, so they are silently ignored:", len(errs), filePath)
	for _, e := range errs {
		fmt.Fprintf(&b, "\n   %s", e)
	}
	b.WriteString("\n\n   Only defaults:, grid: and params: are read at the top level of a param file; " +
		"anything else — including a real spawn setting written one level too high — is dropped before " +
		"validation ever sees it.")
	return fmt.Errorf("%s", b.String())
}

// reportPassthroughParams lists the PARAM_* variables the sweep will set, so an
// intentional parameter is confirmed and an unintentional one is at least visible
// — option (3) of #526, which is the weak half of the fix on its own and the
// useful half once the dangerous names are hard errors.
func reportPassthroughParams(paramFormat *ParamFileFormat) {
	seen := map[string]bool{}
	var keys []string
	for _, m := range append([]map[string]interface{}{paramFormat.Defaults}, paramFormat.Params...) {
		for _, k := range passthroughKeys(m) {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	prefixed := make([]string, 0, len(keys))
	for _, k := range keys {
		// Show the name the workload will actually see, which for an explicit
		// param:budget is PARAM_budget.
		if name, err := resolveParamName(k); err == nil {
			prefixed = append(prefixed, "PARAM_"+name)
		}
	}
	if len(prefixed) == 0 {
		return
	}
	sort.Strings(prefixed)
	fmt.Fprintf(os.Stderr, "   Workload parameters: %s\n", strings.Join(prefixed, ", "))
	fmt.Fprintf(os.Stderr, "   (any key spawn does not recognise is passed through as an env var — "+
		"check this list if a setting seems to have no effect)\n")
}
