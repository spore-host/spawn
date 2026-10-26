package tasklaunch

import (
	"context"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// InstanceLister is the EC2 read ReadFlushStatus needs: (*aws.Client).ListInstances.
type InstanceLister func(ctx context.Context, region, stateFilter string) ([]aws.InstanceInfo, error)

// TaskInstances returns every instance in all (any state) that ran taskID. The
// spawn:task-id tag is authoritative; the Name tag is the fallback for
// instances launched before the tag existed (task run sets Name = task id).
func TaskInstances(all []aws.InstanceInfo, taskID string) []aws.InstanceInfo {
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

// FlushStatus is what EC2 still says about a task's terminal flush (#1295):
// the taskproto.FlushStatusTag of the instance the task last ran on.
//
// The tag lives on the instance, so it can be read only while EC2 lists the
// instance: about an hour after termination. After that InstanceID is empty
// and Available reports false. A listed instance with no tag (Value "") was
// ended without spored's flush, by a spored older than v0.144.0, or is still
// running.
type FlushStatus struct {
	InstanceID    string // the instance whose tag was read; "" when EC2 lists none for the task
	InstanceState string // its EC2 state
	Value         string // its FlushStatusTag value; "" when it carries none
}

// Available reports whether EC2 still lists an instance for the task, so its
// flush status could be read at all.
func (f FlushStatus) Available() bool { return f.InstanceID != "" }

// Recorded reports whether the instance carries a flush status.
func (f FlushStatus) Recorded() bool { return f.Value != "" }

// Ended reports whether the instance is terminated or shutting down.
func (f FlushStatus) Ended() bool {
	return f.InstanceState == "terminated" || f.InstanceState == "shutting-down"
}

// String is a one-line, human-readable form, for a `Flush:` line.
func (f FlushStatus) String() string {
	switch {
	case !f.Available():
		return "no longer available (EC2 lists a terminated instance, and its tags, for only about an hour)"
	case f.Recorded():
		return fmt.Sprintf("%s (instance %s)", f.Value, f.InstanceID)
	case !f.Ended():
		return fmt.Sprintf("none yet (instance %s is %s)", f.InstanceID, f.InstanceState)
	default:
		return fmt.Sprintf("not recorded on instance %s (ended without spored's flush, or by a spored older than v0.144.0)", f.InstanceID)
	}
}

// FlushStatusOf returns the flush status of the instance a task last ended on:
// of insts (one task's instances, as TaskInstances returns), the most recently
// launched terminated or shutting-down one, or the most recently launched of
// any state if none has ended. The zero FlushStatus (not Available) when insts
// is empty.
func FlushStatusOf(insts []aws.InstanceInfo) FlushStatus {
	var ended, latest *aws.InstanceInfo
	for i := range insts {
		inst := &insts[i]
		if latest == nil || inst.LaunchTime.After(latest.LaunchTime) {
			latest = inst
		}
		if inst.State == "terminated" || inst.State == "shutting-down" {
			if ended == nil || inst.LaunchTime.After(ended.LaunchTime) {
				ended = inst
			}
		}
	}
	pick := ended
	if pick == nil {
		pick = latest
	}
	if pick == nil {
		return FlushStatus{}
	}
	return FlushStatus{InstanceID: pick.InstanceID, InstanceState: pick.State, Value: pick.Tags[taskproto.FlushStatusTag]}
}

// ReadFlushStatus lists region's instances in every state and returns the
// flush status of taskID's (FlushStatusOf). It is the one reader of the
// spawn:flush-status tag: `spawn task status` calls it, and so can the MCP
// server, alongside ReadCompletion.
func ReadFlushStatus(ctx context.Context, list InstanceLister, region, taskID string) (FlushStatus, error) {
	all, err := list(ctx, region, "all")
	if err != nil {
		return FlushStatus{}, fmt.Errorf("list instances in %s: %w", region, err)
	}
	return FlushStatusOf(TaskInstances(all, taskID)), nil
}
