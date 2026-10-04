package reaperiam

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reaperSource is the reaper's own code, which this test reads so the gate cannot be
// satisfied by a hand-maintained list that has gone stale — the failure mode behind
// spawn#622 and lagotto #149/#151/#153.
const reaperSource = "../../lambda/ttl-reaper"

// apiServicePrefix maps the reaper's *API interface names and SDK client variables to
// the IAM service prefix their methods belong to.
var apiServicePrefix = map[string]string{
	"ec2": "ec2",
	"fsx": "fsx",
	"ssm": "ssm",
}

// methodCall matches a call on one of the reaper's SDK clients, e.g.
// `ssmClient.SendCommand(` or `fsxClient.DeleteFileSystem(`.
var methodCall = regexp.MustCompile(`\b(ec2|fsx|ssm)Client\.([A-Z][A-Za-z0-9]+)\(`)

// ifaceMethod matches a method line inside a `type xAPI interface { … }` block.
var ifaceMethod = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9]+)\(ctx context\.Context`)

var ifaceOpen = regexp.MustCompile(`^type ([a-z0-9]+)API interface \{`)

// reaperAWSCalls returns every AWS API action the reaper's source calls, as
// "service:Action", discovered from (a) the methods declared on its *API interfaces
// and (b) direct calls on its SDK client variables.
func reaperAWSCalls(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(reaperSource)
	if err != nil {
		t.Fatalf("read %s: %v", reaperSource, err)
	}

	calls := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(reaperSource, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)

		// (a) *API interface method declarations.
		var svc string
		for _, line := range strings.Split(src, "\n") {
			if m := ifaceOpen.FindStringSubmatch(line); m != nil {
				svc = apiServicePrefix[m[1]] // "" for interfaces we don't map (e.g. registryAPI)
				continue
			}
			if svc == "" {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "}") {
				svc = ""
				continue
			}
			if m := ifaceMethod.FindStringSubmatch(line); m != nil {
				calls[svc+":"+m[1]] = true
			}
		}

		// (b) direct client calls.
		for _, m := range methodCall.FindAllStringSubmatch(src, -1) {
			calls[apiServicePrefix[m[1]]+":"+m[2]] = true
		}
	}

	if len(calls) == 0 {
		t.Fatalf("discovered no AWS calls in %s — the gate would pass vacuously", reaperSource)
	}
	return calls
}

// TestPolicyCoversEveryReaperAPICall is the structural end of the drift class.
//
// Every AWS action the reaper's code calls must be granted by the scan-self policy.
// This is the gate that would have caught the gap this package was written for: the
// CFN template granted the Lambda no fsx: actions at all, while the reaper's fsxAPI
// calls DescribeFileSystems and DeleteFileSystem — so a reaper deployed to scan its
// own account could terminate instances but would silently fail to delete the
// orphaned filesystem that #613 was reported for.
func TestPolicyCoversEveryReaperAPICall(t *testing.T) {
	granted := GrantedActions()
	calls := reaperAWSCalls(t)

	var missing []string
	for call := range calls {
		if !granted[call] {
			missing = append(missing, call)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the reaper calls AWS actions the scan-self policy does not grant: %s\n"+
			"Add them to ScanSelfStatements. A missing grant here does not fail loudly — it makes the\n"+
			"reaper silently skip whatever that call was for (spawn#622, lagotto#149/#151/#153).",
			strings.Join(missing, ", "))
	}

	// Report the discovered set so a reviewer can see the gate is looking at
	// something real rather than passing on an empty scan.
	var found []string
	for c := range calls {
		found = append(found, c)
	}
	sort.Strings(found)
	t.Logf("reaper AWS calls discovered from source: %s", strings.Join(found, ", "))
}

// TestPolicyGrantsNothingTheReaperDoesNotCall is the other direction: an over-broad
// policy is its own problem, and an unused grant is usually a leftover.
func TestPolicyGrantsNothingTheReaperDoesNotCall(t *testing.T) {
	calls := reaperAWSCalls(t)
	for action := range GrantedActions() {
		if !calls[action] {
			t.Errorf("the policy grants %s but the reaper never calls it — remove it, or the "+
				"least-privilege claim is only approximately true", action)
		}
	}
}

// TestDestructiveActionsAreTagScoped: the reaper runs in an account that may hold
// resources spawn knows nothing about. An unconditioned TerminateInstances there is
// an outage waiting for a tagging mistake.
func TestDestructiveActionsAreTagScoped(t *testing.T) {
	for _, st := range ScanSelfStatements() {
		for _, action := range st.Action {
			if action != "ec2:TerminateInstances" {
				continue
			}
			if st.Condition == nil {
				t.Fatal("ec2:TerminateInstances must be conditioned on spawn:managed=true")
			}
			raw, err := json.Marshal(st.Condition)
			if err != nil {
				t.Fatalf("marshal condition: %v", err)
			}
			cond := string(raw)
			if !strings.Contains(cond, "spawn:managed") || !strings.Contains(cond, "true") {
				t.Errorf("TerminateInstances condition must require spawn:managed=true, got %s", cond)
			}
		}
	}
}

func TestScanSelfPolicyDocumentIsValidJSON(t *testing.T) {
	doc, err := ScanSelfPolicyDocument()
	if err != nil {
		t.Fatalf("ScanSelfPolicyDocument: %v", err)
	}

	var parsed struct {
		Version   string                   `json:"Version"`
		Statement []map[string]interface{} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, doc)
	}
	if parsed.Version != "2012-10-17" {
		t.Errorf("Version = %q", parsed.Version)
	}
	if len(parsed.Statement) == 0 {
		t.Fatal("policy has no statements")
	}

	// IAM rejects unknown statement elements with MalformedPolicyDocument, which
	// would break the deploy rather than degrade it (the mistake made in #628).
	allowed := map[string]bool{
		"Sid": true, "Effect": true, "Action": true, "NotAction": true,
		"Resource": true, "NotResource": true, "Condition": true,
		"Principal": true, "NotPrincipal": true,
	}
	for i, st := range parsed.Statement {
		for k := range st {
			if !allowed[k] {
				t.Errorf("statement %d has non-IAM element %q", i, k)
			}
		}
	}

	// sts:AssumeRole must NOT appear: this is the scan-self policy, and granting it
	// would reintroduce the cross-account shape the whole feature exists to avoid.
	if strings.Contains(doc, "sts:AssumeRole") {
		t.Error("the scan-self policy must not grant sts:AssumeRole — self-hosting exists " +
			"precisely because cross-account trust is unavailable in a governed account")
	}
}

// crossAccountRolePath is the CFN template deployed into every account spawn
// launches into. It grants the TARGET-ACCOUNT half of the reaper's permissions —
// the same per-account operations ScanSelfStatements covers, just assumed rather
// than native.
const crossAccountRolePath = "../../deployment/cloudformation/ttl-reaper-cross-account-role.yaml"

// crossAccountGrantedActions scrapes the actions the cross-account role allows.
//
// A regex over the YAML rather than a parse: the template is full of CFN
// shorthand (!Sub, !Ref, !GetAtt) that a plain YAML unmarshaller rejects, and
// pulling in a CFN-aware parser to read a list of action strings is not worth it.
func crossAccountGrantedActions(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(crossAccountRolePath)
	if err != nil {
		t.Fatalf("read %s: %v", crossAccountRolePath, err)
	}
	// `                  - ec2:DescribeInstances` — a list item that looks like an
	// IAM action. Condition keys (ec2:ResourceTag/...) are excluded by requiring
	// no slash.
	re := regexp.MustCompile(`(?m)^\s*-\s+((?:ec2|fsx|ssm|sts|iam|dynamodb|kms|route53):[A-Za-z]+)\s*$`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("scraped no actions from %s — has the shape changed?", crossAccountRolePath)
	}
	return out
}

// TestCrossAccountRoleCoversEveryReaperAPICall is spawn#652.
//
// #625 added the missing fsx:/ssm: grants to the Lambda's OWN execution role,
// which covers REAPER_SCAN_SELF. It did not touch the cross-account role, so the
// asymmetry simply inverted: scan-self had the SSM grants and cross-account —
// the production configuration — did not. REAPER_GRACEFUL therefore degraded to
// an immediate hard kill in the mode it is actually deployed in.
//
// It degraded quietly rather than dangerously: tryGracefulPreStop is best-effort
// and the terminate after it always runs, so #72's hard-deadline guarantee held
// throughout. But the drift gate added in #625 only ever read the scan-self
// policy, which is exactly why this gap survived it. Now both roles are checked
// against the same discovered call set.
func TestCrossAccountRoleCoversEveryReaperAPICall(t *testing.T) {
	granted := crossAccountGrantedActions(t)
	calls := reaperAWSCalls(t)

	var missing []string
	for call := range calls {
		if !granted[call] {
			missing = append(missing, call)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the reaper calls AWS actions the CROSS-ACCOUNT role does not grant: %s\n"+
			"Add them to %s. Cross-account is the PRODUCTION configuration "+
			"(ScanSelf=false), so a gap here is not hypothetical — and it will not fail "+
			"loudly: the reaper skips whatever that call was for.",
			strings.Join(missing, ", "), crossAccountRolePath)
	}
}

// TestBothReaperRolesGrantTheSameTargetAccountActions keeps the two from drifting
// in either direction. They do the same per-account work — one natively, one via
// an assumed role — so a grant added to one and not the other is the defect that
// produced #622, #652, and lagotto#149/#151/#153.
//
// Intentionally ignores ec2:DescribeTags, which the cross-account role grants and
// the reaper never calls (tags come from the DescribeInstances response). That is
// a leftover rather than a drift, and TestPolicyGrantsNothingTheReaperDoesNotCall
// already covers the scan-self side.
func TestBothReaperRolesGrantTheSameTargetAccountActions(t *testing.T) {
	self := GrantedActions()
	cross := crossAccountGrantedActions(t)
	ignore := map[string]bool{"ec2:DescribeTags": true}

	for a := range self {
		if !cross[a] && !ignore[a] {
			t.Errorf("scan-self grants %s but the cross-account role does not — the two roles "+
				"perform the same per-account operations, so a one-sided grant means the "+
				"feature works in one deployment mode and silently no-ops in the other", a)
		}
	}
	for a := range cross {
		if !self[a] && !ignore[a] {
			t.Errorf("the cross-account role grants %s but scan-self does not — same problem, "+
				"other direction (this is how #625's fsx:/ssm: gap arose)", a)
		}
	}
}
