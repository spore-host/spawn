package launcher

import (
	"bytes"
	"compress/gzip"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"

	"mvdan.cc/sh/v3/syntax"

	"github.com/spore-host/spawn/internal/shellmin"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// staticTemplates are the script texts BuildLinuxBootstrap runs through
// shellmin.Strip, keyed by the expression bootstrap.go passes it.
// TestEveryStrippedTemplateIsChecked keeps the two in step.
func staticTemplates() map[string]string {
	return map[string]string{
		"sporedinstall.KeySetupScript()":      sporedinstall.KeySetupScript(),
		"sporedinstall.VerifiedInstallFunc()": sporedinstall.VerifiedInstallFunc(),
		"spotReplacementCheckFunc":            spotReplacementCheckFunc,
		"linuxBootstrapBody":                  linuxBootstrapBody,
		"spotReplacementStopScript":           spotReplacementStopScript,
		"bodyStubTemplate":                    bodyStubTemplate,
		"fatalRetireFunc":                     fatalRetireFunc,
		"stagedFilesTemplate":                 stagedFilesTemplate,
	}
}

// Every shellmin.Strip call in bootstrap.go strips a template the same-tree
// gate checks, found by reading the calls rather than trusting this file's
// list, so a template added to the bootstrap cannot skip the gate.
func TestEveryStrippedTemplateIsChecked(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bootstrap.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	known := staticTemplates()
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Strip" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "shellmin" || len(call.Args) != 1 {
			return true
		}
		calls++
		var b strings.Builder
		if err := printer.Fprint(&b, fset, call.Args[0]); err != nil {
			t.Fatal(err)
		}
		if _, ok := known[b.String()]; !ok {
			t.Errorf("bootstrap.go strips %s, which staticTemplates does not list, so nothing checks that stripping it is safe", b.String())
		}
		return true
	})
	if calls != len(known) {
		t.Errorf("bootstrap.go has %d shellmin.Strip calls; staticTemplates lists %d", calls, len(known))
	}
}

// printedTree parses src as bash and prints it back minified. The parser
// drops comments unless asked to keep them, and the minifying printer drops
// blank lines and indentation, so two scripts that differ only in those print
// the same, and two that do anything differently do not.
func printedTree(t *testing.T, name, src string) string {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), name)
	if err != nil {
		t.Fatalf("%s does not parse as bash: %v", name, err)
	}
	var b strings.Builder
	if err := syntax.NewPrinter(syntax.Minify(true)).Print(&b, f); err != nil {
		t.Fatalf("print %s: %v", name, err)
	}
	return b.String()
}

// The gate for shellmin.Strip (#1246): it is a line filter, so the proof that it
// removed only comments is that every template parses to the same syntax tree
// before and after. A line it should have kept, such as one inside a
// multi-line quoted string, a heredoc it mis-tracked, or a "comment" that
// followed a backslash continuation, changes the tree and fails here.
func TestMinifiedTemplatesParseTheSame(t *testing.T) {
	for name, src := range staticTemplates() {
		t.Run(name, func(t *testing.T) {
			min := shellmin.Strip(src)
			if got, want := printedTree(t, name+" (minified)", min), printedTree(t, name, src); got != want {
				t.Errorf("minifying %s changed what it does, not just its comments", name)
			}
			if len(min) >= len(src) && strings.Contains(src, "\n#") {
				t.Errorf("minifying %s removed nothing", name)
			}
		})
	}
}

// The cases a line filter gets wrong if it does not track them. Each one is
// also held to the same-tree rule above.
func TestMinifyShellKeepsData(t *testing.T) {
	cases := map[string]struct {
		src, want string
	}{
		"comments and blank lines go": {
			"#!/bin/bash\n# a comment\n\n  # indented comment\necho hi # trailing stays\n",
			"#!/bin/bash\necho hi # trailing stays\n",
		},
		"quoted heredoc body is data": {
			"cat > f <<'EOF'\n# kept: data\n\nline\nEOF\n# gone\necho done\n",
			"cat > f <<'EOF'\n# kept: data\n\nline\nEOF\necho done\n",
		},
		"unquoted and dash heredocs": {
			"cat <<EOF\n# kept\nEOF\nif true; then\n\tcat <<-END\n\t# kept\n\tEND\nfi\n# gone\n",
			"cat <<EOF\n# kept\nEOF\nif true; then\n\tcat <<-END\n\t# kept\n\tEND\nfi\n",
		},
		"a here-string is not a heredoc": {
			"read -r x <<< \"$y\"\n# gone\necho \"$x\"\n",
			"read -r x <<< \"$y\"\necho \"$x\"\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := shellmin.Strip(tc.src)
			if got != tc.want {
				t.Errorf("shellmin.Strip:\n got %q\nwant %q", got, tc.want)
			}
			if printedTree(t, "minified", got) != printedTree(t, "source", tc.src) {
				t.Errorf("the minified script parses differently from its source")
			}
		})
	}
}

// bootstrapGzipBudget is how big the bare bootstrap may be once gzipped, as
// EC2 counts it. It was 12.8 KB of the 16 KiB limit before #1246, leaving a
// task about 1.6 KB. Raising this is a decision about how much room tasks
// lose, so make it in review, not by accident.
const bootstrapGzipBudget = 8 * 1024

func TestBareBootstrapFitsItsBudget(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if b.Len() > bootstrapGzipBudget {
		t.Errorf("the bare bootstrap is %d bytes gzipped, over its %d-byte budget (#1246); every byte over comes out of a task's command and manifests", b.Len(), bootstrapGzipBudget)
	}
	t.Logf("bare bootstrap: %d bytes raw, %d gzipped (budget %d, EC2 limit 16384)", len(script), b.Len(), bootstrapGzipBudget)
}

// TestEverySplicedInstallerIsStripped: the verified spored installer is
// spliced into user data by more than this package (cmd/app.go, the pipeline
// orchestrator, the SSM upgrade), and each copy costs those paths about 1.6 KB
// of gzipped comments (#1274). Every call of the two generators outside
// pkg/sporedinstall must be the direct argument of shellmin.Strip, whose
// safety TestMinifiedTemplatesParseTheSame proves for both. Found by walking
// the repo's source, so a new splice site is covered without a list.
func TestEverySplicedInstallerIsStripped(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	sites := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			if treewalk.Vanished(root, path, werr) {
				return nil
			}
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.ToSlash(filepath.Dir(path)) == "../../pkg/sporedinstall" {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if call, ok := n.(*ast.CallExpr); ok && isInstallerGenerator(call) {
				sites++
				stripped := false
				if len(stack) > 0 {
					if parent, ok := stack[len(stack)-1].(*ast.CallExpr); ok && isSelector(parent.Fun, "shellmin", "Strip") {
						stripped = true
					}
				}
				if !stripped {
					t.Errorf("%s splices %s unstripped; wrap it in shellmin.Strip (#1274)",
						fset.Position(call.Pos()), call.Fun.(*ast.SelectorExpr).Sel.Name)
				}
			}
			stack = append(stack, n)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sites < 4 {
		t.Fatalf("found %d installer splice sites; the source walk has broken", sites)
	}
}

func isInstallerGenerator(call *ast.CallExpr) bool {
	return len(call.Args) == 0 &&
		(isSelector(call.Fun, "sporedinstall", "VerifiedInstallFunc") || isSelector(call.Fun, "sporedinstall", "KeySetupScript"))
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
