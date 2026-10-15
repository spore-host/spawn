package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// The exit codes are the bootstrap's contract (#1007): 0 replacement, 1
// original, 2 unknown. The bootstrap skips the workload only on 0.
func TestIsSpotReplacementExitCodes(t *testing.T) {
	cases := map[string]struct {
		verdicts  []spotrequest.Detection
		want      int
		wantCalls int
	}{
		"replacement": {[]spotrequest.Detection{{Verdict: spotrequest.VerdictReplacement}}, exitReplacement, 1},
		"original":    {[]spotrequest.Detection{{Verdict: spotrequest.VerdictNotReplacement}}, exitNotReplacement, 1},
		"unknown, then replacement": {[]spotrequest.Detection{
			{Verdict: spotrequest.VerdictUnknown}, {Verdict: spotrequest.VerdictReplacement}}, exitReplacement, 2},
		"unknown, then original": {[]spotrequest.Detection{
			{Verdict: spotrequest.VerdictUnknown}, {Verdict: spotrequest.VerdictNotReplacement}}, exitNotReplacement, 2},
		"access denied is not retried": {[]spotrequest.Detection{{Verdict: spotrequest.VerdictUnknown,
			Err: &smithy.GenericAPIError{Code: "UnauthorizedOperation"}}}, exitUnknown, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			detect := func(context.Context) spotrequest.Detection {
				d := tc.verdicts[min(calls, len(tc.verdicts)-1)]
				calls++
				return d
			}
			var out bytes.Buffer
			got := isSpotReplacement(context.Background(), &out, detect, time.Hour, time.Now,
				func(context.Context, time.Duration) bool { return true })
			if got != tc.want {
				t.Errorf("exit = %d, want %d\n%s", got, tc.want, out.String())
			}
			if calls != tc.wantCalls {
				t.Errorf("detect called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// Unknown is retried, but only within the budget: an original whose EC2 reads
// fail must not wait forever before its workload starts.
func TestIsSpotReplacementUnknownIsBounded(t *testing.T) {
	calls := 0
	detect := func(context.Context) spotrequest.Detection {
		calls++
		return spotrequest.Detection{Verdict: spotrequest.VerdictUnknown, Err: errors.New("throttled")}
	}
	// A fake clock that only sleeps advance.
	clock := time.Unix(0, 0)
	sleeps := 0
	budget := 3 * replacementCheckRetryInterval
	sleep := func(_ context.Context, d time.Duration) bool {
		sleeps++
		clock = clock.Add(d)
		return sleeps < 100
	}
	got := isSpotReplacement(context.Background(), &bytes.Buffer{}, detect, budget,
		func() time.Time { return clock }, sleep)
	if got != exitUnknown {
		t.Fatalf("exit = %d, want %d (unknown)", got, exitUnknown)
	}
	if calls < 2 {
		t.Errorf("an inconclusive check was not retried (%d calls)", calls)
	}
	if waited := clock.Sub(time.Unix(0, 0)); waited > budget {
		t.Errorf("waited %s, past the %s budget (%d sleeps)", waited, budget, sleeps)
	}
}

func TestPermanentDenial(t *testing.T) {
	for code, want := range map[string]bool{
		"UnauthorizedOperation": true, "AccessDenied": true, "AccessDeniedException": true,
		"RequestLimitExceeded": false, "InternalError": false,
	} {
		if got := permanentDenial(&smithy.GenericAPIError{Code: code}); got != want {
			t.Errorf("permanentDenial(%s) = %v, want %v", code, got, want)
		}
	}
	if permanentDenial(errors.New("dial tcp: timeout")) || permanentDenial(nil) {
		t.Error("a non-API error is not a permanent denial")
	}
}

// TestRunDaemonStartsTheReplacementCheck is the gap #1008 left: nothing failed
// if runDaemon stopped starting the check, which is the only thing that ends a
// replacement from the inside. runDaemon cannot run in a test (it needs EC2 and
// blocks on a signal), so this reads its source: it must contain
// `go replacementCheck{...}.run(...)` with a real retirer and powerOff as the
// shutdown, started before the agent is built.
func TestRunDaemonStartsTheReplacementCheck(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var daemon *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runDaemon" && fd.Recv == nil {
			daemon = fd
		}
	}
	if daemon == nil {
		t.Fatal("runDaemon not found in main.go")
	}

	var checkPos, agentPos token.Pos
	ast.Inspect(daemon.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // a closure might never be called
		case *ast.GoStmt:
			sel, ok := n.Call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "run" {
				return true
			}
			lit, ok := sel.X.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "replacementCheck" {
				return true
			}
			fields := map[string]string{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						fields[k.Name] = exprString(kv.Value)
					}
				}
			}
			if fields["shutdown"] != "powerOff" {
				t.Errorf("replacementCheck.shutdown = %q, want powerOff", fields["shutdown"])
			}
			for _, k := range []string{"r", "notify", "sleep"} {
				if v := fields[k]; v == "" || v == "nil" {
					t.Errorf("replacementCheck.%s is unset", k)
				}
			}
			checkPos = n.Pos()
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewAgent" && agentPos == token.NoPos {
				agentPos = n.Pos()
			}
		}
		return true
	})
	if checkPos == token.NoPos {
		t.Fatal("runDaemon no longer starts `go replacementCheck{...}.run(ctx)`: a Spot replacement " +
			"would run with no TTL and nothing to end it (#995)")
	}
	if agentPos != token.NoPos && checkPos > agentPos {
		t.Error("the replacement check starts after agent.NewAgent; it must start first so a " +
			"failing agent cannot keep a replacement alive")
	}
}

func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.CallExpr:
		return exprString(e.Fun) + "(...)"
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	default:
		return "<expr>"
	}
}
