package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMRunResult is the outcome of a one-shot SSM RunCommand invocation.
type SSMRunResult struct {
	Status string // SSM command status, e.g. "Success", "Failed"
	Stdout string
	Stderr string
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
					Status: status,
					Stdout: aws.ToString(out.StandardOutputContent),
					Stderr: aws.ToString(out.StandardErrorContent),
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

// WaitForSSMOnline polls SSM until the instance is registered with PingStatus
// "Online" (or the timeout elapses). This is the strongest "Windows finished
// first boot" signal: the SSM agent registers only after EC2Launch has run the
// user-data (which installs/starts spored + ensures the SSM agent on imported
// AMIs, #95). Used by the warm-AMI build (#98) to know the seed is fully baked
// before imaging it. Polls every 30s, mirroring WaitForPasswordData.
func (c *Client) WaitForSSMOnline(ctx context.Context, region, instanceID string, timeout time.Duration) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ssmClient := ssm.NewFromConfig(cfg)

	deadline := time.Now().Add(timeout)
	for {
		out, err := ssmClient.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{
				{Key: aws.String("InstanceIds"), Values: []string{instanceID}},
			},
		})
		if err == nil {
			for _, info := range out.InstanceInformationList {
				if aws.ToString(info.InstanceId) == instanceID && info.PingStatus == ssmtypes.PingStatusOnline {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
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
