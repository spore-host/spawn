package cmd

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// runConfirm drives confirmLaunchCost with a stubbed price, a chosen
// interactive/non-interactive stdin and an answer, and returns its result and
// what it printed to stderr.
func runConfirm(t *testing.T, interactive, yes bool, answer string, cfg *aws.LaunchConfig, n int) (bool, string) {
	t.Helper()
	oldPrice, oldTTY, oldYes, oldCount := resolveLaunchDryRunPrice, launchStdinIsInteractive, autoYes, count
	t.Cleanup(func() {
		resolveLaunchDryRunPrice, launchStdinIsInteractive, autoYes, count = oldPrice, oldTTY, oldYes, oldCount
	})
	resolveLaunchDryRunPrice = func(context.Context, *aws.Client, string, string) (aws.DisplayPrice, error) {
		return aws.DisplayPrice{PricePerHour: 0.50, Source: "live"}, nil
	}
	launchStdinIsInteractive = func() bool { return interactive }
	autoYes, count = yes, n

	inR, inW, _ := os.Pipe()
	_, _ = inW.WriteString(answer)
	_ = inW.Close()
	errR, errW, _ := os.Pipe()
	oldIn, oldErr := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = inR, errW
	ok := confirmLaunchCost(context.Background(), nil, cfg)
	os.Stdin, os.Stderr = oldIn, oldErr
	_ = errW.Close()
	var b bytes.Buffer
	_, _ = io.Copy(&b, errR)
	return ok, b.String()
}

func cfg4h() *aws.LaunchConfig {
	return &aws.LaunchConfig{InstanceType: "m8a.4xlarge", Region: "us-east-1", TTL: "4h"}
}

// The #1115 repro: an interactive "n" must not launch. Before, nothing was
// asked and `echo n | spawn launch …` launched.
func TestLaunchConfirm_InteractiveNoDoesNotLaunch(t *testing.T) {
	ok, out := runConfirm(t, true, false, "n\n", cfg4h(), 1)
	if ok {
		t.Fatal("answering n on an interactive terminal still launched")
	}
	for _, want := range []string{"About to launch:", "m8a.4xlarge", "Max cost:     ~$2.00", "Launch? [y/N]"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, out)
		}
	}
}

func TestLaunchConfirm_InteractiveYesLaunches(t *testing.T) {
	if ok, _ := runConfirm(t, true, false, "y\n", cfg4h(), 1); !ok {
		t.Fatal("answering y didn't launch")
	}
}

// --yes skips the question but still states the ceiling.
func TestLaunchConfirm_YesSkipsTheQuestion(t *testing.T) {
	ok, out := runConfirm(t, true, true, "", cfg4h(), 1)
	if !ok || strings.Contains(out, "[y/N]") || !strings.Contains(out, "Max cost:") {
		t.Errorf("--yes: ok=%v, output:\n%s", ok, out)
	}
}

// Scripts and CI keep working: no question on a non-terminal stdin, even with
// an "n" piped in, but the maximum cost is printed (the owner's 2026-10-18
// decision on #1115: prompt on a terminal only).
func TestLaunchConfirm_NonInteractiveLaunchesAndStatesTheCost(t *testing.T) {
	ok, out := runConfirm(t, false, false, "n\n", cfg4h(), 1)
	if !ok {
		t.Fatal("a non-interactive launch was refused; scripts without --yes must keep working")
	}
	if strings.Contains(out, "[y/N]") || strings.Contains(out, "About to launch") {
		t.Errorf("a non-interactive launch asked:\n%s", out)
	}
	if !strings.Contains(out, "Max cost:     ~$2.00") {
		t.Errorf("a non-interactive launch didn't state its maximum cost:\n%s", out)
	}
}

// A job array's ceiling is per instance times the count.
func TestLaunchConfirm_ArrayCostCountsEveryInstance(t *testing.T) {
	_, out := runConfirm(t, true, false, "n\n", cfg4h(), 8)
	if !strings.Contains(out, "~$16.00") || !strings.Contains(out, "8 instances") {
		t.Errorf("8 × $0.50/h × 4h should be $16.00 for 8 instances:\n%s", out)
	}
}

// With no TTL the ceiling is unbounded and the summary says so, not "$0".
func TestLaunchConfirm_NoTTLSaysUnbounded(t *testing.T) {
	c := cfg4h()
	c.TTL = ""
	_, out := runConfirm(t, false, false, "", c, 1)
	if !strings.Contains(out, "unbounded") {
		t.Errorf("no-TTL launch should state an unbounded ceiling:\n%s", out)
	}
}

// The question is only worth anything if the launch path asks it. Gate the
// wiring: in the function that calls launchWithProgress, an
// `if !confirmLaunchCost(...)` must come before that call, so a refusal
// returns before anything is launched (#1115).
func TestLaunchPathAsksBeforeLaunching(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	isCall := func(e ast.Expr, name string) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == name
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name == "launchWithProgress" {
			continue
		}
		var guard, launch token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.IfStmt:
				if u, ok := x.Cond.(*ast.UnaryExpr); ok && u.Op == token.NOT && isCall(u.X, "confirmLaunchCost") && guard == 0 {
					guard = x.Pos()
				}
			case *ast.CallExpr:
				if isCall(x, "launchWithProgress") && launch == 0 {
					launch = x.Pos()
				}
			}
			return true
		})
		if launch == 0 {
			continue
		}
		found = true
		if guard == 0 || guard > launch {
			t.Errorf("%s calls launchWithProgress without first checking `if !confirmLaunchCost(...)`", fn.Name.Name)
		}
	}
	if !found {
		t.Fatal("no function in launch_single.go calls launchWithProgress; this gate would pass vacuously")
	}
}
