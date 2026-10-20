package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// restartedFromTagInstance was first launched three days ago (spawn:launch-time) and
// started again an hour ago, so EC2's LaunchTime — the latest START — says one
// hour. It has run for five hours in total (spored's spawn:compute-seconds) and
// has a 96h TTL but no spawn:ttl-deadline, so its deadline is the fallback
// spawn:launch-time + spawn:ttl that spored and the TTL reaper enforce: one day
// from now. Anchored to EC2's LaunchTime it would be ~4 days from now (#1029).
func restartedFromTagInstance(now time.Time) aws.InstanceInfo {
	first := now.Add(-72*time.Hour - 30*time.Second)
	return aws.InstanceInfo{
		InstanceID:       "i-0restarted",
		Name:             "restarted",
		InstanceType:     "m5.large",
		State:            "running",
		AvailabilityZone: "us-east-1a",
		PublicIP:         "203.0.113.7",
		IAMRole:          "spored-role",
		LaunchTime:       now.Add(-time.Hour),
		TTL:              "96h",
		Tags: map[string]string{
			"spawn:managed":         "true",
			"spawn:launch-time":     first.UTC().Format(time.RFC3339),
			"spawn:ttl":             "96h",
			"spawn:compute-seconds": "18000",
			"spawn:price-per-hour":  "1.000000",
		},
	}
}

