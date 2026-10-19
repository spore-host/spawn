package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// `spawn task status --check-complete` exit codes, mirroring `spawn status`.
const (
	taskStatusExitCompleted = 0
	taskStatusExitFailed    = 1
	taskStatusExitRunning   = 2
	taskStatusExitError     = 3
)

// instanceLister is (*aws.Client).ListInstances, a seam for tests.
type instanceLister func(ctx context.Context, region, stateFilter string) ([]aws.InstanceInfo, error)

// taskInstances returns every instance (any state) that ran taskID. The
// spawn:task-id tag is authoritative; the Name tag is the fallback for
// instances launched before the tag existed (task run sets Name = task id).
func taskInstances(all []aws.InstanceInfo, taskID string) []aws.InstanceInfo {
	var out []aws.InstanceInfo
	for _, inst := range all {
		if id, ok := inst.Tags["spawn:task-id"]; ok {
			if id == taskID {
				out = append(out, inst)
			}
			continue
		}
		if inst.Name == taskID {
			out = append(out, inst)
		}
	}
	return out
}

// reportTaskWithoutRecord decides what `task status` says when there is no
// completion record, by asking EC2 what became of the task's instance.
//
// Before spawn#802 the answer was always "running", which is false forever
// for an instance killed without spored's involvement (`spawn terminate` from
// an older CLI, `aws ec2 terminate-instances`, a host failure): nothing will
// ever write the record. The rule:
//
//   - any live instance (pending/running/stopping/stopped) → running
//   - only terminated/shutting-down instances → terminated, no record (failed)
//   - no instance at all (never launched, or EC2 has forgotten it) → unknown
//
// Returns the --check-complete exit code; prints a line unless quiet.
func reportTaskWithoutRecord(ctx context.Context, list instanceLister, region, taskID string, out io.Writer, quiet bool) int {
	all, err := list(ctx, region, "all")
	if err != nil {
		if !quiet {
			fmt.Fprintf(out, "Task %s: unknown (no completion record; could not look up its instance: %v)\n", taskID, err)
		}
		return taskStatusExitError
	}
	insts := taskInstances(all, taskID)

	var live, gone *aws.InstanceInfo
	for i := range insts {
		inst := &insts[i]
		switch inst.State {
		case "terminated", "shutting-down":
			if gone == nil || inst.LaunchTime.After(gone.LaunchTime) {
				gone = inst
			}
		default:
			if live == nil || inst.LaunchTime.After(live.LaunchTime) {
				live = inst
			}
		}
	}

	switch {
	case live != nil:
		if !quiet {
			fmt.Fprintf(out, "Task %s: running (no completion record yet; instance %s is %s)\n", taskID, live.InstanceID, live.State)
		}
		return taskStatusExitRunning
	case gone != nil:
		if !quiet {
			why := ""
			if r := strings.TrimSpace(gone.StateTransitionReason); r != "" {
				why = ": " + r
			}
			fmt.Fprintf(out, "Task %s: terminated (no completion record) — instance %s is %s%s\n", taskID, gone.InstanceID, gone.State, why)
			fmt.Fprintf(out, "  It was stopped without a terminal flush, so its command.log may not have been saved.\n")
		}
		return taskStatusExitFailed
	default:
		if !quiet {
			fmt.Fprintf(out, "Task %s: unknown (no completion record, and no instance for it in %s)\n", taskID, region)
		}
		return taskStatusExitError
	}
}
