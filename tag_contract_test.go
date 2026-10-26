package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// The spawn:* tags a launch writes and the keys spawn's readers parse must be
// the same set (#689 part A).
//
// #683's drain filtered on a tag key nobody could see the way it was read, and
// terminated nothing; a unit test with a hand-built fixture passed forever,
// because the fixture encoded the author's belief about the data rather than
// what the producer emits. The general form of that bug is a writer and a
// reader that disagree about a key: a tag written at launch that nothing reads
// is a setting that silently does nothing, and a tag read that nothing writes
// is a branch that never runs.
//
// Both sides are DISCOVERED from source (CLAUDE.md, "Writing a gate", rule 4)
// and matched structurally (rule 3), so a comment or log line naming a key
// counts as neither:
//
//	writes: map-literal key {"spawn:x": v}; assigned index m["spawn:x"] = v;
//	        EC2 tag struct types.Tag{Key: aws.String("spawn:x")}; an argument
//	        to a set/put/append/add/update/write call; shell `Key=spawn:x,`
//	reads:  index on the right-hand side m["spawn:x"]; `case "spawn:x":`;
//	        `== "spawn:x"`; an argument to a get/lookup/has/read call; an EC2
//	        filter "tag:spawn:x" or tagprefix.FilterTag; shell
//	        `Name=key,Values=spawn:x` and `Key==` + "`spawn:x`"
//
// spelled as the literal, as tagprefix.Tag("x"), or as a string constant
// holding the key. Every non-test .go file is scanned, nested lambda modules
// included. The launch writer is pkg/aws.buildTags.
//
// The checks:
//
//  1. every key buildTags writes is read somewhere;
//  2. every key read is written somewhere (by buildTags, spored, a lambda, or
//     any other writer), or is on externallyWrittenTags;
//  3. the scan found buildTags's writes and a plausible number of reads, so a
//     broken walk cannot pass by finding nothing.
func TestSpawnTagContract(t *testing.T) {
	sc := scanTagUses(t, ".")

	launch := sc.writesIn["pkg/aws.buildTags"]
	if len(launch) < 30 {
		t.Fatalf("found only %d tag writes in pkg/aws.buildTags; the scan has broken", len(launch))
	}
	if len(sc.reads) < 30 {
		t.Fatalf("found only %d distinct tag reads; the scan has broken", len(sc.reads))
	}

	for _, key := range sortedKeySet(launch) {
		if _, ok := sc.reads[key]; ok || readByFamily(key, sc.reads) {
			continue
		}
		if why, ok := readOutsideThisRepo[key]; ok {
			t.Logf("%s: written at launch, read outside this repo (%s)", key, why)
			continue
		}
		t.Errorf("%s is written at launch by pkg/aws.buildTags (%s) and read by nothing in this repo: "+
			"either a reader parses it under another name, or the setting does nothing", key, launch[key])
	}

	for _, key := range sortedKeySet(sc.reads) {
		if _, ok := sc.writes[key]; ok || writtenByFamily(key, sc.writes) {
			continue
		}
		if why, ok := externallyWrittenTags[key]; ok {
			t.Logf("%s: read here, written outside spawn (%s)", key, why)
			continue
		}
		t.Errorf("%s is read (%s) but nothing in this repo writes it: the branch that reads it can never fire",
			key, sc.reads[key])
	}

	for key := range externallyWrittenTags {
		if _, ok := sc.writes[key]; ok {
			t.Errorf("%s is on externallyWrittenTags but spawn writes it (%s); remove it from the allowlist", key, sc.writes[key])
		}
	}
	for key := range readOutsideThisRepo {
		if _, ok := sc.reads[key]; ok {
			t.Errorf("%s is on readOutsideThisRepo but this repo reads it (%s); remove it from the allowlist", key, sc.reads[key])
		}
		if _, ok := launch[key]; !ok {
			t.Errorf("%s is on readOutsideThisRepo but buildTags no longer writes it; remove it from the allowlist", key)
		}
	}
}

