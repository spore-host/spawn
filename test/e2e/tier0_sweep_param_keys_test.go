//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 regression coverage for #526: a param-file key that looks like a spawn
// setting must fail the launch, and a key that is genuinely a workload parameter
// must still reach the instance.
//
// The bug was the `default:` arm of buildLaunchConfigFromParams: every unrecognised
// key became a PARAM_<key> env var, so `ttl_hours: 4` and `on-complete: terminate`
// launched instances with no bound and no complaint. The table in #526 lists six
// such spellings; the two here are the ones that leave an instance running.
//
// Both halves are asserted deliberately. A test suite that only checked the
// rejections would be satisfied by a denylist that rejected everything, which
// would break the passthrough that makes a sweep useful — and passthrough is the
// half with no test to protect it before now.

// TestTier0_SweepRejectsMisspelledBound: `ttl_hours: 4` is a bound the user
// believes they set. The CLI carries a real --ttl so the launch would otherwise
// succeed — without that, a non-zero exit would prove only that the sweep was
// unbounded, not that the key was rejected.
func TestTier0_SweepRejectsMisspelledBound(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
    ttl_hours: 4
`)
	stdout, stderr, code := env.run(
		"launch", "misspelled-bound",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--ttl", "1h",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for ttl_hours:, got 0 — pre-fix this launched with "+
			"PARAM_ttl_hours=4 and no TTL of its own\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "ttl_hours") {
		t.Errorf("the error does not name the offending key\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "ttl:") {
		t.Errorf("the error does not name the correct spelling, which is the whole point of "+
			"rejecting it\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a param file with ttl_hours:")
}

// TestTier0_SweepRejectsHyphenatedKey: `on-complete:` is the hyphen/underscore
// slip. It is worth its own case because it fails twice over — it is not a spawn
// key, and `export PARAM_on-complete="terminate"` is not even a line the shell
// will accept in /etc/profile.d (pkg/launcher/bootstrap.go writes it verbatim).
func TestTier0_SweepRejectsHyphenatedKey(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  ttl: 1h
params:
  - instance_type: c5.large
    on-complete: terminate
`)
	stdout, stderr, code := env.run(
		"launch", "hyphenated",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for on-complete:, got 0\nstdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}
	if !strings.Contains(stderr, "on_complete") {
		t.Errorf("the error does not suggest the underscore spelling\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a param file with on-complete:")
}

// TestTier0_SweepPassesThroughWorkloadParams is the half that must not regress: a
// real workload parameter still becomes a spawn:param:* tag (and thus a PARAM_*
// env var), and an explicit param:<name> escape hatch strips its prefix on the way.
// `budget` is on the reserved list precisely because it is ambiguous, so this also
// pins that the escape hatch works on a name that is otherwise refused.
func TestTier0_SweepPassesThroughWorkloadParams(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
    isa: neon
    nsteps: 500000
    param:budget: 50
`)
	env.launchForegroundSweep(file)

	tags := env.tagsByInstanceType()["c5.large"]
	requireTag(t, "workload param", tags, "spawn:param:isa", "neon")
	requireTag(t, "workload param", tags, "spawn:param:nsteps", "500000")
	requireTag(t, "escaped reserved name", tags, "spawn:param:budget", "50")
	if v, ok := tags["spawn:param:param:budget"]; ok {
		t.Errorf("the param: prefix leaked into the tag name: %q", v)
	}
	// The escape hatch must not touch the real setting it shares a name with.
	requireTag(t, "real bound alongside an escaped param", tags, "spawn:ttl", "1h")
	if v, ok := tags["spawn:cost-limit"]; ok {
		t.Errorf("param:budget was mistaken for a cost limit: spawn:cost-limit=%q", v)
	}
}

// TestTier0_SweepListsPassthroughParams covers option (3) of #526: the PARAM_*
// variables are named at launch, so a key that is silently passing through has
// somewhere to be noticed.
func TestTier0_SweepListsPassthroughParams(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
    isa: neon
`)
	args := []string{
		"launch", "listed-params",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	}
	stdout, stderr, code := env.run(args...)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "PARAM_isa") {
		t.Errorf("the sweep header does not name the passthrough parameter it is about to set\n"+
			"stderr:\n%s", stderr)
	}
}
