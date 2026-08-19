//go:build e2e_tier0

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Tier 0 regression coverage for #525: the CLI spend controls must actually
// reach the instances a parameter sweep launches.
//
// buildLaunchConfigFromParams "start[s] with an empty config" and the sweep
// dispatch copied only Region/InstanceType/Name off the base config, so --ttl,
// --idle-timeout and --cost-limit were silently discarded for every row — while
// launch_sweep.go printed "Using safeguards: ttl=4h, idle-timeout=" from the
// variables it was about to throw away. `cost_limit:` in the param file did not
// work either: there was no parser case, so it became a PARAM_cost_limit env var
// that capped nothing. Both routes failed, which left a sweep with no
// per-instance dollar cap at all.
//
// The assertions below are on EC2 tags rather than on any in-process value,
// because the tags are what spored and the out-of-band reaper actually enforce.
// A test that asserted "the flag was parsed" would have passed throughout the
// bug's life: the flag WAS parsed, onto a config nobody copied from.
//
// All three tests take the FOREGROUND path (--no-detach), which is the one that
// provisions in-process and therefore the one Tier 0 can observe end to end. The
// detached path is covered at the same seam by construction — both paths read
// the same `defaults:` map, and the detached one uploads it to S3 for the Lambda
// orchestrator — but that orchestrator does not live in this repo, so its half
// of the fix is not verifiable here. Stated rather than implied.

// writeSweepFile writes a param file with the given body and returns its path.
func writeSweepFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sweep.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write param file: %v", err)
	}
	return path
}

// tagsByInstanceType reads every instance out of EC2 and returns its tags keyed
// by instance type, which is how a sweep's rows are told apart here. It fails if
// two instances share a type, since that would make the mapping ambiguous.
func (e *spawnEnv) tagsByInstanceType() map[string]map[string]string {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		e.t.Fatalf("DescribeInstances: %v", err)
	}
	byType := map[string]map[string]string{}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			itype := string(inst.InstanceType)
			if _, dup := byType[itype]; dup {
				e.t.Fatalf("two instances of type %s — cannot attribute tags to a row", itype)
			}
			tags := map[string]string{}
			for _, tg := range inst.Tags {
				tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
			}
			byType[itype] = tags
		}
	}
	return byType
}

// launchForegroundSweep runs a real (foreground) sweep and requires exit 0.
func (e *spawnEnv) launchForegroundSweep(paramFile string, extra ...string) {
	e.t.Helper()
	args := append([]string{
		"launch", "spend-check",
		"--param-file", paramFile,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	}, extra...)
	stdout, stderr, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("spawn %v: expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			args, code, stdout, stderr)
	}
}

// requireTag asserts one tag's exact value, and reports every tag on the
// instance when it is missing — the pre-fix failure was an ABSENT tag, and
// "spawn:ttl not found" on its own does not show that the launch otherwise
// succeeded.
func requireTag(t *testing.T, where string, tags map[string]string, key, want string) {
	t.Helper()
	got, ok := tags[key]
	if !ok {
		t.Errorf("%s: tag %s is absent, want %q. Tags present: %v", where, key, want, tags)
		return
	}
	if got != want {
		t.Errorf("%s: tag %s = %q, want %q", where, key, got, want)
	}
}

// TestTier0_SweepAppliesCLISpendControls is the core #525 case: a param file that
// declares no bounds of its own, launched with the spend controls on the command
// line. Pre-fix every row came up with spawn:ttl absent and no spawn:cost-limit,
// bounded only by the unrelated 1h *idle* timeout the zombie guard substitutes —
// which never fires on a compute-bound row.
func TestTier0_SweepAppliesCLISpendControls(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
`)
	env.launchForegroundSweep(file, "--ttl", "1h", "--cost-limit", "3")

	byType := env.tagsByInstanceType()
	if len(byType) != 2 {
		t.Fatalf("expected 2 instances (one per row), got %d: %v", len(byType), byType)
	}
	for itype, tags := range byType {
		requireTag(t, itype, tags, "spawn:ttl", "1h")
		requireTag(t, itype, tags, "spawn:cost-limit", "3.0000")
		// The idle timeout must NOT be substituted once an explicit --ttl
		// arrives: applyIdleTimeoutDefault only fills in when both are empty,
		// and pre-fix the CLI ttl never got that far, so an idle timeout the
		// user never asked for showed up in its place and read as a bound.
		if v, ok := tags["spawn:idle-timeout"]; ok {
			t.Errorf("%s: spawn:idle-timeout=%q was substituted despite an explicit --ttl", itype, v)
		}
	}
}

// TestTier0_SweepRowTTLBeatsCLI pins the precedence: most specific wins, so a
// row's own ttl: outranks the command line. This is what lets a matrix put an
// expensive GPU row on a shorter leash than the rest of the sweep, and it is the
// half of the fix that could plausibly have been implemented backwards.
func TestTier0_SweepRowTTLBeatsCLI(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
    ttl: 30m
`)
	env.launchForegroundSweep(file, "--ttl", "4h")

	byType := env.tagsByInstanceType()
	requireTag(t, "row without its own ttl", byType["c5.large"], "spawn:ttl", "4h")
	requireTag(t, "row with ttl: 30m", byType["c5.xlarge"], "spawn:ttl", "30m")
}