// externallyWrittenTags are keys spawn reads but never writes, because
// something outside spawn's code sets them. Add one only with the writer named.
var externallyWrittenTags = map[string]string{
	// AMI metadata the user supplies: `spawn create-ami --tag` accepts spawn:
	// keys (cmd/create_ami.go), and pkg/aws/ami_mgmt.go lists them.
	"spawn:stack":      "set by the user via `spawn create-ami --tag spawn:stack=...`",
	"spawn:base":       "set by the user via `spawn create-ami --tag spawn:base=...`",
	"spawn:deprecated": "set by the user via `spawn create-ami --tag spawn:deprecated=true`",
	// Legacy: `spawn stop` wrote it before #928; start still explains it.
	"spawn:ttl-remaining": "written by spawn stop before #928, read for instances an older spawn stopped",
	// Read by spored and used by nothing: no exporter consults an endpoint, so
	// #1308 added no launch flag for it.
	"spawn:tracing-endpoint": "set out of band with aws ec2 create-tags; no tracing exporter reads it (#1335)",
}

// readOutsideThisRepo are keys buildTags writes that no reader in this repo
// parses. Add one only with the reader named, or the issue deciding its fate.
var readOutsideThisRepo = map[string]string{
	"spawn:created-by":        "descriptive, no spawn reader (#1245)",
	"spawn:root":              "descriptive, no spawn reader (#1245)",
	"spawn:job-array-created": "descriptive, no spawn reader (#1245)",
	"spawn:efs-mount-point":   "descriptive, no spawn reader (#1245)",
	"spawn:fsx-mount-name":    "descriptive, no spawn reader (#1245)",
}

// isTagPrefix reports whether key is a family prefix rather than a key: one
// tag per member ("spawn:param:<name>") or a prefix match ("spawn:dns-").
func isTagPrefix(key string) bool {
	return strings.HasSuffix(key, ":") || strings.HasSuffix(key, "-")
}

// readByFamily reports whether a prefix read (strings.HasPrefix(k,
// "spawn:param:"), a describe-tags "spawn:param:*") covers key.
func readByFamily(key string, reads map[string]string) bool {
	for r := range reads {
		if isTagPrefix(r) && strings.HasPrefix(key, r) {
			return true
		}
	}
	return false
}

// writtenByFamily reports whether a prefix read is satisfied: something writes
// a key with that prefix, or writes the family prefix itself.
func writtenByFamily(key string, writes map[string]string) bool {
	if !isTagPrefix(key) {
		return false
	}
	for w := range writes {
		if strings.HasPrefix(w, key) {
			return true
		}
	}
	return false
}

