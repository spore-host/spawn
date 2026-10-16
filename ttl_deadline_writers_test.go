package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every function that writes the spawn:ttl tag must also write
// spawn:ttl-deadline (#983).
//
// spawn:ttl-deadline is what is enforced: spored treats it as authoritative
// whenever it is set, and the TTL reaper reads nothing else. spawn:ttl alone is
// only a pre-deadline fallback. So a writer that moves spawn:ttl without the
// deadline reports success and changes nothing that terminates the instance.
// That shipped three times: `spored config set ttl` (#553), `spawn extend
// --job-array-*` and `spawn start`'s TTL "restore" (#983).
//
// Writers are DISCOVERED, not listed (CLAUDE.md rule 4): every non-test .go file
// in the repo, nested lambda modules included, is parsed and every write of the
// key is found structurally (rule 3), so a comment or a log line naming the tag
// does not count, and neither does a read such as tags["spawn:ttl"] on the
// right-hand side or a `case "spawn:ttl":`. A write is the key appearing as
//
//   - a map-literal key:            map[string]string{"spawn:ttl": v}
//   - an assigned index:            tags["spawn:ttl"] = v
//   - the Key of an EC2 tag struct: types.Tag{Key: aws.String("spawn:ttl")}
//
// spelled either as the literal or as tagprefix.Tag("ttl"). The deadline counts
// as written when it is set OR deleted in the same function, because removing
// it is how `config set ttl 0` disables enforcement; that is still deciding the
// deadline rather than ignoring it. The unit is the enclosing top-level
// declaration, so a helper closure inside the function is covered.
func TestEveryTTLTagWriterAlsoWritesTheDeadline(t *testing.T) {
	writers, missing := scanTTLWriters(t, ".")
	if len(writers) == 0 {
		t.Fatal("discovered no writer of spawn:ttl at all; the scan is broken, not the repo clean")
	}
	for _, w := range writers {
		t.Logf("spawn:ttl writer: %s", w)
	}
	for _, m := range missing {
		t.Errorf("%s writes spawn:ttl but never writes spawn:ttl-deadline, the tag spored and the TTL reaper "+
			"actually enforce. Move both together (see pkg/ttl.Tags, or computeExtendedTTLTags for extend).", m)
	}
}

// scanTTLWriters returns every declaration (file:line name) that writes
// spawn:ttl, and the subset that does not also write spawn:ttl-deadline.
func scanTTLWriters(t *testing.T, root string) (writers, missing []string) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, decl := range f.Decls {
			ttl, deadline := tagWrites(decl)
			if !ttl {
				continue
			}
			label := path + ":" + strconv.Itoa(fset.Position(decl.Pos()).Line) + " " + declName(decl)
			writers = append(writers, label)
			if !deadline {
				missing = append(missing, label)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files == 0 {
		t.Fatalf("found no Go files under %s", root)
	}
	sort.Strings(writers)
	sort.Strings(missing)
	return writers, missing
}

// tagWrites reports whether decl writes spawn:ttl and whether it writes (or
// deletes) spawn:ttl-deadline, by the structural forms listed on the gate.
func tagWrites(decl ast.Decl) (ttl, deadline bool) {
	mark := func(e ast.Expr) {
		switch tagKey(e) {
		case "ttl":
			ttl = true
		case "ttl-deadline":
			deadline = true
		}
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Key" {
					// types.Tag{Key: aws.String(k)} or Key: k
					v := kv.Value
					if call, ok := v.(*ast.CallExpr); ok && len(call.Args) == 1 {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "String" {
							v = call.Args[0]
						}
					}
					mark(v)
					continue
				}
				mark(kv.Key) // map-literal key
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if ix, ok := lhs.(*ast.IndexExpr); ok {
					mark(ix.Index)
				}
			}
		}
		return true
	})
	return ttl, deadline
}

// tagKey returns "ttl" / "ttl-deadline" when e names that spawn tag, either as
// the literal "spawn:<suffix>" or as tagprefix.Tag("<suffix>"); "" otherwise.
func tagKey(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return ""
		}
		if suffix, ok := strings.CutPrefix(s, "spawn:"); ok {
			return suffix
		}
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Tag" || len(e.Args) != 1 {
			return ""
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "tagprefix" {
			return ""
		}
		if lit, ok := e.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, _ := strconv.Unquote(lit.Value)
			return s
		}
	}
	return ""
}

func declName(decl ast.Decl) string {
	if fd, ok := decl.(*ast.FuncDecl); ok {
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			return "method " + fd.Name.Name
		}
		return "func " + fd.Name.Name
	}
	return "package-level declaration"
}

// The detector itself, on the shapes it must catch and the ones it must not.
// Without this a detector that matched nothing would make the gate above pass
// for any tree that happened to contain one compliant writer.
func TestTTLWriterDetectorShapes(t *testing.T) {
	cases := []struct {
		name               string
		src                string
		wantTTL, wantDdlne bool
	}{
		{"map literal, ttl only (#983's array path)", `func f() { _ = map[string]string{"spawn:ttl": x} }`, true, false},
		{"index assign, ttl only (spawn start)", `func f() { tags["spawn:ttl"] = x }`, true, false},
		{"ec2 tag struct", `func f() { _ = types.Tag{Key: aws.String("spawn:ttl"), Value: v} }`, true, false},
		{"tagprefix form", `func f() { m[tagprefix.Tag("ttl")] = v }`, true, false},
		{"closure inside the func", `func f() { g := func() { _ = map[string]string{"spawn:ttl": x} }; g() }`, true, false},
		{"both", `func f() { _ = map[string]string{"spawn:ttl": a, "spawn:ttl-deadline": b} }`, true, true},
		{"deadline deleted", `func f() { tags["spawn:ttl"] = "0"; del(types.Tag{Key: aws.String("spawn:ttl-deadline")}) }`, true, true},
		{"read is not a write", `func f() { x := tags["spawn:ttl"]; _ = x }`, false, false},
		{"switch case is not a write", `func f() { switch k { case "spawn:ttl": } }`, false, false},
		{"other tag", `func f() { tags["spawn:ttl-remaining"] = x }`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+c.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			ttl, dl := tagWrites(f.Decls[0])
			if ttl != c.wantTTL || dl != c.wantDdlne {
				t.Errorf("tagWrites = (ttl %v, deadline %v), want (%v, %v)", ttl, dl, c.wantTTL, c.wantDdlne)
			}
		})
	}
}
