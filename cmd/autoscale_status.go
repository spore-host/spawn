package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

func runAutoscaleStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// If group name specified, show just that group
	if len(args) > 0 {
		group, err := as.GetGroupByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("get group: %w", err)
		}

		printGroupStatus(group)
		return nil
	}

	// Otherwise list all active groups (same view as `autoscale list`).
	return listAutoscaleGroups(ctx, as)
}

func runAutoscaleList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	return listAutoscaleGroups(ctx, as)
}

// listAutoscaleGroups prints a table of all active autoscale groups.
func listAutoscaleGroups(ctx context.Context, as *autoscaler.AutoScaler) error {
	groups, err := as.ListActiveGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}

	if len(groups) == 0 {
		fmt.Println("No active autoscale groups")
		return nil
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "NAME\tSTATUS\tDESIRED\tMIN\tMAX\tCREATED")
	for _, group := range groups {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n",
			group.GroupName,
			group.Status,
			group.DesiredCapacity,
			group.MinCapacity,
			group.MaxCapacity,
			group.CreatedAt.Format("2006-01-02 15:04"),
		)
	}
	return w.Flush()
}

func runAutoscaleHealth(cmd *cobra.Command, args []string) error {
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

	// Get instances
	cfg, _ := config.LoadDefaultConfig(ctx)
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:spawn:autoscale-group"),
				Values: []string{group.AutoScaleGroupID},
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

	if len(instanceIDs) == 0 {
		fmt.Println("No instances found")
		return nil
	}

	// Check health
	dynamoClient := dynamodb.NewFromConfig(cfg)
	healthChecker := autoscaler.NewHealthChecker(ec2Client, dynamoClient, "spawn-hybrid-registry")

	health, err := healthChecker.CheckInstances(ctx, group.JobArrayID, instanceIDs)
	if err != nil {
		return fmt.Errorf("check health: %w", err)
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "INSTANCE\tSTATE\tHEARTBEAT\tSPOT\tHEALTH")
	for _, h := range health {
		spotStr := "no"
		if h.SpotInterruption {
			spotStr = "yes"
		}

		heartbeatStr := "N/A"
		if h.HeartbeatAge > 0 {
			heartbeatStr = fmt.Sprintf("%v ago", h.HeartbeatAge.Round(time.Second))
		}

		healthStr := "✓ healthy"
		if !h.Healthy {
			healthStr = fmt.Sprintf("✗ %s", h.Reason)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			h.InstanceID, h.EC2State, heartbeatStr, spotStr, healthStr)
	}
	return w.Flush()
}

func printGroupStatus(group *autoscaler.AutoScaleGroup) {
	fmt.Printf("Group: %s (%s)\n", group.AutoScaleGroupID, group.GroupName)
	fmt.Printf("Job Array ID: %s\n", group.JobArrayID)
	fmt.Printf("Status: %s\n", group.Status)
	fmt.Printf("Capacity: desired=%d, min=%d, max=%d\n",
		group.DesiredCapacity, group.MinCapacity, group.MaxCapacity)
	fmt.Printf("Created: %s\n", group.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("Updated: %s\n", group.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
	if !group.LastScaleEvent.IsZero() {
		fmt.Printf("Last Scale Event: %s\n", group.LastScaleEvent.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Printf("Health Check Interval: %v\n", group.HealthCheckInterval)

	// Display scaling policy info
	if group.ScalingPolicy != nil {
		fmt.Printf("\nScaling Policy: %s\n", group.ScalingPolicy.PolicyType)

		// Display queues (multi-queue or single queue)
		if len(group.ScalingPolicy.Queues) > 1 {
			fmt.Printf("  Queues: %d (multi-queue)\n", len(group.ScalingPolicy.Queues))
			for i, q := range group.ScalingPolicy.Queues {
				weight := q.Weight
				if weight == 0 {
					weight = 1.0
				}
				fmt.Printf("    %d. %s (weight: %.1f)\n", i+1, q.QueueURL, weight)
			}
		} else if len(group.ScalingPolicy.Queues) == 1 {
			fmt.Printf("  Queue: %s\n", group.ScalingPolicy.Queues[0].QueueURL)
		} else if group.ScalingPolicy.QueueURL != "" {
			// Backward compat
			fmt.Printf("  Queue: %s\n", group.ScalingPolicy.QueueURL)
		}

		fmt.Printf("  Target: %d messages/instance\n", group.ScalingPolicy.TargetMessagesPerInstance)
		fmt.Printf("  Cooldowns: up=%ds, down=%ds\n",
			group.ScalingPolicy.ScaleUpCooldownSeconds,
			group.ScalingPolicy.ScaleDownCooldownSeconds)

		if group.ScalingState != nil {
			fmt.Printf("\nCurrent State:\n")
			fmt.Printf("  Queue Depth: %d messages\n", group.ScalingState.LastQueueDepth)
			if !group.ScalingState.LastScaleUp.IsZero() {
				fmt.Printf("  Last Scale Up: %s ago\n",
					time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
			}
			if !group.ScalingState.LastScaleDown.IsZero() {
				fmt.Printf("  Last Scale Down: %s ago\n",
					time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
			}
		}
	} else {
		fmt.Println("\nScaling Policy: Manual (no queue-based scaling)")
	}

	// Display metric policy info
	if group.MetricPolicy != nil {
		fmt.Printf("\nMetric Policy: %s\n", group.MetricPolicy.MetricType)
		fmt.Printf("  Metric: %s (%s)\n", group.MetricPolicy.MetricName, group.MetricPolicy.Namespace)
		fmt.Printf("  Statistic: %s\n", group.MetricPolicy.Statistic)
		fmt.Printf("  Target: %.1f\n", group.MetricPolicy.TargetValue)
		fmt.Printf("  Period: %ds\n", group.MetricPolicy.PeriodSeconds)
	}

	// Display schedule info
	if group.ScheduleConfig != nil && len(group.ScheduleConfig.Actions) > 0 {
		fmt.Printf("\nScheduled Actions: %d\n", len(group.ScheduleConfig.Actions))
		for _, action := range group.ScheduleConfig.Actions {
			status := "enabled"
			if !action.Enabled {
				status = "disabled"
			}
			fmt.Printf("  - %s (%s)\n", action.Name, status)
			fmt.Printf("    Schedule: %s\n", action.Schedule)
			fmt.Printf("    Desired: %d", action.DesiredCapacity)
			if action.MinCapacity > 0 || action.MaxCapacity > 0 {
				fmt.Printf(" (min: %d, max: %d)", action.MinCapacity, action.MaxCapacity)
			}
			fmt.Println()
			if action.Timezone != "" && action.Timezone != "UTC" {
				fmt.Printf("    Timezone: %s\n", action.Timezone)
			}
		}
	}
}
