// Package iamcoverage answers, from the code rather than from a list, which IAM
// actions a spore.host principal needs, so a test can hold the policy
// spore.host provisions for that principal to it (spawn#254, spore-host#610).
//
// The class it closes has broken production repeatedly (#212, #221, #622, #652,
// #1069): code gains an AWS SDK call, nothing fails at build or test time, and
// the call is denied at runtime in an account, usually on a best-effort path
// that turns the AccessDenied into a silent no-op.
//
// # How calls are discovered
//
// A per-file regex over `xClient.Method(` (the original pkg/reaperiam gate)
// misses every call made through an interface, a method value, a paginator, a
// helper in another package, or a client whose variable name it did not
// anticipate, and it cannot tell which principal's code a call belongs to.
// Instead this package type-checks the principal's main package and everything
// it imports, builds SSA, and computes a call graph:
//
//   - RTA (rapid type analysis) from main and init gives the reachable
//     functions and the runtime types;
//   - VTA (variable type analysis) over that set refines dynamic calls, so an
//     io.Reader passed to one function does not make every Read in the program
//     reachable from every other.
//
// A principal is then a set of root functions, optionally minus "cut" functions
// whose subtree runs under a different role (a spored subcommand launched with a
// feature role, say). Every function reachable from the roots, without passing
// through a cut, is scanned for SDK operations.
//
// # What counts as an SDK operation
//
// An AWS SDK for Go v2 operation has one shape in every service:
//
//	func (c *Client) Op(ctx context.Context, params *OpInput, optFns ...func(*Options)) (*OpOutput, error)
//
// so a call, interface invocation or method value named Op whose signature takes
// a *svc.OpInput from a github.com/aws/aws-sdk-go-v2/service/<svc> package is an
// operation, whatever the receiver variable is called and whether the receiver
// is the concrete client, a narrow interface in spawn, or a func-typed field.
// Paginators and waiters are recognised by their <Op>APIClient parameter,
// presigners by their Presign<Op> name, and the two non-operation ways spawn
// reaches AWS (an STS assume-role credentials provider, and SigV4-signing a
// Function URL request by hand) are recognised explicitly in actions.go.
//
// Code inside the SDK itself is not scanned: its credential chain (SSO, STS web
// identity) authenticates rather than acts. The one SDK library spawn calls that
// makes operations on the caller's behalf, the S3 transfer manager, is mapped at
// its API boundary instead.
//
// # Over-approximation
//
// Both analyses are sound over-approximations (modulo reflection), so the
// inventory can name a call that never runs in practice; it should not omit one
// that does. A false "needs" shows up as a gap to reason about, which is the
// safe direction for a gate whose failure mode is silent AccessDenied.
package iamcoverage

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// sdkRoot is the AWS SDK for Go v2 module family. Packages under it are loaded
// for their types but never scanned for calls.
const sdkRoot = "github.com/aws/aws-sdk-go-v2/"

// sdkServicePrefix is where every generated service client lives.
const sdkServicePrefix = sdkRoot + "service/"

// Program is one loaded, analysed build of one or more main packages.
type Program struct {
	root  string
	fset  *token.FileSet
	cg    *callgraph.Graph
	byStr map[string]*ssa.Function
	mains []*ssa.Function
	ops   map[*ssa.Function][]siteOp // memoized per-function scan
}

type siteOp struct {
	site Site
	op   foundOp
}

// LoadConfig selects what to load.
type LoadConfig struct {
	// Dir is the module directory to run `go list` in.
	Dir string
	// GOOS and GOARCH select the build. Required: a principal runs on a
	// specific platform, and platform-specific files can hold SDK calls.
	GOOS, GOARCH string
	// Patterns are the main packages to load, relative to Dir.
	Patterns []string
	// Root, if set, is the directory call-site positions are reported
	// relative to (the repository root, for stable messages and docs).
	Root string
}

