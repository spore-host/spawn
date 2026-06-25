package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ErrSSMUnreachable signals that SSM registration is structurally impossible for
// an instance (e.g. no IAM instance profile attached, so the agent has no
// permission to register) — i.e. the instance is "dead" for SSM, not merely
// slow. Callers can fail fast on this instead of waiting out the full timeout.
var ErrSSMUnreachable = fmt.Errorf("instance cannot register with SSM")

// SSMRunResult is the outcome of a one-shot SSM RunCommand invocation.
type SSMRunResult struct {
	Status string // SSM command status, e.g. "Success", "Failed"
	Stdout string
	Stderr string
	// ResponseCode is the remote command's exit code (e.g. spored status'
	// 0/1/2/3 for --check-complete). Meaningful once the invocation reaches a
	// terminal Status; 0 on a clean success.
	ResponseCode int32
}

// RunPowerShell runs a PowerShell command on a Windows instance via SSM
// (AWS-RunPowerShellScript) and waits for it to finish. This is the Windows
// equivalent of `connect -- <cmd>` (one-shot exec), since Windows has no SSH
// command path in Phase 1. The instance must have the SSM agent online (the
// stock Windows AMIs do) and an instance profile with SSM core permissions.
func (c *Client) RunPowerShell(ctx context.Context, region, instanceID, command string, timeout time.Duration) (*SSMRunResult, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ssmClient := ssm.NewFromConfig(cfg)

	send, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunPowerShellScript"),
		Parameters:   map[string][]string{"commands": {command}},
	})
	if err != nil {
		return nil, fmt.Errorf("ssm send-command: %w", err)
	}
	commandID := aws.ToString(send.Command.CommandId)

	// Poll for completion. SSM invocations are eventually consistent right after
	// SendCommand, so tolerate InvocationDoesNotExist briefly.
	deadline := time.Now().Add(timeout)
	for {
		out, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err == nil {
			status := string(out.Status)
			switch status {
			case "Success", "Failed", "Cancelled", "TimedOut":
				return &SSMRunResult{
					Status:       status,
					Stdout:       aws.ToString(out.StandardOutputContent),
					Stderr:       aws.ToString(out.StandardErrorContent),
					ResponseCode: out.ResponseCode,
				}, nil
			}
			// Pending / InProgress / Delayed → keep polling.
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ssm command %s did not complete within %s", commandID, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// RunShellScript runs a shell command on a Linux instance via SSM
// (AWS-RunShellScript) and waits for it to finish. This is the Linux sibling of
// RunPowerShell — used by `spawn connect` to inject an authorized key over SSM
// when the caller doesn't hold the instance's launch key (e.g. instances
// launched headlessly by lagotto, which has no SSH key on disk). The instance
// must have the SSM agent online and an instance profile with SSM core
// permissions (the spored role attaches AmazonSSMManagedInstanceCore).
func (c *Client) RunShellScript(ctx context.Context, region, instanceID, command string, timeout time.Duration) (*SSMRunResult, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ssmClient := ssm.NewFromConfig(cfg)

	send, err := ssmClient.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters:   map[string][]string{"commands": {command}},
	})
	if err != nil {
		return nil, fmt.Errorf("ssm send-command: %w", err)
	}
	commandID := aws.ToString(send.Command.CommandId)

	// Poll for completion. SSM invocations are eventually consistent right after
	// SendCommand, so tolerate the brief window before the invocation exists.
	deadline := time.Now().Add(timeout)
	for {
		out, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err == nil {
			status := string(out.Status)
			switch status {
			case "Success", "Failed", "Cancelled", "TimedOut":
				return &SSMRunResult{
					Status:       status,
					Stdout:       aws.ToString(out.StandardOutputContent),
					Stderr:       aws.ToString(out.StandardErrorContent),
					ResponseCode: out.ResponseCode,
				}, nil
			}
			// Pending / InProgress / Delayed → keep polling.
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ssm command %s did not complete within %s", commandID, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// instanceHasInstanceProfile reports whether the instance currently has an IAM
// instance profile attached. SSM registration is impossible without one (the
// agent has no credentials to call ssm:UpdateInstanceInformation), so this is
// the cheap, decisive "can this ever come Online?" precondition — it
// distinguishes a structurally-dead instance from one that's merely booting
// slowly. A profile attached after launch can take a minute to appear in
// DescribeInstances, so callers should treat a single "false" as provisional
// early on, not proof of death.
func (c *Client) instanceHasInstanceProfile(ctx context.Context, region, instanceID string) (bool, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return false, err
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if aws.ToString(inst.InstanceId) != instanceID {
				continue
			}
			return inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil, nil
		}
	}
	return false, fmt.Errorf("instance %s not found", instanceID)
}

// WaitForSSMOnline polls SSM until the instance is registered with PingStatus
// "Online" (or the timeout elapses). This is the strongest "Windows finished
// first boot" signal: the SSM agent registers only after EC2Launch has run the
// user-data (which installs/starts spored + ensures the SSM agent on imported
// AMIs, #95). Used by the warm-AMI build (#98) to know the seed is fully baked
// before imaging it. Polls every 30s, mirroring WaitForPasswordData.
//
// It distinguishes "dead" from "slow": up front, and again if registration
// hasn't happened partway through, it checks that an IAM instance profile is
// attached. With no profile the agent can never register, so it returns
// ErrSSMUnreachable immediately rather than burning the whole timeout on a
// doomed wait. An optional onProgress callback is invoked each poll with the
// elapsed time so a long-but-live wait is visibly progressing.
func (c *Client) WaitForSSMOnline(ctx context.Context, region, instanceID string, timeout time.Duration) error {
	return c.WaitForSSMOnlineProgress(ctx, region, instanceID, timeout, nil)
}

// WaitForSSMOnlineProgress is WaitForSSMOnline with a per-poll progress callback
// (elapsed since the wait began). onProgress may be nil.
func (c *Client) WaitForSSMOnlineProgress(ctx context.Context, region, instanceID string, timeout time.Duration, onProgress func(elapsed time.Duration)) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ssmClient := ssm.NewFromConfig(cfg)

	isOnline := func() bool {
		out, err := ssmClient.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{
				{Key: aws.String("InstanceIds"), Values: []string{instanceID}},
			},
		})
		if err != nil {
			return false
		}
		for _, info := range out.InstanceInformationList {
			if aws.ToString(info.InstanceId) == instanceID && info.PingStatus == ssmtypes.PingStatusOnline {
				return true
			}
		}
		return false
	}

	// Distinguish "dead" from "slow", but only when it matters: if the instance
	// is NOT yet Online, ask whether it ever *can* be. The decisive structural
	// signal is the IAM instance profile — without one the agent has no
	// credentials to register, so PingStatus can never reach Online. We only
	// consult it when registration is still pending (an already-Online instance
	// makes the question moot), so a transient describe gap on a healthy instance
	// can't produce a false "dead".
	deadIfNoProfile := func(elapsed time.Duration) error {
		if hasProfile, err := c.instanceHasInstanceProfile(ctx, region, instanceID); err == nil && !hasProfile {
			return fmt.Errorf("%w: instance %s has no IAM instance profile, so the SSM agent cannot register (PingStatus will never reach Online) — failing fast after %s instead of waiting out the timeout", ErrSSMUnreachable, instanceID, elapsed.Round(time.Second))
		}
		return nil
	}

	start := time.Now()
	deadline := start.Add(timeout)

	// Up-front: if not already Online, a missing profile means it's dead — bail now.
	if !isOnline() {
		if err := deadIfNoProfile(0); err != nil {
			return err
		}
	} else {
		return nil
	}

	// Re-check the structural precondition once a third of the way in (catches a
	// profile that was detached or never actually associated) — turns a silent
	// long hang into a fast, clear failure.
	structuralRecheckAt := start.Add(timeout / 3)
	recheckDone := false
	for {
		now := time.Now()
		if isOnline() {
			return nil
		}
		if !recheckDone && now.After(structuralRecheckAt) {
			recheckDone = true
			if err := deadIfNoProfile(now.Sub(start)); err != nil {
				return err
			}
		}
		if onProgress != nil {
			onProgress(now.Sub(start))
		}
		if now.After(deadline) {
			return fmt.Errorf("instance %s did not register with SSM (PingStatus=Online) within %s", instanceID, timeout)
		}
		// Poll every 30s, but never sleep past the deadline (so short timeouts
		// don't overshoot by a whole interval).
		interval := 30 * time.Second
		if rem := time.Until(deadline); rem < interval {
			interval = rem
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
