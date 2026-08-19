//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 regression coverage for #531: the VALUE half of a param-file row must
// also be safe to hand to the instance, not just the key (which #526 covers in
// tier0_sweep_param_keys_test.go).
//
// The bug was in pkg/launcher/bootstrap.go's generated bootstrap script, which
// wrote each spawn:param:* tag into /etc/profile.d/spawn-params.sh as
//
//	export PARAM_<name>="<value>"
//
// with the value double-quoted and completely unescaped. Every login shell
// that sources that file then reinterprets $, `, and " in the value instead of
// treating it as literal text — so a value like $HOME/out was shell-expanded
// to the instance's real home directory, and a value with an embedded double
// quote broke the generated line's quoting outright. Substrate emulates the
// AWS control plane only (no real instance boot or shell sourcing), so that
// half of the fix is verified by a real-shell-exec unit test instead:
// pkg/launcher/bootstrap_test.go's TestBuildLinuxBootstrap_ParamValueSurvivesRealShellParsing
// extracts the actual generated while-loop and runs it under bash end to end.
//
// What Tier 0 CAN observe end to end is the other half of #531: a value
// containing a literal newline must fail the launch before anything is
// provisioned, because it cannot survive the round trip through an EC2 tag
// and back through pkg/launcher/bootstrap.go's one-tag-per-line `read -r key
// value` loop. That's exactly the kind of "given AWS responses, does spawn's
// CLI behavior hold" case Tier 0 exists for, so it gets full fail-without/
// pass-with coverage here, on the same "assert via instance inventory, not
// just exit code" convention as #526's key-side test.
func TestTier0_SweepRejectsNewlineInParamValue(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, "defaults:\n"+
		"  on_complete: terminate\n"+
		"  ttl: 1h\n"+
		"params:\n"+
		"  - instance_type: c5.large\n"+
		"    label: \"line one\\nline two\"\n")
	stdout, stderr, code := env.run(
		"launch", "newline-value",
		"--param-file", file,
		"--region", "us-east-1",
		"--no-detach",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	)
	if code == 0 {
		t.Errorf("expected a non-zero exit for a param value containing a newline, got 0\n"+
			"stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "label") {
		t.Errorf("the error does not name the offending key\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "newline") {
		t.Errorf("the error does not explain why the value was rejected\nstderr:\n%s", stderr)
	}
	env.requireNothingLaunched("a param file with a newline in a workload parameter value")
}

// TestTier0_SweepAcceptsShellMetacharactersInParamValue is the pass-with half:
// values that look dangerous to a naive double-quoted shell embed — an
// embedded double quote, a leading $, a backtick — but contain no newline must
// still launch and reach the instance as a spawn:param:* tag exactly as
// written. This is the passthrough #531's fix must not break: the tag itself
// was never wrong (bootstrap.go's INTERPRETATION of it was), so the tag-level
// assertion here is deliberately unchanged from #526's style — the
// shell-safety assertion belongs to the bootstrap_test.go real-shell-exec test
// referenced above, not to Tier 0, which cannot render user-data at all.
func TestTier0_SweepAcceptsShellMetacharactersInParamValue(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, "defaults:\n"+
		"  on_complete: terminate\n"+
		"  ttl: 1h\n"+
		"params:\n"+
		"  - instance_type: c5.large\n"+
		"    label: 'run \"A\"'\n"+
		"    prefix: \"$HOME/out\"\n")
	env.launchForegroundSweep(file)

	tags := env.tagsByInstanceType()["c5.large"]
	requireTag(t, "value with an embedded double quote", tags, "spawn:param:label", `run "A"`)
	requireTag(t, "value with a dollar sign", tags, "spawn:param:prefix", "$HOME/out")
}
