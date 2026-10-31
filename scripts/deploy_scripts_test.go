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
	"errors"
	"io/fs"
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
			// An entry another package's test removed mid-walk (#1334). This
			// module cannot import internal/treewalk, so its rule is inline.
			if errors.Is(err, fs.ErrNotExist) && path != root {
				return nil
			}
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
//
// IAM verbs are in the set because leaving them out had a consequence: two
// committed scripts granted sts:AssumeRole into an account the project's hard
// constraint forbids, neither asserted its target account, and neither was
// discovered here — because this regex listed the verbs I happened to be fixing
// in #799 (lambda + cloudformation) rather than the verbs that mutate
// deployed state. The most security-relevant script class was the one class
// outside the gate.
var deploys = regexp.MustCompile(`aws (lambda (create-function|update-function-code|update-function-configuration)|cloudformation (deploy|create-stack|update-stack)|iam (create|put|attach|update)-[a-z-]+)`)

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

// `--environment` on update-function-configuration REPLACES the whole variable
// map, so a LITERAL map is unconditionally destructive on an update path.
//
// The first version of this gate matched only `Variables={}` — the empty map.
// That was the anecdote, not the mechanism: the bug it was written for happened
// to wipe the environment to nothing, and "wipes it to nothing" is the memorable
// half. deploy-custom-dns.sh passed
// `Variables={HOSTED_ZONE_ID=$HOSTED_ZONE_ID,DOMAIN=$DOMAIN}` against a live
// function whose only variable was DOMAIN_ZONES, and sailed through: non-empty,
// so not `{}`, and just as destructive. It would also have silently cleared
// DNS_AUTHZ_MODE, turning off #835's enforcement as a side effect of shipping
// unrelated code. CLAUDE.md rule 6 — gate the mechanism (a literal cannot
// preserve anything) rather than a correlate of it (the literal was empty).
//
// The permitted form is a shell variable built from a prior read of the deployed
// map, so both halves are checked: no literal, and the read must exist.
//
// Replacing IS right for a script that is meant to be the declarative source of
// truth for its function's configuration — deploy-scheduler-handler.sh argues
// exactly that, and the argument holds (the live function carries only the two
// variables that script sets, checked). So the property gated here is not "never
// replace" but "replacing is a decision that has to be stated next to the call",
// via an `env-replace-ok:` marker in the adjacent comment. Same shape as the
// `swallow-ok:` marker this repo already uses for a deliberate `|| true`, and
// adjacency is the point: a marker anywhere in the file would licence every call
// in it.
func TestDeployScriptsDoNotReplaceTheEnvironmentOnUpdate(t *testing.T) {
	for _, f := range lambdaDeployingScripts(t) {
		s := readRel(t, f)
		cmds := awsCommands(s, "update-function-configuration")
		for _, c := range cmds {
			if !strings.Contains(c, "--environment") {
				continue
			}
			if !envLiteral.MatchString(c) {
				if !strings.Contains(s, "Environment.Variables") {
					t.Errorf("%s passes --environment on an update but never reads the deployed "+
						"Environment.Variables, so it cannot be preserving them", f)
				}
				continue
			}
			if deliberateEnvReplace(t, f, c) {
				continue
			}
			t.Errorf("%s passes a literal Variables={...} to update-function-configuration:\n"+
				"  %s\n"+
				"  --environment REPLACES the entire map, so this deletes every variable not "+
				"named here, including any set out of band. Either read the deployed map with "+
				"`get-function-configuration --query 'Environment.Variables'` and merge, or — if "+
				"this script is deliberately the source of truth for that function's config — say "+
				"so with an `env-replace-ok: <reason>` comment beside the call.", f, c)
		}
	}
}

// envLiteral matches an inline variable map, quoted or not.
var envLiteral = regexp.MustCompile(`--environment\s+"?Variables=\{`)

