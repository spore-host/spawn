package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
)

// The DNS lease format is a contract between components built separately, and
// one of them cannot import the definition (#dns-lease).
//
// pkg/dns.LeasePrefix is what the reaper reads and what spored relies on;
// lambda/dns-updater is its own Go module that does not import the root module,
// so it carries a COPY. If the copy drifts, the updater writes leases under a name
// the reaper never looks for — every record silently becomes unexpirable — or in a
// format the reaper cannot parse.
//
// Read with go/parser and compared by VALUE, not by searching the file for the
// string: the explanatory comment beside the copy names the prefix too, and a
// substring match would be satisfied by that sentence (CLAUDE.md rule 3).
func TestDNSLeaseContractMatchesTheUpdater(t *testing.T) {
	const path = "lambda/dns-updater/lease.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	consts := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					consts[name.Name] = vs.Values[i]
				}
			}
		}
	}
	if len(consts) == 0 {
		t.Fatalf("found no constants in %s — the parse has broken, not the contract", path)
	}

	// The prefix: a string literal that must equal pkg/dns.LeasePrefix.
	pe, ok := consts["leasePrefix"]
	if !ok {
		t.Fatalf("%s has no leasePrefix constant; the updater's lease name is no longer "+
			"pinned to pkg/dns.LeasePrefix", path)
	}
	lit, ok := pe.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("leasePrefix is not a string literal, so it cannot be checked against pkg/dns")
	}
	got, _ := strconv.Unquote(lit.Value)
	if got != dns.LeasePrefix {
		t.Errorf("the updater writes leases as %q but the reaper reads %q — every lease the "+
			"updater writes would be invisible to the reaper, and no abandoned record would "+
			"ever be expired", got, dns.LeasePrefix)
	}

	// The layout: must be time.RFC3339, which is what pkg/dns.ParseLease accepts.
	le, ok := consts["leaseLayout"]
	if !ok {
		t.Fatalf("%s has no leaseLayout constant", path)
	}
	sel, ok := le.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RFC3339" {
		t.Errorf("the updater's leaseLayout is not time.RFC3339; pkg/dns.ParseLease reads " +
			"only RFC 3339, so the reaper would find every lease unparseable and refuse to act")
	}

	// And the shared format itself must still be the one ParseLease reads, so a
	// change on the pkg/dns side is caught here as well.
	if _, err := dns.ParseLease(`"` + time.Now().UTC().Format(time.RFC3339) + `"`); err != nil {
		t.Errorf("pkg/dns.ParseLease no longer reads RFC 3339, which is what the updater writes: %v", err)
	}
}

// The ownership marker and the lease age the updater takes over at are the
// same kind of contract: the reaper deletes markers by pkg/dns.OwnerName, and
// the updater lets an instance take over a record whose lease is older than its
// copy of pkg/dns.LeaseMaxAge. Read from the updater's source by value.
func TestDNSOwnerContractMatchesTheUpdater(t *testing.T) {
	const path = "lambda/dns-updater/ownership.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	consts := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					consts[name.Name] = vs.Values[i]
				}
			}
		}
	}
	if len(consts) == 0 {
		t.Fatalf("found no constants in %s — the parse has broken, not the contract", path)
	}

	lit, ok := consts["ownerPrefix"].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("%s has no string-literal ownerPrefix constant", path)
	}
	if got, _ := strconv.Unquote(lit.Value); got != dns.OwnerPrefix {
		t.Errorf("the updater writes owner markers as %q but the reaper deletes %q: every marker "+
			"would outlive its record", got, dns.OwnerPrefix)
	}

	// leaseMaxAge = <int> * time.Hour.
	be, ok := consts["leaseMaxAge"].(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("%s: leaseMaxAge is not <n> * time.<unit>", path)
	}
	n, okN := be.X.(*ast.BasicLit)
	unit, okU := be.Y.(*ast.SelectorExpr)
	if !okN || !okU || be.Op != token.MUL {
		t.Fatalf("%s: leaseMaxAge is not <n> * time.<unit>", path)
	}
	units := map[string]time.Duration{"Hour": time.Hour, "Minute": time.Minute, "Second": time.Second}
	k, _ := strconv.Atoi(n.Value)
	if got := time.Duration(k) * units[unit.Sel.Name]; got != dns.LeaseMaxAge {
		t.Errorf("the updater takes over a record whose lease is older than %s, but pkg/dns.LeaseMaxAge is %s", got, dns.LeaseMaxAge)
	}
}
