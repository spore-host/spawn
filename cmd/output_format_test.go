package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// resetRootOutputFlag puts the global -o/--output back to its default so a test
// that sets it does not leak a bad value into the next test's Execute.
func resetRootOutputFlag(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		f := rootCmd.PersistentFlags().Lookup("output")
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
}

// executeRoot runs the real command tree with args. Every case here is
// expected to be decided in PersistentPreRunE, before any RunE (and so before
// any AWS call) runs.
func executeRoot(t *testing.T, args ...string) error {
	t.Helper()
	resetRootOutputFlag(t)
	var sink strings.Builder
	rootCmd.SetOut(&sink)
	rootCmd.SetErr(&sink)
	rootCmd.SetArgs(args)
	return rootCmd.Execute()
}

// The issue's acceptance test: an --output value outside the documented set
// exits non-zero and names the valid values (#975). Before the fix every one of
// these printed the table and exited 0.
func TestGlobalOutputRejectsUnknownFormat(t *testing.T) {
	for _, args := range [][]string{
		{"list", "-o", "bogus"},
		{"list", "--output", "jsno"},
		{"version", "-o", "bogus"},
		{"list", "-o", "JSON"}, // readers compare exactly; "JSON" would render the table
		// `spawn app` declares its own PersistentPreRunE. Without
		// EnableTraverseRunHooks it replaced the root hook for the whole app
		// subtree, so the validation (and --profile/--region) never ran there.
		{"app", "list", "-o", "bogus"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := executeRoot(t, args...)
			if err == nil {
				t.Fatalf("spawn %s: want an error, got success", strings.Join(args, " "))
			}
			msg := err.Error()
			for _, want := range outputFormats {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name the valid format %q", msg, want)
				}
			}
			if code := exitForError(err); code == 0 {
				t.Errorf("exit code = 0 for %q", msg)
			}
		})
	}
}

func TestGlobalOutputAcceptsSupportedFormats(t *testing.T) {
	for _, format := range outputFormats {
		t.Run(format, func(t *testing.T) {
			if err := executeRoot(t, "version", "-o", format); err != nil {
				t.Fatalf("spawn version -o %s: %v", format, err)
			}
		})
	}
}

// A file name given to the format flag is the #935 mistake. Where the command
// has a file flag, the error must point at it.
func TestGlobalOutputSuggestsOutputFile(t *testing.T) {
	err := executeRoot(t, "sweep", "collect", "sweep-x", "--output", "results.csv")
	if err == nil {
		t.Fatal("spawn sweep collect --output results.csv: want an error, got success")
	}
	if !strings.Contains(err.Error(), "--output-file results.csv") {
		t.Errorf("error %q does not suggest --output-file", err)
	}
}

// A command whose own local --output is a path shadows the root flag, so its
// value is not a format and must not be judged as one. Tested on the validator
// directly, because running these commands does real work.
func TestLocalOutputFlagIsNotValidated(t *testing.T) {
	resetRootOutputFlag(t)
	rootFlag := rootCmd.PersistentFlags().Lookup("output")
	found := 0
	walkCommands(rootCmd, func(c *cobra.Command) {
		local := c.LocalNonPersistentFlags().Lookup("output")
		if local == nil || local == rootFlag {
			return
		}
		found++
		if err := c.Flags().Set("output", "some/file.json"); err != nil {
			t.Fatalf("%s: set --output: %v", c.CommandPath(), err)
		}
		t.Cleanup(func() { _ = local.Value.Set(local.DefValue); local.Changed = false })
		if err := validateOutputFormat(c); err != nil {
			t.Errorf("%s has a local --output, but the format check rejected its value: %v", c.CommandPath(), err)
		}
	})
	if found == 0 {
		t.Fatal("found no command with a local --output; discovery is broken (collect-results has one)")
	}
}

