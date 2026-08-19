//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 regression coverage for #530: a param-file key that looks like a
// spawn setting but sits OUTSIDE defaults:/grid:/params: (one level higher
// than the row/defaults-level keys #526 covers) must fail the launch, not
// vanish with no trace at all.
//
// pkg/params.ParamFileFormat has exactly three tagged fields, and both
// json.Unmarshal and yaml.Unmarshal silently drop anything else BEFORE
// validateSweepParamKeys (#526's check) ever runs — so `ttl: 2h` written at
// the top level was not even passed through as a PARAM_* env var (which would
// at least leave a tag to grep for); it was gone. Two of spawn's own shipped
// examples were written this way (examples/simple-params.yaml,
// examples/schedule-params.yaml), fixed in the same change as this test.

// TestTier0_SweepRejectsTopLevelTTL: `ttl: 2h` at the top level, with one
// otherwise-unbounded row. This is the dangerous case from the issue: a real
// TTL a user believes they set is silently dropped, producing an unbounded
// instance with no error and no tag. The launch must fail AND nothing must
// have been created — a non-zero exit alone would not distinguish "rejected"
// from "launched unbounded and then failed for some unrelated reason".
//
// A real --ttl is passed on the command line so the pre-existing --no-detach
// "every row needs a bound" guard (#525) would otherwise let this launch
// through — without it, a non-zero exit would prove only that the row was
// unbounded, not that the top-level ttl: key itself was caught.
func TestTier0_SweepRejectsTopLevelTTL(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `ttl: 2h
defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
`)
	stdout, stderr, code := env.run(
		"launch", "top-level-ttl",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--ttl", "1h",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for a top-level ttl:, got 0 — pre-fix this key was silently "+
			"dropped and the row launched with no TTL at all\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "ttl") {
		t.Errorf("the error does not name the offending key\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "defaults:") {
		t.Errorf("the error does not tell the user where the key belongs\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a param file with a top-level ttl:")
}

// TestTier0_SweepRejectsTopLevelCostLimit is the second dangerous case named
// in the issue: cost_limit: at the top level. Distinct row/key from the TTL
// test so a fix that only special-cased "ttl" specifically would still be
// caught.
func TestTier0_SweepRejectsTopLevelCostLimit(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `cost_limit: 5
defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
`)
	stdout, stderr, code := env.run(
		"launch", "top-level-cost-limit",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--ttl", "1h",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for a top-level cost_limit:, got 0\nstdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}
	if !strings.Contains(stderr, "cost_limit") {
		t.Errorf("the error does not name the offending key\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a param file with a top-level cost_limit:")
}

// TestTier0_SweepWarnsOnHarmlessTopLevelKey is the half that must not
// regress: a top-level key that is neither a recognized spawn setting nor on
// the reserved (CLI-only/near-miss) list — ordinary metadata like
// description: — must only warn, and the sweep must still launch normally.
// A blanket error on every unrecognized top-level key would break files that
// carry harmless metadata, which is explicitly not what the issue asked for.
func TestTier0_SweepWarnsOnHarmlessTopLevelKey(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `description: nightly benchmark run
defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
`)
	stdout, stderr, code := env.run(
		"launch", "harmless-top-level",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code != 0 {
		t.Fatalf("expected exit 0 for a harmless top-level key, got %d\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "description") {
		t.Errorf("the warning does not name the harmless key, so it has nowhere to be noticed\n"+
			"stderr:\n%s", stderr)
	}
	// The sweep must have actually proceeded, not merely exited 0 for an
	// unrelated reason (e.g. an early return before anything was attempted).
	tags := env.tagsByInstanceType()["c5.large"]
	requireTag(t, "sweep proceeded despite the warning", tags, "spawn:ttl", "1h")
}
