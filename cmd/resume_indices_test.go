package cmd

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1248: `spawn resume` launched only the pending parameter sets but recorded
// them by their position in the pending list, into a state written from
// scratch with TotalParams = len(pending). The next resume then read params
// 0 and 1 as having run and everything else as pending, and relaunched
// completed work.

// fakeSweepLauncher records every launch and reports each launched instance
// as terminated, so the rolling queue moves straight on to the next set.
type fakeSweepLauncher struct {
	launched []string // config names, in launch order
	results  []aws.InstanceInfo
}

func (f *fakeSweepLauncher) Launch(_ context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	id := fmt.Sprintf("i-new-%s", cfg.Name)
	f.launched = append(f.launched, cfg.Name)
	f.results = append(f.results, aws.InstanceInfo{InstanceID: id, State: "terminated"})
	return &aws.LaunchResult{InstanceID: id, Name: cfg.Name, Region: cfg.Region, State: "pending"}, nil
}

func (f *fakeSweepLauncher) ListInstances(context.Context, string, string) ([]aws.InstanceInfo, error) {
	return f.results, nil
}

func (f *fakeSweepLauncher) Terminate(context.Context, string, string) error { return nil }

// unverifiedGuard is a fleet guard with verification off, for tests about the
// queue's bookkeeping rather than spored.
func unverifiedGuard() *fleetGuard {
	return newFleetGuard("sweep", io.Discard, &fakeSweepLauncher{}, nil, false)
}

func fastRollingQueue(t *testing.T) {
	t.Helper()
	old := rollingQueuePollInterval
	rollingQueuePollInterval = time.Millisecond
	t.Cleanup(func() { rollingQueuePollInterval = old })
}

func TestResume_RelaunchRecordsParameterIndicesAndMergesState(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	const total = 10
	done := []int{0, 1, 2, 4, 5, 6, 8, 9}
	orig := &SweepState{
		SweepID:       "sweep-1248",
		SweepName:     "sweep",
		CreatedAt:     time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ParamFile:     "/data/params.yaml",
		TotalParams:   total,
		MaxConcurrent: 1,
		LaunchDelay:   "0s",
	}
	for _, i := range done {
		orig.Instances = append(orig.Instances, InstanceState{
			Index: i, InstanceID: fmt.Sprintf("i-old-%d", i), Region: "us-west-2", State: "running",
		})
	}
	if err := saveSweepState(orig); err != nil {
		t.Fatal(err)
	}

	state, err := loadSweepState(orig.SweepID)
	if err != nil {
		t.Fatal(err)
	}
	// None of the old instances is in EC2 any more: all completed.
	running, completed, failed, pending := reconcileSweepState(state, map[string]string{})
	if fmt.Sprint(pending) != "[3 7]" || len(completed) != len(done) || len(running)+len(failed) != 0 {
		t.Fatalf("first reconcile: running=%v completed=%v failed=%v pending=%v; want pending [3 7]", running, completed, failed, pending)
	}

	configs := make([]*aws.LaunchConfig, 0, len(pending))
	for _, idx := range pending {
		configs = append(configs, &aws.LaunchConfig{Name: fmt.Sprintf("sweep-%d", idx), Region: "us-west-2"})
	}
	launcher := &fakeSweepLauncher{}
	// maxConcurrent 1: set 3 launches in the initial batch and set 7 from the
	// rolling loop, so both recording paths are exercised.
	if _, failures, n, err := relaunchPending(context.Background(), launcher, state, configs, pending, 1, unverifiedGuard()); err != nil || n != 2 || len(failures) != 0 {
		t.Fatalf("relaunchPending: n=%d failures=%v err=%v", n, failures, err)
	}
	if fmt.Sprint(launcher.launched) != "[sweep-3 sweep-7]" {
		t.Fatalf("launched %v, want [sweep-3 sweep-7]", launcher.launched)
	}

	after, err := loadSweepState(orig.SweepID)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalParams != total {
		t.Errorf("TotalParams = %d after resume, want %d (the sweep's size, not the pending count)", after.TotalParams, total)
	}
	if after.ParamFile != orig.ParamFile || after.SweepName != orig.SweepName || !after.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("sweep metadata not kept: param_file=%q name=%q created=%v", after.ParamFile, after.SweepName, after.CreatedAt)
	}
	byIndex := map[int]string{}
	for _, inst := range after.Instances {
		if prev, dup := byIndex[inst.Index]; dup {
			t.Errorf("index %d recorded twice (%s, %s)", inst.Index, prev, inst.InstanceID)
		}
		byIndex[inst.Index] = inst.InstanceID
	}
	for _, i := range done {
		if want := fmt.Sprintf("i-old-%d", i); byIndex[i] != want {
			t.Errorf("index %d: record %q, want the completed %q kept", i, byIndex[i], want)
		}
	}
	for _, i := range pending {
		if want := fmt.Sprintf("i-new-sweep-%d", i); byIndex[i] != want {
			t.Errorf("index %d: record %q, want the relaunched %q", i, byIndex[i], want)
		}
	}
	if len(byIndex) != total {
		idx := make([]int, 0, len(byIndex))
		for i := range byIndex {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		t.Errorf("recorded indices %v, want 0..%d", idx, total-1)
	}

	// The second reconcile: the relaunched instances have finished too.
	ec2 := map[string]string{}
	for _, r := range launcher.results {
		ec2[r.InstanceID] = r.State
	}
	if _, _, _, again := reconcileSweepState(after, ec2); len(again) != 0 {
		t.Errorf("second reconcile finds %v pending; a resume would relaunch completed parameters", again)
	}
}

