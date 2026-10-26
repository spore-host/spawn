package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// Every notification event spored sends must be one spore-bot handles (#1004).
//
// spored POSTs lifecycle events to spore-bot's /notify, which formats them for
// Slack, Discord and SMS. spore-bot keeps the list of events it handles in
// lambda/spore-bot/testdata/spored_events.txt (spore-host/spore-host#558) and gates
// that every listed event has a message on each channel — but its CI cannot see
// this repo, so an event added here was only caught if someone ran its check by
// hand. fsx_mount_failed, fsx_dra_failed and region_vacated all shipped that way
// and reached users as a generic fallback message.
//
// The bot's list is vendored at testdata/spore-bot/spored_events.txt so this test
// runs offline. TestSporeBotEventsVendoredCopyIsCurrent (run in CI with
// SPORE_BOT_EVENTS_SYNC=1) fetches the upstream list and fails when the copy has
// drifted, so the copy cannot quietly go stale.
//
// Discovered, not listed (rule 4): every non-test Go file of this module is
// parsed, and the event argument of each Notifier.Notify call is resolved — a
// string literal, a constant, or a local variable whose every assignment is one
// (idle_stopped/idle_hibernated are chosen through a variable). An argument the
// resolver cannot reduce to string literals fails the test rather than being
// skipped, and so does finding no events at all.

const (
	vendoredSporeBotEvents = "testdata/spore-bot/spored_events.txt"
	upstreamSporeBotEvents = "https://raw.githubusercontent.com/spore-host/spore-host/main/lambda/spore-bot/testdata/spored_events.txt"
)

