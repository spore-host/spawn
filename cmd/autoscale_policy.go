package cmd

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

func runAutoscaleSetPolicy(cmd *cobra.Command, args []string) error {
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

	// Handle --none flag (remove policy)
	if removePolicyFlag {
		group.ScalingPolicy = nil
		fmt.Printf("Removed scaling policy from %s (reverted to manual mode)\n", groupName)
	} else {
		// Validate and set policy
		if scalingPolicy == "" {
			return fmt.Errorf("--scaling-policy required (or use --none to remove)")
		}
		if scalingPolicy != "queue-depth" {
			return fmt.Errorf("invalid scaling policy: %s (only 'queue-depth' supported)", scalingPolicy)
		}

		// Build queue configuration (multi-queue or single queue)
		queues, err := buildQueueConfig(queueURLs, queueWeights, queueURL)
		if err != nil {
			return err
		}

		group.ScalingPolicy = &autoscaler.ScalingPolicy{
			PolicyType:                scalingPolicy,
			Queues:                    queues,
			TargetMessagesPerInstance: targetMessagesPerInstance,
			ScaleUpCooldownSeconds:    scaleUpCooldown,
			ScaleDownCooldownSeconds:  scaleDownCooldown,
		}

		if len(queues) == 1 {
			fmt.Printf("Updated scaling policy for %s (single queue)\n", groupName)
		} else {
			fmt.Printf("Updated scaling policy for %s (%d queues)\n", groupName, len(queues))
		}
	}

	// Update group in DynamoDB
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscaleScalingActivity(cmd *cobra.Command, args []string) error {
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

	// Display scaling state
	if group.ScalingPolicy == nil {
		fmt.Println("No scaling policy configured (manual mode)")
		return nil
	}

	fmt.Printf("Scaling Policy: %s\n", group.ScalingPolicy.PolicyType)
	fmt.Printf("Queue: %s\n", group.ScalingPolicy.QueueURL)
	fmt.Printf("Target: %d messages/instance\n", group.ScalingPolicy.TargetMessagesPerInstance)
	fmt.Println()

	if group.ScalingState == nil {
		fmt.Println("No scaling activity yet")
		return nil
	}

	fmt.Printf("Last Queue Depth: %d messages\n", group.ScalingState.LastQueueDepth)
	fmt.Printf("Last Calculated Capacity: %d instances\n", group.ScalingState.LastCalculatedCapacity)

	if !group.ScalingState.LastScaleUp.IsZero() {
		fmt.Printf("Last Scale Up: %s (%s ago)\n",
			group.ScalingState.LastScaleUp.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
	}
	if !group.ScalingState.LastScaleDown.IsZero() {
		fmt.Printf("Last Scale Down: %s (%s ago)\n",
			group.ScalingState.LastScaleDown.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
	}

	return nil
}

func runAutoscaleSetMetricPolicy(cmd *cobra.Command, args []string) error {
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

	// Handle --none flag (remove policy)
	if removePolicyFlag {
		group.MetricPolicy = nil
		fmt.Printf("Removed metric policy from %s\n", groupName)
	} else {
		// Validate and set policy
		if metricPolicy == "" {
			return fmt.Errorf("--metric-policy required (or use --none to remove)")
		}

		policy, err := buildMetricPolicy(metricPolicy, targetValue, metricName, metricNamespace, metricStatistic, periodSeconds)
		if err != nil {
			return err
		}

		group.MetricPolicy = policy
		fmt.Printf("Updated metric policy for %s\n", groupName)
	}

	// Update group in DynamoDB
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscaleMetricActivity(cmd *cobra.Command, args []string) error {
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

	// Display metric policy
	if group.MetricPolicy == nil {
		fmt.Println("No metric policy configured")
		return nil
	}

	fmt.Printf("Metric Policy: %s\n", group.MetricPolicy.MetricType)
	fmt.Printf("Metric: %s (%s)\n", group.MetricPolicy.MetricName, group.MetricPolicy.Namespace)
	fmt.Printf("Statistic: %s\n", group.MetricPolicy.Statistic)
	fmt.Printf("Target: %.1f\n", group.MetricPolicy.TargetValue)
	fmt.Printf("Period: %ds\n", group.MetricPolicy.PeriodSeconds)
	fmt.Println()

	if group.ScalingState == nil {
		fmt.Println("No scaling activity yet")
		return nil
	}

	fmt.Printf("Last Calculated Capacity: %d instances\n", group.ScalingState.LastCalculatedCapacity)

	if !group.ScalingState.LastScaleUp.IsZero() {
		fmt.Printf("Last Scale Up: %s (%s ago)\n",
			group.ScalingState.LastScaleUp.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
	}
	if !group.ScalingState.LastScaleDown.IsZero() {
		fmt.Printf("Last Scale Down: %s (%s ago)\n",
			group.ScalingState.LastScaleDown.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
	}

	return nil
}
