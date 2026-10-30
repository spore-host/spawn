package iamcoverage

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	spawncmd "github.com/spore-host/spawn/cmd"
	"github.com/spore-host/spawn/internal/treewalk"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/reaperiam"
	"github.com/spore-host/spawn/pkg/taskiam"
	"golang.org/x/tools/go/ssa"
)

// repoRoot is the spawn repository, from this package's directory.
const repoRoot = "../.."

const sporedPkg = "github.com/spore-host/spawn/cmd/spored"

// build is one main package as it is compiled for a principal.
type build struct {
	dir  string   // module directory, relative to repoRoot
	pkg  string   // package pattern, relative to dir
	goos []string // every platform the principal runs on; inventories are unioned
}

// variant is one role a principal's code can run under. Each variant must
// cover every call on its own: spored runs under the shared
// spored-instance-role OR a spawn-instance-* role, never both.
type variant struct {
	name   string
	policy func(t *testing.T) *Policy
}

// credContext is one set of credentials a principal uses. A Lambda that
// assumes a role into a member account makes its EC2 calls with that role and
// its DynamoDB calls with its own, so a grant in the wrong one is still a
// denial. services are the IAM prefixes this context's clients call; nil means
// every service no other context claims. An entry with a colon is one action
// (s3:GetObject), for a service whose calls are split between contexts; an
// action claimed that way beats any prefix. pkgs claims call sites instead:
// an action called from a function in one of these packages runs on this
// context, whatever its name, for a service whose calls (s3:ListBucket, say)
// are made on different credentials by different code. A site claim beats
// both.
type credContext struct {
	name     string
	services []string
	pkgs     []string
	variants []variant
}

// principal is one identity spore.host provisions, and the code that runs as it.
type principal struct {
	name  string
	build build
	// roots are the SSA functions the principal's code starts at; empty means
	// the program's entry points (main, and any Lambda handler).
	roots []string
	// cuts are functions whose subtree runs under a different principal: a
	// spored subcommand that only ever runs on a feature role.
	cuts     []string
	contexts []credContext
	// passes are the roles the principal's code passes, for a principal that
	// calls iam:PassRole (#1397). PassRole is resource-scoped, and which role a
	// call passes is a runtime value (a launch's iam_role, a profile name read
	// from state), so the gate cannot read it off the call site; it is
	// declared here from the code's own constants, and the gate checks the
	// policy lets each one be passed to its service. This proves the grant
	// covers the roles spawn passes by design, not that the code passes only
	// those.
	passes []passedRole
}

// passedRole is one role a principal passes, and the service it passes it to.
type passedRole struct {
	role    string // role name, an example of the family for a prefix
	service string // iam:PassedToService
}

// --- policy sources -------------------------------------------------------