// Load type-checks the patterns and their dependencies from source and builds
// the call graph. Module downloads are disabled (GOPROXY=off): the gate must run
// offline under the hermetic test wrapper, so a module missing from the cache is
// an error naming the module, not a network fetch.
func Load(cfg LoadConfig) (*Program, error) {
	if cfg.GOOS == "" || cfg.GOARCH == "" {
		return nil, fmt.Errorf("iamcoverage.Load: GOOS and GOARCH are required")
	}
	env := append(os.Environ(),
		"GOOS="+cfg.GOOS, "GOARCH="+cfg.GOARCH, "CGO_ENABLED=0",
		"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOWORK=off")
	pcfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: cfg.Dir, Env: env, Tests: false}
	pkgs, err := packages.Load(pcfg, cfg.Patterns...)
	if err != nil {
		return nil, fmt.Errorf("load %v in %s: %w", cfg.Patterns, cfg.Dir, err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		if len(loadErrs) > 10 {
			loadErrs = append(loadErrs[:10], fmt.Sprintf("... and %d more", len(loadErrs)-10))
		}
		return nil, fmt.Errorf("load %v in %s (GOOS=%s): %s\n(a module missing from the cache "+
			"needs `go mod download` in that module; this loader never fetches)",
			cfg.Patterns, cfg.Dir, cfg.GOOS, strings.Join(loadErrs, "\n"))
	}

	prog := ssa.NewProgram(pkgs[0].Fset, ssa.InstantiateGenerics)
	var mainPkgs []*ssa.Package
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		// Service packages are huge and only their signatures matter: build them
		// without bodies. Everything else, including the stdlib and the SDK's
		// own runtime, keeps its bodies so dynamic calls through it resolve.
		if strings.HasPrefix(p.PkgPath, sdkServicePrefix) {
			prog.CreatePackage(p.Types, nil, nil, true)
			return
		}
		sp := prog.CreatePackage(p.Types, p.Syntax, p.TypesInfo, true)
		if p.Name == "main" {
			mainPkgs = append(mainPkgs, sp)
		}
	})
	prog.Build()

	if len(mainPkgs) == 0 {
		return nil, fmt.Errorf("load %v in %s: no main package", cfg.Patterns, cfg.Dir)
	}
	var roots, mains []*ssa.Function
	for _, m := range mainPkgs {
		mf := m.Func("main")
		if mf == nil {
			return nil, fmt.Errorf("%s has no func main", m.Pkg.Path())
		}
		mains = append(mains, mf)
		roots = append(roots, mf, m.Func("init"))
	}
	res := rta.Analyze(roots, true)
	// The Lambda runtime invokes the handler by reflection, so no call edge
	// reaches it and RTA would find a lambda's main calls nothing. Every
	// function handed to github.com/aws/aws-lambda-go/lambda is an entry point.
	if handlers := lambdaHandlers(res); len(handlers) > 0 {
		mains = append(mains, handlers...)
		roots = append(roots, handlers...)
		res = rta.Analyze(roots, true)
	}
	funcs := make(map[*ssa.Function]bool, len(res.Reachable))
	for f := range res.Reachable {
		funcs[f] = true
	}
	cg := vta.CallGraph(funcs, res.CallGraph)

	byStr := make(map[string]*ssa.Function, len(funcs))
	for f := range funcs {
		byStr[f.String()] = f
	}
	root := ""
	if cfg.Root != "" {
		if abs, err := filepath.Abs(cfg.Root); err == nil {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
			}
			root = abs + string(filepath.Separator)
		}
	}
	return &Program{root: root, fset: prog.Fset, cg: cg, byStr: byStr, mains: mains, ops: map[*ssa.Function][]siteOp{}}, nil
}

// Mains returns the program's entry points: each main function, plus every
// handler passed to the Lambda runtime.
func (p *Program) Mains() []*ssa.Function { return p.mains }

// lambdaRuntime is the package whose Start functions take a handler.
const lambdaRuntime = "github.com/aws/aws-lambda-go/lambda"