func sortedKeySet(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type tagScan struct {
	writes   map[string]string            // key → first write site
	reads    map[string]string            // key → first read site
	writesIn map[string]map[string]string // decl id → key → site
}

var (
	tagKeyRe = regexp.MustCompile(`^spawn:[a-z0-9][a-z0-9-]*(:[a-z0-9-]*)?$`)
	// Shell spellings inside generated scripts.
	shellReadRe  = regexp.MustCompile(`Name=key,Values=(spawn:[a-z0-9:-]+)|Key==` + "`" + `(spawn:[a-z0-9:-]+)` + "`")
	shellWriteRe = regexp.MustCompile(`Key=(spawn:[a-z0-9:-]+),Value=`)
	readCallRe   = regexp.MustCompile(`(?i)^(get|lookup|has|read|parse|tag(value|s?or)?|value|must)|(get|lookup|value)$`)
	writeCallRe  = regexp.MustCompile(`(?i)^(set|put|append|add|update|write|create|with|tag(ged)?resource)|(set|unique)$`)
)

// scanTagUses walks every non-test .go file under root.
func scanTagUses(t *testing.T, root string) *tagScan {
	t.Helper()
	sc := &tagScan{writes: map[string]string{}, reads: map[string]string{}, writesIn: map[string]map[string]string{}}

	type file struct {
		dir string
		f   *ast.File
	}
	var files []file
	fset := token.NewFileSet()
	// pass 1: parse, and collect string constants that hold a tag key, by
	// "<dir>.<Name>", so a key spelled through a constant is still seen.
	consts := map[string]string{}
	type constAlias struct {
		dir, name string
		f         *ast.File
		val       ast.Expr
	}
	var aliases []constAlias
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
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
		src, err := os.ReadFile(path) //nolint:gosec // the repo's own sources
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		files = append(files, file{dir, f})
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil && (tagKeyRe.MatchString(s) || strings.HasPrefix(s, "tag:spawn:")) {
							consts[dir+"."+name.Name] = s
						}
					} else {
						// A constant defined as another constant (const K =
						// elasticip.InstanceIDTagKey): resolved after the walk,
						// once every literal constant is known.
						aliases = append(aliases, constAlias{dir: dir, name: name.Name, f: f, val: vs.Values[i]})
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found no Go files")
	}
	// Resolve constants that alias other constants, in or across packages, until
	// nothing changes, so a key spelled through an alias chain is still seen.
	for changed := true; changed; {
		changed = false
		for _, al := range aliases {
			if _, done := consts[al.dir+"."+al.name]; done {
				continue
			}
			var target string
			switch v := al.val.(type) {
			case *ast.Ident:
				target = al.dir + "." + v.Name
			case *ast.SelectorExpr:
				pkg, ok := v.X.(*ast.Ident)
				if !ok {
					continue
				}
				for _, im := range al.f.Imports {
					p, _ := strconv.Unquote(im.Path.Value)
					rel, ok := strings.CutPrefix(p, "github.com/spore-host/spawn/")
					if !ok {
						continue
					}
					local := filepath.Base(rel)
					if im.Name != nil {
						local = im.Name.Name
					}
					if local == pkg.Name {
						target = rel + "." + v.Sel.Name
					}
				}
			}
			if s, ok := consts[target]; ok {
				consts[al.dir+"."+al.name] = s
				changed = true
			}
		}
	}

	const module = "github.com/spore-host/spawn/"
	for _, fl := range files {
		imports := map[string]string{} // local name → repo dir
		for _, im := range fl.f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			rel, ok := strings.CutPrefix(p, module)
			if !ok {
				continue
			}
			name := filepath.Base(rel)
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = rel
		}
		// keyOf returns the tag key e spells, if any.
		keyOf := func(e ast.Expr) (string, bool) {
			switch e := e.(type) {
			case *ast.BasicLit:
				if e.Kind != token.STRING {
					return "", false
				}
				s, err := strconv.Unquote(e.Value)
				if err == nil && tagKeyRe.MatchString(s) {
					return s, true
				}
			case *ast.Ident:
				if s, ok := consts[fl.dir+"."+e.Name]; ok && tagKeyRe.MatchString(s) {
					return s, true
				}
			case *ast.SelectorExpr:
				if pkg, ok := e.X.(*ast.Ident); ok {
					if dir, ok := imports[pkg.Name]; ok {
						if s, ok := consts[dir+"."+e.Sel.Name]; ok && tagKeyRe.MatchString(s) {
							return s, true
						}
					}
				}
			case *ast.CallExpr:
				// tagprefix.Tag("x")
				if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Tag" && len(e.Args) == 1 {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "tagprefix" {
						if lit, ok := e.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							s, _ := strconv.Unquote(lit.Value)
							return "spawn:" + s, true
						}
					}
				}
			}
			return "", false
		}

		for _, decl := range fl.f.Decls {
			declID := fl.dir + "." + tagDeclName(decl)
			var stack []ast.Node
			write := func(key string, pos token.Pos) {
				site := fset.Position(pos).String()
				if _, ok := sc.writes[key]; !ok {
					sc.writes[key] = site
				}
				if sc.writesIn[declID] == nil {
					sc.writesIn[declID] = map[string]string{}
				}
				if _, ok := sc.writesIn[declID][key]; !ok {
					sc.writesIn[declID][key] = site
				}
			}
			read := func(key string, pos token.Pos) {
				if _, ok := sc.reads[key]; !ok {
					sc.reads[key] = fset.Position(pos).String()
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)

				// Shell spellings and EC2 filters live inside string literals.
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						return true
					}
					for _, m := range shellReadRe.FindAllStringSubmatch(s, -1) {
						read(strings.TrimSuffix(m[1]+m[2], "*"), lit.Pos())
					}
					for _, m := range shellWriteRe.FindAllStringSubmatch(s, -1) {
						write(m[1], lit.Pos())
					}
					if k, ok := strings.CutPrefix(s, "tag:"); ok && tagKeyRe.MatchString(k) {
						read(k, lit.Pos())
					}
				}
				// tagprefix.FilterTag("x") and a filter constant are EC2 filters.
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "FilterTag" && len(call.Args) == 1 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							s, _ := strconv.Unquote(lit.Value)
							read("spawn:"+s, call.Pos())
						}
					}
				}

				e, ok := n.(ast.Expr)
				if !ok {
					return true
				}
				key, ok := keyOf(e)
				if !ok || len(stack) < 2 {
					return true
				}
				// tagprefix.Tag("x") was matched as a whole; don't revisit its arg.
				classifyTagUse(stack, e, key, write, read)
				if _, isCall := e.(*ast.CallExpr); isCall {
					return false
				}
				return true
			})
		}
	}
	return sc
}

