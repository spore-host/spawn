package cmd

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// countingCleaner records cleanup calls a failed launch makes (#1298).
type countingCleaner struct {
	terminated, fsxDeleted []string
	terminateErr           error
}

func (c *countingCleaner) Terminate(_ context.Context, _, id string) error {
	c.terminated = append(c.terminated, id)
	return c.terminateErr
}

func (c *countingCleaner) DeleteFSxFilesystem(_ context.Context, id, _ string) error {
	c.fsxDeleted = append(c.fsxDeleted, id)
	return nil
}

// The failures the issue names, as launchWithProgress returns them after
// RunInstances: each must terminate the instance exactly once by default.
var postLaunchFailures = map[string]error{
	"spored never came up":   sporedVerificationError(errors.New("spored did not become ready within 5m0s")),
	"spored not verifiable":  sporedVerificationError(&sporedUnverifiableError{err: errors.New("ec2:DescribeInstances denied")}),
	"WaitForRunning failed":  errors.New("exceeded max wait time for InstanceRunning waiter"),
	"public-IP lookup":       errors.New("describe instance i-0abc: throttled"),
	"ephemeral FSx failed":   errors.New("instance i-0abc launched, but starting the ephemeral FSx failed: quota"),
	"output marshal (other)": errors.New("failed to marshal JSON"),
}

func TestFailedLaunchTerminatesByDefault(t *testing.T) {
	for name, failure := range postLaunchFailures {
		c := &countingCleaner{}
		var w bytes.Buffer
		err := finishLaunch(context.Background(), &w, c, "us-west-2", "i-0abc", "", false, failure)
		if len(c.terminated) != 1 || c.terminated[0] != "i-0abc" {
			t.Errorf("%s: terminated %v, want [i-0abc] exactly once", name, c.terminated)
		}
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "terminated it") {
			t.Errorf("%s: err = %v, want the failure, saying the instance was terminated", name, err)
		}
		if !strings.Contains(w.String(), "--keep-on-error") {
			t.Errorf("%s: notice %q does not name the opt-out", name, w.String())
		}
	}
}

// The ephemeral FSx a launch created for the instance goes with it.
func TestFailedLaunchDeletesItsEphemeralFSx(t *testing.T) {
	c := &countingCleaner{}
	_ = finishLaunch(context.Background(), &bytes.Buffer{}, c, "us-west-2", "i-0abc", "fs-0123", false, errors.New("boom"))
	if len(c.terminated) != 1 || len(c.fsxDeleted) != 1 || c.fsxDeleted[0] != "fs-0123" {
		t.Errorf("terminated %v, deleted FSx %v; want the instance and fs-0123", c.terminated, c.fsxDeleted)
	}
}

// --keep-on-error keeps the instance and its FSx, and says it has no safety net.
func TestKeepOnErrorKeepsTheInstanceAndWarns(t *testing.T) {
	for name, failure := range postLaunchFailures {
		c := &countingCleaner{}
		var w bytes.Buffer
		err := finishLaunch(context.Background(), &w, c, "us-west-2", "i-0abc", "fs-0123", true, failure)
		if len(c.terminated)+len(c.fsxDeleted) != 0 {
			t.Errorf("%s: --keep-on-error terminated %v / deleted %v", name, c.terminated, c.fsxDeleted)
		}
		if !errors.Is(err, failure) {
			t.Errorf("%s: err = %v, want the failure still returned", name, err)
		}
		for _, want := range []string{"NO TTL", "spawn terminate i-0abc", "fs-0123"} {
			if !strings.Contains(w.String(), want) {
				t.Errorf("%s: warning %q does not mention %q", name, w.String(), want)
			}
		}
	}
}

// A successful launch, and one that failed before anything launched, clean up
// nothing.
func TestSuccessAndPreLaunchFailureTerminateNothing(t *testing.T) {
	c := &countingCleaner{}
	if err := finishLaunch(context.Background(), &bytes.Buffer{}, c, "us-west-2", "i-0abc", "fs-0123", false, nil); err != nil {
		t.Errorf("success returned %v", err)
	}
	pre := errors.New("InsufficientInstanceCapacity")
	if err := finishLaunch(context.Background(), &bytes.Buffer{}, c, "us-west-2", "", "", false, pre); !errors.Is(err, pre) || err.Error() != pre.Error() {
		t.Errorf("pre-launch failure returned %v, want it unchanged", err)
	}
	if len(c.terminated)+len(c.fsxDeleted) != 0 {
		t.Errorf("terminated %v / deleted %v with nothing to clean up", c.terminated, c.fsxDeleted)
	}
}

// A terminate that fails says the instance is still running.
func TestFailedTerminateSaysTheInstanceIsStillRunning(t *testing.T) {
	c := &countingCleaner{terminateErr: errors.New("UnauthorizedOperation")}
	err := finishLaunch(context.Background(), &bytes.Buffer{}, c, "us-west-2", "i-0abc", "", false, errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), "STILL RUNNING") {
		t.Errorf("err = %v, want it to say the instance is still running", err)
	}
}

func TestResolveKeepOnError(t *testing.T) {
	cases := []struct {
		keepSet, keep, termSet, term bool
		want                         bool
		wantErr                      bool
	}{
		{false, false, false, false, false, false}, // default: terminate
		{true, true, false, false, true, false},    // --keep-on-error
		{false, false, true, true, false, false},   // deprecated --terminate-on-error: the default
		{false, false, true, false, true, false},   // --terminate-on-error=false: keep, as it always meant
		{true, false, false, false, false, false},  // --keep-on-error=false
		{true, true, true, true, false, true},      // contradiction
	}
	for _, c := range cases {
		got, err := resolveKeepOnError(c.keepSet, c.keep, c.termSet, c.term)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("resolveKeepOnError(%v,%v,%v,%v) = %v, %v; want %v, err=%v", c.keepSet, c.keep, c.termSet, c.term, got, err, c.want, c.wantErr)
		}
	}
	if f := launchCmd.Flags().Lookup("terminate-on-error"); f == nil || f.Deprecated == "" {
		t.Error("--terminate-on-error must stay accepted, marked deprecated")
	}
	if launchCmd.Flags().Lookup("keep-on-error") == nil {
		t.Error("no --keep-on-error flag")
	}
}

