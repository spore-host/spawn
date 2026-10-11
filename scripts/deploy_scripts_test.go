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
	"path/filepath"
	"regexp"
	"sort"
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

// repoRoot walks up to the module root so these gates can see the whole tree,
// not just this directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/spore-host/spawn\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root go.mod")
	return ""
}

// shellScripts returns every .sh in the repo, repo-relative.
func shellScripts(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "docs-gen", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".sh") {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

// deploys matches a script that CREATES OR MUTATES deployed infrastructure.
//
// Keyed on what the script DOES, not on where it lives or what it is called.
// Both of those filters have already failed here: the gates below each named two
// files by hand and so missed six, including four under lambda/*/ and
// scripts/setup-schedules-dynamodb.sh — which sits in this very directory and
// escaped a `scripts/deploy-*.sh` glob because of its name. That is the same
// failure as the unprefixed `scheduler-handler` function (#790) and the
// lambda/*-scoped module loop (#136): a filter built from a convention cannot
// find the thing that does not follow it.
var deploys = regexp.MustCompile(`aws (lambda (create-function|update-function-code|update-function-configuration)|cloudformation (deploy|create-stack|update-stack))`)

// deployingScripts discovers the scripts the gates below apply to.
func deployingScripts(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, rel := range shellScripts(t) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		if deploys.Match(b) {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		// A discovery that finds nothing has broken; it has not proven the repo
		// clean. Same rule as scripts/nested-modules.sh.
		t.Fatal("found no deploying scripts in the repo — the discovery is broken, not the repo")
	}
	return out
}

// readRel is read() for a repo-relative path.
func readRel(t *testing.T, rel string) string {
	t.Helper()
	return read(t, filepath.Join(repoRoot(t), rel))
}

// deploysLambda is the narrower set: scripts that create or update a Lambda
// FUNCTION, as opposed to a CloudFormation stack.
//
// The region gate below applies only to these. Its hazard is a function
// silently created in a region nothing invokes — which is specific to a
// function pinned to one region. A stack script may legitimately be
// multi-region: scripts/setup-schedules-dynamodb.sh takes AWS_REGION on
// purpose, documents it, offers --region, and echoes "Region: $REGION" before
// acting, and three regional spawn-schedules buckets exist. Widening the gate
// flagged it, and the right response was to scope the gate rather than break a
// documented feature — a gate is not evidence that working code is wrong.
var deploysLambda = regexp.MustCompile(`aws lambda (create-function|update-function-code|update-function-configuration)`)

// lambdaDeployingScripts is deployingScripts narrowed to Lambda deploys.
func lambdaDeployingScripts(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, rel := range deployingScripts(t) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		if deploysLambda.Match(b) {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Lambda-deploying scripts — the discovery is broken, not the repo")
	}
	return out
}

// accountAssigned and accountCompared are the two halves of a real account
// assertion. See TestDeployScriptsAssertTheAccount for why a bare substring
// match is not enough.
var (
	accountAssigned = regexp.MustCompile(`(?m)^[[:space:]]*EXPECTED_ACCOUNT=`)
	accountCompared = regexp.MustCompile(`\$\{?ACCOUNT_ID\}?"?[[:space:]]*!=[[:space:]]*"?\$\{?EXPECTED_ACCOUNT`)
)

// A deploy script must assert which account it is pointed at. The account comes
// from whatever credentials are ambient, so without this the script will happily
// create a parallel copy of production in someone's sandbox.
func TestDeployScriptsAssertTheAccount(t *testing.T) {
	for _, f := range deployingScripts(t) {
		src := readRel(t, f)
		// The ASSIGNMENT and the COMPARISON, not the bare identifier.
		//
		// This gate used to accept `strings.Contains(s, "EXPECTED_ACCOUNT")`, and
		// widening it proved that insufficient: renaming the assignment to
		// EXPECTED_ACCT_RENAMED still PASSED, because the script's own error
		// message ("expected $EXPECTED_ACCOUNT") keeps the substring alive. That
		// is the fifth time in this repo a gate has matched its own prose.
		//
		// Both halves are required because either alone is satisfiable without
		// asserting anything: an assignment that is never compared is dead, and a
		// comparison against an unset variable is always false.
		if !accountAssigned.MatchString(src) {
			t.Errorf("%s never assigns EXPECTED_ACCOUNT — ambient credentials decide where it deploys", f)
		}
		if !accountCompared.MatchString(src) {
			t.Errorf("%s assigns EXPECTED_ACCOUNT but never compares it to the caller's account, "+
				"so it asserts nothing", f)
		}
	}
}

// A region must never come from the ambient environment alone. AWS_REGION being
// set to something else is normal, and the failure mode is a duplicate function
// in the wrong region that nothing then invokes.
func TestDeployScriptsDoNotTakeRegionFromAWSREGION(t *testing.T) {
	for _, f := range lambdaDeployingScripts(t) {
		s := readRel(t, f)
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
	for _, f := range deployingScripts(t) {
		s := readRel(t, f)
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
	// Not every `|| true` is a defect, and widening this gate proved it: of the
	// four in the repo, two swallowed real CloudFormation failures behind an
	// unconditional "✅ Stack updated successfully", and two are correct — a
	// get-role inside a propagation poll loop (which is the FIX for the bug
	// TestNoBareSleepAfterIAMMutation guards) and an idempotent
	// remove-permission whose absence is fine.
	//
	// A gate cannot infer intent, so the legitimate ones carry an explicit
	// `# swallow-ok: <reason>` marker. Modelled on the nosemgrep convention
	// already used here: honoured on the offending line or the one immediately
	// above, and nowhere else. The point is that an exemption has to be written
	// down and reviewed, rather than the gate being weakened to the level of its
	// most awkward case.
	root := repoRoot(t)
	for _, f := range deployingScripts(t) {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "|| true") {
				continue
			}
			if strings.Contains(line, "swallow-ok:") {
				continue
			}
			if i > 0 && strings.Contains(lines[i-1], "swallow-ok:") {
				continue
			}
			t.Errorf("%s:%d swallows a failure with || true and carries no "+
				"`# swallow-ok: <reason>` marker:\n    %s", f, i+1, strings.TrimSpace(line))
		}
	}
	// set -e alone leaves a failure on the left of a pipe unnoticed.
	//
	// Deliberately still scoped to this one script. Widening it fails several
	// others that use a bare `set -e`, and the fix is not mechanical: `set -u`
	// turns an unset variable from an empty string into an abort, which can
	// break a script that works today. That is a change needing per-script
	// verification against a real deploy, not a line edited to satisfy a gate.
	// Tracked rather than rushed.
	if !strings.Contains(readRel(t, "scripts/deploy-sweep-orchestrator.sh"), "set -euo pipefail") {
		t.Error("deploy-sweep-orchestrator.sh is not set -euo pipefail")
	}
}

// A shell script must not wait out IAM eventual consistency with a bare sleep.
//
// There is no `aws iam wait` for instance profiles or roles, so the correct
// substitute is a bounded poll on the read that has to succeed. Two scripts did
// it with a flat sleep (#752):
//
//   - setup-spawnd-iam-role.sh slept 10s after add-role-to-instance-profile;
//   - deploy-custom-dns.sh slept 3s after create-role and then ran a bare
//     get-role with no retry, so a slow propagation left ROLE_ARN EMPTY and the
//     script created a Lambda with an empty role.
//
// Matched per line, immediately after an `aws iam` mutation, so a legitimate
// sleep inside a poll loop elsewhere is not flagged.
func TestNoBareSleepAfterIAMMutation(t *testing.T) {
	iamMutation := regexp.MustCompile(`aws iam (create|add|attach|put|update|tag)-`)
	bareSleep := regexp.MustCompile(`^\s*sleep \d+\s*$`)

	root := repoRoot(t)
	for _, rel := range shellScripts(t) {
		e := struct{ name string }{rel}
		b, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !iamMutation.MatchString(line) {
				continue
			}
			// Scan forward past the mutation's own continuation lines for a bare
			// sleep that is not inside a `while`/`for` poll.
			inLoop := false
			for j := i + 1; j < min(i+12, len(lines)); j++ {
				t2 := strings.TrimSpace(lines[j])
				if strings.HasPrefix(t2, "#") {
					continue
				}
				if strings.HasPrefix(t2, "while ") || strings.HasPrefix(t2, "for ") {
					inLoop = true
				}
				if bareSleep.MatchString(lines[j]) && !inLoop {
					t.Errorf("%s:%d sleeps %q after the IAM mutation at :%d — poll the read "+
						"that must succeed instead; IAM's consistency tail is longer than any "+
						"constant you can pick", e.name, j+1, t2, i+1)
				}
				// A new command ends the window.
				if strings.HasPrefix(t2, "aws ") && j > i+1 {
					break
				}
			}
		}
	}
}