func TestSporedNotifyEventsHandledBySporeBot(t *testing.T) {
	sent, err := discoverSporedNotifyEvents(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) == 0 {
		t.Fatal("found no Notifier.Notify events — the discovery is broken, not the repo")
	}

	for ev, where := range sent {
		t.Logf("spored sends %q at %s", ev, strings.Join(where, ", "))
	}

	data, err := os.ReadFile(vendoredSporeBotEvents)
	if err != nil {
		t.Fatal(err)
	}
	handled := parseSporeBotEvents(string(data))
	if len(handled) == 0 {
		t.Fatalf("%s lists no events", vendoredSporeBotEvents)
	}

	var missing []string
	for ev, where := range sent {
		if !handled[ev] {
			missing = append(missing, fmt.Sprintf("%q (sent at %s)", ev, strings.Join(where, ", ")))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("spored sends events spore-bot does not handle:\n  %s\n\n"+
			"Add each to spore-host/spore-host lambda/spore-bot/testdata/spored_events.txt "+
			"with a message on every channel, merge that, then re-vendor it into %s.",
			strings.Join(missing, "\n  "), vendoredSporeBotEvents)
	}
	for ev := range handled {
		if _, ok := sent[ev]; !ok {
			// Not a failure here: a stale entry costs the bot an unused case, not
			// a user a missing message. spore-bot's own TestSporedEventsMatchSpawn
			// is the check for that direction.
			t.Logf("spore-bot handles %q, which spored does not send", ev)
		}
	}
}

// parseSporeBotEvents reads spore-bot's format: one event per line, '#' starts a
// comment. Same rules as the bot's own loader.
func parseSporeBotEvents(s string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out
}

// discoverSporedNotifyEvents returns every event passed to Notifier.Notify in the
// module rooted at root, mapped to the call sites that send it.
//
// Matching is structural: a call whose function is a selector `.Notify` whose
// receiver is not an imported package (that is signal.Notify). This is sound only
// while (*Notifier).Notify is the module's one method named Notify, so that is
// checked too — a second one fails the test rather than being mistaken for it.
// A `.Notify` selector used other than as a call (a method value) also fails,
// since its event argument would be out of reach.
func discoverSporedNotifyEvents(root string) (map[string][]string, error) {
	fset := token.NewFileSet()
	events := map[string][]string{}
	var problems []string
	var notifyMethods []string
	pkgs := map[string]*pkgValues{} // by directory

	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // nested module: not spored
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, f)
		dir := filepath.Dir(path)
		if pkgs[dir] == nil {
			pkgs[dir] = &pkgValues{consts: map[string][]ast.Expr{}, fields: map[string][]ast.Expr{},
				unkeyed: map[string][]string{}}
		}
		pkgs[dir].files = append(pkgs[dir].files, f)
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
				collectValueSpecs(gd, pkgs[dir].consts)
			}
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "Notify" {
				notifyMethods = append(notifyMethods, fmt.Sprintf("%s (%s)", recvTypeName(fd), fset.Position(fd.Pos())))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("parsed no Go files under %s", root)
	}
	if len(notifyMethods) != 1 || !strings.HasPrefix(notifyMethods[0], "Notifier ") {
		return nil, fmt.Errorf("expected exactly one method named Notify, on Notifier; found %v — "+
			"the call matching below can no longer tell them apart and must use type information", notifyMethods)
	}

	for _, pv := range pkgs {
		pv.collectFields(fset)
	}

	for _, f := range files {
		imports := map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = true
		}
		consts := pkgs[filepath.Dir(fset.Position(f.Pos()).Filename)]
		isPkgSel := func(sel *ast.SelectorExpr) bool {
			id, ok := sel.X.(*ast.Ident)
			return ok && imports[id.Name]
		}

		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			locals := map[string][]ast.Expr{}
			params := map[string]bool{}
			for _, fl := range []*ast.FieldList{fd.Recv, fd.Type.Params} {
				if fl == nil {
					continue
				}
				for _, fld := range fl.List {
					for _, n := range fld.Names {
						params[n.Name] = true
					}
				}
			}
			calls := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						id, ok := lhs.(*ast.Ident)
						if !ok {
							continue
						}
						if len(n.Lhs) == len(n.Rhs) {
							locals[id.Name] = append(locals[id.Name], n.Rhs[i])
						} else {
							locals[id.Name] = append(locals[id.Name], nil) // multi-value: unresolvable
						}
					}
				case *ast.RangeStmt:
					for _, e := range []ast.Expr{n.Key, n.Value} {
						if id, ok := e.(*ast.Ident); ok {
							locals[id.Name] = append(locals[id.Name], nil)
						}
					}
				case *ast.DeclStmt:
					if gd, ok := n.Decl.(*ast.GenDecl); ok {
						collectValueSpecs(gd, locals)
					}
				case *ast.FuncLit:
					for _, fld := range n.Type.Params.List {
						for _, nm := range fld.Names {
							params[nm.Name] = true
						}
					}
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Notify" && !isPkgSel(sel) {
						calls[sel] = true
					}
				}
				return true
			})

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Notify" || isPkgSel(sel) || calls[sel] {
					return true
				}
				problems = append(problems, fmt.Sprintf("%s: Notify used as a method value; its event cannot be resolved",
					fset.Position(sel.Pos())))
				return true
			})

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !calls[sel] {
					return true
				}
				pos := fset.Position(call.Pos())
				where := fmt.Sprintf("%s:%d", filepath.ToSlash(pos.Filename), pos.Line)
				if len(call.Args) < 2 {
					problems = append(problems, where+": Notify call without an event argument")
					return true
				}
				vals, err := resolveStrings(call.Args[1], locals, params, consts, 0)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: cannot resolve event argument: %v", where, err))
					return true
				}
				for _, v := range vals {
					events[v] = append(events[v], where)
				}
				return true
			})
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("unresolvable Notify events (refine the resolver, or pass a literal):\n  %s",
			strings.Join(problems, "\n  "))
	}
	return events, nil
}

// collectValueSpecs records the values of a const/var declaration by name. A
// name with no value of its own (iota-continued const, zero-valued var) is
// recorded as unresolvable.
func collectValueSpecs(gd *ast.GenDecl, into map[string][]ast.Expr) {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, n := range vs.Names {
			if i < len(vs.Values) && len(vs.Values) == len(vs.Names) {
				into[n.Name] = append(into[n.Name], vs.Values[i])
			} else {
				into[n.Name] = append(into[n.Name], nil)
			}
		}
	}
}

