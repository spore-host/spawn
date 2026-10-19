package cmd

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// t4gLadder is the arm64 burstable ladder E1 (spawn#1171) sized onto: every
// size has 2 vCPUs, so a cpu-only request matches all of them and price alone
// picks — which is how a container task landed on the 0.5 GiB nano. The fake
// finder returns the whole ladder whatever it is asked, as a finder that ignores
// MinMemory would, so the floor has to hold in the sizer itself.
var t4gLadder = []taskproto.Candidate{
	{InstanceType: "t4g.nano", Family: "t4g", VCPUs: 2, MemoryGiB: 0.5, OnDemandPrice: 0.0042},
	{InstanceType: "t4g.micro", Family: "t4g", VCPUs: 2, MemoryGiB: 1, OnDemandPrice: 0.0084},
	{InstanceType: "t4g.small", Family: "t4g", VCPUs: 2, MemoryGiB: 2, OnDemandPrice: 0.0168},
	{InstanceType: "t4g.medium", Family: "t4g", VCPUs: 2, MemoryGiB: 4, OnDemandPrice: 0.0336},
}

func containerCPUOnlySpec() *taskproto.TaskSpec {
	return &taskproto.TaskSpec{
		TaskID:    "e1-container",
		Command:   []string{"true"},
		Container: "public.ecr.aws/docker/library/alpine:3.20",
		Resources: taskproto.ResourceRequest{CPU: 2},
		Lifecycle: taskproto.Lifecycle{TTL: "30m", OnComplete: "terminate"},
	}
}

// #1180 gate: a container spec with only resources.cpu never sizes below the
// container memory floor, and the dry run says why it chose a bigger box.
func TestRenderTaskDryRun_ContainerCPUOnlySizesAtFloor(t *testing.T) {
	var buf bytes.Buffer
	if err := renderTaskDryRun(context.Background(), &buf, containerCPUOnlySpec(), fakeTaskFinder{cands: t4gLadder}, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Instance:     t4g.small ") {
		t.Errorf("container task with cpu only should size to the cheapest >= %.0f GiB type (t4g.small):\n%s", taskproto.ContainerMemoryFloorGiB, got)
	}
	if !strings.Contains(got, "container floor") {
		t.Errorf("dry run does not explain the memory floor:\n%s", got)
	}

	// The same ladder for a host (non-container) task still picks the nano: the
	// floor is about Docker, not a blanket minimum.
	host := containerCPUOnlySpec()
	host.Container = ""
	buf.Reset()
	if err := renderTaskDryRun(context.Background(), &buf, host, fakeTaskFinder{cands: t4gLadder}, "us-east-1"); err != nil {
		t.Fatalf("renderTaskDryRun (host): %v", err)
	}
	if !strings.Contains(buf.String(), "Instance:     t4g.nano ") {
		t.Errorf("host task should still size to t4g.nano:\n%s", buf.String())
	}
}

// recordingFinder records the request it was asked for and stops the run there.
type recordingFinder struct{ got []taskproto.ResourceRequest }

var errFinderStop = errors.New("recordingFinder: stop")

func (f *recordingFinder) FindCandidates(_ context.Context, req taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	f.got = append(f.got, req)
	return nil, errFinderStop
}

// The real launch path asks the finder for at least the floor — the truffle
// finder's MinMemory is what keeps sub-floor types out of the candidate list in
// the first place. runTaskReal sizes before touching AWS, so a nil client is
// never reached: the finder's error ends the run.
func TestRunTaskReal_ContainerAsksFinderForFloor(t *testing.T) {
	for name, run := range map[string]func(taskproto.InstanceFinder) error{
		"real": func(f taskproto.InstanceFinder) error {
			return runTaskReal(context.Background(), &bytes.Buffer{}, nil, containerCPUOnlySpec(), f, "us-east-1")
		},
		"dry-run": func(f taskproto.InstanceFinder) error {
			return renderTaskDryRun(context.Background(), &bytes.Buffer{}, containerCPUOnlySpec(), f, "us-east-1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &recordingFinder{}
			if err := run(f); !errors.Is(err, errFinderStop) {
				t.Fatalf("err = %v, want the finder's stop error", err)
			}
			if len(f.got) != 1 {
				t.Fatalf("finder called %d times, want 1", len(f.got))
			}
			if mem := taskproto.EffectiveMemoryGiB(f.got[0]); mem < taskproto.ContainerMemoryFloorGiB {
				t.Errorf("finder asked for %.2f GiB, below the %.0f GiB container floor", mem, taskproto.ContainerMemoryFloorGiB)
			}
		})
	}
}

// Every taskproto.Size call in cmd sizes spec.SizingRequest(), never
// spec.Resources: the behavioural tests above cover the two call sites that
// exist today; this discovers any new one.
func TestCmd_TaskprotoSizeUsesSizingRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Size" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "taskproto" {
				return true
			}
			found++
			pos := fset.Position(call.Pos())
			if len(call.Args) != 3 {
				t.Errorf("%s: taskproto.Size with %d args", pos, len(call.Args))
				return true
			}
			arg, ok := call.Args[2].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s: taskproto.Size must size <spec>.SizingRequest(), got %T", pos, call.Args[2])
				return true
			}
			if m, ok := arg.Fun.(*ast.SelectorExpr); !ok || m.Sel.Name != "SizingRequest" {
				t.Errorf("%s: taskproto.Size must size <spec>.SizingRequest()", pos)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no taskproto.Size call in cmd; the discovery is broken")
	}
}

// A pinned type is honoured, but a container pin below the floor is warned about.
func TestContainerPinBelowFloorWarns(t *testing.T) {
	spec := containerCPUOnlySpec()
	spec.Resources.InstanceType = "t4g.nano"

	var buf bytes.Buffer
	f := &fakeMemoryLookup{mib: 512, ok: true}
	warnIfContainerPinBelowFloor(context.Background(), f, &buf, "us-east-1", spec)
	if f.seen != "t4g.nano" {
		t.Errorf("looked up %q, want the pinned type", f.seen)
	}
	if !strings.Contains(buf.String(), "t4g.nano has 0.5 GiB") || !strings.Contains(buf.String(), "#1180") {
		t.Errorf("missing pin warning:\n%s", buf.String())
	}

	if got := spec.SizingRequest(); got.InstanceType != "t4g.nano" || got.MemoryGiB != 0 {
		t.Errorf("SizingRequest altered a pinned request: %+v", got)
	}

	for name, tc := range map[string]struct {
		mutate func(*taskproto.TaskSpec)
		mib    int64
	}{
		"at floor":       {func(*taskproto.TaskSpec) {}, 2048},
		"host task":      {func(s *taskproto.TaskSpec) { s.Container = "" }, 512},
		"no pin":         {func(s *taskproto.TaskSpec) { s.Resources.InstanceType = "" }, 512},
		"memory unknown": {func(*taskproto.TaskSpec) {}, 0},
	} {
		s := containerCPUOnlySpec()
		s.Resources.InstanceType = "t4g.nano"
		tc.mutate(s)
		buf.Reset()
		warnIfContainerPinBelowFloor(context.Background(), &fakeMemoryLookup{mib: tc.mib, ok: tc.mib > 0}, &buf, "us-east-1", s)
		if buf.Len() != 0 {
			t.Errorf("%s: unexpected warning:\n%s", name, buf.String())
		}
	}
}
