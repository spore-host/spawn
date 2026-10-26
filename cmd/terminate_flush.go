package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// terminateFlushTimeout bounds the SSM wait for the task flush that runs ahead
// of `spawn terminate`. The hook itself is capped on the box at
// taskFlushHookSeconds plus the output seconds it is granted (spawn#1312) plus
// taskproto.FlushOutputsSlackSeconds; the extra 30 s is SSM delivery and
// polling. The wait ends as soon as the hook does, so it is this long only
// for a hook that uses its whole budget or an agent that never answers.
//
// terminateFlushOutputsSeconds is the most the CLI grants: whatever the task
// declared (lifecycle.flush_outputs_seconds), up to its maximum. A user
// terminate is the one SSM flush with no other instances waiting behind it.
const (
	taskFlushHookSeconds         = taskproto.FlushHookSeconds
	terminateFlushOutputsSeconds = taskproto.MaxFlushOutputsSeconds
	terminateFlushTimeout        = time.Duration(taskFlushHookSeconds+terminateFlushOutputsSeconds+taskproto.FlushOutputsSlackSeconds)*time.Second + 30*time.Second
)

// ssmShellRunner is the one SSM call the terminate flush makes. It is
// (*aws.Client).RunShellScript in production and a recording fake in tests.
type ssmShellRunner func(ctx context.Context, region, instanceID, command string, timeout time.Duration) (*aws.SSMRunResult, error)

// taskFlushCommand is the shell the CLI sends over SSM to run a task's
// terminal-flush hook with the user_terminated reason, granting the hook its
// declared output seconds (spawn#1312). The command itself, trust check,
// grant and on-box bound included, is taskproto.FlushCommandWithOutputs,
// shared with the TTL reaper (#1186).
func taskFlushCommand() string {
	return taskproto.FlushCommandWithOutputs(taskproto.ExitUserTerminated, terminateFlushOutputsSeconds)
}

// needsTaskFlush reports whether inst is a running `spawn task run` instance,
// the only kind with a flush hook to run. A stopped instance has no agent to
// receive the command.
func needsTaskFlush(inst *aws.InstanceInfo) bool {
	return inst.State == "running" && inst.Tags["spawn:task-id"] != ""
}

// flushTaskBeforeTerminate runs the task terminal-flush hook on inst over SSM
// before the caller terminates it, so `spawn terminate` leaves the same
// command.log and completion.json that a TTL or cost-limit kill does
// (spawn#802; the lifecycle paths got this in #643).
//
// spored only runs the hook on exits it mediates; a user terminate goes straight
// to EC2, so before this the task's log was lost and `task status` said
// "running" forever. Best-effort by design: a failure is reported on warn and
// never blocks the termination. Returns whether the flush ran successfully.
func flushTaskBeforeTerminate(ctx context.Context, run ssmShellRunner, inst *aws.InstanceInfo, warn io.Writer) bool {
	if !needsTaskFlush(inst) {
		return false
	}
	fmt.Fprintf(warn, "Flushing task %s's log, declared outputs and completion record before terminating...\n", inst.Tags["spawn:task-id"])
	res, err := run(ctx, inst.Region, inst.InstanceID, taskFlushCommand(), terminateFlushTimeout)
	switch {
	case err != nil:
		fmt.Fprintf(warn, "⚠️  Could not flush task record over SSM (%v); terminating anyway — command.log may be lost.\n", err)
		return false
	case res == nil || res.Status != "Success" || res.ResponseCode != 0:
		detail := ""
		if res != nil {
			detail = fmt.Sprintf("status %s, exit %d: %s", res.Status, res.ResponseCode, strings.TrimSpace(res.Stderr))
		}
		fmt.Fprintf(warn, "⚠️  Task flush did not complete (%s); terminating anyway — command.log may be lost.\n", detail)
		return false
	}
	return true
}

// flushTasksBeforeTerminate is the job-array form: flushes run concurrently so
// an array of N task instances waits for the slowest flush, not the sum.
func flushTasksBeforeTerminate(ctx context.Context, run ssmShellRunner, insts []aws.InstanceInfo, warn io.Writer) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	lw := lockedWriter{w: warn, mu: &mu}
	for i := range insts {
		if !needsTaskFlush(&insts[i]) {
			continue
		}
		wg.Add(1)
		go func(inst *aws.InstanceInfo) {
			defer wg.Done()
			flushTaskBeforeTerminate(ctx, run, inst, lw)
		}(&insts[i])
	}
	wg.Wait()
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
