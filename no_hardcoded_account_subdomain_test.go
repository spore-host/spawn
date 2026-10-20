package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/dns"
)

// No non-test Go file may carry a literal account subdomain of spore.host
// (#1152).
//
// cmd/connect.go built its DCV fallback host as dns + ".c0zxr0ao.spore.host":
// one account's base36 label, for every user. Any other account opened a host
// that was not theirs or did not exist. Instance FQDNs are
// {name}.{account-base36}.{zone}, with the account and zone taken from the
// instance (spawn:account-id, spawn:dns-domain) — see pkg/dns GetFullDNSName.
//
// Matched on the code, not the text (rule 3): only string literals are checked,
// so the comments documenting the FQDN shape (pkg/dns, lambda/dns-updater) do
// not trip it. A '+' chain is flattened and its adjacent literals joined, so
// splitting the host across literals ("." + "c0zxr0ao" + ".spore.host") is
// still caught. Every non-test .go file under the repo is discovered, nested
// modules included; finding none fails.
//
// A candidate label is confirmed with pkg/dns DecodeAccountID, so a service
// host such as "updates.spore.host" is not mistaken for an account subdomain.
var accountSubdomainCandidate = regexp.MustCompile(`(?:^|\.)([0-9a-zA-Z]{6,10})\.spore\.host\b`)

// hasAccountSubdomain reports whether s contains {account-base36}.spore.host.
func hasAccountSubdomain(s string) bool {
	for _, m := range accountSubdomainCandidate.FindAllStringSubmatch(s, -1) {
		if _, ok := dns.DecodeAccountID(m[1]); ok {
			return true
		}
	}
	return false
}

func TestNoHardcodedAccountSubdomain(t *testing.T) {
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "build", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		scanned++
		seen := map[token.Pos]bool{} // a literal and the '+' run holding it report once
		for _, s := range stringRuns(f) {
			if hasAccountSubdomain(s.text) && !seen[s.pos] {
				seen[s.pos] = true
				t.Errorf("%s: literal account subdomain %q — derive the host from the instance's "+
					"spawn:account-id and spawn:dns-domain (pkg/dns GetFullDNSName)", fset.Position(s.pos), s.text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files — the discovery is broken, not the repo clean", scanned)
	}
}

type stringRun struct {
	pos  token.Pos
	text string
}

// stringRuns returns every string literal in f, plus, for each '+' chain, the
// concatenation of each run of adjacent literal operands.
func stringRuns(f *ast.File) []stringRun {
	var out []stringRun
	lit := func(e ast.Expr) (string, bool) {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(bl.Value)
		return v, err == nil
	}
	var flatten func(e ast.Expr) []ast.Expr
	flatten = func(e ast.Expr) []ast.Expr {
		if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			return append(flatten(b.X), flatten(b.Y)...)
		}
		if p, ok := e.(*ast.ParenExpr); ok {
			return flatten(p.X)
		}
		return []ast.Expr{e}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if v, ok := lit(x); ok {
				out = append(out, stringRun{x.Pos(), v})
			}
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return true
			}
			var b strings.Builder
			start := x.Pos()
			flush := func() {
				if b.Len() > 0 {
					out = append(out, stringRun{start, b.String()})
					b.Reset()
				}
			}
			for _, op := range flatten(x) {
				if v, ok := lit(op); ok {
					if b.Len() == 0 {
						start = op.Pos()
					}
					b.WriteString(v)
					continue
				}
				flush()
			}
			flush()
		}
		return true
	})
	return out
}

// The matcher catches the shipped form and a split one, and leaves the
// documented shapes alone.
func TestAccountSubdomainMatcher(t *testing.T) {
	src := `package p
var a = dns + ".c0zxr0ao.spore.host"
var b = name + "." + "c0zxr0ao" + ".spore.host"
var ok1 = "spore.host"
var ok2 = "%s.%s.spore.host"
var ok3 = name + "." + label + ".spore.host"
var ok4 = "https://updates.spore.host/x"
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, s := range stringRuns(f) {
		if hasAccountSubdomain(s.text) {
			hits = append(hits, s.text)
		}
	}
	joined := strings.Join(hits, " | ")
	if !strings.Contains(joined, ".c0zxr0ao.spore.host") || !strings.Contains(joined, "c0zxr0ao.spore.host") || len(hits) < 2 {
		t.Errorf("matcher missed a hardcoded subdomain; hits: %q", hits)
	}
	for _, h := range hits {
		if !strings.Contains(h, "c0zxr0ao") {
			t.Errorf("matcher flagged a non-account literal: %q", h)
		}
	}
}
