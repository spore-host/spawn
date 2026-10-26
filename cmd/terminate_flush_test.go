package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// callLog records SSM sends and terminate calls in the order they happened.
type callLog struct {
	mu     sync.Mutex
	events []string
	ssmErr error
	ssmRes *aws.SSMRunResult
}

func (c *callLog) ssm(_ context.Context, region, id, command string, timeout time.Duration) (*aws.SSMRunResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "ssm:"+id+"|"+command)
	if timeout <= 0 {
		c.events = append(c.events, "unbounded-timeout")
	}
	if c.ssmErr != nil {
		return nil, c.ssmErr
	}
	if c.ssmRes != nil {
		return c.ssmRes, nil
	}
	return &aws.SSMRunResult{Status: "Success"}, nil
}

func (c *callLog) terminate(_ context.Context, region, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "terminate:"+id)
	return nil
}

func taskInst(id, state string) aws.InstanceInfo {
	return aws.InstanceInfo{InstanceID: id, Region: "us-west-2", State: state, Name: "t-" + id,
		Tags: map[string]string{"spawn:managed": "true", "spawn:task-id": "t-" + id}}
}

// isFlushInvocation reports whether an SSM command runs the flush hook with
// the user_terminated reason — the invocation, not just a mention of the path.
func isFlushInvocation(command string) bool {
	want := `"$H" ` + string(taskproto.ExitUserTerminated)
	return strings.Contains(command, "H="+taskproto.FlushScriptPath()+"\n") &&
		strings.Contains(command, "exec timeout ") && strings.HasSuffix(command, want)
}