// The other half of the `spawn app` hook bug: with the root hook replaced,
// --profile/--region/--account never reached the config layer for any app
// command. A no-op child keeps RunE (and AWS) out of it.
func TestAppSubtreeRunsRootHook(t *testing.T) {
	resetRootOutputFlag(t)
	probe := &cobra.Command{Use: "hook-probe", RunE: func(*cobra.Command, []string) error { return nil }}
	appGroupCmd.AddCommand(probe)
	t.Cleanup(func() {
		appGroupCmd.RemoveCommand(probe)
		f := rootCmd.PersistentFlags().Lookup("region")
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
		spawnconfig.SetSharedFlags("", "", "")
	})
	rootCmd.SetArgs([]string{"app", "hook-probe", "--region", "eu-north-1"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("spawn app hook-probe: %v", err)
	}
	if got := spawnconfig.SharedRegion(); got != "eu-north-1" {
		t.Errorf("spawn app ... --region eu-north-1: the config layer resolved region %q; the root PersistentPreRunE did not run", got)
	}
}

func TestOutputFormatsAnnotationExtendsOneCommand(t *testing.T) {
	resetRootOutputFlag(t)
	ext := &cobra.Command{Use: "ext", Annotations: map[string]string{outputFormatsAnnotation: "yaml, csv"}}
	plain := &cobra.Command{Use: "plain"}
	for _, c := range []*cobra.Command{ext, plain} {
		rootCmd.AddCommand(c)
	}
	t.Cleanup(func() { rootCmd.RemoveCommand(ext, plain) })

	if err := ext.ParseFlags([]string{"-o", "yaml"}); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputFormat(ext); err != nil {
		t.Errorf("ext declares yaml but rejected it: %v", err)
	}
	if err := plain.ParseFlags([]string{"-o", "yaml"}); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputFormat(plain); err == nil {
		t.Error("plain does not declare yaml but accepted it")
	}
}

// TestOutputFormatsMatchTheirReaders discovers, from the output code itself,
// every format a command renders: each string literal compared (== / != /
// switch case) against getOutputFormat() or spawnOutputFormat in cmd/*.go.
//
// Both directions must hold. A rendered format the validator rejects would make
// a working `-o X` an error; an accepted format nothing renders would bring back
// the silent table fall-through #975 removed. A format only some commands
// render belongs in their outputFormatsAnnotation, not in outputFormats.
func TestOutputFormatsMatchTheirReaders(t *testing.T) {
	read := discoverOutputFormatReaders(t)
	if len(read) == 0 {
		t.Fatal("discovered no comparison against getOutputFormat()/spawnOutputFormat; discovery is broken")
	}

	declared := map[string]bool{}
	for _, f := range outputFormats {
		declared[f] = true
	}
	walkCommands(rootCmd, func(c *cobra.Command) {
		for _, f := range supportedOutputFormats(c) {
			declared[f] = true
		}
	})

	for format, where := range read {
		if !declared[format] {
			t.Errorf("%s renders -o %q, but no command accepts it: add it to outputFormats, or to the "+
				"%s annotation of the commands that render it", strings.Join(where, ", "), format, outputFormatsAnnotation)
		}
	}
	for _, format := range outputFormats {
		if format == outputFormatTable {
			continue // the default: rendered by every reader's fall-through, never compared
		}
		if _, ok := read[format]; !ok {
			t.Errorf("outputFormats accepts %q, but no command compares against it, so -o %s would silently print the table", format, format)
		}
	}
	if def := rootCmd.PersistentFlags().Lookup("output").DefValue; !slices.Contains(outputFormats, def) {
		t.Errorf("--output defaults to %q, which outputFormats does not accept", def)
	}
}

// discoverOutputFormatReaders maps each literal compared against the global
// format to the file:line positions that compare it.
func discoverOutputFormatReaders(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	isFormat := func(e ast.Expr) bool {
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			return x.Name == "spawnOutputFormat"
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			return ok && id.Name == "getOutputFormat" && len(x.Args) == 0
		}
		return false
	}
	literal := func(e ast.Expr) (string, bool) {
		bl, ok := ast.Unparen(e).(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(bl.Value)
		return s, err == nil
	}

	out := map[string][]string{}
	fset := token.NewFileSet()
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
		record := func(s string, pos token.Pos) {
			out[s] = append(out[s], fset.Position(pos).String())
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.EQL && x.Op != token.NEQ {
					return true
				}
				if isFormat(x.X) {
					if s, ok := literal(x.Y); ok {
						record(s, x.Pos())
					}
				} else if isFormat(x.Y) {
					if s, ok := literal(x.X); ok {
						record(s, x.Pos())
					}
				}
			case *ast.SwitchStmt:
				if x.Tag == nil || !isFormat(x.Tag) {
					return true
				}
				for _, stmt := range x.Body.List {
					for _, e := range stmt.(*ast.CaseClause).List {
						if s, ok := literal(e); ok {
							record(s, e.Pos())
						}
					}
				}
			}
			return true
		})
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn)
	}
}