// TestTier0_SweepCLIBeatsFileDefaults is the other half of the precedence rule: a
// flag passed at invocation time outranks a value checked into the file's
// defaults:. Without this, `--ttl 30m` on a file that says `ttl: 8h` would look
// like it worked and leave the 8h bound in place.
func TestTier0_SweepCLIBeatsFileDefaults(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 8h
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file, "--ttl", "30m")

	requireTag(t, "file says 8h, CLI says 30m", env.tagsByInstanceType()["c5.large"], "spawn:ttl", "30m")
}

// TestTier0_SweepParamFileCostLimit covers the param-file route on its own, with
// no --cost-limit on the command line. `cost_limit:` had no parser case, so it
// fell through to the unknown-key arm and became the PARAM_cost_limit env var /
// spawn:param:cost_limit tag — a spend control that parsed, launched, tagged,
// and capped nothing (#526 is the general form of that failure).
func TestTier0_SweepParamFileCostLimit(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
  cost_limit: 5
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file)

	tags := env.tagsByInstanceType()["c5.large"]
	requireTag(t, "cost_limit: 5 in defaults", tags, "spawn:cost-limit", "5.0000")
	if v, ok := tags["spawn:param:cost_limit"]; ok {
		t.Errorf("cost_limit still leaked to spawn:param:cost_limit=%q — it is a real config "+
			"key now, not a PARAM_* passthrough", v)
	}
}

// TestTier0_SweepFileTTLSatisfiesNoDetach: the --no-detach guard used to read the
// CLI ttl variable, so a param file carrying its own `ttl:` was refused even
// though that value is the one which actually reached the instances. The only way
// through was to pass --ttl to satisfy the guard and have its value discarded —
// one flag to pass the check, another to take effect. A file-provided bound is
// now accepted, and the instance gets it.
func TestTier0_SweepFileTTLSatisfiesNoDetach(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 2h
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file) // no --ttl on the command line

	requireTag(t, "ttl: 2h from the file", env.tagsByInstanceType()["c5.large"], "spawn:ttl", "2h")
}

// TestTier0_SweepNoDetachNamesUnboundedRows: the guard is per row now, so a file
// that bounds only some of its rows is refused — and the refusal names the rows,
// because "some row is unbounded" is not actionable on a 30-row sweep. The old
// CLI-variable check could not see this case at all: one --ttl satisfied it and
// then reached nothing.
func TestTier0_SweepNoDetachNamesUnboundedRows(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
    ttl: 1h
  - instance_type: c5.xlarge
`)
	stdout, stderr, code := env.run(
		"launch", "partly-bounded",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit when a row has no bound, got 0\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "c5.xlarge") {
		t.Errorf("the error does not name the unbounded row (c5.xlarge)\nstdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}
	if strings.Contains(stderr, "row 0") {
		t.Errorf("row 0 has ttl: 1h and must not be reported as unbounded\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a sweep with one unbounded row")
}

// TestTier0_SweepRejectsUnusableCostLimit: a cost limit that cannot be read must
// fail the launch, not default to 0. A silent 0 means "no cap", which is the
// opposite of what the operator wrote — and the only signal would have been an
// instance with no spawn:cost-limit tag, which nobody inspects until the bill
// arrives.
//
// The --ttl on the command line and the "cost_limit" assertion on the message are
// both load-bearing. Without them this test PASSED against the pre-fix tree: the
// old --no-detach guard rejected the file for having no CLI --ttl, long before
// anything looked at cost_limit, so a non-zero exit on its own proved nothing
// about the behaviour under test. Same trap the rest of this repo's spend checks
// keep hitting — a check that cannot fail is worse than no check.
func TestTier0_SweepRejectsUnusableCostLimit(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  cost_limit: eight dollars
params:
  - instance_type: c5.large
`)
	stdout, stderr, code := env.run(
		"launch", "bad-cost-limit",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--ttl", "1h",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for an unparseable cost_limit, got 0\nstdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}
	if !strings.Contains(stderr, "cost_limit") {
		t.Errorf("the failure does not name cost_limit, so it may be failing for an unrelated "+
			"reason\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	env.requireNothingLaunched("unparseable cost_limit")
}
