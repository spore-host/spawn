package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// instanceWaker is the slice of *aws.Client that waking an instance for
// connect needs. An interface so the wake contract is testable without EC2.
type instanceWaker interface {
	StartInstance(ctx context.Context, region, instanceID string) error
	WaitForRunning(ctx context.Context, region, instanceID string, timeout time.Duration) error
	WaitForStopped(ctx context.Context, region, instanceID string, timeout time.Duration) error
}

// wakeTimeout bounds each wait. A hibernated instance restores RAM from its
// root volume on resume, so it can take longer than a cold start.
const wakeTimeout = 5 * time.Minute

// isHibernated reports whether a stopped/stopping instance got there by
// hibernating. EC2 has no "hibernated" state: hibernation is StopInstances with
// Hibernate=true, the state is "stopped", and StartInstances resumes it. The
// distinguishing fact is EC2's StateReason ("Client.UserInitiatedHibernate:
// User initiated hibernate"); spawn's own spawn:last-stop-reason tag is the
// fallback for a describe that carried no reason.
func isHibernated(inst *aws.InstanceInfo) bool {
	if strings.Contains(strings.ToLower(inst.StateReason), "hibernate") {
		return true
	}
	return inst.Tags["spawn:last-stop-reason"] == "user-hibernated"
}

// wakeForConnect is the one auto-start every `spawn connect` path shares
// (#57): the SSH/SSM path, the one-shot command path and the DCV app path. A
// user who types connect expects it to work whatever the instance's state, so
// a stopped OR hibernated instance is started (resumed), and a "stopping" one
// is waited out and then started — EC2 refuses to start an instance mid-stop.
// Any other state is returned untouched for the caller to judge.
//
// noStart is --no-start, the opt-out. refresh re-reads the instance once it is
// running, for the public IP and tags a stop/start reassigns.
func wakeForConnect(ctx context.Context, c instanceWaker, inst *aws.InstanceInfo, noStart bool, identifier string, w io.Writer,
	refresh func(ctx context.Context, instanceID string) (*aws.InstanceInfo, error)) (*aws.InstanceInfo, error) {
	if inst.State != "stopped" && inst.State != "stopping" {
		return inst, nil
	}
	hibernated := isHibernated(inst)
	label := inst.State
	verb := "starting"
	if hibernated {
		label = "hibernated"
		if inst.State == "stopping" {
			label = "hibernating"
		}
		verb = "resuming"
	}
	if noStart {
		return nil, fmt.Errorf("instance is %s and --no-start was given — start it with: spawn start %s", label, identifier)
	}

	if inst.State == "stopping" {
		_, _ = fmt.Fprintf(w, "Instance is %s — waiting for it to finish before %s it...", label, verb)
		if err := c.WaitForStopped(ctx, inst.Region, inst.InstanceID, wakeTimeout); err != nil {
			_, _ = fmt.Fprintf(w, "\n")
			return nil, fmt.Errorf("instance did not finish stopping: %w", err)
		}
		_, _ = fmt.Fprintf(w, " stopped\n")
	} else {
		_, _ = fmt.Fprintf(w, "Instance is %s — %s it...\n", label, verb)
	}

	if err := c.StartInstance(ctx, inst.Region, inst.InstanceID); err != nil {
		return nil, fmt.Errorf("start instance: %w", err)
	}
	// WaitForRunning rather than a hand-rolled loop (#752). It wraps the SDK's
	// instance-running waiter, absorbs the #78 NotFound window, and returns the
	// moment the state flips.
	_, _ = fmt.Fprintf(w, "Waiting for instance to reach running state...")
	if err := c.WaitForRunning(ctx, inst.Region, inst.InstanceID, wakeTimeout); err != nil {
		_, _ = fmt.Fprintf(w, "\n")
		return nil, fmt.Errorf("instance did not reach running state: %w", err)
	}
	refreshed, err := refresh(ctx, inst.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("re-read instance after start: %w", err)
	}
	_, _ = fmt.Fprintf(w, " running\n\n")
	return refreshed, nil
}
