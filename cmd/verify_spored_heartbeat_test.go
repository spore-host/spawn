package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// fakeSignals is an instance as verification sees it. heartbeatOnPoll is the
// liveness poll (1-based) on which spored's heartbeat tag first appears; 0 means
// never. Each method counts its calls, so the tests measure polls, not time.
type fakeSignals struct {
	heartbeatOnPoll int
	describeErr     error // returned by every liveness call
	ssmUp           bool  // the SSM agent is registered
	ssmOnlineErr    error // returned by every ssmOnline call
	probeErr        error
	probeStatus     string

	livenessCalls, onlineCalls, probeCalls int
}

func (f *fakeSignals) liveness(context.Context) (spawnclient.SporedLiveness, error) {
	f.livenessCalls++
	if f.describeErr != nil {
		return spawnclient.SporedLiveness{}, f.describeErr
	}
	l := spawnclient.SporedLiveness{HasInstanceProfile: true, State: "running"}
	if f.heartbeatOnPoll > 0 && f.livenessCalls >= f.heartbeatOnPoll {
		l.Heartbeat = "2026-10-19T12:00:00Z"
	}
	return l, nil
}

func (f *fakeSignals) ssmOnline(context.Context) (bool, error) {
	f.onlineCalls++
	return f.ssmUp, f.ssmOnlineErr
}

func (f *fakeSignals) probe(context.Context) (*spawnclient.SSMRunResult, error) {
	f.probeCalls++
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	status := f.probeStatus
	if status == "" {
		status = "Failed"
	}
	return &spawnclient.SSMRunResult{Status: status}, nil
}

// fakeClock advances only when verification sleeps.
type fakeClock struct {
	t      time.Time
	sleeps int
}

func (c *fakeClock) clock() verifyClock {
	return verifyClock{
		now: func() time.Time { return c.t },
		sleep: func(_ context.Context, d time.Duration) error {
			c.sleeps++
			c.t = c.t.Add(d)
			return nil
		},
	}
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)} }

func ssmDenied(op string) error {
	return &spawnclient.AccessDeniedError{Action: "ssm:" + op, Err: &smithy.GenericAPIError{Code: "AccessDeniedException"}}
}

const testVerifyTimeout = 5 * time.Minute

// #1126 + #1053: the caller may not use SSM at all, yet spored is up and
// heartbeating. That is a verified launch.
func TestVerifySpored_SSMDeniedButHeartbeatVerifies(t *testing.T) {
	f := &fakeSignals{heartbeatOnPoll: 4, ssmOnlineErr: ssmDenied("DescribeInstanceInformation")}
	clk := newFakeClock()
	if err := verifySporedWith(context.Background(), f, testVerifyTimeout, clk.clock()); err != nil {
		t.Fatalf("verify = %v, want nil: the heartbeat tag proves spored is up", err)
	}
	if f.livenessCalls != 4 {
		t.Errorf("liveness polled %d times, want 4 (verified on the poll the heartbeat appeared)", f.livenessCalls)
	}
	if f.onlineCalls != 1 {
		t.Errorf("ssmOnline called %d times, want 1: a refusal is not asked again", f.onlineCalls)
	}
}

// #1053: an AMI without amazon-ssm-agent (Rocky, RHEL). SSM never registers;
// spored heartbeats. That is a verified launch.
func TestVerifySpored_NoSSMAgentButHeartbeatVerifies(t *testing.T) {
	f := &fakeSignals{heartbeatOnPoll: 7}
	if err := verifySporedWith(context.Background(), f, testVerifyTimeout, newFakeClock().clock()); err != nil {
		t.Fatalf("verify = %v, want nil: the heartbeat tag proves spored is up without SSM", err)
	}
	if f.probeCalls != 0 {
		t.Errorf("probed %d times over SSM with no registered agent", f.probeCalls)
	}
}

// SSM is still a signal: `spored status` succeeding over SSM verifies before
// the first heartbeat is written.
func TestVerifySpored_SSMProbeVerifiesBeforeHeartbeat(t *testing.T) {
	f := &fakeSignals{ssmUp: true, probeStatus: "Success"}
	if err := verifySporedWith(context.Background(), f, testVerifyTimeout, newFakeClock().clock()); err != nil {
		t.Fatalf("verify = %v, want nil", err)
	}
	if f.livenessCalls != 1 || f.probeCalls != 1 {
		t.Errorf("liveness=%d probe=%d, want 1/1", f.livenessCalls, f.probeCalls)
	}
}

// No heartbeat within the budget is still the failure it always was — the
// instance has no lifecycle agent — and is reported as "never came up".
func TestVerifySpored_NoHeartbeatTimesOut(t *testing.T) {
	f := &fakeSignals{}
	clk := newFakeClock()
	err := verifySporedWith(context.Background(), f, testVerifyTimeout, clk.clock())
	if err == nil {
		t.Fatal("verify = nil with no heartbeat and no SSM, want the timeout failure")
	}
	var unv *sporedUnverifiableError
	if errors.As(err, &unv) {
		t.Fatalf("a quiet instance was reported as unverifiable: %v", err)
	}
	if !strings.Contains(err.Error(), spawnclient.HeartbeatTag) {
		t.Errorf("timeout error %q does not say which evidence was missing", err)
	}
	// It polled for the whole budget, not one poll then gave up.
	if want := int(testVerifyTimeout/sporedVerifyPollInterval) + 1; f.livenessCalls != want {
		t.Errorf("liveness polled %d times, want %d (every interval across the budget)", f.livenessCalls, want)
	}
	launchErr := sporedVerificationFailure(context.Background(), nil, "us-west-2", "i-1", false, err)
	if !strings.Contains(launchErr.Error(), "spored never came up") {
		t.Errorf("launch error %q, want the existing \"spored never came up\" verdict", launchErr)
	}
}

