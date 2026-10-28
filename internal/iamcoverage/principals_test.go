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
// action claimed that way beats any prefix.
type credContext struct {
	name     string
	services []string
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
			{name: "function role", services: []string{"sts", "route53", "dynamodb", "kms", "s3:GetObject"}, variants: []variant{
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
		name:  "sweep-orchestrator",
		build: build{dir: "lambda/sweep-orchestrator", pkg: ".", goos: []string{"linux"}},
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
		name:  "pipeline-orchestrator",
		build: build{dir: ".", pkg: "./lambda/pipeline-orchestrator", goos: []string{"linux"}},
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
	}
	return out, nil
}

func appendSite(sites []Site, s Site) []Site {
	for _, x := range sites {
		if x == s {
			return sites
		}
	}
	return append(sites, s)
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
	used := map[string]bool{}
	policies := map[string]*Policy{}
	policyFor := func(v variant) *Policy {
		if p, ok := policies[v.name]; ok {
			return p
		}
		p := v.policy(t)
		policies[v.name] = p
		return p
	}

	for _, p := range principals {
		inv := invs[p.name]
		if inv == nil || len(inv.actions) == 0 {
			t.Errorf("%s: discovered no AWS calls — the discovery has broken, not the principal", p.name)
			continue
		}
		for _, u := range inv.unmapped {
			t.Errorf("%s: an AWS operation this gate cannot map to an IAM action: %s\n"+
				"An unmapped call is invisible to every coverage check — add it to actions.go.", p.name, u)
		}
		actions := make([]string, 0, len(inv.actions))
		for a := range inv.actions {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		for _, action := range actions {
			ctx, ok := p.contextFor(action)
			if !ok {
				t.Errorf("%s calls %s, but no credential context of this principal claims the %q service", p.name, action, strings.SplitN(action, ":", 2)[0])
				continue
			}
			for _, v := range ctx.variants {
				pol := policyFor(v)
				grant, src := pol.Grants(action)
				if grant == Exact {
					continue
				}
				k := gapKey(p.name, v.name, action)
				if _, ok := knownGaps[k]; ok {
					used[k] = true
					continue
				}
				t.Error(gapMessage(p, ctx, v, pol, action, grant, src, inv))
			}
		}
	}

	var stale []string
	for k := range knownGaps {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("knownGaps entry %q is no longer a gap (the grant was added, the call removed, or the principal renamed) — delete it", k)
	}
}

func gapMessage(p principal, ctx credContext, v variant, pol *Policy, action string, grant Grant, src string, inv *principalInventory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s calls %s, which %q (%s credentials) does not grant", p.name, action, v.name, ctx.name)
	switch grant {
	case WildcardOnly:
		fmt.Fprintf(&b, " by name — only a wildcard in %s matches it, and a wildcard does not count", src)
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