// deliberateEnvReplace reports whether the comment block adjacent to the call
// carrying the literal declares the replacement intentional.
//
// Adjacency is enforced by walking UP from the --environment line through the
// rest of the command, blank lines and comments, and stopping at the first line
// that is none of those — so the marker has to be attached to this call rather
// than to the file.
func deliberateEnvReplace(t *testing.T, rel, cmd string) bool {
	t.Helper()
	return adjacentMarker(t, rel, envLiteral, cmd, "env-replace-ok:")
}

// adjacentMarker reports whether the comment block attached to the command
// carrying a match for lineRe declares the decision with marker.
//
// Adjacency is the whole point and is enforced by walking UP from the matching
// line through the rest of the command, blank lines and comments, stopping at the
// first line that is none of those — so a marker has to be attached to this call
// rather than sitting anywhere in the file. Shared by every "state the decision"
// gate here so the walk exists once.
func adjacentMarker(t *testing.T, rel string, lineRe *regexp.Regexp, cmd, marker string) bool {
	t.Helper()
	// The RAW file, not read()'s comment-stripped form. read() drops comment lines
	// on purpose — a gate that scans raw text flags a comment explaining a fixed
	// bug as the bug (rule 3) — but the marker being looked for here IS a comment,
	// so this one case has to go around it. The first version of this function
	// took the stripped text and could never match, while reporting the two
	// annotated calls as unannotated.
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) //nolint:gosec // repo's own scripts
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if !lineRe.MatchString(line) {
			continue
		}
		// Confirm this line belongs to the command being examined, not another.
		if fn := commandFunctionName(cmd); fn != "" && !commandNear(lines, i, fn) {
			continue
		}
		for j := i - 1; j >= 0 && i-j <= 40; j-- {
			t := strings.TrimSpace(lines[j])
			switch {
			case t == "":
				continue
			case strings.HasPrefix(t, "#"):
				if strings.Contains(t, marker) {
					return true
				}
			case strings.HasSuffix(t, "\\"), strings.HasPrefix(t, "aws "):
				continue
			default:
				return false
			}
		}
	}
	return false
}

