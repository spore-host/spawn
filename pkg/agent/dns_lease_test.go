package agent

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/provider"
)

func TestNextLeaseRefreshIsJitteredAroundTheInterval(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	lo := now.Add(dns.LeaseRefreshInterval - dns.LeaseRefreshJitter)
	hi := now.Add(dns.LeaseRefreshInterval + dns.LeaseRefreshJitter)
	seen := map[time.Time]bool{}
	for i := 0; i < 200; i++ {
		next := nextLeaseRefresh(now, true)
		if next.Before(lo) || next.After(hi) {
			t.Fatalf("next renewal %s is outside [%s, %s]", next, lo, hi)
		}
		seen[next] = true
	}
	// Without jitter a job array launched together renews in one burst and trips
	// Route53's five-per-second limit.
	if len(seen) < 10 {
		t.Errorf("200 schedules produced only %d distinct times — the renewal is not jittered", len(seen))
	}
}

// A failed renewal is retried soon. A lease only survives dns.LeaseMaxAge, so
// waiting the full interval after a failure would burn a sixth of that budget per
// failure.
func TestFailedRenewalRetriesSoon(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	next := nextLeaseRefresh(now, false)
	if d := next.Sub(now); d != dnsRetryAfterFailure {
		t.Errorf("retry after failure in %s, want %s", d, dnsRetryAfterFailure)
	}
	if dnsRetryAfterFailure >= dns.LeaseRefreshInterval {
		t.Error("a failed renewal waits as long as a successful one")
	}
}

// Not due, or no DNS configured: nothing happens, and certainly no call to the
// updater from a spore that never registered.
func TestRefreshIsANoOpWhenNotDueOrNotRegistered(t *testing.T) {
	a := &Agent{}
	a.refreshDNSLease(context.Background()) // no client: must not panic or act

	a.config = &provider.Config{DNSName: "box"}
	a.nextDNSRefresh = time.Now().Add(time.Hour)
	a.refreshDNSLease(context.Background()) // client still nil
	if a.lastDNSStatus != "" {
		t.Errorf("status changed to %q without a registration", a.lastDNSStatus)
	}
}

// THE invariant: anything that asks for a lease must be something that renews it.
//
// A lease that is never renewed expires its record dns.LeaseMaxAge later, under an
// instance that is still alive. spored renews — refreshDNSLease runs on every tick
// — so spored, and only spored, may call SetRenewing(true). `spawn dns register`
// shares the same client for a one-shot registration and must not.
//
// Discovered, not listed: every non-test Go file in the module is parsed and every
// CALL of SetRenewing(true) located, so a new caller anywhere is caught (rule 4).
// Matched as a call expression, so a comment mentioning it cannot satisfy or trip
// the gate (rule 3).
func TestOnlyARenewingCallerMayRequestALease(t *testing.T) {
	root := "../.."
	var callers []string
	files := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "docs-gen", "lambda", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetRenewing" || len(call.Args) != 1 {
				return true
			}
			if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == "false" {
				return true // explicitly opting out is always fine
			}
			callers = append(callers, filepath.ToSlash(path))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files < 50 {
		t.Fatalf("parsed only %d files — the walk has broken, which would make this pass vacuously", files)
	}
	if len(callers) == 0 {
		t.Fatal("nothing calls SetRenewing(true), so spored no longer requests a lease and no " +
			"abandoned record can ever be expired by lease")
	}
	for _, c := range callers {
		if !strings.HasSuffix(c, "pkg/agent/agent.go") {
			t.Errorf("%s calls SetRenewing(true) but is not spored's agent. Only the agent "+
				"renews (refreshDNSLease, every tick); a lease requested by anything else is "+
				"never renewed, so its record expires under a live instance %s later.",
				c, dns.LeaseMaxAge)
		}
	}

	// And the agent must actually renew: refreshDNSLease must be CALLED from the
	// tick path, not merely defined.
	b, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	f, _ := parser.ParseFile(token.NewFileSet(), "agent.go", b, 0)
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "refreshDNSLease" {
				called = true
			}
		}
		return true
	})
	if !called {
		t.Error("the agent requests a lease (SetRenewing(true)) but never calls refreshDNSLease, " +
			"so every lease it asks for expires its own record under a live instance")
	}
}