// A refusal of the SSM probe after registration is named in the timeout message.
func TestVerifySpored_TimeoutNamesTheRefusedSSMAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeSignals
		want string
	}{
		{"describe", &fakeSignals{ssmOnlineErr: ssmDenied("DescribeInstanceInformation")}, "ssm:DescribeInstanceInformation"},
		{"send", &fakeSignals{ssmUp: true, probeErr: ssmDenied("SendCommand")}, "ssm:SendCommand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySporedWith(context.Background(), tc.f, testVerifyTimeout, newFakeClock().clock())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
			if tc.f.probeCalls > 1 {
				t.Errorf("probed %d times after a refusal, want at most 1", tc.f.probeCalls)
			}
		})
	}
}

// #1126: if the caller may not read the evidence at all, verification stops at
// once, names the action, and the launch does NOT say spored never came up.
func TestVerifySpored_DescribeDeniedIsUnverifiableAndNamed(t *testing.T) {
	f := &fakeSignals{describeErr: &spawnclient.AccessDeniedError{
		Action: "ec2:DescribeInstances", Err: &smithy.GenericAPIError{Code: "UnauthorizedOperation"},
	}}
	clk := newFakeClock()
	err := verifySporedWith(context.Background(), f, testVerifyTimeout, clk.clock())
	var unv *sporedUnverifiableError
	if !errors.As(err, &unv) {
		t.Fatalf("err = %v, want sporedUnverifiableError", err)
	}
	if !strings.Contains(err.Error(), "ec2:DescribeInstances") {
		t.Errorf("err %q does not name ec2:DescribeInstances", err)
	}
	if f.livenessCalls != 1 || clk.sleeps != 0 {
		t.Errorf("liveness=%d sleeps=%d, want 1/0: a refusal is terminal, not polled", f.livenessCalls, clk.sleeps)
	}
	launchErr := sporedVerificationFailure(context.Background(), nil, "us-west-2", "i-1", false, err)
	if strings.Contains(launchErr.Error(), "never came up") {
		t.Errorf("launch error %q blames the instance for the caller's missing permission", launchErr)
	}
	if !strings.Contains(launchErr.Error(), "could not be verified") || !strings.Contains(launchErr.Error(), "ec2:DescribeInstances") {
		t.Errorf("launch error %q, want \"could not be verified\" naming ec2:DescribeInstances", launchErr)
	}
}

// The heartbeat the CLI waits for must be the one spored writes, and spored
// must write it on Windows too (#878): the writer is in a file every GOOS
// builds, and the key is the constant the CLI reads.
func TestHeartbeatTagIsWhatSporedWritesOnEveryOS(t *testing.T) {
	dir := filepath.Join("..", "pkg", "agent")
	for _, goos := range []string{"linux", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		pkg, err := ctx.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("GOOS=%s: %v", goos, err)
		}
		files := pkg.GoFiles
		writers := 0
		for _, name := range files {
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name.Name != "writeHeartbeatTag" || fd.Body == nil {
					continue
				}
				writers++
				var keys []string
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					kv, ok := n.(*ast.KeyValueExpr)
					if !ok {
						return true
					}
					if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Key" {
						return true
					}
					if call, ok := kv.Value.(*ast.CallExpr); ok && len(call.Args) == 1 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok {
							v, _ := strconv.Unquote(lit.Value)
							keys = append(keys, v)
						}
					}
					return true
				})
				if len(keys) != 1 || keys[0] != spawnclient.HeartbeatTag {
					t.Errorf("GOOS=%s: writeHeartbeatTag writes tag keys %q, the CLI waits for %q", goos, keys, spawnclient.HeartbeatTag)
				}
			}
		}
		if writers != 1 {
			t.Errorf("GOOS=%s: found %d writeHeartbeatTag in pkg/agent's build, want 1 — spored would not heartbeat there", goos, writers)
		}
	}
	// And the monitor tick calls it.
	src, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "agent.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	ast.Inspect(src, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "checkAndAct" {
			return true
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "writeHeartbeatTag" {
					called = true
				}
			}
			return true
		})
		return false
	})
	if !called {
		t.Error("checkAndAct no longer calls writeHeartbeatTag: no launch could verify spored by its heartbeat")
	}
}

// #878: --wait-for-ssh=false skips verification, but must say so. The `if` that
// decides verification has an else branch that writes a notice naming the
// heartbeat tag; without it a skipped check reads exactly like a passed one.
func TestSkippedSporedVerificationIsAnnounced(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "launch_single.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil {
			return true
		}
		decides := false
		ast.Inspect(ifs.Init, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "sporedVerificationFor" {
					decides = true
				}
			}
			return true
		})
		if !decides {
			return true
		}
		found++
		announces := false
		// A bare block: an `else if` would make the notice conditional again.
		if blk, ok := ifs.Else.(*ast.BlockStmt); ok {
			ast.Inspect(blk, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Fprintf" || len(call.Args) < 2 {
					return true
				}
				if id, ok := call.Args[0].(*ast.SelectorExpr); !ok || id.Sel.Name != "Stderr" {
					return true
				}
				for _, a := range call.Args[2:] {
					if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "HeartbeatTag" {
						announces = true
					}
				}
				return true
			})
		}
		if !announces {
			t.Error("launch_single skips spored verification without telling the user (no stderr notice naming HeartbeatTag in the else branch)")
		}
		return true
	})
	if found != 1 {
		t.Fatalf("found %d verification decisions in launch_single.go, want 1", found)
	}
}