// classifyTagUse records the use of key at e (the last node on stack) as a
// write, a read, or neither, from its syntactic parent.
func classifyTagUse(stack []ast.Node, e ast.Expr, key string, write, read func(string, token.Pos)) {
	parent := stack[len(stack)-2]
	// A family prefix concatenated with a member name ("spawn:param:"+k) is
	// used the way the concatenation is.
	for len(stack) >= 3 {
		bin, ok := parent.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD || bin.X != e || !isTagPrefix(key) {
			break
		}
		e, parent = bin, stack[len(stack)-3]
		stack = stack[:len(stack)-1]
	}
	// Unwrap aws.String(k): its parent decides.
	if call, ok := parent.(*ast.CallExpr); ok && len(stack) >= 3 {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "String" && len(call.Args) == 1 {
			e, parent = call, stack[len(stack)-3]
			stack = stack[:len(stack)-1]
		}
	}
	switch p := parent.(type) {
	case *ast.KeyValueExpr:
		if p.Key == e {
			write(key, e.Pos()) // map-literal key
			return
		}
		if id, ok := p.Key.(*ast.Ident); ok && id.Name == "Key" {
			write(key, e.Pos()) // types.Tag{Key: ...}
			return
		}
	case *ast.IndexExpr:
		if p.Index != e {
			return
		}
		// cobra's cmd.Annotations is keyed with the same spelling
		// (cmd/output_format.go) and is not an EC2 tag.
		if sel, ok := p.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Annotations" {
			return
		}
		if len(stack) >= 3 {
			if as, ok := stack[len(stack)-3].(*ast.AssignStmt); ok {
				for _, lhs := range as.Lhs {
					if lhs == p {
						write(key, e.Pos())
						return
					}
				}
			}
		}
		read(key, e.Pos())
	case *ast.CaseClause:
		read(key, e.Pos())
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			read(key, e.Pos())
		}
	case *ast.CallExpr:
		name := ""
		switch f := p.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		switch {
		case name == "HasPrefix" || name == "TrimPrefix" || name == "CutPrefix":
			read(key, e.Pos())
		case name == "delete":
			write(key, e.Pos())
		case writeCallRe.MatchString(name):
			write(key, e.Pos())
		case readCallRe.MatchString(name):
			read(key, e.Pos())
		}
	}
}

func tagDeclName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			typ := d.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				return id.Name + "." + d.Name.Name
			}
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, s := range d.Specs {
			if vs, ok := s.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return vs.Names[0].Name
			}
		}
	}
	return "?"
}
