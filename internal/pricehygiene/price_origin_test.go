// Package pricehygiene holds the structural half of the #1502 gate.
//
// The owner rule (truffle#184, spawn#1502): a static or fallback price shown
// anywhere must say that it is static, WHY the live Price List was not used,
// and WHEN the static data was collected. The behavioural half lives next to
// the code: each package that renders a price keeps a staticPriceRenderers
// registry in its _test.go files, and a test there renders a static price
// through every entry and checks for all three. This file makes sure that set
// cannot silently fall behind the code — the same shape as truffle's
// internal/hygiene/price_origin_test.go.
package pricehygiene

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

const root = "../.."

// producers are the functions whose result is a quoted on-demand price with
// its origin. Calling one is what makes a function a price display site. The
// set is extended at discovery time with every function or func-typed var
// whose result type is DisplayPrice (the cmd/ and lambda seams), so a new
// seam is found without being listed here.
var producers = map[string]bool{"ResolveDisplayPrice": true}

// rawPriceCalls are truffle's own on-demand price lookups. Calling one
// directly bypasses DisplayPrice, so each caller must be registered or
// explained like any other display site.
var rawPriceCalls = map[string]bool{
	"OnDemandPrice":           true,
	"OnDemandPriceWithSource": true,
	"OnDemandPriceWithOrigin": true,
}

// originSelectors are the fields and methods that carry a quoted price's
// origin to a renderer. Reading one makes a function a display site.
var originSelectors = map[string]bool{
	"SourceLabel":          true, // aws.DisplayPrice's label, in truffle's wording
	"priceLabel":           true, // cmd launchPlan's label
	"OnDemandPricePerHour": true, // cmd launchPlan's rate
	"PriceAsOf":            true,
	"PriceFallbackReason":  true,
	"StaticPriceNotes":     true, // pkg/cost CostBreakdown's static-origin labels (and truffle's builder of them)
	"PriceSourceTag":       true, // the spawn:price-source tag a rate read back from an instance carries
}

// notRenderSites are the discovered functions that obtain or carry a price
// without rendering it, each with the reason. A discovered function that is in
// neither this list nor its package's staticPriceRenderers registry fails the
// gate, so a new display site must be registered (and so behaviourally
// tested) or explained here. Keys are "<dir>.<func>", with methods as
// "<dir>.<Type>.<method>".
var notRenderSites = map[string]string{
	"pkg/aws.ResolveDisplayPrice": "builds the truffle client and delegates to resolveDisplayPrice, which is registered",
	"pkg/aws.resolvePricePerHour": "records the rate for --cost-limit enforcement; its only human-facing output is the log " +
		"line TestResolvePricePerHourLogsTheRealFallbackReason checks for the date and reason",
	"cmd.confirmLaunchCost":         "stores the price and origin on the plan (setPrice); renderLaunchConfirmation and maxCostLine render it",
	"cmd.runLaunchDryRun":           "stores the price and origin on the plan (setPrice); renderLaunchPlanTable renders it, and TestLaunchDryRunJSONCarriesThePriceOrigin gates its JSON",
	"cmd.runLaunch":                 "the --estimate-only path: passes the DisplayPrice to renderInstanceCostEstimate, which is registered",
	"cmd.launchPlan.setPrice":       "copies a DisplayPrice's rate and origin onto the plan; prints nothing",
	"cmd.launchPlan.priceLabel":     "the label helper renderLaunchConfirmation and renderLaunchPlanTable print; gated through both",
	"cmd.sweepPricer.Resolve":       "stores each quote with its origin in the cache; SourceSummary renders it",
	"cmd.resolveLaunchDryRunPrice":  "production seam returning a DisplayPrice; its callers render it",
	"cmd.resolveServiceDryRunPrice": "production seam returning a DisplayPrice; its callers render it",
	"cmd.resolveSweepRowPrice":      "production seam returning a DisplayPrice; its callers render it",
	"pkg/cost.quoteRate":            "asks the pricer for the rate and its origin; GetCostBreakdown records the origin, and is registered",
	"cmd.singleInstanceCostReport": "KNOWN GAP spawn#1512: the instance records only spawn:price-source=static, not the date or reason, " +
		"so the report says static with the date and reason unknown (TestSingleInstanceCostReportSaysAStaticRateIsStatic); " +
		"recording both at launch is tracked there",
	"lambda/pipeline-orchestrator.priceFn": "production seam returning a DisplayPrice; ensureStagePrice renders it",
	"cmd.costLimitBasisLabel": "KNOWN GAP spawn#1512, as singleInstanceCostReport: labels the on-demand fallback a Spot " +
		"instance's --cost-limit falls back to (#1497) from spawn:price-source alone, so a static rate says static with " +
		"the date and reason unknown (TestCostLimitBasisLabelSaysAStaticFallbackIsStatic)",
}

