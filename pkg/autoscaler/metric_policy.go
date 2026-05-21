package autoscaler

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// MetricScalingPolicy defines metric-based scaling configuration
type MetricScalingPolicy struct {
	MetricType    string  `dynamodbav:"metric_type"`    // "cpu", "memory", "custom"
	MetricName    string  `dynamodbav:"metric_name"`    // CloudWatch metric name
	Namespace     string  `dynamodbav:"namespace"`      // CloudWatch namespace
	Statistic     string  `dynamodbav:"statistic"`      // "Average", "Maximum", "Minimum"
	TargetValue   float64 `dynamodbav:"target_value"`   // Target metric value (e.g., 70.0 for 70%)
	PeriodSeconds int     `dynamodbav:"period_seconds"` // Metric evaluation period (default: 300)
}

// CloudWatchClient defines the interface for CloudWatch operations
type CloudWatchClient interface {
	GetMetricStatistics(ctx context.Context, params *cloudwatch.GetMetricStatisticsInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricStatisticsOutput, error)
}

// MetricEvaluator evaluates metric-based scaling policies
type MetricEvaluator struct {
	cwClient CloudWatchClient
}

// NewMetricEvaluator creates a new metric evaluator
func NewMetricEvaluator(cwClient CloudWatchClient) *MetricEvaluator {
	return &MetricEvaluator{
		cwClient: cwClient,
	}
}

// EvaluateMetricPolicy calculates desired capacity based on metric policy
// Returns: (newDesiredCapacity, currentMetricValue, changed, error)
func (m *MetricEvaluator) EvaluateMetricPolicy(
	ctx context.Context,
	group *AutoScaleGroup,
	instanceIDs []string,
) (int, float64, bool, error) {
	if group.MetricPolicy == nil {
		return group.DesiredCapacity, 0, false, nil
	}

	// Get current metric value across all instances
	metricValue, err := m.getAggregatedMetric(ctx, group.MetricPolicy, instanceIDs)
	if err != nil {
		return 0, 0, false, fmt.Errorf("get metric: %w", err)
	}

	// If no instances, can't evaluate metrics - keep current capacity
	if len(instanceIDs) == 0 {
		return group.DesiredCapacity, metricValue, false, nil
	}

	// Calculate needed capacity based on target tracking
	needed := m.calculateCapacityFromMetric(
		metricValue,
		group.MetricPolicy.TargetValue,
		group.DesiredCapacity,
	)

	// Enforce min/max bounds
	if needed < group.MinCapacity {
		needed = group.MinCapacity
	}
	if needed > group.MaxCapacity {
		needed = group.MaxCapacity
	}

	// Check if change is needed
	if needed == group.DesiredCapacity {
		return needed, metricValue, false, nil
	}

	return needed, metricValue, true, nil
}

// getAggregatedMetric queries CloudWatch for metric across instances
func (m *MetricEvaluator) getAggregatedMetric(
	ctx context.Context,
	policy *MetricScalingPolicy,
	instanceIDs []string,
) (float64, error) {
	if len(instanceIDs) == 0 {
		return 0, nil
	}

	// Default period is 5 minutes
	period := policy.PeriodSeconds
	if period == 0 {
		period = 300
	}

	endTime := time.Now()
	startTime := endTime.Add(-time.Duration(period) * time.Second)

	// Get metric for each instance and aggregate
	var totalValue float64
	var dataPointCount int

	for _, instanceID := range instanceIDs {
		dimensions := []types.Dimension{
			{
				Name:  aws.String("InstanceId"),
				Value: aws.String(instanceID),
			},
		}

		result, err := m.cwClient.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String(policy.Namespace),
			MetricName: aws.String(policy.MetricName),
			Dimensions: dimensions,
			StartTime:  aws.Time(startTime),
			EndTime:    aws.Time(endTime),
			Period:     aws.Int32(int32(period)),
			Statistics: []types.Statistic{types.Statistic(policy.Statistic)},
		})
		if err != nil {
			return 0, fmt.Errorf("get metric statistics: %w", err)
		}

		// Aggregate data points
		for _, dp := range result.Datapoints {
			var value float64
			switch policy.Statistic {
			case "Average":
				if dp.Average != nil {
					value = *dp.Average
				}
			case "Maximum":
				if dp.Maximum != nil {
					value = *dp.Maximum
				}
			case "Minimum":
				if dp.Minimum != nil {
					value = *dp.Minimum
				}
			default:
				if dp.Average != nil {
					value = *dp.Average
				}
			}

			totalValue += value
			dataPointCount++
		}
	}

	if dataPointCount == 0 {
		return 0, nil
	}

	// Return average across all instances
	return totalValue / float64(dataPointCount), nil
}

// calculateCapacityFromMetric computes needed capacity using target tracking
func (m *MetricEvaluator) calculateCapacityFromMetric(
	currentMetric float64,
	targetMetric float64,
	currentCapacity int,
) int {
	if targetMetric == 0 || currentCapacity == 0 {
		return currentCapacity
	}

	// Target tracking formula:
	// needed = current * (currentMetric / targetMetric)
	//
	// Examples:
	// - 80% CPU with 70% target and 5 instances → 5 * (80/70) = 5.7 → 6 instances
	// - 50% CPU with 70% target and 5 instances → 5 * (50/70) = 3.5 → 4 instances
	ratio := currentMetric / targetMetric
	needed := float64(currentCapacity) * ratio

	// Round up to be conservative
	return int(needed + 0.5)
}

// GetMetricPolicyDefaults returns default metric policies for common metrics
func GetMetricPolicyDefaults(metricType string) *MetricScalingPolicy {
	switch metricType {
	case "cpu":
		return &MetricScalingPolicy{
			MetricType:    "cpu",
			MetricName:    "CPUUtilization",
			Namespace:     "AWS/EC2",
			Statistic:     "Average",
			TargetValue:   70.0,
			PeriodSeconds: 300,
		}
	case "memory":
		return &MetricScalingPolicy{
			MetricType:    "memory",
			MetricName:    "mem_used_percent",
			Namespace:     "CWAgent",
			Statistic:     "Average",
			TargetValue:   80.0,
			PeriodSeconds: 300,
		}
	default:
		return nil
	}
}
