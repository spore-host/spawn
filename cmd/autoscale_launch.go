package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

func runAutoscaleLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Validate inputs
	if autoscaleDesired < 1 {
		return fmt.Errorf("desired-capacity must be at least 1")
	}

	// Validate scaling policy flags
	if scalingPolicy != "" {
		if scalingPolicy != "queue-depth" {
			return fmt.Errorf("invalid scaling policy: %s (only 'queue-depth' supported)", scalingPolicy)
		}
		if queueURL == "" {
			return fmt.Errorf("--queue-url required when --scaling-policy is set")
		}
	}

	// Set defaults
	if autoscaleMin < 0 {
		autoscaleMin = 0
	}
	if autoscaleMax <= 0 {
		autoscaleMax = autoscaleDesired * 2
	}
	if autoscaleJobArrayID == "" {
		autoscaleJobArrayID = fmt.Sprintf("%s-%d", autoscaleName, time.Now().Unix())
	}

	// Validate capacity ranges
	if autoscaleMin > autoscaleDesired {
		return fmt.Errorf("min-capacity cannot exceed desired-capacity")
	}
	if autoscaleMax < autoscaleDesired {
		return fmt.Errorf("max-capacity cannot be less than desired-capacity")
	}

	// Decode user data if provided
	userData := autoscaleUserData
	if userData != "" {
		if decoded, err := base64.StdEncoding.DecodeString(userData); err == nil {
			userData = string(decoded)
		}
	}

	// Merge tags from the canonical --tag (repeatable key=value) and the
	// deprecated --tags (key=value map). --tag wins on conflict.
	tags, err := parseKVTags(autoscaleTagList)
	if err != nil {
		return err
	}
	for k, v := range autoscaleTags {
		if _, ok := tags[k]; !ok {
			tags[k] = v
		}
	}

	// Create autoscaler
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Create group
	groupID := fmt.Sprintf("asg-%s-%d", autoscaleName, time.Now().Unix())
	group := &autoscaler.AutoScaleGroup{
		AutoScaleGroupID: groupID,
		GroupName:        autoscaleName,
		JobArrayID:       autoscaleJobArrayID,
		DesiredCapacity:  autoscaleDesired,
		MinCapacity:      autoscaleMin,
		MaxCapacity:      autoscaleMax,
		Status:           "active",
		LaunchTemplate: autoscaler.LaunchTemplate{
			InstanceType:       autoscaleInstanceType,
			AMI:                autoscaleAMI,
			Spot:               autoscaleSpot,
			KeyName:            autoscaleKeyName,
			SubnetID:           autoscaleSubnetID,
			SecurityGroups:     autoscaleSecurityGroups,
			IAMInstanceProfile: autoscaleIAMProfile,
			UserData:           userData,
			Tags:               tags,
		},
		HealthCheckInterval: 60 * time.Second,
		ReplacementStrategy: "immediate",
	}

	// Add scaling policy if specified
	if scalingPolicy == "queue-depth" {
		group.ScalingPolicy = &autoscaler.ScalingPolicy{
			PolicyType:                "queue-depth",
			QueueURL:                  queueURL,
			TargetMessagesPerInstance: targetMessagesPerInstance,
			ScaleUpCooldownSeconds:    scaleUpCooldown,
			ScaleDownCooldownSeconds:  scaleDownCooldown,
		}
	}

	// Add metric policy if specified
	if metricPolicy != "" {
		policy, err := buildMetricPolicy(metricPolicy, targetValue, metricName, metricNamespace, metricStatistic, periodSeconds)
		if err != nil {
			return err
		}
		group.MetricPolicy = policy
	}

	if err := as.CreateGroup(ctx, group); err != nil {
		return fmt.Errorf("create group: %w", err)
	}

	fmt.Printf("Created autoscale group: %s\n", groupID)
	fmt.Printf("Group name: %s\n", autoscaleName)
	fmt.Printf("Job array ID: %s\n", autoscaleJobArrayID)
	fmt.Printf("Desired capacity: %d\n", autoscaleDesired)
	fmt.Printf("Min/Max: %d/%d\n", autoscaleMin, autoscaleMax)

	// Trigger Lambda immediately
	if err := triggerLambda(ctx, groupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
		fmt.Println("\nGroup created but Lambda not triggered. Instances will launch on next scheduled run (within 1 minute).")
	} else {
		fmt.Println("\nTriggered immediate reconciliation. Instances will launch shortly.")
	}

	return nil
}

// buildQueueConfig creates queue configuration from CLI flags
func buildQueueConfig(queueURLs []string, queueWeights []float64, legacyQueueURL string) ([]autoscaler.QueueConfig, error) {
	// Multi-queue mode: --queue flags
	if len(queueURLs) > 0 {
		// Validate weights
		if len(queueWeights) > 0 && len(queueWeights) != len(queueURLs) {
			return nil, fmt.Errorf("number of --queue-weight (%d) must match number of --queue (%d)",
				len(queueWeights), len(queueURLs))
		}

		queues := make([]autoscaler.QueueConfig, len(queueURLs))
		for i, url := range queueURLs {
			weight := 1.0
			if i < len(queueWeights) {
				weight = queueWeights[i]
				if weight <= 0 || weight > 1.0 {
					return nil, fmt.Errorf("queue weight must be between 0.0 and 1.0, got %.2f", weight)
				}
			}
			queues[i] = autoscaler.QueueConfig{
				QueueURL: url,
				Weight:   weight,
			}
		}
		return queues, nil
	}

	// Single queue mode (backward compat): --queue-url flag
	if legacyQueueURL != "" {
		return []autoscaler.QueueConfig{
			{
				QueueURL: legacyQueueURL,
				Weight:   1.0,
			},
		}, nil
	}

	return nil, fmt.Errorf("--queue or --queue-url required when --scaling-policy is set")
}

func buildMetricPolicy(policyType string, target float64, name, namespace, statistic string, period int) (*autoscaler.MetricScalingPolicy, error) {
	var policy *autoscaler.MetricScalingPolicy

	switch policyType {
	case "cpu", "memory":
		policy = autoscaler.GetMetricPolicyDefaults(policyType)
		if target > 0 {
			policy.TargetValue = target
		}
	case "custom":
		if name == "" || namespace == "" {
			return nil, fmt.Errorf("--metric-name and --metric-namespace required for custom metrics")
		}
		if target == 0 {
			return nil, fmt.Errorf("--target-value required for custom metrics")
		}
		policy = &autoscaler.MetricScalingPolicy{
			MetricType:    "custom",
			MetricName:    name,
			Namespace:     namespace,
			Statistic:     statistic,
			TargetValue:   target,
			PeriodSeconds: period,
		}
	default:
		return nil, fmt.Errorf("invalid metric policy: %s (use 'cpu', 'memory', or 'custom')", policyType)
	}

	// Apply custom statistic and period if specified
	if statistic != "Average" {
		policy.Statistic = statistic
	}
	if period != 300 {
		policy.PeriodSeconds = period
	}

	return policy, nil
}