func mustPolicy(t *testing.T) func(*Policy, error) *Policy {
	return func(p *Policy, err error) *Policy {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
}

func goPolicy(source, doc string) func(*testing.T) *Policy {
	return func(t *testing.T) *Policy {
		t.Helper()
		if doc == "" {
			t.Fatalf("%s rendered an empty policy", source)
		}
		return mustPolicy(t)(ParsePolicyJSON(source, doc))
	}
}

func cfnPolicy(file string, ids ...string) func(*testing.T) *Policy {
	return func(t *testing.T) *Policy {
		t.Helper()
		return mustPolicy(t)(ParseCFNPolicies(filepath.Join(repoRoot, file), ids...))
	}
}

func shellPolicy(file string) func(*testing.T) *Policy {
	return func(t *testing.T) *Policy {
		t.Helper()
		return mustPolicy(t)(ParseShellPolicies(filepath.Join(repoRoot, file)))
	}
}

func union(parts ...func(*testing.T) *Policy) func(*testing.T) *Policy {
	return func(t *testing.T) *Policy {
		t.Helper()
		p := NewPolicy()
		for _, part := range parts {
			p.Merge(part(t))
		}
		return p
	}
}

// Representative inputs for the policies spawn renders per launch. Only the
// actions are compared, so the values only need to be well-formed.
const (
	exAccount = "123456789012"
	exRegion  = "us-east-1"
)

var (
	sporedBaseline = goPolicy("pkg/aws SporedBaselinePolicyDocument (spored-baseline-policy)",
		spawnaws.SporedBaselinePolicyDocument())
	sporedInstanceRole = goPolicy("pkg/aws SporedInstanceRolePolicyDocument (spored-instance-role)",
		spawnaws.SporedInstanceRolePolicyDocument())
	poolWorkerInline = goPolicy("cmd PoolWorkerPolicy",
		spawncmd.PoolWorkerPolicy(exAccount, exRegion, "spawn-pool-specs-ex", "r1", []string{"in-bucket"}, []string{"out-bucket"}))
	stageData = goPolicy("pkg/pipeline StageDataPolicy",
		pipeline.StageDataPolicy("pipeline-bucket", "pipelines/ex"))
	taskRun = goPolicy("pkg/taskiam TaskRunPolicyFor",
		taskiam.TaskRunPolicyFor([]string{"in-bucket"}, []string{"out-bucket"}, "spawn-results-ex", nil, "spawn-staging-ex"))
)

func batchQueue(t *testing.T) *Policy {
	t.Helper()
	doc, err := spawncmd.BatchQueuePolicy("spawn-schedules-ex", "out-bucket", "runs/ex")
	if err != nil {
		t.Fatal(err)
	}
	return mustPolicy(t)(ParsePolicyJSON("cmd BatchQueuePolicy", doc))
}

func scanSelfPolicy(t *testing.T) *Policy {
	t.Helper()
	doc, err := reaperiam.ScanSelfPolicyDocument()
	if err != nil {
		t.Fatal(err)
	}
	return mustPolicy(t)(ParsePolicyJSON("pkg/reaperiam ScanSelfPolicyDocument", doc))
}

// --- the principals -------------------------------------------------------

// sporeRolesToEC2 are the instance roles an orchestrator launches a spore
// with: spored-instance-role (the profile the CLI ensures and seeds as a
// member's or stage's default iam_role) and a per-policy spawn-instance-<hash>
// role (--iam-policy and friends, #1331).
var sporeRolesToEC2 = []passedRole{
	{role: spawnaws.SporedRoleName, service: "ec2.amazonaws.com"},
	{role: instancerole.NamePrefix + "abc12345", service: "ec2.amazonaws.com"},
}

var sporedBuild = build{dir: ".", pkg: "./cmd/spored", goos: []string{"linux", "windows"}}

// sporedFeatureEntries are the spored subcommands that only run on an instance
// launched with a feature role. Their subtrees are their own principals below;
// cutting them out of the core keeps a feature's grant from being demanded of
// every spore.
var sporedFeatureEntries = []string{
	sporedPkg + ".runPoolWorker",
	sporedPkg + ".runPipelineStage",
	sporedPkg + ".provStageIn",
	sporedPkg + ".provUpload",
	"(*github.com/spore-host/spawn/pkg/agent.QueueRunner).Run",
}

var principals = []principal{
	{
		// Every spore: the daemon and the in-instance CLI. It runs under
		// spored-instance-role when the launch named no IAM, and under a
		// spawn-instance-* role (the baseline plus the caller's grants) when it
		// did — so the baseline alone must cover it.
		name:  "spored",
		build: sporedBuild,
		cuts:  sporedFeatureEntries,
		contexts: []credContext{{name: "instance role", variants: []variant{
			{"spawn-instance-* (spored-baseline-policy)", sporedBaseline},
			{"spored-instance-role", sporedInstanceRole},
		}}},
	},
	{
		name:  "spored pool-worker",
		build: sporedBuild,
		roots: []string{sporedPkg + ".runPoolWorker"},
		contexts: []credContext{{name: "instance role", variants: []variant{
			{"spawn-pool-worker-* (baseline + PoolWorkerPolicy)", union(sporedBaseline, poolWorkerInline)},
		}}},
	},
	{
		// `spawn pipeline launch` gives stages a spawn-instance-* role carrying
		// the pipeline's StageDataPolicy (#911).
		name:  "spored run-pipeline-stage",
		build: sporedBuild,
		roots: []string{sporedPkg + ".runPipelineStage"},
		contexts: []credContext{{name: "instance role", variants: []variant{
			{"pipeline stage role (baseline + StageDataPolicy)", union(sporedBaseline, stageData)},
		}}},
	},
	{
		// `spored provenance stage-in|upload` runs inside a `spawn task run`
		// task (and a pool worker's job script), on the task role.
		name:  "spored provenance",
		build: sporedBuild,
		roots: []string{sporedPkg + ".provStageIn", sporedPkg + ".provUpload"},
		contexts: []credContext{{name: "instance role", variants: []variant{
			{"task role (baseline + TaskRunPolicyFor)", union(sporedBaseline, taskRun)},
			{"spawn-pool-worker-* (baseline + PoolWorkerPolicy)", union(sporedBaseline, poolWorkerInline)},
		}}},
	},
	{
		// `spawn launch --batch-queue` with no --iam-role gives the queue a
		// spawn-instance-* role carrying BatchQueuePolicy (#1344).
		name:  "spored run-queue",
		build: sporedBuild,
		roots: []string{"(*github.com/spore-host/spawn/pkg/agent.QueueRunner).Run"},
		contexts: []credContext{{name: "instance role", variants: []variant{
			{"batch queue role (baseline + BatchQueuePolicy)", union(sporedBaseline, batchQueue)},
		}}},
	},
	{
		// The reaper's own credentials make the STS, Route53 and registry calls;
		// everything that acts on a member account's resources runs on the
		// assumed cross-account role, or, with ScanSelf, on the function's own
		// role carrying the scan-self policy.
		name:  "ttl-reaper",
		build: build{dir: "lambda/ttl-reaper", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{
			// s3:GetObject is the schedules' params (#1378, pkg/instancerole
			// ScheduleSource), read in the reaper's own account; nothing the
			// reaper does in a member account reads an object.
			// pkg/schedulesweep is the orphaned schedule params sweep (#1398),
			// on the same own-account credentials; its s3:ListBucket and
			// s3:DeleteObject are also called in member accounts by other
			// sweeps, so it is claimed by call site, not by action. This gate
			// checks action names only, and the SAM variant also holds the
			// ScanSelf branch's ListBucket/DeleteObject on other buckets, so
			// the resource scope of those two is held by pkg/reaperiam's
			// TestSAMTemplateGrantsTheScheduleRead, not here.
			{name: "function role", services: []string{"sts", "route53", "dynamodb", "kms", "s3:GetObject"},
				pkgs: []string{"github.com/spore-host/spawn/pkg/schedulesweep"}, variants: []variant{
					{"SAM TTLReaperFunction", cfnPolicy("lambda/ttl-reaper/template.yaml", "TTLReaperFunction")},
				}},
			{name: "target account", variants: []variant{
				{"cross-account TTLReaperCrossAccountRole", cfnPolicy("deployment/cloudformation/ttl-reaper-cross-account-role.yaml", "TTLReaperCrossAccountRole")},
				{"scan-self (pkg/reaperiam, written by `spawn reaper deploy`)", scanSelfPolicy},
				{"scan-self (SAM TTLReaperFunction, ScanSelf=true)", cfnPolicy("lambda/ttl-reaper/template.yaml", "TTLReaperFunction")},
			}},
		},
	},
	{
		name:   "sweep-orchestrator",
		build:  build{dir: "lambda/sweep-orchestrator", pkg: ".", goos: []string{"linux"}},
		passes: sporeRolesToEC2,
		contexts: []credContext{
			{name: "member account", services: []string{"ec2", "iam"}, variants: []variant{
				{"SpawnSweepCrossAccountRole", shellPolicy("scripts/setup-sweep-cross-account-role.sh")},
			}},
			{name: "function role", variants: []variant{
				{"SpawnSweepOrchestratorRole", shellPolicy("scripts/setup-sweep-orchestrator-lambda-role.sh")},
			}},
		},
	},
	{
		name:  "scheduler-handler",
		build: build{dir: "lambda/scheduler-handler", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{{name: "function role", variants: []variant{
			{"SpawnSchedulerHandlerExecutionRole", cfnPolicy("deployment/cloudformation/scheduler-handler-role.yaml", "SchedulerHandlerExecutionRole")},
		}}},
	},
	{
		// The role `scripts/deploy-custom-dns.sh` creates for a self-hosted
		// updater (#891). spore.host's own updater's role is hand-managed in
		// the infra account and is not in this repository.
		name:  "dns-updater",
		build: build{dir: "lambda/dns-updater", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{{name: "function role", variants: []variant{
			{"deploy-custom-dns.sh role", shellPolicy("scripts/deploy-custom-dns.sh")},
		}}},
	},
	{
		name:   "pipeline-orchestrator",
		build:  build{dir: ".", pkg: "./lambda/pipeline-orchestrator", goos: []string{"linux"}},
		passes: sporeRolesToEC2,
		contexts: []credContext{{name: "function role", variants: []variant{
			{"SAM PipelineOrchestratorFunction", cfnPolicy("lambda/pipeline-orchestrator/template.yaml", "PipelineOrchestratorFunction")},
		}}},
	},
	{
		// EC2 reads in a member account are made AS THE CALLER (#815): the
		// caller's credentials assume SpawnDashboardCrossAccountReadRole. That
		// AssumeRole is the caller's, not SpawnDashboardLambdaRole's, so its one
		// function is cut (#1349); the caller's own role is the dashboard's
		// Cognito role, provisioned outside this repository.
		name:  "dashboard-api",
		build: build{dir: "lambda/dashboard-api", pkg: ".", goos: []string{"linux"}},
		cuts:  []string{"github.com/spore-host/spawn/lambda/dashboard-api.callerAssumeRoleProvider"},
		contexts: []credContext{
			{name: "member account", services: []string{"ec2"}, variants: []variant{
				{"SpawnDashboardCrossAccountReadRole", shellPolicy("lambda/dashboard-api/scripts/setup-cross-account-role.sh")},
			}},
			{name: "function role", variants: []variant{
				{"SpawnDashboardLambdaRole", shellPolicy("lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh")},
			}},
		},
	},
	{
		name:  "dashboard-websocket",
		build: build{dir: "lambda/dashboard-websocket", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{{name: "function role", variants: []variant{
			{"SpawnDashboardLambdaRole", shellPolicy("lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh")},
		}}},
	},
	{
		name:  "dashboard-websocket-processor",
		build: build{dir: "lambda/dashboard-websocket-processor", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{{name: "function role", variants: []variant{
			{"SpawnDashboardLambdaRole", shellPolicy("lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh")},
		}}},
	},
	{
		name:  "cost-history-collector",
		build: build{dir: "lambda/cost-history-collector", pkg: ".", goos: []string{"linux"}},
		contexts: []credContext{{name: "function role", variants: []variant{
			{"SpawnDashboardLambdaRole", shellPolicy("lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh")},
		}}},
	},
}

// --- inventories ----------------------------------------------------------

type principalInventory struct {
	actions  map[string][]Site
	unmapped []string
	paths    map[string][]string // site func -> call chain
}

var (
	inventoriesOnce sync.Once
	inventoriesVal  map[string]*principalInventory
	inventoriesErr  error
)

// inventories loads each distinct build once per platform and computes every
// principal that uses it, then drops the program: a loaded program holds
// gigabytes, and there are more than a dozen.
func inventories(t *testing.T) map[string]*principalInventory {
	t.Helper()
	inventoriesOnce.Do(func() {
		inventoriesVal, inventoriesErr = computeInventories(principals)
	})
	if inventoriesErr != nil {
		t.Fatal(inventoriesErr)
	}
	return inventoriesVal
}

func computeInventories(ps []principal) (map[string]*principalInventory, error) {
	type key struct{ dir, pkg, goos string }
	var order []key
	users := map[key][]principal{}
	for _, p := range ps {
		for _, g := range p.build.goos {
			k := key{p.build.dir, p.build.pkg, g}
			if _, ok := users[k]; !ok {
				order = append(order, k)
			}
			users[k] = append(users[k], p)
		}
	}
	out := map[string]*principalInventory{}
	for _, k := range order {
		prog, err := Load(LoadConfig{Dir: filepath.Join(repoRoot, k.dir), GOOS: k.goos, GOARCH: "amd64", Patterns: []string{k.pkg}, Root: repoRoot})
		if err != nil {
			return nil, err
		}
		for _, p := range users[k] {
			roots := prog.Mains()
			if len(p.roots) > 0 {
				roots = nil
				for _, r := range p.roots {
					f, err := prog.Func(r)
					if err != nil {
						return nil, fmt.Errorf("%s (GOOS=%s) root: %w", p.name, k.goos, err)
					}
					roots = append(roots, f)
				}
			}
			var cuts []*ssa.Function
			for _, c := range p.cuts {
				f, err := prog.Func(c)
				if err != nil {
					return nil, fmt.Errorf("%s (GOOS=%s) cut: %w", p.name, k.goos, err)
				}
				cuts = append(cuts, f)
			}
			inv := prog.Calls(roots, cuts)
			if inv.Reached < 20 {
				return nil, fmt.Errorf("%s (GOOS=%s): reached only %d functions — the traversal has broken", p.name, k.goos, inv.Reached)
			}
			pi := out[p.name]
			if pi == nil {
				pi = &principalInventory{actions: map[string][]Site{}, paths: map[string][]string{}}
				out[p.name] = pi
			}
			pi.add(inv)
		}
	}
	return out, nil
}

// add merges one platform's inventory into pi.
func (pi *principalInventory) add(inv Inventory) {
	for a, sites := range inv.Actions {
		for _, s := range sites {
			pi.actions[a] = appendSite(pi.actions[a], s)
			if _, ok := pi.paths[s.Func]; !ok {
				pi.paths[s.Func] = inv.PathTo(s.Func)
			}
		}
	}
	pi.unmapped = append(pi.unmapped, inv.Unmapped...)
}

func appendSite(sites []Site, s Site) []Site {
	for _, x := range sites {
		if x == s {
			return sites
		}
	}
	return append(sites, s)
}

// contextsFor are the contexts an action's call sites run on: a site in a
// package a context claims (pkgs) runs on it, and every other site on
// contextFor(action).
func (p principal) contextsFor(action string, sites []Site) ([]credContext, bool) {
	var out []credContext
	seen := map[string]bool{}
	add := func(c credContext) {
		if !seen[c.name] {
			seen[c.name] = true
			out = append(out, c)
		}
	}
	unclaimed := len(sites) == 0
	for _, s := range sites {
		if c, ok := p.siteContext(s); ok {
			add(c)
			continue
		}
		unclaimed = true
	}
	if unclaimed {
		c, ok := p.contextFor(action)
		if !ok {
			return out, false
		}
		add(c)
	}
	return out, true
}

// siteContext is the context that claims a call site's package, if any.
func (p principal) siteContext(s Site) (credContext, bool) {
	fn := strings.TrimLeft(s.Func, "(*")
	for _, c := range p.contexts {
		for _, pkg := range c.pkgs {
			if strings.HasPrefix(fn, pkg+".") {
				return c, true
			}
		}
	}
	return credContext{}, false
}

func (p principal) contextFor(action string) (credContext, bool) {
	for _, c := range p.contexts {
		for _, s := range c.services {
			if s == action {
				return c, true
			}
		}
	}
	prefix, _, _ := strings.Cut(action, ":")
	var fallback *credContext
	for i, c := range p.contexts {
		if c.services == nil {
			fallback = &p.contexts[i]
			continue
		}
		for _, s := range c.services {
			if s == prefix {
				return c, true
			}
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return credContext{}, false
}

func gapKey(principal, variant, action string) string {
	return principal + " | " + variant + " | " + action
}

// TestEveryPrincipalCallIsGranted is the gate (#254, spore-host#610 part 4):
// every AWS action a spore.host principal's code can make must be granted, by
// exact name, by the policy spore.host provisions for it.
//
// A wildcard does not count. "Action": "*" or "s3:*" would satisfy any call,
// so it cannot tell a deliberate grant from an accident; if a principal is
// granted by wildcard on purpose, that is a ledger entry with a reason.
//
// knownGaps is the ledger of gaps that exist today. A new gap fails; so does an
// entry that is no longer a gap, so the ledger cannot outlive what it records.
func TestEveryPrincipalCallIsGranted(t *testing.T) {
	invs := inventories(t)
	policies := map[string]*Policy{}
	policyFor := func(v variant) *Policy {
		if p, ok := policies[v.name]; ok {
			return p
		}
		p := v.policy(t)
		policies[v.name] = p
		return p
	}
	for _, e := range coverageErrors(principals, invs, policyFor, knownGaps) {
		t.Error(e)
	}
}

// coverageErrors is the gate's verdict: every call of every principal that its
// policy does not grant by exact name and gaps does not record, every unmapped
// call, and every gaps entry that is no longer a gap.
func coverageErrors(ps []principal, invs map[string]*principalInventory, policyFor func(variant) *Policy, gaps map[string]string) []string {
	var errs []string
	used := map[string]bool{}
	for _, p := range ps {
		inv := invs[p.name]
		if inv == nil || len(inv.actions) == 0 {
			errs = append(errs, fmt.Sprintf("%s: discovered no AWS calls — the discovery has broken, not the principal", p.name))
			continue
		}
		for _, u := range inv.unmapped {
			errs = append(errs, fmt.Sprintf("%s: an AWS operation this gate cannot map to an IAM action: %s\n"+
				"An unmapped call is invisible to every coverage check — add it to actions.go.", p.name, u))
		}
		actions := make([]string, 0, len(inv.actions))
		for a := range inv.actions {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		for _, action := range actions {
			ctxs, ok := p.contextsFor(action, inv.actions[action])
			if !ok {
				errs = append(errs, fmt.Sprintf("%s calls %s, but no credential context of this principal claims the %q service", p.name, action, strings.SplitN(action, ":", 2)[0]))
				continue
			}
			for _, ctx := range ctxs {
				for _, v := range ctx.variants {
					pol := policyFor(v)
					grant, src := pol.Grants(action)
					if grant == Exact && strings.EqualFold(action, "iam:PassRole") {
						errs = append(errs, passRoleErrors(p, v, pol)...)
						continue
					}
					if grant == Exact {
						continue
					}
					k := gapKey(p.name, v.name, action)
					if _, ok := gaps[k]; ok {
						used[k] = true
						continue
					}
					errs = append(errs, gapMessage(p, ctx, v, pol, action, grant, src, inv))
				}
			}
		}
	}

	for _, p := range ps {
		if inv := invs[p.name]; inv != nil && len(p.passes) > 0 && len(inv.actions["iam:PassRole"]) == 0 {
			errs = append(errs, fmt.Sprintf("%s declares roles it passes (%v) but its code calls no operation that passes one — delete the declaration", p.name, p.passes))
		}
	}

	var stale []string
	for k := range gaps {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		errs = append(errs, fmt.Sprintf("knownGaps entry %q is no longer a gap (the grant was added, the call removed, or the principal renamed) — delete it", k))
	}
	return errs
}

// passRoleErrors checks a principal holding iam:PassRole by name against the
// roles it declares it passes: each must be passable, to its service, under
// the policy's PassRole statements (an Allow whose Resource matches the role
// and whose condition admits the service, and no Deny that matches). A
// principal that passes a role and declares none is an error: the grant's
// scope would go unchecked.
func passRoleErrors(p principal, v variant, pol *Policy) []string {
	if len(p.passes) == 0 {
		return []string{fmt.Sprintf("%s calls iam:PassRole under %q but declares no passes: name the roles its code "+
			"passes (principal.passes) so the grant's Resource can be checked (#1397)", p.name, v.name)}
	}
	var errs []string
	for _, pr := range p.passes {
		if !passableTo(pol.PassStatements(), pr.role, pr.service) {
			errs = append(errs, fmt.Sprintf("%s passes role/%s to %s, which %q does not allow: its iam:PassRole "+
				"grant's Resource or iam:PassedToService condition does not match, or a Deny does (sources %v)",
				p.name, pr.role, pr.service, v.name, pol.Sources))
		}
	}
	return errs
}

// passableTo simulates iam:PassRole of role/<role>, in whatever account each
// statement's Resource names, to service: some Allow matches and no Deny does.
// Conditions on keys other than iam:PassedToService are not modelled; such a
// statement counts as not matching (so a conditional Allow does not grant, and
// a conditional Deny does not deny).
func passableTo(stmts []map[string]any, role, service string) bool {
	allowed := false
	for _, st := range stmts {
		if !passesRole(st) || !toService(st, service) {
			continue
		}
		if _, ok := st["NotResource"]; ok {
			continue
		}
		matches := false
		for _, r := range resourceList(st["Resource"]) {
			if r == "*" {
				matches = true
				continue
			}
			if m := arnAccount.FindStringSubmatch(r); m != nil && globMatch(r, "arn:aws:iam::"+m[1]+":role/"+role) {
				matches = true
			}
		}
		if !matches {
			continue
		}
		switch st["Effect"] {
		case "Deny":
			return false
		case "Allow":
			allowed = true
		}
	}
	return allowed
}

func gapMessage(p principal, ctx credContext, v variant, pol *Policy, action string, grant Grant, src string, inv *principalInventory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s calls %s, which %q (%s credentials) does not grant", p.name, action, v.name, ctx.name)
	switch grant {
	case WildcardOnly:
		fmt.Fprintf(&b, " by name — only a wildcard in %s matches it, and a wildcard does not count", src)
	case Denied:
		fmt.Fprintf(&b, " — an unconditional Deny in %s names it", src)
	case NotGranted:
	}
	if len(pol.Opaque) > 0 {
		fmt.Fprintf(&b, " (AWS-managed policies attached, not modelled: %s)", strings.Join(pol.Opaque, ", "))
	}
	b.WriteString(".\nCalled at:\n")
	sites := inv.actions[action]
	for i, s := range sites {
		if i == 5 {
			fmt.Fprintf(&b, "  ... and %d more\n", len(sites)-5)
			break
		}
		fmt.Fprintf(&b, "  %s  %s in %s\n", s.Pos, s.Op, s.Func)
	}
	if chain := inv.paths[sites[0].Func]; len(chain) > 0 {
		fmt.Fprintf(&b, "Reached by: %s\n", strings.Join(shortNames(chain), " -> "))
	}
	fmt.Fprintf(&b, "Grant it in the policy spore.host provisions for this role, or make the feature "+
		"degrade with a clear message and record it in knownGaps with the reason (key %q).",
		gapKey(p.name, v.name, action))
	return b.String()
}

func shortNames(chain []string) []string {
	out := make([]string, len(chain))
	for i, c := range chain {
		out[i] = strings.ReplaceAll(c, "github.com/spore-host/spawn/", "")
	}
	return out
}

// mainFunc matches a main function declaration.
var mainFunc = regexp.MustCompile(`(?m)^func main\(\)`)

// TestEveryMainPackageIsAPrincipal discovers the principals rather than trusting
// the list above: every main package in the repository is a binary that runs as
// some identity, and each must be registered — except the spawn CLI itself,
// which runs as the user (spore-host#610's other half: see
// TestCLIActionInventoryIsCurrent) and the repository's own test tooling.
func TestEveryMainPackageIsAPrincipal(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range principals {
		registered[filepath.ToSlash(filepath.Clean(filepath.Join(p.build.dir, p.build.pkg)))] = true
	}
	notPrincipals := map[string]string{
		".":             "the spawn CLI, which runs as the user (cli-iam-actions.md)",
		"test/hermetic": "the test wrapper",
		// Run by the release workflow with no AWS credentials; it reads git and
		// the Go module graph only. The scripts module has no AWS SDK dependency.
		"scripts/deploychecklist": "the release deploy checklist (#1444), which calls no AWS API",
		// It does run as an identity, GitHubActions-spawn-lambda-deploy, but it
		// calls AWS only through the `aws` CLI, which this SDK call-graph
		// analysis cannot see. Its own gate does the same job for it:
		// TestEveryLambdaCallIsGrantedByTheCIRole reads every subcommand it can
		// invoke and checks each against scripts/setup-lambda-deploy-ci-role.sh.
		"scripts/deploylambdas": "the CI Lambda deployer (#1445), gated against its role by its own test",
	}
	var found []string
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if treewalk.Vanished(repoRoot, path, err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if strings.HasPrefix(rel, "internal/") || strings.HasPrefix(rel, "test/e2e") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repository
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "\npackage main\n") && !strings.HasPrefix(string(b), "package main\n") {
			return nil
		}
		if !mainFunc.Match(b) {
			return nil
		}
		found = append(found, filepath.ToSlash(filepath.Dir(rel)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 5 {
		t.Fatalf("found only %d main packages; the walk has broken", len(found))
	}
	for _, dir := range found {
		if registered[dir] {
			continue
		}
		if _, ok := notPrincipals[dir]; ok {
			continue
		}
		t.Errorf("%s is a main package with no registered principal. Every binary spore.host ships runs "+
			"as some IAM identity; add it to principals with the policy spore.host provisions for it.", dir)
	}
	for dir := range registered {
		ok := false
		for _, f := range found {
			if f == dir {
				ok = true
			}
		}
		if !ok {
			t.Errorf("principal build %s is not a main package any more", dir)
		}
	}
}