// TestEveryPriceDisplaySiteIsGated discovers every non-test function in the
// module that obtains a quoted on-demand price or reads its origin, and
// requires each to be in its package's staticPriceRenderers registry (whose
// entries that package's behavioural test renders with a static price and
// checks for "static", the collection date and the reason) or in
// notRenderSites with a reason. Registry and notRenderSites entries must name
// functions that still exist and still read a price, so neither list can go
// stale.
func TestEveryPriceDisplaySiteIsGated(t *testing.T) {
	pkgs := parseModule(t)
	for _, p := range pkgs {
		for name := range p.producers {
			producers[name] = true
		}
	}

	allReaders := map[string]bool{}
	registries := 0
	for _, dir := range sortedKeys(pkgs) {
		p := pkgs[dir]
		readers := p.readers()
		registry := registeredRenderers(t, filepath.Join(root, filepath.FromSlash(dir)))
		if len(registry) > 0 {
			registries++
		}
		if len(readers) > 0 || len(registry) > 0 {
			t.Logf("%s: price display readers %v; registered renderers %v", dir, readers, sortedKeys(registry))
		}
		for _, fn := range readers {
			q := dir + "." + fn
			allReaders[q] = true
			_, isNot := notRenderSites[q]
			switch {
			case registry[fn] && isNot:
				t.Errorf("%s is both registered in staticPriceRenderers and listed in notRenderSites", q)
			case !registry[fn] && !isNot:
				t.Errorf("%s obtains or renders an on-demand price but is not in %s's staticPriceRenderers (so no test "+
					"proves a static price it shows says static, why and when) nor in notRenderSites (#1502)", q, dir)
			}
		}
		for fn := range registry {
			if !contains(readers, fn) {
				t.Errorf("%s staticPriceRenderers names %s, which is not a function there that reads a price; drop or fix it", dir, fn)
			}
		}
	}
	if len(allReaders) == 0 {
		t.Fatal("discovered no price display site anywhere in the module; discovery is broken")
	}
	if registries == 0 {
		t.Fatal("found no staticPriceRenderers registry in any package; registry discovery is broken")
	}
	for q := range notRenderSites {
		if !allReaders[q] {
			t.Errorf("notRenderSites lists %s, which no longer obtains or reads a price; drop it", q)
		}
	}
}

// TestDiscoveryFindsTheKnownSites guards the discovery itself (CLAUDE.md,
// "Writing a gate", rule 4): if the AST walk stops seeing the sites the issue
// named, the gate above would pass vacuously.
func TestDiscoveryFindsTheKnownSites(t *testing.T) {
	pkgs := parseModule(t)
	for _, p := range pkgs {
		for name := range p.producers {
			producers[name] = true
		}
	}
	for _, q := range []string{
		"cmd.renderLaunchConfirmation",
		"cmd.renderLaunchPlanTable",
		"cmd.renderServiceDryRun",
		"cmd.sweepPricer.SourceSummary",
		"lambda/pipeline-orchestrator.ensureStagePrice",
		"pkg/aws.resolveDisplayPrice",
	} {
		dir, fn, _ := strings.Cut(q, ".")
		p, ok := pkgs[dir]
		if !ok || !contains(p.readers(), fn) {
			t.Errorf("discovery did not find %s", q)
		}
	}
}

type pkgFuncs struct {
	// funcs maps a function key ("name" or "Type.method") to its body.
	funcs map[string]*ast.BlockStmt
	// producers are the names here whose result type is DisplayPrice.
	producers map[string]bool
}

// readers returns, sorted, the functions whose body (closures included)
// calls a producer or truffle's raw price lookup, or reads an origin selector.
func (p *pkgFuncs) readers() []string {
	var out []string
	for name, body := range p.funcs {
		hit := false
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if name := callName(x.Fun); producers[name] || rawPriceCalls[name] {
					hit = true
				}
			case *ast.SelectorExpr:
				if originSelectors[x.Sel.Name] {
					hit = true
				}
			}
			return !hit
		})
		if hit {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func callName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// mentionsDisplayPrice reports whether a function type's results name the
// DisplayPrice type (bare or package-qualified).
func mentionsDisplayPrice(ft *ast.FuncType) bool {
	if ft == nil || ft.Results == nil {
		return false
	}
	found := false
	for _, r := range ft.Results.List {
		ast.Inspect(r.Type, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "DisplayPrice" {
				found = true
			}
			return !found
		})
	}
	return found
}

// parseModule parses every non-test Go file of the root module, keyed by
// slash-separated directory relative to the module root. Nested modules
// (directories with their own go.mod) and testdata are skipped: they are not
// built into this module.
func parseModule(t *testing.T) map[string]*pkgFuncs {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string]*pkgFuncs{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if treewalk.Vanished(root, path, err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		p := pkgs[dir]
		if p == nil {
			p = &pkgFuncs{funcs: map[string]*ast.BlockStmt{}, producers: map[string]bool{}}
			pkgs[dir] = p
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				key := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					key = recvName(d.Recv.List[0].Type) + "." + key
				}
				if d.Name.Name == "init" {
					key = "init"
					if prev := p.funcs[key]; prev != nil {
						// Several init funcs: concatenate so any of them counts.
						d.Body.List = append(append([]ast.Stmt{}, prev.List...), d.Body.List...)
					}
				}
				p.funcs[key] = d.Body
				if mentionsDisplayPrice(d.Type) {
					p.producers[d.Name.Name] = true
				}
			case *ast.GenDecl:
				// var seams: `var resolveX = func(...) (aws.DisplayPrice, error) {...}`.
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if ft, ok := vs.Type.(*ast.FuncType); ok && mentionsDisplayPrice(ft) {
							p.producers[name.Name] = true
						}
						if i < len(vs.Values) {
							if fl, ok := vs.Values[i].(*ast.FuncLit); ok {
								// A package-level func var is a function for this
								// gate too: its body can obtain or render a price.
								p.funcs[name.Name] = fl.Body
								if mentionsDisplayPrice(fl.Type) {
									p.producers[name.Name] = true
								}
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no Go files; discovery is broken")
	}
	return pkgs
}

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvName(x.X)
	}
	return "?"
}

// registeredRenderers parses the keys of the package-level
// `var staticPriceRenderers = map[string]...{...}` in dir's _test.go files.
func registeredRenderers(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if name.Name != "staticPriceRenderers" || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.CompositeLit)
					if !ok {
						t.Fatalf("%s: staticPriceRenderers is not a map literal", e.Name())
					}
					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
							if k, err := strconv.Unquote(bl.Value); err == nil {
								out[k] = true
							}
						}
					}
				}
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
