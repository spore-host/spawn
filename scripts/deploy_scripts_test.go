// Package scripts holds gates on the hand-written deploy scripts.
//
// These functions have no CloudFormation stack, so the script IS the record of
// how they are configured — and a defect in one is only discovered by running it
// against production. Two have already bitten:
//
//   - deploy-scheduler-handler.sh took its region from the ambient AWS_REGION and
//     CREATED a duplicate function in us-west-2.
//   - deploy-sweep-orchestrator.sh sent `Variables={}` on every update, which
//     wipes the function's environment, under `|| true` so it could not fail
//     loudly.
//
// Deliberately NOT behind a build tag. The first version was, to keep a normal
// `go test ./...` from reading files outside its module — but the files it reads
// are the sibling deploy scripts in this same directory, so there was nothing to
// isolate. The tag bought nothing and cost two workflow changes, one of which
// broke: a module whose only package is tag-gated has NO packages by default, so
// `govulncheck ./...` matched nothing and exited 1.
package scripts

import (
	"os"
	"strings"
	"testing"
)

// read returns the script with comment lines stripped.
//
// Stripping matters: these scripts explain their own hazards in comments ("this
// used to end in `|| true`", "deliberately NOT from AWS_REGION"), so a scan of
// the raw text flags the documentation of a fixed bug as the bug. Both of these
// gates failed that way on first run.
func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// A deploy script must assert which account it is pointed at. The account comes
// from whatever credentials are ambient, so without this the script will happily
// create a parallel copy of production in someone's sandbox.
func TestDeployScriptsAssertTheAccount(t *testing.T) {
	for _, f := range []string{
		"deploy-sweep-orchestrator.sh",
		"deploy-scheduler-handler.sh",
	} {
		s := read(t, f)
		if !strings.Contains(s, "EXPECTED_ACCOUNT") {
			t.Errorf("%s does not assert the target account — ambient credentials decide where it deploys", f)
		}
	}
}

// A region must never come from the ambient environment alone. AWS_REGION being
// set to something else is normal, and the failure mode is a duplicate function
// in the wrong region that nothing then invokes.
func TestDeployScriptsDoNotTakeRegionFromAWSREGION(t *testing.T) {
	for _, f := range []string{
		"deploy-sweep-orchestrator.sh",
		"deploy-scheduler-handler.sh",
	} {
		s := read(t, f)
		// An ASSIGNMENT to REGION, matched per line and anchored at the start, so
		// the `echo "note: AWS_REGION=$AWS_REGION is set but ignored"` that warns
		// about this very hazard is not itself reported as the hazard. That was
		// this gate's first false positive.
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "REGION=") {
				continue
			}
			if strings.Contains(line, "AWS_REGION") {
				t.Errorf("%s assigns REGION from AWS_REGION (%s) — pin it explicitly instead", f, line)
			}
		}
	}
}

// `--environment "Variables={}"` on an update REPLACES the function's
// environment with nothing. It is indistinguishable from a no-op on a function
// that has no variables set, which is why it survived.
func TestDeployScriptsDoNotWipeTheEnvironment(t *testing.T) {
	for _, f := range []string{
		"deploy-sweep-orchestrator.sh",
		"deploy-scheduler-handler.sh",
	} {
		s := read(t, f)
		if strings.Contains(s, "Variables={}") {
			t.Errorf("%s passes Variables={} — that wipes the live environment, it does not preserve it", f)
		}
	}
}

// "The call returned 200" is not evidence the function runs the new binary.
// Comparing CodeSha256 against the locally-built zip is.
func TestDeploySweepOrchestratorVerifiesTheDeployedCode(t *testing.T) {
	s := read(t, "deploy-sweep-orchestrator.sh")
	for _, want := range []string{"CodeSha256", "LOCAL_SHA", "REMOTE_SHA"} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy-sweep-orchestrator.sh has no %s check — a deploy would be unverified", want)
		}
	}
	// A failing comparison must stop the script, not print and continue.
	if !strings.Contains(s, "does not match the zip that was built") {
		t.Error("the CodeSha256 mismatch does not fail the deploy")
	}
}

// `|| true` on an AWS mutation swallows the failure. There is no case in these
// scripts where a failed configuration update is acceptable.
func TestDeployScriptsDoNotSwallowFailures(t *testing.T) {
	s := read(t, "deploy-sweep-orchestrator.sh")
	if strings.Contains(s, "|| true") {
		t.Error("deploy-sweep-orchestrator.sh swallows a failure with || true")
	}
	// set -e alone leaves a failure on the left of a pipe unnoticed.
	if !strings.Contains(s, "set -euo pipefail") {
		t.Error("deploy-sweep-orchestrator.sh is not set -euo pipefail")
	}
}
