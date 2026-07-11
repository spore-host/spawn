package cmd

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

func runAutoscaleUpdate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	changed := false
	if autoscaleNewDesired >= 0 {
		group.DesiredCapacity = autoscaleNewDesired
		changed = true
	}
	if autoscaleNewMin >= 0 {
		group.MinCapacity = autoscaleNewMin
		changed = true
	}
	if autoscaleNewMax >= 0 {
		group.MaxCapacity = autoscaleNewMax
		changed = true
	}

	if !changed {
		return fmt.Errorf("no changes specified")
	}

	// Validate
	if group.MinCapacity > group.DesiredCapacity {
		return fmt.Errorf("min-capacity cannot exceed desired-capacity")
	}
	if group.MaxCapacity < group.DesiredCapacity {
		return fmt.Errorf("max-capacity cannot be less than desired-capacity")
	}

	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Updated group %s\n", groupName)
	fmt.Printf("New capacity: desired=%d, min=%d, max=%d\n",
		group.DesiredCapacity, group.MinCapacity, group.MaxCapacity)

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscalePause(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	group.Status = "paused"
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Paused group %s (instances preserved)\n", groupName)
	return nil
}

func runAutoscaleResume(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	group.Status = "active"
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Resumed group %s\n", groupName)

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	}

	return nil
}

func runAutoscaleTerminate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	if !confirmYes(autoscaleTerminateYes, fmt.Sprintf("Terminate auto-scaling group %q and all its instances? This cannot be undone.", groupName)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Terminate all instances
	cfg, _ := config.LoadDefaultConfig(ctx)
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:spawn:autoscale-group"),
				Values: []string{group.AutoScaleGroupID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("describe instances: %w", err)
	}

	instanceIDs := make([]string, 0)
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil {
				instanceIDs = append(instanceIDs, aws.ToString(instance.InstanceId))
			}
		}
	}

	if len(instanceIDs) > 0 {
		_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: instanceIDs,
		})
		if err != nil {
			return fmt.Errorf("terminate instances: %w", err)
		}
		fmt.Printf("Terminated %d instances\n", len(instanceIDs))
	}

	// Delete group
	if err := as.DeleteGroup(ctx, group.AutoScaleGroupID); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}

	fmt.Printf("Terminated group %s\n", groupName)
	return nil
}
