package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"

	"github.com/spore-host/libs/i18n"
)

// TestNoDoubledEllipsisInLocalizedOutput is #1129's gate. `spawn list` printed
// "Searching all AWS regions......": cmd/list.go formatted the catalogue string
// with "%s...", and the catalogue entry already ends in "..." — in all six
// languages.
//
// The defect is a format string adding punctuation the translation already
// carries, so this gate renders every such line. It finds every fmt
// Printf/Fprintf/Sprintf/Errorf call with a literal format in the non-test
// sources of cmd/ and pkg/ whose arguments include an i18n.T / i18n.Tf call,
// renders it in every language the catalogue supports (i18n.SupportedLanguages,
// not a list here), and fails if the result has a run of four or more dots.
func TestNoDoubledEllipsisInLocalizedOutput(t *testing.T) {
	langs := i18n.SupportedLanguages()
	if len(langs) == 0 {
		t.Fatal("i18n reports no supported languages")
	}
	localizers := map[string]*i18n.Localizer{}
	for _, l := range langs {
		loc, err := i18n.NewLocalizer(i18n.Config{Language: l, NoEmoji: true})
		if err != nil {
			t.Fatalf("localizer %s: %v", l, err)
		}
		localizers[l] = loc
	}

	tooManyDots := regexp.MustCompile(`\.{4,}|…\.|\.…`)
	rendered := 0
	for _, root := range []string{"cmd", "pkg"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if treewalk.Vanished(root, path, err) {
					return nil
				}
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			fmtName := importLocalName(f, "fmt")
			i18nName := importLocalName(f, "github.com/spore-host/libs/i18n")
			if fmtName == "" || i18nName == "" {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				format, args, ok := localizedFormatCall(call, fmtName, i18nName)
				if !ok {
					return true
				}
				for _, lang := range langs {
					vals := make([]interface{}, len(args))
					for i, a := range args {
						vals[i] = a(localizers[lang])
					}
					line := fmt.Sprintf(format, vals...)
					rendered++
					if tooManyDots.MatchString(line) {
						t.Errorf("%s (%s): renders %q — the format adds an ellipsis the catalogue string already has",
							fset.Position(call.Pos()), lang, strings.TrimSpace(line))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if rendered == 0 {
		t.Fatal("rendered no localized format call: discovery is broken, not the output clean")
	}
}

// localizedFormatCall recognises fmt.<Printf-like>(…, "literal format", args…)
// where at least one arg is i18n.T("key", …) or i18n.Tf("key", …). Each arg is
// returned as a renderer: i18n calls translate through the localizer, anything
// else renders as a placeholder word.
func localizedFormatCall(call *ast.CallExpr, fmtName, i18nName string) (string, []func(*i18n.Localizer) interface{}, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", nil, false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != fmtName {
		return "", nil, false
	}
	formatAt := map[string]int{"Printf": 0, "Sprintf": 0, "Errorf": 0, "Fprintf": 1}[sel.Sel.Name]
	if _, known := map[string]bool{"Printf": true, "Sprintf": true, "Errorf": true, "Fprintf": true}[sel.Sel.Name]; !known {
		return "", nil, false
	}
	if len(call.Args) <= formatAt {
		return "", nil, false
	}
	lit, ok := call.Args[formatAt].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", nil, false
	}
	format, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", nil, false
	}
	var renderers []func(*i18n.Localizer) interface{}
	localized := false
	for _, a := range call.Args[formatAt+1:] {
		key, isT := i18nKey(a, i18nName)
		if isT {
			localized = true
			k := key
			renderers = append(renderers, func(l *i18n.Localizer) interface{} { return l.T(k) })
			continue
		}
		renderers = append(renderers, func(*i18n.Localizer) interface{} { return "x" })
	}
	if !localized {
		return "", nil, false
	}
	// %w is Errorf-only; Sprintf would render it as an error.
	return strings.ReplaceAll(format, "%w", "%v"), renderers, true
}

// i18nKey returns the literal key of an i18n.T / i18n.Tf call.
func i18nKey(e ast.Expr, i18nName string) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "T" && sel.Sel.Name != "Tf") {
		return "", false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != i18nName {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	key, err := strconv.Unquote(lit.Value)
	return key, err == nil
}

// importLocalName is the name f refers to path by, or "" if f does not import it.
func importLocalName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}
