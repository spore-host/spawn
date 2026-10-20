package cmd

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #57: `spawn connect` should "just work" whatever the instance's state — a
// stopped or hibernated instance is woken and connected, a stopping one waited
// out and then started — through one shared code path, with --no-start the
// opt-out, and a message that says "resuming" for a hibernated instance.

type fakeWaker struct {
	calls []string
}

func (f *fakeWaker) StartInstance(_ context.Context, region, id string) error {
	f.calls = append(f.calls, "start:"+region+":"+id)
	return nil
}

func (f *fakeWaker) WaitForRunning(_ context.Context, _, id string, _ time.Duration) error {
	f.calls = append(f.calls, "wait-running:"+id)
	return nil
}

func (f *fakeWaker) WaitForStopped(_ context.Context, _, id string, _ time.Duration) error {
	f.calls = append(f.calls, "wait-stopped:"+id)
	return nil
}

func wakeCase(t *testing.T, inst *aws.InstanceInfo, noStart bool) (*fakeWaker, *aws.InstanceInfo, string, error) {
	t.Helper()
	f := &fakeWaker{}
	var out bytes.Buffer
	got, err := wakeForConnect(context.Background(), f, inst, noStart, "my-box", &out,
		func(_ context.Context, id string) (*aws.InstanceInfo, error) {
			f.calls = append(f.calls, "refresh:"+id)
			return &aws.InstanceInfo{InstanceID: id, Region: inst.Region, State: "running", PublicIP: "203.0.113.9"}, nil
		})
	return f, got, out.String(), err
}

func TestWakeForConnect_StartsStoppedAndHibernated(t *testing.T) {
	cases := map[string]struct {
		inst    *aws.InstanceInfo
		wantMsg string
	}{
		"stopped": {
			inst:    &aws.InstanceInfo{InstanceID: "i-1", Region: "us-west-2", State: "stopped", StateReason: "Client.UserInitiatedShutdown: User initiated shutdown"},
			wantMsg: "Instance is stopped — starting it",
		},
		// EC2 reports a hibernated instance as "stopped"; StateReason is what
		// tells them apart.
		"hibernated (EC2 StateReason)": {
			inst:    &aws.InstanceInfo{InstanceID: "i-1", Region: "us-west-2", State: "stopped", StateReason: "Client.UserInitiatedHibernate: User initiated hibernate"},
			wantMsg: "Instance is hibernated — resuming it",
		},
		"hibernated (spawn tag, no reason)": {
			inst:    &aws.InstanceInfo{InstanceID: "i-1", Region: "us-west-2", State: "stopped", Tags: map[string]string{"spawn:last-stop-reason": "user-hibernated"}},
			wantMsg: "Instance is hibernated — resuming it",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, got, out, err := wakeCase(t, tc.inst, false)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"start:us-west-2:i-1", "wait-running:i-1", "refresh:i-1"}
			if strings.Join(f.calls, ",") != strings.Join(want, ",") {
				t.Errorf("calls = %v, want %v", f.calls, want)
			}
			if got.State != "running" || got.PublicIP == "" {
				t.Errorf("returned instance %+v; want the refreshed, running one (a start reassigns the IP)", got)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Errorf("message %q does not contain %q", out, tc.wantMsg)
			}
		})
	}
}

func TestWakeForConnect_StoppingIsWaitedOutThenStarted(t *testing.T) {
	// EC2 rejects StartInstances on a stopping instance, so the stop must land
	// first. Both a plain stop and a hibernate in progress.
	for _, reason := range []string{"", "Client.UserInitiatedHibernate: User initiated hibernate"} {
		f, got, _, err := wakeCase(t, &aws.InstanceInfo{InstanceID: "i-2", Region: "eu-west-1", State: "stopping", StateReason: reason}, false)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"wait-stopped:i-2", "start:eu-west-1:i-2", "wait-running:i-2", "refresh:i-2"}
		if strings.Join(f.calls, ",") != strings.Join(want, ",") {
			t.Errorf("reason %q: calls = %v, want %v", reason, f.calls, want)
		}
		if got.State != "running" {
			t.Errorf("reason %q: state %q", reason, got.State)
		}
	}
}

func TestWakeForConnect_NoStartRefusesWithoutTouchingEC2(t *testing.T) {
	for _, state := range []string{"stopped", "stopping"} {
		f, _, _, err := wakeCase(t, &aws.InstanceInfo{InstanceID: "i-3", State: state}, true)
		if err == nil || !strings.Contains(err.Error(), "spawn start my-box") {
			t.Errorf("%s + --no-start: err = %v, want a refusal pointing at spawn start", state, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s + --no-start made EC2 calls: %v", state, f.calls)
		}
	}
}

func TestWakeForConnect_LeavesOtherStatesAlone(t *testing.T) {
	for _, state := range []string{"running", "pending", "shutting-down", "terminated"} {
		inst := &aws.InstanceInfo{InstanceID: "i-4", State: state}
		f, got, _, err := wakeCase(t, inst, false)
		if err != nil || got != inst || len(f.calls) != 0 {
			t.Errorf("%s: got %+v err %v calls %v; want the instance back untouched", state, got, err, f.calls)
		}
	}
}

// TestEveryConnectPathWakesThroughTheSharedHelper: there used to be two
// auto-starts in cmd/connect.go that disagreed — the DCV app path started only
// "stopped" and ignored --no-start (#57). Discovered from the source: no
// function in a connect*.go file may call StartInstance except
// wakeForConnect, and both connect entry points (the shell/one-shot path and
// the DCV app path) must call it.
func TestEveryConnectPathWakesThroughTheSharedHelper(t *testing.T) {
	files, err := filepath.Glob("connect*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	callers := map[string]bool{}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			scanned++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "StartInstance" && fn.Name.Name != "wakeForConnect" {
						t.Errorf("%s: %s calls StartInstance directly; wake through wakeForConnect so every connect path behaves the same (#57)",
							fset.Position(call.Pos()), fn.Name.Name)
					}
				case *ast.Ident:
					if fun.Name == "wakeForConnect" {
						callers[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("discovered no functions in connect*.go: the discovery is broken")
	}
	for _, entry := range []string{"runConnect", "connectDCV"} {
		if !callers[entry] {
			t.Errorf("%s does not call wakeForConnect: that connect path will not wake a stopped/hibernated instance", entry)
		}
	}
}
