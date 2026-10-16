package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #884: `spawn launch --os windows` with no --ami launched Amazon Linux while
// every surface called it Windows, and the Windows rate was quoted at the Linux
// price. Both are tested through the real paths a user hits, not only through
// the helpers.

// The refusal must come from ensureAMIAndPreflight — the path both a real launch
// and --dry-run take — not merely exist as a function.
func TestWindowsWithoutAnAMIIsRefusedBeforeLaunch(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)

	prevOS := osFlag
	t.Cleanup(func() { osFlag = prevOS })

	for _, a := range []string{"", "auto", "AUTO"} {
		osFlag = "windows"
		cfg := &spawnaws.LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", AMI: a, TTL: "1h"}
		var out bytes.Buffer
		err := runLaunchDryRun(context.Background(), &out, client, cfg)
		if err == nil {
			t.Fatalf("--os windows with --ami %q was accepted; auto-detection picks Amazon Linux, "+
				"so this launches Linux labelled Windows (resolved AMI %s, target %s)", a, cfg.AMI, cfg.TargetOS)
		}
		for _, want := range []string{"--ami", "ami-windows-latest"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--ami %q: the refusal does not say how to proceed (missing %q): %v", a, want, err)
			}
		}
		if out.Len() != 0 {
			t.Errorf("--ami %q: a refused launch still printed a plan:\n%s", a, out.String())
		}
	}

	// Linux auto-detect is unaffected.
	osFlag = "linux"
	cfg := &spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "auto", TTL: "1h"}
	if err := runLaunchDryRun(context.Background(), &bytes.Buffer{}, client, cfg); err != nil {
		t.Fatalf("--os linux with auto AMI: %v", err)
	}
}

func stubDryRunPrice(t *testing.T, rate float64) {
	t.Helper()
	prev := resolveLaunchDryRunPrice
	t.Cleanup(func() { resolveLaunchDryRunPrice = prev })
	resolveLaunchDryRunPrice = func(context.Context, *spawnaws.Client, string, string) (spawnaws.DisplayPrice, error) {
		return spawnaws.DisplayPrice{PricePerHour: rate, Source: "live"}, nil
	}
}

func runWindowsAwareDryRun(t *testing.T, targetOS, format string) string {
	t.Helper()
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	stubDryRunPrice(t, 0.2016)

	prevOS, prevOut := osFlag, spawnOutputFormat
	t.Cleanup(func() { osFlag, spawnOutputFormat = prevOS, prevOut })
	osFlag, spawnOutputFormat = targetOS, format

	cfg := &spawnaws.LaunchConfig{
		Name: "w", InstanceType: "m7i.xlarge", Region: "us-east-1",
		AMI: "ami-0abc123", TTL: "4h", SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
	}
	var out bytes.Buffer
	if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
		t.Fatalf("runLaunchDryRun (%s): %v", targetOS, err)
	}
	return out.String()
}

func TestWindowsDryRunSaysTheRateExcludesTheLicence(t *testing.T) {
	out := runWindowsAwareDryRun(t, "windows", "")
	if !strings.Contains(out, "Rate:") {
		t.Fatalf("the preview printed no rate, so this test proves nothing:\n%s", out)
	}
	if !strings.Contains(out, windowsLicenceNote) {
		t.Errorf("a Windows preview quotes the Linux rate without saying so; the licence "+
			"EC2 adds is missing, so the figure understates:\n%s", out)
	}

	if lin := runWindowsAwareDryRun(t, "linux", ""); strings.Contains(lin, "Windows licence") {
		t.Errorf("a Linux preview carries the Windows caveat:\n%s", lin)
	}
}

func TestWindowsDryRunJSONMarksTheRateALowerBound(t *testing.T) {
	var p launchPlan
	if err := json.Unmarshal([]byte(runWindowsAwareDryRun(t, "windows", "json")), &p); err != nil {
		t.Fatal(err)
	}
	if p.OnDemandPricePerHour == 0 {
		t.Fatal("the JSON plan carries no rate, so this test proves nothing")
	}
	if !p.PriceIsLowerBound {
		t.Error("the JSON plan for a Windows target does not mark its rate as a lower bound; " +
			"a wrapper reading -o json would take the Linux rate as the Windows price")
	}

	var l launchPlan
	if err := json.Unmarshal([]byte(runWindowsAwareDryRun(t, "linux", "json")), &l); err != nil {
		t.Fatal(err)
	}
	if l.PriceIsLowerBound {
		t.Error("a Linux plan is marked as a lower bound")
	}
}