func TestRestartedInstanceIsShownFromItsFirstLaunch(t *testing.T) {
	now := time.Now()
	inst := restartedFromTagInstance(now)
	first, _ := time.Parse(time.RFC3339, inst.Tags["spawn:launch-time"])

	t.Run("TTL deadline is anchored on spawn:launch-time", func(t *testing.T) {
		got, ok := lifecycleDeadline(&inst)
		if want := first.Add(96 * time.Hour); !ok || !got.Equal(want) {
			t.Fatalf("deadline = %v (ok=%v), want spawn:launch-time + spawn:ttl = %v", got, ok, want)
		}
		if left := ttlRemaining(inst, now); !strings.HasPrefix(left, "23h") {
			t.Errorf("TTL remaining = %q, want ~1 day (23h…); EC2 LaunchTime gives ~4 days", left)
		}
	})

	t.Run("spawn list Age is ~3d", func(t *testing.T) {
		setColorInputs(t, false, true)
		out := captureStdout(t, func() { displayStandaloneInstances([]aws.InstanceInfo{inst}, false) })
		var row []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, inst.InstanceID) {
				row = strings.Fields(line)
			}
		}
		// ID, name, type, state, IAM role, AZ, AGE, TTL, IP, spot.
		if len(row) < 8 {
			t.Fatalf("no row for %s in:\n%s", inst.InstanceID, out)
		}
		if row[6] != "3d" {
			t.Errorf("Age = %q, want 3d (since first launch); EC2 LaunchTime gives 1h\n%s", row[6], out)
		}
		if !strings.HasPrefix(row[7], "23h") {
			t.Errorf("TTL = %q, want ~1 day left\n%s", row[7], out)
		}
	})

	t.Run("spawn cost counts compute across every boot", func(t *testing.T) {
		out := singleInstanceCostReport(&inst, now)
		for _, want := range []string{"Since launch:    3d", "Compute time:    5h", "~$5.00"} {
			if !strings.Contains(out, want) {
				t.Errorf("cost report missing %q (5h of compute at $1/hr, 3 days since first launch):\n%s", want, out)
			}
		}
	})

	t.Run("spawn cost without spored's tag is a labelled upper bound", func(t *testing.T) {
		noTag := restartedFromTagInstance(now)
		delete(noTag.Tags, "spawn:compute-seconds")
		out := singleInstanceCostReport(&noTag, now)
		for _, want := range []string{"at most 3d", "at most ~$72.0"} {
			if !strings.Contains(out, want) {
				t.Errorf("cost report missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("spawn status max compute cost includes earlier boots", func(t *testing.T) {
		block := lifecycleProtectionBlock(&inst)
		// 5h of compute so far (+5 min tag lag, as spawn extend sizes it) plus
		// ~24h to the deadline at $1/hr. Deadline - EC2 LaunchTime gave ~$95
		// (with the EC2-anchored deadline); deadline - first launch, which counts
		// the stopped time as compute, would give ~$96.
		if !strings.Contains(block, "~$29.0") {
			t.Errorf("max compute cost is not compute so far + time to deadline (~$29.08):\n%s", block)
		}
	})
}

// The display anchor is the reaper's rule: the EARLIER of the tag and EC2's
// LaunchTime (GHSA-c93q-4jh7-6pvj), so a forged later spawn:launch-time cannot
// make an instance look younger than the reaper treats it.
func TestOriginalLaunchTimeIsTheEarlierOfTagAndEC2(t *testing.T) {
	ec2 := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	forged := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	inst := &aws.InstanceInfo{LaunchTime: ec2, Tags: map[string]string{"spawn:launch-time": forged.Format(time.RFC3339)}}
	if got := originalLaunchTime(inst); !got.Equal(ec2) {
		t.Errorf("tag later than EC2's LaunchTime: %v, want the earlier EC2 time %v", got, ec2)
	}
}

// TestNoDisplayDurationFromRawLaunchTime keeps every cmd/ reader of EC2's
// LaunchTime that measures a duration, an age or a deadline on
// originalLaunchTime, the one place that applies the reaper's rule (#1029).
//
// It discovers every `.LaunchTime` selector in the package's non-test files and
// allows only uses that are not arithmetic: ordering two instances
// (`.After`/`.Before`/`.Equal`), exporting the raw EC2 field (`.Format`), and
// copying it into a struct field. Anything else — time.Since(x.LaunchTime),
// x.LaunchTime.Add/Sub/IsZero, passing it to a function, assigning it — fails.
func TestNoDisplayDurationFromRawLaunchTime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen, inHelper := 0, 0
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			helper := isFunc && fn.Recv == nil && fn.Name.Name == "originalLaunchTime"
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "LaunchTime" {
					return true
				}
				seen++
				if helper {
					inHelper++
					return true
				}
				if !allowedLaunchTimeUse(stack) {
					bad = append(bad, fset.Position(sel.Pos()).String())
				}
				return true
			})
		}
	}
	if seen == 0 || inHelper == 0 {
		t.Fatalf("discovery found %d .LaunchTime reads (%d in originalLaunchTime); the scan is broken", seen, inHelper)
	}
	for _, p := range bad {
		t.Errorf("%s: reads EC2's LaunchTime directly for something other than ordering/export/copy; "+
			"use originalLaunchTime (the reaper's rule, #1029) — EC2 resets LaunchTime on every start", p)
	}
}

// allowedLaunchTimeUse reports whether the `.LaunchTime` selector at the top of
// stack is used in a way that measures nothing.
func allowedLaunchTimeUse(stack []ast.Node) bool {
	sel := stack[len(stack)-1]
	if len(stack) < 2 {
		return false
	}
	switch p := stack[len(stack)-2].(type) {
	case *ast.KeyValueExpr:
		// `LaunchTime: in.LaunchTime` in a composite literal: a copy.
		return p.Value == sel
	case *ast.SelectorExpr:
		// x.LaunchTime.After(...) / .Before / .Equal / .Format(...).
		switch p.Sel.Name {
		case "After", "Before", "Equal", "Format":
			return len(stack) >= 3 && isCallOf(stack[len(stack)-3], p)
		}
	case *ast.CallExpr:
		// y.LaunchTime as the argument of a.LaunchTime.After(y.LaunchTime).
		if fun, ok := p.Fun.(*ast.SelectorExpr); ok {
			switch fun.Sel.Name {
			case "After", "Before", "Equal":
				if inner, ok := fun.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "LaunchTime" {
					return true
				}
			}
		}
	}
	return false
}

func isCallOf(n ast.Node, fun ast.Expr) bool {
	c, ok := n.(*ast.CallExpr)
	return ok && c.Fun == fun
}
