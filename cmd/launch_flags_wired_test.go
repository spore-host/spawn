package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// knownUnwiredFlags are flags whose bound variable is deliberately (or at least
// knowingly) unread. Each entry needs an issue, because an entry here means the
// flag is accepted on the command line and does nothing.
var knownUnwiredFlags = map[string]string{
	// --vpc has no LaunchConfig field at all: every consumer calls GetDefaultVPC
	// instead, so `spawn launch --vpc vpc-xxx` is accepted and the instance
	// launches in the DEFAULT VPC. Found while fixing #667; wiring it means
	// adding the field and honouring it at the GetDefaultVPC/subnet-selection
	// call sites, which changes EFS/FSx subnet resolution too.
	"vpc": "#673",

	// --cartesian advertises the cartesian product of --param lists. The
	// capability exists (pkg/params.expandGrid) but is only reachable via the
	// `grid:` key in a params file, so the flag silently produces the WRONG RUN
	// COUNT — the worst of the three, because it bills for a sweep of the wrong
	// shape rather than merely doing nothing.
	"cartesian": "#674",

	// --use-reservation predates #216's --reservation-id/--capacity-block, which
	// are wired. Passing it yields an on-demand instance at on-demand price while
	// reading as "target my reserved capacity". Should become a MarkDeprecated
	// alias like --subnet/--security-group/--key-pair.
	"use-reservation": "#675",
}

// TestLaunchFlagsAreWired catches flags that cobra parses into a package global
// which NOTHING reads, so passing the flag has no effect and no error.
//
// Nothing in the compiler catches this: the variable IS used, by the
// Flags().*Var() call that binds it. So this test parses the package, finds every
// variable bound to a launch flag, and asserts something other than the binding
// reads it.
//
// SCOPE, stated precisely because it is narrower than it looks: this gate would
// NOT have caught #667, the bug it was written alongside. There, sgIDs was read
// — just on the batch-queue path only, never on the ordinary launch path — so it
// has a reader and passes here. Catching THAT shape means knowing which call
// paths matter, which is not something this can decide statically. #667's own
// regression tests in launch_network_flags_test.go are what cover it.
//
// What this does catch is the strictly-unread case, and it found three on its
// first run: --vpc (#673), --cartesian (#674) and --use-reservation (#675).
// --vpc is the one that shows why the shadowing rule below is load-bearing.
//
// Shadowing is the subtlety that hid --vpc. launch_single.go contains
// `vpcID, err := awsClient.GetDefaultVPC(...)` — a different, function-local
// variable that happens to share the name. A naive identifier count reads those
// as uses of the global and reports the flag as wired. So a function that
// declares the name locally gives no credit at all.
func TestLaunchFlagsAreWired(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	type binding struct{ flag, file string }
	bound := map[string]binding{} // variable name -> flag it is bound to
	parsed := map[string]*ast.File{}

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed[name] = f

		// Find launchCmd.Flags().XxxVar(&ident, "flag-name", ...).
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(sel.Sel.Name, "Var") {
				return true
			}
			// The receiver must be launchCmd.Flags() / launchCmd.PersistentFlags().
			inner, ok := sel.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			innerSel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(innerSel.Sel.Name, "Flags") {
				return true
			}
			recv, ok := innerSel.X.(*ast.Ident)
			if !ok || recv.Name != "launchCmd" {
				return true
			}

			unary, ok := call.Args[0].(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				return true
			}
			target, ok := unary.X.(*ast.Ident)
			if !ok {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			flagName := strings.Trim(lit.Value, `"`)

			// Deprecated aliases bind a second flag to the same variable; the
			// first binding is the canonical one to report.
			if _, seen := bound[target.Name]; !seen {
				bound[target.Name] = binding{flag: flagName, file: name}
			}
			return true
		})
	}

	if len(bound) < 20 {
		t.Fatalf("only found %d flag bindings; the matcher is probably broken, which would "+
			"make this gate pass vacuously", len(bound))
	}

	// Count reads of each bound variable, skipping any function that declares a
	// local of the same name.
	reads := map[string]int{}
	for name, f := range parsed {
		for varName := range bound {
			if countReads(f, varName, name) {
				reads[varName]++
			}
		}
	}

	for varName, b := range bound {
		if reads[varName] > 0 {
			if issue, allowed := knownUnwiredFlags[b.flag]; allowed {
				t.Errorf("--%s (%s) is now wired up, but still listed in "+
					"knownUnwiredFlags as %s; remove the entry", b.flag, varName, issue)
			}
			continue
		}
		if issue, allowed := knownUnwiredFlags[b.flag]; allowed {
			t.Logf("--%s (%s) is accepted but unread; tracked as %s", b.flag, varName, issue)
			continue
		}
		t.Errorf("--%s is bound to %s in %s, but nothing reads %s.\n\n"+
			"The flag is parsed and then silently discarded: a user who passes it gets no "+
			"error and no effect. This is spawn#667, where --security-group-ids never "+
			"reached the LaunchConfig and every launch quietly used the VPC default group.\n"+
			"Wire it through to the LaunchConfig (or delete the flag). If it is genuinely "+
			"unwired on purpose, add it to knownUnwiredFlags with an issue.",
			b.flag, varName, b.file, varName)
	}
}

// countReads reports whether f reads the package-level variable named varName
// somewhere other than its declaration or a flag binding.
//
// Any function declaring varName locally is skipped entirely rather than
// searched: within it, the name refers to the local, so counting occurrences
// would credit the global for uses that are not its own.
func countReads(f *ast.File, varName, fileName string) bool {
	found := false

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if declaresLocal(fn, varName) {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			// Don't count the &varName in a Flags().XxxVar(...) binding.
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && strings.HasSuffix(sel.Sel.Name, "Var") {
					return false
				}
			}
			if id, ok := n.(*ast.Ident); ok && id.Name == varName {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// declaresLocal reports whether fn declares varName as a parameter, named
// result, `:=` target, or local `var`.
func declaresLocal(fn *ast.FuncDecl, varName string) bool {
	shadowed := false

	nameIn := func(fl *ast.FieldList) bool {
		if fl == nil {
			return false
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				if n.Name == varName {
					return true
				}
			}
		}
		return false
	}
	if nameIn(fn.Type.Params) || nameIn(fn.Type.Results) {
		return true
	}

	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range node.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == varName {
					shadowed = true
				}
			}
		case *ast.ValueSpec:
			for _, n := range node.Names {
				if n.Name == varName {
					shadowed = true
				}
			}
		case *ast.FuncLit:
			if nameIn(node.Type.Params) || nameIn(node.Type.Results) {
				shadowed = true
			}
		case *ast.RangeStmt:
			if node.Tok == token.DEFINE {
				for _, x := range []ast.Expr{node.Key, node.Value} {
					if id, ok := x.(*ast.Ident); ok && id.Name == varName {
						shadowed = true
					}
				}
			}
		}
		return !shadowed
	})
	return shadowed
}