// spawn#802: `spawn terminate` on a running task instance must run the task
// flush hook (user_terminated) BEFORE TerminateInstances, so command.log and
// completion.json survive the way they do on a TTL kill.
func TestTerminateWithFlush_FlushesTaskBeforeTerminating(t *testing.T) {
	c := &callLog{}
	inst := taskInst("i-1", "running")
	if err := terminateWithFlush(context.Background(), c.ssm, c.terminate, &inst, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != 2 {
		t.Fatalf("want [flush, terminate], got %q", c.events)
	}
	if !strings.HasPrefix(c.events[0], "ssm:i-1|") || !isFlushInvocation(strings.TrimPrefix(c.events[0], "ssm:i-1|")) {
		t.Errorf("first call must be the flush hook over SSM, got %q", c.events[0])
	}
	if c.events[1] != "terminate:i-1" {
		t.Errorf("terminate must come after the flush, got %q", c.events)
	}
}

// The flush protects diagnostics; it must never protect the instance from
// being terminated. A failed flush still terminates.
func TestTerminateWithFlush_FlushFailureStillTerminates(t *testing.T) {
	for name, c := range map[string]*callLog{
		"ssm error":    {ssmErr: errors.New("InvalidInstanceId: not connected")},
		"hook refused": {ssmRes: &aws.SSMRunResult{Status: "Failed", ResponseCode: 2}},
	} {
		inst := taskInst("i-2", "running")
		var warn bytes.Buffer
		if err := terminateWithFlush(context.Background(), c.ssm, c.terminate, &inst, &warn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.events[len(c.events)-1] != "terminate:i-2" {
			t.Errorf("%s: instance must still be terminated, got %q", name, c.events)
		}
		if !strings.Contains(warn.String(), "terminating anyway") {
			t.Errorf("%s: the failed flush must be reported, got %q", name, warn.String())
		}
	}
}

// Non-task and non-running instances have no hook / no agent: no SSM call.
func TestTerminateWithFlush_SkipsNonTaskAndStopped(t *testing.T) {
	plain := aws.InstanceInfo{InstanceID: "i-3", Region: "us-west-2", State: "running", Tags: map[string]string{"spawn:managed": "true"}}
	stopped := taskInst("i-4", "stopped")
	for _, inst := range []aws.InstanceInfo{plain, stopped} {
		c := &callLog{}
		if err := terminateWithFlush(context.Background(), c.ssm, c.terminate, &inst, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if len(c.events) != 1 || c.events[0] != "terminate:"+inst.InstanceID {
			t.Errorf("%s: want only a terminate, got %q", inst.InstanceID, c.events)
		}
	}
}

// Job array: every running task member is flushed, and every flush happens
// before the first TerminateInstances call.
func TestTerminateArrayMembers_FlushesEveryTaskBeforeAnyTerminate(t *testing.T) {
	c := &callLog{}
	insts := []aws.InstanceInfo{taskInst("i-a", "running"), taskInst("i-b", "running"), taskInst("i-c", "terminated")}
	ok, failed := terminateArrayMembers(context.Background(), insts, c.ssm, c.terminate, func(*aws.InstanceInfo) {}, &bytes.Buffer{})
	if ok != 2 || len(failed) != 0 {
		t.Fatalf("ok=%d failed=%v", ok, failed)
	}
	firstTerminate, flushes := -1, 0
	for i, e := range c.events {
		if strings.HasPrefix(e, "terminate:") && firstTerminate < 0 {
			firstTerminate = i
		}
		if strings.HasPrefix(e, "ssm:") {
			if firstTerminate >= 0 {
				t.Errorf("flush %q came after a terminate: %q", e, c.events)
			}
			flushes++
		}
	}
	if flushes != 2 {
		t.Errorf("want 2 flushes (the running task members), got %d: %q", flushes, c.events)
	}
}

// The command applies spored's hook trust check before exec'ing as root, and
// is bounded on the box, not only by the SSM wait.
func TestTaskFlushCommand_TrustCheckAndBound(t *testing.T) {
	cmd := taskFlushCommand()
	for _, want := range []string{`[ -L "$H" ]`, `[ ! -O "$H" ]`, `-perm /022`, "exec timeout 45 "} {
		if !strings.Contains(cmd, want) {
			t.Errorf("flush command missing %q:\n%s", want, cmd)
		}
	}
	if terminateFlushTimeout <= time.Duration(taskFlushHookSeconds)*time.Second {
		t.Errorf("SSM wait %s must exceed the on-box hook cap %ds", terminateFlushTimeout, taskFlushHookSeconds)
	}
}

// spawn#1312: `spawn terminate`'s flush stages the task's declared outputs as
// a lifecycle kill does, granting the hook its declared budget as $2 up to
// flush_outputs_seconds' default (60 s), not its 300 s maximum, because the
// instance bills for the whole wait. The SSM wait covers the longest on-box
// bound that grant gives the hook, and the user is told how long it may be.
func TestTerminateFlushGrantsOutputsAndWaitsForThem(t *testing.T) {
	var sent string
	var wait time.Duration
	run := func(_ context.Context, _, _, command string, timeout time.Duration) (*aws.SSMRunResult, error) {
		sent, wait = command, timeout
		return &aws.SSMRunResult{Status: "Success"}, nil
	}
	inst := taskInst("i-1", "running")
	var out bytes.Buffer
	flushTaskBeforeTerminate(context.Background(), run, &inst, &out)

	if !strings.Contains(sent, `"$H" `+string(taskproto.ExitUserTerminated)+` "$G"`) {
		t.Errorf("the flush grants the hook no output seconds:\n%s", sent)
	}
	if !strings.Contains(sent, fmt.Sprintf(`[ "$G" -le %d ] || G=%d`, taskproto.DefaultFlushOutputsSeconds, taskproto.DefaultFlushOutputsSeconds)) {
		t.Errorf("the flush does not cap the grant at the %d s default:\n%s", taskproto.DefaultFlushOutputsSeconds, sent)
	}
	longest := time.Duration(taskproto.FlushHookSeconds+taskproto.DefaultFlushOutputsSeconds+taskproto.FlushOutputsSlackSeconds) * time.Second
	if wait <= longest {
		t.Errorf("SSM wait %v does not cover a hook granted %d s (on-box bound %v)", wait, taskproto.DefaultFlushOutputsSeconds, longest)
	}
	if wait > longest+time.Minute {
		t.Errorf("SSM wait %v is far past the on-box bound %v: a silent agent holds the user, and the bill, for all of it", wait, longest)
	}
	if want := "Waiting up to " + wait.String(); !strings.Contains(out.String(), want) {
		t.Errorf("the user is not told how long the flush may take (want %q):\n%s", want, out.String())
	}
}