// commandFunctionName extracts the --function-name argument, used only to tell
// two update-function-configuration calls in one script apart.
func commandFunctionName(cmd string) string {
	f := strings.Fields(cmd)
	for i, w := range f {
		if w == "--function-name" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// commandNear reports whether fn appears within a few lines of idx, i.e. in the
// same invocation.
func commandNear(lines []string, idx int, fn string) bool {
	for j := idx - 8; j <= idx+8; j++ {
		if j >= 0 && j < len(lines) && strings.Contains(lines[j], fn) {
			return true
		}
	}
	return false
}

// Attribute-scoped DynamoDB grants must also pin dynamodb:Select.
//
// ForAllValues:StringEquals is satisfied by an EMPTY set, so a request naming no
// attributes at all passes an `ForAllValues:StringEquals dynamodb:Attributes`
// condition and reads every field of every item. Pinning Select to
// SPECIFIC_ATTRIBUTES is what turns that from a loophole into a denial — the same
// family as ForAllValues:StringNotEquals on aws:TagKeys being defeated by one
// innocuous tag key. Simulated both ways before this gate was written.
func TestAttributeScopedDynamoGrantsPinSelect(t *testing.T) {
	checked := 0
	for _, f := range shellScripts(t) {
		s := readRel(t, f)
		if !strings.Contains(s, "dynamodb:Attributes") {
			continue
		}
		checked++
		if !strings.Contains(s, `"dynamodb:Select": "SPECIFIC_ATTRIBUTES"`) {
			t.Errorf("%s scopes a DynamoDB grant with dynamodb:Attributes but does not pin "+
				"dynamodb:Select to SPECIFIC_ATTRIBUTES. ForAllValues is true for an empty set, "+
				"so a Scan naming no attributes satisfies the condition and returns every "+
				"field of every item — the attribute scoping is decorative without it.", f)
		}
	}
	// Rule 4: an empty discovery is reported, not silently passed. This gate was
	// added alongside the only such grant in the tree, so zero matches means that
	// grant was removed (update this gate deliberately) or the scan has broken.
	if checked == 0 {
		t.Error("no script uses dynamodb:Attributes — this gate shipped with the grant in " +
			"deploy-custom-dns.sh, so either that was removed or this discovery has broken")
	}
}

// awsCommands returns each logical `aws ...` command containing verb, with
// backslash line-continuations joined, so a multi-line invocation is examined as
// the one command it is rather than as unrelated lines.
func awsCommands(script, verb string) []string {
	joined := strings.ReplaceAll(script, "\\\n", " ")
	var out []string
	for _, line := range strings.Split(joined, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "aws ") && strings.Contains(line, verb) {
			out = append(out, strings.Join(strings.Fields(line), " "))
		}
	}
	return out
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

// A script may not create a public, unauthenticated API Gateway route (#870).
//
// deploy-custom-dns.sh created a REST API with POST /update-dns at
// authorizationType NONE and wired it to the DNS updater — a public endpoint
// into the function that writes records in the real spore.host zone. It could
// not actually write (the integration was incompatible with the handler's
// Function URL event type, so it returned 502) but it did invoke a billable
// function, and it was one handler-signature change away from being live.
//
// Two ways to get NONE, and the gate has to catch both: naming it, and omitting
// the flag entirely, since the API defaults to NONE when it is absent. The
// second is the one that would slip through review.
//
// Deliberately NOT failing on an empty discovery, unlike the other gates here.
// Zero API Gateway methods is a correct state — the DNS updater is reached
// through its Function URL. The rule is conditional: if a script creates a route,
// the authorization decision must be explicit.
//
// And "authenticated at the edge" is NOT the rule, because that would need an
// allowlist of the things that are fine — rule 6's tell. On its first run this
// gate flagged two scripts that are deliberate: `setup-dashboard-api-gateway.sh`
// and `setup-websocket-api.sh` both create open routes and authenticate in the
// HANDLER instead — dashboard-api via getUserFromRequest /
// getUserFromCredentialsHeader / verifySlackRequest, and the websocket via a
// ticket minted by `POST /api/ws-token`. That is a legitimate design, just a
// weaker one, so the rule is that leaving the edge open has to be SAID, with the
// reason, next to the call. Same `-ok:` convention as env-replace-ok above.
func TestNoDeployScriptCreatesAnUnauthenticatedAPIGatewayRoute(t *testing.T) {
	for _, f := range shellScripts(t) {
		// read() strips comments, so the paragraph above — which names the flag
		// it forbids — cannot satisfy this gate (rule 3).
		s := readRel(t, f)
		for _, verb := range []string{"apigateway put-method", "apigatewayv2 create-route"} {
			for _, c := range awsCommands(s, verb) {
				auth := commandFlagValue(c, "--authorization-type")
				if auth == "" || auth == "NONE" || auth == "none" {
					if adjacentMarker(t, f, regexp.MustCompile(regexp.QuoteMeta(verb)), c, "authz-none-ok:") {
						continue
					}
				}
				switch auth {
				case "":
					t.Errorf("%s runs `aws %s` with no --authorization-type:\n  %s\n"+
						"  The API defaults to NONE when the flag is absent, so this creates a "+
						"PUBLIC unauthenticated route. Name an authorizer explicitly.", f, verb, c)
				case "NONE", "none":
					t.Errorf("%s creates an API Gateway route with --authorization-type NONE:\n  %s\n"+
						"  That is a public unauthenticated entry point. The DNS updater is reached "+
						"through its Function URL with AuthType: AWS_IAM (#173); a REST API in front "+
						"of it was removed in #870.", f, c)
				}
			}
		}
	}
}

// commandFlagValue returns the token after flag in a joined command, or "" if
// the flag is absent. The NEXT token specifically — a flag's value is never
// anywhere else on the line, and treating "appears later" as "is the value" is
// how a gate ends up with forty false positives (CLAUDE.md rule 6).
func commandFlagValue(cmd, flag string) string {
	f := strings.Fields(cmd)
	for i, w := range f {
		if w == flag && i+1 < len(f) {
			return strings.Trim(f[i+1], `"'`)
		}
	}
	return ""
}

// goBuild is a `go build` invocation in a script, and goBuildFiles one whose
// target is a list of .go files rather than a package.
var (
	goBuild      = regexp.MustCompile(`(?m)^[^#\n]*\bgo build\b[^\n]*$`)
	goBuildFiles = regexp.MustCompile(`\s\S+\.go\s*$`)
)

// A Lambda is built as a package, never as a list of files. `go build main.go`
// compiles only main.go, so a function that grows a second file stops
// building: deploy-sweep-orchestrator.sh did exactly that once the orchestrator
// gained spored.go, tags.go and cancel.go, and no deploy could have run (#1368).
func TestLambdaBuildsArePackages(t *testing.T) {
	builds := 0
	for _, f := range shellScripts(t) {
		for _, line := range goBuild.FindAllString(readRel(t, f), -1) {
			builds++
			if goBuildFiles.MatchString(line) {
				t.Errorf("%s builds a file list, which compiles only the files named: %s", f, strings.TrimSpace(line))
			}
		}
	}
	if builds == 0 {
		t.Fatal("found no go build in any script; the discovery is broken, not the repo")
	}
}

// The orchestrator renders the spored bootstrap of legacy sweep members, whose
// stub fetches the body published for the release it was built from (#1368).
// So the deploy refuses anything but a clean release tag, refuses a release
// whose body is not published or is not the one compiled in, and injects that
// release into the build.
func TestDeploySweepOrchestratorDeploysOnlyAPublishedRelease(t *testing.T) {
	s := read(t, "deploy-sweep-orchestrator.sh")
	for what, re := range map[string]string{
		"take the version from the exact release tag":   `(?m)^SPAWN_VERSION=\$\(git describe --tags --exact-match `,
		"refuse a version that is not X.Y.Z":            `(?m)^if ! \[\[ "\$SPAWN_VERSION" =~ \^\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\$ \]\]; then\n\s+echo[^\n]*\n\s+exit 1`,
		"refuse a dirty tree":                           `(?m)^if \[ -n "\$\(git status --porcelain\)" \]; then\n\s+echo[^\n]*\n\s+exit 1`,
		"hash the body this release compiles in":        `(?m)^if ! WANT_BODY_SHA=\$\( \(cd \.\./\.\. && [^\n]*go run \. bootstrap-body\)`,
		"fetch the body published for the version":      `(?m)^BODY_URL="https://spawn-binaries-us-east-1\.s3\.us-east-1\.amazonaws\.com/spawn/versions/\$\{SPAWN_VERSION\}/bootstrap\.sh"`,
		"refuse a body that differs":                    `(?m)^if \[ "\$GOT_BODY_SHA" != "\$WANT_BODY_SHA" \]; then\n\s+echo[^\n]*\n\s+exit 1`,
		"inject the version into the build":             `(?m)^GOOS=linux [^\n]*go build [^\n]*-ldflags "-X main\.Version=\$\{SPAWN_VERSION\}"`,
		"stamp the version it built, not a re-describe": `--tags "[^"]*spawn:version=\$SPAWN_VERSION"`,
	} {
		if !regexp.MustCompile(re).MatchString(s) {
			t.Errorf("deploy-sweep-orchestrator.sh does not %s", what)
		}
	}
	// The stamp is the checked version; a second assignment would let it drift.
	if n := len(regexp.MustCompile(`(?m)^\s*SPAWN_VERSION=`).FindAllString(s, -1)); n != 1 {
		t.Errorf("SPAWN_VERSION is assigned %d times, want once", n)
	}
	// And the checks come before the build.
	if strings.Index(s, "GOT_BODY_SHA\" != ") > strings.Index(s, "go build") {
		t.Error("the body check runs after the build")
	}
}
