package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// WindowsSecurityGroupPurpose is the spawn:purpose of the security group
// `spawn launch --os windows` creates for an instance, spawn-windows-<name>
// (CreateOrGetWindowsSecurityGroup). The TTL reaper reclaims an unused one
// after an hour rather than the seven days it gives a group a later launch
// may reuse (lambda/ttl-reaper netresources.go, #1623).
const WindowsSecurityGroupPurpose = "windows"

// windowsGroupAPI is the slice of EC2 the Windows group deleter uses.
type windowsGroupAPI interface {
	ec2.DescribeInstancesAPIClient
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// InstanceWindowsSecurityGroups returns the per-instance Windows security
// groups (spawn:managed=true, spawn:purpose=windows) attached to instanceID.
// Read before the instance is terminated: a terminated instance soon stops
// reporting its groups.
func (c *Client) InstanceWindowsSecurityGroups(ctx context.Context, region, instanceID string) ([]string, error) {
	return windowsGroupsOf(ctx, c.regionalEC2(region), instanceID)
}

// DeleteSecurityGroupsAfterTerminate is the named deleter for an instance's
// Windows security groups (#1623). It waits up to wait for instanceID to
// reach terminated, then deletes each group no other live instance uses. A
// group's network interface detaches a little after the instance terminates,
// so DependencyViolation is retried for a short while. It returns the groups
// deleted; groups it leaves (still used by another instance, or still
// attached when it gave up) are left for the TTL reaper and named in the error.
func (c *Client) DeleteSecurityGroupsAfterTerminate(ctx context.Context, region, instanceID string, groupIDs []string, wait time.Duration) ([]string, error) {
	return deleteGroupsAfterTerminate(ctx, c.regionalEC2(region), instanceID, groupIDs, wait, 10*time.Second, sleepCtx)
}

func windowsGroupsOf(ctx context.Context, api windowsGroupAPI, instanceID string) ([]string, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", instanceID, err)
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			for _, g := range inst.SecurityGroups {
				if id := aws.ToString(g.GroupId); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sgs, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: ids})
	if err != nil {
		return nil, fmt.Errorf("describe security groups of %s: %w", instanceID, err)
	}
	var windows []string
	for _, sg := range sgs.SecurityGroups {
		if isWindowsInstanceGroup(sg.Tags) {
			windows = append(windows, aws.ToString(sg.GroupId))
		}
	}
	return windows, nil
}

// isWindowsInstanceGroup reports whether tags mark a group spawn created for
// one Windows instance.
func isWindowsInstanceGroup(tags []types.Tag) bool {
	var managed, purpose string
	for _, t := range tags {
		switch aws.ToString(t.Key) {
		case "spawn:managed":
			managed = aws.ToString(t.Value)
		case "spawn:purpose":
			purpose = aws.ToString(t.Value)
		}
	}
	return managed == "true" && purpose == WindowsSecurityGroupPurpose
}

func deleteGroupsAfterTerminate(ctx context.Context, api windowsGroupAPI, instanceID string, groupIDs []string, wait, poll time.Duration, sleep func(context.Context, time.Duration) error) ([]string, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	waiter := ec2.NewInstanceTerminatedWaiter(api, func(o *ec2.InstanceTerminatedWaiterOptions) {
		o.MinDelay, o.MaxDelay = poll, poll
	})
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}}, wait); err != nil {
		return nil, fmt.Errorf("%s did not reach terminated within %s, so its security group(s) %s were left: %w",
			instanceID, wait, strings.Join(groupIDs, ", "), err)
	}

	var deleted, left []string
	var errs []error
	for _, id := range groupIDs {
		if other, err := otherInstanceUsing(ctx, api, id, instanceID); err != nil {
			left, errs = append(left, id), append(errs, err)
			continue
		} else if other != "" {
			left, errs = append(left, id), append(errs, fmt.Errorf("%s is also used by %s", id, other))
			continue
		}
		var err error
		for attempt := 0; attempt < 12; attempt++ {
			_, err = api.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
			if err == nil || !strings.Contains(err.Error(), "DependencyViolation") {
				break
			}
			if serr := sleep(ctx, poll); serr != nil {
				err = serr
				break
			}
		}
		switch {
		case err == nil, strings.Contains(fmt.Sprint(err), "InvalidGroup.NotFound"):
			deleted = append(deleted, id)
		default:
			left, errs = append(left, id), append(errs, fmt.Errorf("delete %s: %w", id, err))
		}
	}
	if len(left) > 0 {
		return deleted, fmt.Errorf("left security group(s) %s: %w", strings.Join(left, ", "), errors.Join(errs...))
	}
	return deleted, nil
}

// otherInstanceUsing returns a live instance other than self holding group, or "".
func otherInstanceUsing(ctx context.Context, api windowsGroupAPI, group, self string) (string, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("instance.group-id"), Values: []string{group}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "shutting-down", "stopping", "stopped"}},
	}})
	if err != nil {
		return "", fmt.Errorf("check who else uses %s: %w", group, err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if id := aws.ToString(inst.InstanceId); id != "" && id != self {
				return id, nil
			}
		}
	}
	return "", nil
}