// The structural half: launchWithProgress must run finishLaunch over its own
// returned error, deferred BEFORE RunInstances (so every return after it is
// covered, whatever is added later), with launchedID set from the launch
// result. Each assertion is on the AST, not the text, so a comment cannot
// satisfy it.
func TestLaunchWithProgressDefersFinishLaunchOverRunInstances(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "launchWithProgress" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("launchWithProgress not found")
	}
	res := fn.Type.Results
	if res == nil || len(res.List) != 1 || len(res.List[0].Names) != 1 || res.List[0].Names[0].Name != "err" {
		t.Fatal("launchWithProgress must name its error result err, so a deferred finishLaunch can see and replace it")
	}

	var deferPos, launchPos, assignPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeferStmt:
			ast.Inspect(x, func(m ast.Node) bool {
				as, ok := m.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				lhs, ok := as.Lhs[0].(*ast.Ident)
				call, ok2 := as.Rhs[0].(*ast.CallExpr)
				if !ok || !ok2 || lhs.Name != "err" {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "finishLaunch" && len(call.Args) == 8 {
					k, kok := call.Args[6].(*ast.Ident)
					if a, ok := call.Args[4].(*ast.Ident); ok && a.Name == "launchedID" && kok && k.Name == "keepOnError" {
						if e, ok := call.Args[7].(*ast.Ident); ok && e.Name == "err" {
							deferPos = x.Pos()
						}
					}
				}
				return true
			})
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Launch" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "awsClient" {
					launchPos = x.Pos()
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if l, ok := x.Lhs[0].(*ast.Ident); ok && l.Name == "launchedID" {
					if r, ok := x.Rhs[0].(*ast.SelectorExpr); ok && r.Sel.Name == "InstanceID" {
						assignPos = x.Pos()
					}
				}
			}
		}
		return true
	})
	switch {
	case deferPos == token.NoPos:
		t.Fatal("launchWithProgress does not defer `err = finishLaunch(..., launchedID, ..., keepOnError, err)`: a failure after RunInstances would leave the instance running (#1298)")
	case launchPos == token.NoPos:
		t.Fatal("awsClient.Launch call not found in launchWithProgress")
	case deferPos > launchPos:
		t.Error("finishLaunch is deferred after RunInstances; failures between the two are not covered")
	case assignPos == token.NoPos || assignPos < launchPos:
		t.Error("launchedID is not set from the launch result after RunInstances, so finishLaunch never knows an instance exists")
	}
}

// Every launch is told when no reaper scans its region and spored is all that
// enforces its TTL (#1298) — but only on evidence, never on a probe that could
// not run.
func TestLaunchReaperAdviceCoversSporedOnlyLaunches(t *testing.T) {
	notCovered := aws.ReaperCoverage{Determined: true}
	if msg := launchReaperAdvice(notCovered, false, "terminate", ""); !strings.Contains(msg, "spored") {
		t.Errorf("--on-complete terminate, no reaper: advice %q, want a warning", msg)
	}
	if msg := launchReaperAdvice(notCovered, false, "", ""); msg == "" {
		t.Error("no --on-complete, no reaper: no warning")
	}
	if msg := launchReaperAdvice(aws.ReaperCoverage{Covered: true, Determined: true}, false, "terminate", ""); msg != "" {
		t.Errorf("covered: advice %q, want none", msg)
	}
	if msg := launchReaperAdvice(aws.ReaperCoverage{Why: "AccessDenied"}, false, "terminate", ""); msg != "" {
		t.Errorf("undetermined: advice %q, want none (it would print on every launch without lambda/iam reads)", msg)
	}
	if msg := launchReaperAdvice(notCovered, true, "stop", ""); msg != aws.ReaperCoverageAdvice(notCovered, "stop", "") {
		t.Error("a launch that leans on the reaper no longer gets ReaperCoverageAdvice")
	}
}

// The coverage probe must be asked about the LAUNCH region.
func TestReaperCoverageIsCheckedForTheLaunchRegion(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "warnIfReaperWontCover" && len(call.Args) == 4 {
			if sel, ok := call.Args[2].(*ast.SelectorExpr); ok && sel.Sel.Name == "Region" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "config" {
					found = true
				}
			}
		}
		return true
	})
	if !found {
		t.Error("warnIfReaperWontCover is not passed config.Region, the launch region")
	}
}

// runLaunch must resolve keepOnError from the flags; without it --keep-on-error
// would parse and be ignored. Anchored at line start so a comment cannot pass.
func TestRunLaunchResolvesKeepOnError(t *testing.T) {
	src := readSourceFile(t, "launch_single.go")
	re := regexp.MustCompile(`(?m)^\s*keepOnError, kerr = resolveKeepOnError\(cmd\.Flags\(\)\.Changed\("keep-on-error"\), keepOnErrorFlag,\s*\n\s*cmd\.Flags\(\)\.Changed\("terminate-on-error"\), terminateOnError\)`)
	if !re.MatchString(src) {
		t.Error("runLaunch does not resolve keepOnError from --keep-on-error / --terminate-on-error")
	}
}
