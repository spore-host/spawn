package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

// HeartbeatTag is the tag spored stamps with the current time on every monitor
// tick, throttled to once a minute (pkg/agent writeHeartbeatTag, #497). Its
// presence on an instance is direct evidence that spored ran there: only spored
// writes it, and EC2 tags are never inherited from an AMI.
const HeartbeatTag = "spawn:last-heartbeat"

// IAMBaselineURL is where the caller's least-privilege policy is documented.
const IAMBaselineURL = "https://spore.host/reference/iam-permissions"

// AccessDeniedError reports that the caller's credentials lack an action. It is
// a statement about the CALLER, never about the instance: a launch whose check
// was refused has learned nothing about whether spored came up (#1126).
type AccessDeniedError struct {
	Action string // the IAM action that was refused, e.g. "ssm:DescribeInstanceInformation"
	Err    error
}

func (e *AccessDeniedError) Error() string {
	return fmt.Sprintf("your credentials lack %s (see the IAM baseline, %s): %v", e.Action, IAMBaselineURL, e.Err)
}

func (e *AccessDeniedError) Unwrap() error { return e.Err }

// IsAccessDenied reports whether err is an authorization failure from AWS:
// AccessDenied/AccessDeniedException (most services, SSM included) or
// UnauthorizedOperation (EC2). An AccessDeniedError counts too.
func IsAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	var ade *AccessDeniedError
	if errors.As(err, &ade) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
			return true
		}
	}
	return false
}

// denied wraps err as an AccessDeniedError naming action when it is an
// authorization failure, and returns nil otherwise.
func denied(action string, err error) error {
	if !IsAccessDenied(err) {
		return nil
	}
	var ade *AccessDeniedError
	if errors.As(err, &ade) {
		return ade
	}
	return &AccessDeniedError{Action: action, Err: err}
}

// SporedLiveness is what one DescribeInstances says about whether spored is up.
type SporedLiveness struct {
	// Heartbeat is the spawn:last-heartbeat value, or "" if spored has not
	// written one yet.
	Heartbeat string
	// HasInstanceProfile is false when no profile is attached. spored then has
	// no credentials to tag with, and the SSM agent none to register with.
	HasInstanceProfile bool
	// State is the EC2 instance state name, e.g. "running".
	State string
}

// DescribeSporedLiveness reads the instance's heartbeat tag, profile and state
// in one ec2:DescribeInstances, the call the IAM baseline already grants on "*".
// An authorization failure is returned as an *AccessDeniedError naming
// ec2:DescribeInstances.
func (c *Client) DescribeSporedLiveness(ctx context.Context, region, instanceID string) (SporedLiveness, error) {
	inst, err := c.describeInstance(ctx, c.regionalEC2(region), instanceID)
	if err != nil {
		if d := denied("ec2:DescribeInstances", err); d != nil {
			return SporedLiveness{}, d
		}
		return SporedLiveness{}, err
	}
	l := SporedLiveness{
		HasInstanceProfile: inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil,
	}
	if inst.State != nil {
		l.State = string(inst.State.Name)
	}
	for _, t := range inst.Tags {
		if aws.ToString(t.Key) == HeartbeatTag {
			l.Heartbeat = aws.ToString(t.Value)
		}
	}
	return l, nil
}

// SSMOnline asks SSM once whether the instance's agent is registered with
// PingStatus Online. It does not wait. An authorization failure is returned as
// an *AccessDeniedError naming ssm:DescribeInstanceInformation, so a caller can
// tell "the agent is not up" from "you may not ask" (#1126).
func (c *Client) SSMOnline(ctx context.Context, region, instanceID string) (bool, error) {
	out, err := ssm.NewFromConfig(c.regionalConfig(region)).DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{
			{Key: aws.String("InstanceIds"), Values: []string{instanceID}},
		},
	})
	if err != nil {
		if d := denied("ssm:DescribeInstanceInformation", err); d != nil {
			return false, d
		}
		return false, err
	}
	for _, info := range out.InstanceInformationList {
		if aws.ToString(info.InstanceId) == instanceID && info.PingStatus == ssmtypes.PingStatusOnline {
			return true, nil
		}
	}
	return false, nil
}