// resolveStrings reduces e to the set of string literals it can hold: a literal,
// or an identifier every one of whose assignments (in the enclosing function,
// else at package level) resolves. Anything else is an error, never a skip.
func resolveStrings(e ast.Expr, locals map[string][]ast.Expr, params map[string]bool,
	pkg *pkgValues, depth int) ([]string, error) {
	if depth > 10 {
		return nil, fmt.Errorf("resolution too deep")
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil, fmt.Errorf("non-string literal %s", e.Value)
		}
		s, err := strconv.Unquote(e.Value)
		return []string{s}, err
	case *ast.ParenExpr:
		return resolveStrings(e.X, locals, params, pkg, depth+1)
	case *ast.Ident:
		if params[e.Name] {
			return nil, fmt.Errorf("%s is a parameter", e.Name)
		}
		defs, scope := locals[e.Name], "local"
		if len(defs) == 0 {
			defs, scope = pkg.consts[e.Name], "package"
		}
		if len(defs) == 0 {
			return nil, fmt.Errorf("no assignment to %s found", e.Name)
		}
		var out []string
		for _, d := range defs {
			if d == nil {
				return nil, fmt.Errorf("%s %s is assigned a value that is not a single expression", scope, e.Name)
			}
			// Package-level values resolve against package scope only.
			l := locals
			if scope == "package" {
				l = nil
			}
			v, err := resolveStrings(d, l, nil, pkg, depth+1)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name, err)
			}
			out = append(out, v...)
		}
		return out, nil
	case *ast.SelectorExpr:
		// A struct field (#1005 passes stopAttempt.failEvent). Resolved from every
		// value the package gives a field of that name: keyed composite literals
		// and assignments. Those values are resolved against package scope only,
		// so a field set from a local variable fails rather than being guessed.
		name := e.Sel.Name
		if pos := pkg.unkeyed[name]; len(pos) > 0 {
			return nil, fmt.Errorf("field %s is set by unkeyed composite literals at %s", name, strings.Join(pos, ", "))
		}
		defs := pkg.fields[name]
		if len(defs) == 0 {
			return nil, fmt.Errorf("no value for field %s found (a selector into another package is not resolved)", name)
		}
		var out []string
		for _, d := range defs {
			if d == nil {
				return nil, fmt.Errorf("field %s is assigned a value that is not a single expression", name)
			}
			v, err := resolveStrings(d, nil, nil, pkg, depth+1)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			out = append(out, v...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported expression %T", e)
}

// pkgValues is what one package directory can give an identifier or a field.
type pkgValues struct {
	files   []*ast.File
	consts  map[string][]ast.Expr // package-level const/var name -> values
	fields  map[string][]ast.Expr // field name -> every value assigned to it
	unkeyed map[string][]string   // field name -> positions of unkeyed literals of a declaring struct
}

// collectFields records every value the package assigns to a named field: in a
// keyed composite literal, or by `x.f = v`. By name rather than by type, so it is
// a superset — a same-named field elsewhere can only add values. An unkeyed
// literal of a struct declaring the field is recorded, because its values would
// otherwise escape.
func (p *pkgValues) collectFields(fset *token.FileSet) {
	fieldsOf := map[string][]string{} // struct type -> its field names
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, fld := range st.Fields.List {
					for _, nm := range fld.Names {
						fieldsOf[ts.Name.Name] = append(fieldsOf[ts.Name.Name], nm.Name)
					}
				}
			}
			return true
		})
	}
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							p.fields[id.Name] = append(p.fields[id.Name], kv.Value)
						}
						continue
					}
					if id, ok := n.Type.(*ast.Ident); ok {
						for _, fname := range fieldsOf[id.Name] {
							p.unkeyed[fname] = append(p.unkeyed[fname], fset.Position(n.Pos()).String())
						}
					}
					break
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if len(n.Lhs) == len(n.Rhs) {
						p.fields[sel.Sel.Name] = append(p.fields[sel.Sel.Name], n.Rhs[i])
					} else {
						p.fields[sel.Sel.Name] = append(p.fields[sel.Sel.Name], nil)
					}
				}
			}
			return true
		})
	}
}

func recvTypeName(fd *ast.FuncDecl) string {
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%T", t)
}