func lambdaHandlers(res *rta.Result) []*ssa.Function {
	seen := map[*ssa.Function]bool{}
	var out []*ssa.Function
	for f := range res.Reachable {
		for _, b := range f.Blocks {
			for _, ins := range b.Instrs {
				call, ok := ins.(ssa.CallInstruction)
				if !ok {
					continue
				}
				callee := call.Common().StaticCallee()
				if callee == nil || funcPkgPath(callee) != lambdaRuntime {
					continue
				}
				for _, a := range call.Common().Args {
					var h *ssa.Function
					switch v := a.(type) {
					case *ssa.Function:
						h = v
					case *ssa.MakeClosure:
						h, _ = v.Fn.(*ssa.Function)
					case *ssa.MakeInterface:
						switch x := v.X.(type) {
						case *ssa.Function:
							h = x
						case *ssa.MakeClosure:
							h, _ = x.Fn.(*ssa.Function)
						}
					}
					if h != nil && !seen[h] {
						seen[h] = true
						out = append(out, h)
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Func finds a reachable function by its SSA name, e.g.
// "github.com/spore-host/spawn/cmd/spored.runPoolWorker" or
// "(*github.com/spore-host/spawn/pkg/agent.Agent).Monitor". A name that is not
// found is an error, not an empty set: a root or cut that was renamed would
// otherwise silently widen or empty a principal.
func (p *Program) Func(name string) (*ssa.Function, error) {
	if f, ok := p.byStr[name]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("function %q is not reachable from the loaded main package(s) — renamed or removed?", name)
}

// FuncsAt returns the reachable functions of package pkgPath declared at
// fileBase:line — how a function value found at run time (a cobra RunE, say)
// is matched to its SSA function.
func (p *Program) FuncsAt(pkgPath, fileBase string, line int) []*ssa.Function {
	var out []*ssa.Function
	for _, f := range p.byStr {
		if funcPkgPath(f) != pkgPath || !f.Pos().IsValid() {
			continue
		}
		pos := p.fset.Position(f.Pos())
		if pos.Line == line && filepath.Base(pos.Filename) == fileBase {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Site is one place an operation is made.
type Site struct {
	Func string // the calling function, SSA name
	Pos  string // file:line
	Op   string // the SDK operation as written, e.g. "ec2.DescribeInstances"
}

// Inventory maps each IAM action to where it is needed.
type Inventory struct {
	Actions map[string][]Site
	// Unmapped are operations this package found but cannot translate to an
	// IAM action: an SDK service it has no prefix for, a signer used for a
	// service it does not know, and so on. A gate must fail on any — an
	// unmapped call is invisible to every coverage check (the route53 hole
	// in the original reaper gate, spawn#254).
	Unmapped []string
	// Reached is how many functions were scanned, so a caller can refuse an
	// inventory built from a broken (empty) traversal.
	Reached int

	parent map[*ssa.Function]*ssa.Function
	byStr  map[string]*ssa.Function
}

// PathTo is the call chain from a root to fn (an SSA function name), outermost
// first, or nil if fn was not reached. A gap message carries it so a reader can
// see why the call is attributed to this principal.
func (inv Inventory) PathTo(fn string) []string {
	f, ok := inv.byStr[fn]
	if !ok {
		return nil
	}
	if _, ok := inv.parent[f]; !ok {
		return nil
	}
	var chain []string
	for i := 0; i < 64 && f != nil; i++ {
		chain = append(chain, f.String())
		p := inv.parent[f]
		if p == f {
			break
		}
		f = p
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// Sorted returns the inventory's actions in order.
func (inv Inventory) Sorted() []string {
	out := make([]string, 0, len(inv.Actions))
	for a := range inv.Actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Calls returns the IAM actions needed by everything reachable from roots
// without passing through a cut. Cuts themselves are not scanned.
func (p *Program) Calls(roots, cuts []*ssa.Function) Inventory {
	cut := map[*ssa.Function]bool{}
	for _, c := range cuts {
		cut[c] = true
	}
	seen := map[*ssa.Function]bool{}
	parent := map[*ssa.Function]*ssa.Function{}
	var queue []*ssa.Function
	for _, r := range roots {
		if !cut[r] && !seen[r] {
			seen[r] = true
			parent[r] = r
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		n := p.cg.Nodes[f]
		if n == nil {
			continue
		}
		for _, e := range n.Out {
			g := e.Callee.Func
			if !seen[g] && !cut[g] {
				seen[g] = true
				parent[g] = f
				queue = append(queue, g)
			}
		}
	}

	inv := Inventory{Actions: map[string][]Site{}, Reached: len(seen), parent: parent, byStr: p.byStr}
	unmapped := map[string]bool{}
	for f := range seen {
		for _, so := range p.opsIn(f) {
			if so.op.err != "" {
				unmapped[fmt.Sprintf("%s at %s (%s)", so.op.display, so.site.Pos, so.op.err)] = true
				continue
			}
			for _, a := range so.op.actions {
				inv.Actions[a] = append(inv.Actions[a], so.site)
			}
		}
	}
	for u := range unmapped {
		inv.Unmapped = append(inv.Unmapped, u)
	}
	sort.Strings(inv.Unmapped)
	for a := range inv.Actions {
		sites := inv.Actions[a]
		sort.Slice(sites, func(i, j int) bool { return sites[i].Pos < sites[j].Pos })
	}
	return inv
}

// opsIn scans one function for operations, once.
func (p *Program) opsIn(f *ssa.Function) []siteOp {
	if ops, ok := p.ops[f]; ok {
		return ops
	}
	var out []siteOp
	if f.Pkg == nil || !strings.HasPrefix(f.Pkg.Pkg.Path(), sdkRoot) {
		for _, b := range f.Blocks {
			for _, ins := range b.Instrs {
				for _, op := range operationsAt(ins) {
					out = append(out, siteOp{site: Site{Func: f.String(), Pos: p.pos(ins.Pos(), f), Op: op.display}, op: op})
				}
			}
		}
	}
	p.ops[f] = out
	return out
}

func (p *Program) pos(pos token.Pos, f *ssa.Function) string {
	if !pos.IsValid() {
		pos = f.Pos()
	}
	if !pos.IsValid() {
		return f.String()
	}
	pp := p.fset.Position(pos)
	file := pp.Filename
	if p.root != "" {
		if real, err := filepath.EvalSymlinks(file); err == nil {
			file = real
		}
		file = strings.TrimPrefix(file, p.root)
	}
	if i := strings.Index(file, "/pkg/mod/"); i >= 0 {
		file = file[i+len("/pkg/mod/"):] // module cache: module@version/path
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(file), pp.Line)
}

// foundOp is one recognised operation and what it maps to.
type foundOp struct {
	display string
	actions []string
	err     string // non-empty when it could not be mapped
}

// operationsAt recognises SDK operations made or referenced by one instruction.
func operationsAt(ins ssa.Instruction) []foundOp {
	var out []foundOp
	if call, ok := ins.(ssa.CallInstruction); ok {
		com := call.Common()
		if com.IsInvoke() {
			sig, _ := com.Method.Type().(*types.Signature)
			if op, ok := opFromMethod(com.Method.Name(), sig); ok {
				out = append(out, op)
			}
			if op, ok := signerCall(com.Method.Name(), sig, com.Args, false); ok {
				out = append(out, op)
			}
		} else if f := com.StaticCallee(); f != nil {
			if op, ok := signerCall(f.Name(), f.Signature, com.Args, f.Signature.Recv() != nil); ok {
				out = append(out, op)
			}
		}
	}
	// Function references: direct calls, method values (`client.Op` passed as a
	// func), paginator and waiter constructors, credential providers.
	var rands []*ssa.Value
	for _, v := range ins.Operands(rands) {
		if v == nil || *v == nil {
			continue
		}
		fn, ok := (*v).(*ssa.Function)
		if !ok {
			continue
		}
		if op, ok := opFromFunction(fn); ok {
			out = append(out, op)
		}
	}
	return out
}

// opFromFunction recognises a reference to an SDK operation method (or its
// bound/thunk wrapper), a paginator or waiter constructor, or a credentials
// provider that calls an operation.
func opFromFunction(fn *ssa.Function) (foundOp, bool) {
	name := strings.TrimSuffix(strings.TrimSuffix(fn.Name(), "$bound"), "$thunk")
	if op, ok := opFromMethod(name, fn.Signature); ok {
		return op, true
	}
	pkg := funcPkgPath(fn)
	if pkg == "" {
		return foundOp{}, false
	}
	if acts, ok := credentialProviders[pkg+"."+name]; ok {
		return foundOp{display: pkgBase(pkg) + "." + name, actions: acts}, true
	}
	if !strings.HasPrefix(pkg, sdkServicePrefix) {
		return foundOp{}, false
	}
	// Paginators and waiters take the <Op>APIClient interface of the operation
	// they drive.
	params := fn.Signature.Params()
	for i := 0; i < params.Len(); i++ {
		n, ok := params.At(i).Type().(*types.Named)
		if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != pkg {
			continue
		}
		if opName, ok := strings.CutSuffix(n.Obj().Name(), "APIClient"); ok && opName != "" {
			return mapOp(pkg, opName, pkgBase(pkg)+"."+name)
		}
	}
	return foundOp{}, false
}

// opFromMethod recognises the operation shape: a method or function named Op
// (or Presign<Op>) taking a *<Op>Input from an SDK service or mapped feature
// package.
func opFromMethod(name string, sig *types.Signature) (foundOp, bool) {
	if sig == nil {
		return foundOp{}, false
	}
	params := sig.Params()
	for i := 0; i < params.Len(); i++ {
		ptr, ok := params.At(i).Type().(*types.Pointer)
		if !ok {
			continue
		}
		n, ok := ptr.Elem().(*types.Named)
		if !ok || n.Obj().Pkg() == nil {
			continue
		}
		pkg := n.Obj().Pkg().Path()
		opName, ok := strings.CutSuffix(n.Obj().Name(), "Input")
		if !ok || opName == "" {
			continue
		}
		if name != opName && name != "Presign"+opName {
			continue
		}
		if !strings.HasPrefix(pkg, sdkRoot) {
			continue
		}
		return mapOp(pkg, opName, pkgBase(pkg)+"."+name)
	}
	return foundOp{}, false
}

// signerCall recognises SigV4-signing a request by hand. The service the
// request is for is the signer's service argument, and it must be a constant —
// an unknown or computed service is reported, not guessed.
func signerCall(name string, sig *types.Signature, args []ssa.Value, hasRecv bool) (foundOp, bool) {
	if name != "SignHTTP" && name != "PresignHTTP" {
		return foundOp{}, false
	}
	if sig == nil || sig.Params().Len() < 5 {
		return foundOp{}, false
	}
	// SignHTTP(ctx, creds aws.Credentials, r, payloadHash, service, region, signingTime, ...)
	creds, ok := sig.Params().At(1).Type().(*types.Named)
	if !ok || creds.Obj().Pkg() == nil || creds.Obj().Pkg().Path() != "github.com/aws/aws-sdk-go-v2/aws" || creds.Obj().Name() != "Credentials" {
		return foundOp{}, false
	}
	idx := 4
	if hasRecv {
		idx++
	}
	display := "v4." + name
	if idx >= len(args) {
		return foundOp{display: display, err: "cannot read the service argument"}, true
	}
	c, ok := args[idx].(*ssa.Const)
	if !ok || c.Value == nil {
		return foundOp{display: display, err: "the signing service is not a constant"}, true
	}
	svc := strings.Trim(c.Value.ExactString(), `"`)
	acts, ok := signedServices[svc]
	if !ok {
		return foundOp{display: display + "(" + svc + ")", err: "no IAM action mapped for SigV4 service " + svc}, true
	}
	return foundOp{display: display + "(" + svc + ")", actions: acts}, true
}

func funcPkgPath(fn *ssa.Function) string {
	if fn.Pkg != nil {
		return fn.Pkg.Pkg.Path()
	}
	if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		return obj.Pkg().Path()
	}
	return ""
}

func pkgBase(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