func TestWindowsEstimateSaysTheRateExcludesTheLicence(t *testing.T) {
	for _, n := range []int{1, 3} {
		var b strings.Builder
		renderInstanceCostEstimate(&b, "m7i.xlarge", "us-east-1", "windows", 0.2016, "live", "4h", n)
		if !strings.Contains(b.String(), windowsLicenceNote) {
			t.Errorf("count=%d: the Windows estimate omits the licence caveat:\n%s", n, b.String())
		}
		b.Reset()
		renderInstanceCostEstimate(&b, "m7i.xlarge", "us-east-1", "linux", 0.2016, "live", "4h", n)
		if strings.Contains(b.String(), "Windows licence") {
			t.Errorf("count=%d: the Linux estimate carries the Windows caveat", n)
		}
	}
}

// --estimate-only resolves no AMI, so it has no config.TargetOS. What it passes
// as the target OS must be estimateTargetOS(...) — a literal or config.TargetOS
// there would be "" and silently drop the caveat.
func TestEstimateOnlyPassesTheResolvedTargetOS(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); !ok || id.Name != "renderInstanceCostEstimate" {
			return true
		}
		calls++
		arg, _ := c.Args[3].(*ast.CallExpr)
		var fn *ast.Ident
		if arg != nil {
			fn, _ = arg.Fun.(*ast.Ident)
		}
		if fn == nil || fn.Name != "estimateTargetOS" {
			t.Errorf("%s: renderInstanceCostEstimate's target OS is not estimateTargetOS(...)",
				fset.Position(c.Pos()))
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no renderInstanceCostEstimate call in launch_single.go; the gate is reading the wrong file")
	}
}

func TestEstimateTargetOSHonoursOSFlag(t *testing.T) {
	prev := osFlag
	t.Cleanup(func() { osFlag = prev })
	osFlag = "windows"
	// AMI "auto" must not reach DescribeImages; a nil client would panic if it did.
	if got := estimateTargetOS(context.Background(), nil, &spawnaws.LaunchConfig{AMI: "auto"}); got != "windows" {
		t.Errorf("estimateTargetOS with --os windows = %q, want windows", got)
	}
	// No --os and AMI "auto": nothing to look up, so Linux — and "auto" must never
	// be handed to DescribeImages as an image id.
	osFlag = ""
	if got := estimateTargetOS(context.Background(), nil, &spawnaws.LaunchConfig{AMI: "auto"}); got != "linux" {
		t.Errorf("estimateTargetOS with no --os and AMI auto = %q, want linux", got)
	}
}

// #1058: a Windows --cost-limit would be enforced at the Linux rate. Launch
// refuses it, but only after IAM, security group, key and FSx exist, so the CLI
// must refuse it in preflight — and the preview must report the same refusal.
func TestWindowsCostLimitIsRefusedInPreflight(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	stubDryRunPrice(t, 0.2016)

	prevOS := osFlag
	t.Cleanup(func() { osFlag = prevOS })

	osFlag = "windows"
	cfg := &spawnaws.LaunchConfig{InstanceType: "m7i.xlarge", Region: "us-east-1", AMI: "ami-0abc123",
		TTL: "1h", CostLimit: 10, SecurityGroupIDs: []string{"sg-0123456789abcdef0"}}
	var out bytes.Buffer
	err := runLaunchDryRun(context.Background(), &out, client, cfg)
	if err == nil {
		t.Fatalf("a Windows --cost-limit passed preflight; spored would enforce it at the Linux rate:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "cost-limit") || !strings.Contains(err.Error(), "--ttl") {
		t.Errorf("refusal is not actionable: %v", err)
	}

	osFlag = "linux"
	cfg = &spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", AMI: "ami-0abc123", TTL: "1h", CostLimit: 10}
	if err := runLaunchDryRun(context.Background(), &bytes.Buffer{}, client, cfg); err != nil {
		t.Fatalf("a Linux --cost-limit was refused: %v", err)
	}
}

// --estimate-only resolves no AMI and skips ensureAMIAndPreflight, so it needs
// the check itself, inside its own block, with the OS it can actually resolve.
func TestEstimateOnlyRefusesAWindowsCostLimit(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found, blocks int
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if id, ok := ifs.Cond.(*ast.Ident); !ok || id.Name != "estimateOnly" {
			return true
		}
		blocks++
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "CheckCostLimitEnforceable" || len(c.Args) != 2 {
				return true
			}
			if w, ok := c.Args[1].(*ast.CallExpr); ok {
				if id, ok := w.Fun.(*ast.Ident); ok && id.Name == "isWindowsTarget" && len(w.Args) == 1 {
					if inner, ok := w.Args[0].(*ast.CallExpr); ok {
						if iid, ok := inner.Fun.(*ast.Ident); ok && iid.Name == "estimateTargetOS" {
							found++
						}
					}
				}
			}
			return true
		})
		return true
	})
	if blocks == 0 {
		t.Fatal("no `if estimateOnly {` block in launch_single.go; the gate is reading the wrong code")
	}
	if found == 0 {
		t.Error("the --estimate-only block does not call aws.CheckCostLimitEnforceable(config, isWindowsTarget(estimateTargetOS(...)))")
	}
}
