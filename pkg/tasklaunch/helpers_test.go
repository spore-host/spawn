package tasklaunch

import (
	"context"
	"io"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// Test doubles and fixtures moved here from cmd's task tests along with the
// code they exercise.

type fakeTaskFinder struct{ cands []taskproto.Candidate }

func (f fakeTaskFinder) FindCandidates(context.Context, taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return f.cands, nil
}

type fakeMemoryLookup struct {
	mib  int64
	ok   bool
	seen string // instance type asked about
}

func (f *fakeMemoryLookup) InstanceMemoryMiB(_ context.Context, _, instanceType string) (int64, bool) {
	f.seen = instanceType
	return f.mib, f.ok
}

// fakeResultStore records the keys it was asked to delete.
type fakeResultStore struct {
	deleted []string
	delErr  error
}

func (f *fakeResultStore) DeleteS3Object(_ context.Context, _, _, key string) error {
	f.deleted = append(f.deleted, key)
	return f.delErr
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

// renderDryRun is the CLI's dry run as the library produces it: Plan, then
// Render.
func renderDryRun(ctx context.Context, out io.Writer, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) error {
	p, err := Plan(ctx, Deps{Finder: finder}, spec, region)
	if err != nil {
		return err
	}
	Render(out, p)
	return nil
}

// pinnedR8gSpec is the #1253 repro: a pinned r8g.2xlarge, 45m TTL.
func pinnedR8gSpec() *taskproto.TaskSpec {
	return &taskproto.TaskSpec{
		TaskID:    "pinned-r8g",
		Command:   []string{"true"},
		Resources: taskproto.ResourceRequest{InstanceType: "r8g.2xlarge", Purchase: taskproto.PurchaseOnDemand},
		Lifecycle: taskproto.Lifecycle{TTL: "45m", OnComplete: "terminate", CostLimit: 0.50},
	}
}
