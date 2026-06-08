package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
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