// The fresh-launch caller passes identity indices: the rolling queue must
// still record set i at Index i, with the full TotalParams.
func TestLaunchWithRollingQueue_FreshSweepRecordsEveryIndex(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)

	configs := []*aws.LaunchConfig{{Name: "s-0"}, {Name: "s-1"}, {Name: "s-2"}}
	state := &SweepState{SweepID: "fresh", SweepName: "s", TotalParams: len(configs), LaunchDelay: "0s"}
	if _, _, n, err := launchWithRollingQueue(context.Background(), &fakeSweepLauncher{}, unverifiedGuard(), state, configs, []int{0, 1, 2}, 1, "0s"); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	saved, err := loadSweepState("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if saved.TotalParams != 3 || len(saved.Instances) != 3 {
		t.Fatalf("saved TotalParams=%d instances=%d, want 3/3", saved.TotalParams, len(saved.Instances))
	}
	for i, inst := range saved.Instances {
		if inst.Index != i || inst.InstanceID != fmt.Sprintf("i-new-s-%d", i) {
			t.Errorf("record %d = index %d %s", i, inst.Index, inst.InstanceID)
		}
	}
}

// The merge gate above drives relaunchPending; this pins runResume to it. The
// resume must hand the rolling queue the state it loaded (relaunchPending's
// third argument is runResume's `state`), never call launchWithRollingQueue
// itself, where a fresh state would drop every completed record.
func TestRunResume_RelaunchesThroughTheLoadedState(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "resume.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runResume *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runResume" {
			runResume = fd
		}
	}
	if runResume == nil {
		t.Fatal("runResume not found in resume.go")
	}
	viaLoaded := 0
	ast.Inspect(runResume.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch fn.Name {
		case "launchWithRollingQueue":
			t.Errorf("%s: runResume calls launchWithRollingQueue directly; go through relaunchPending with the loaded state (#1248)", fset.Position(call.Pos()))
		case "relaunchPending":
			if len(call.Args) > 2 {
				if id, ok := call.Args[2].(*ast.Ident); ok && id.Name == "state" {
					viaLoaded++
					return true
				}
			}
			t.Errorf("%s: relaunchPending must be passed runResume's loaded `state`", fset.Position(call.Pos()))
		}
		return true
	})
	if viaLoaded != 1 {
		t.Errorf("runResume relaunches through relaunchPending(…, state, …) %d times, want 1", viaLoaded)
	}
}
