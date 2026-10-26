package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/tasklaunch"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// `spawn task status --check-complete` exit codes, mirroring `spawn status`.
const (
	taskStatusExitCompleted = 0
	taskStatusExitFailed    = 1
	taskStatusExitRunning   = 2
	taskStatusExitError     = 3
)

// instanceLister is (*aws.Client).ListInstances, a seam for tests.
type instanceLister = tasklaunch.InstanceLister

// taskInstances returns every instance (any state) that ran taskID.
func taskInstances(all []aws.InstanceInfo, taskID string) []aws.InstanceInfo {
	return tasklaunch.TaskInstances(all, taskID)
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
// For an ended instance it also names the spawn:flush-status tag spored wrote
// (#1295), and once EC2 has forgotten the instance it says that the flush
// status is gone with it, rather than leaving the user to guess.
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
			printFlushWithoutRecord(out, tasklaunch.FlushStatusOf([]aws.InstanceInfo{*gone}), region)
		}
		return taskStatusExitFailed
	default:
		if !quiet {
			fmt.Fprintf(out, "Task %s: unknown (no completion record, and no instance for it in %s)\n", taskID, region)
			fmt.Fprintf(out, "  Flush: %s.\n", tasklaunch.FlushStatus{})
		}
		return taskStatusExitError
	}
}

// printFlushWithoutRecord explains a terminated task's missing record from its
// flush status (#1295).
func printFlushWithoutRecord(out io.Writer, fs tasklaunch.FlushStatus, region string) {
	switch {
	case !fs.Recorded():
		fmt.Fprintf(out, "  Flush: %s.\n", fs)
		fmt.Fprintf(out, "  It was stopped without a terminal flush, so its command.log may not have been saved.\n")
	case fs.Value == taskproto.FlushStatusOK:
		fmt.Fprintf(out, "  Flush: %s — the flush wrote a record, so it is probably under another results prefix (pass --results-prefix).\n", fs.Value)
	default:
		fmt.Fprintf(out, "  Flush: %s — the flush hook's output is on the serial console: aws ec2 get-console-output --instance-id %s --region %s\n", fs.Value, fs.InstanceID, region)
	}
}

// printRecordFlushStatus adds the compact `Flush:` line under a completion
// record (#1295). Best-effort: the record is the answer, so a failed lookup
// prints nothing rather than failing the command.
func printRecordFlushStatus(ctx context.Context, out io.Writer, list instanceLister, region, taskID string) {
	fs, err := tasklaunch.ReadFlushStatus(ctx, list, region, taskID)
	if err != nil {
		return
	}
	fmt.Fprintf(out, "Flush:      %s\n", fs)
}
