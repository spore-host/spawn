package autoscaler

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// HealthChecker checks the health of instances
type HealthChecker struct {
	ec2Client     *ec2.Client
	dynamoClient  *dynamodb.Client
	registryTable string
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(ec2Client *ec2.Client, dynamoClient *dynamodb.Client, registryTable string) *HealthChecker {
	return &HealthChecker{
		ec2Client:     ec2Client,
		dynamoClient:  dynamoClient,
		registryTable: registryTable,
	}
}

// CheckInstances checks the health of multiple instances
func (h *HealthChecker) CheckInstances(ctx context.Context, jobArrayID string, instanceIDs []string) ([]HealthStatus, error) {
	if len(instanceIDs) == 0 {
		return []HealthStatus{}, nil
	}

	results := make([]HealthStatus, 0, len(instanceIDs))

	// Get EC2 states (batch up to 1000)
	ec2States, err := h.getEC2States(ctx, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("get ec2 states: %w", err)
	}

	// Check each instance
	for _, instanceID := range instanceIDs {
		status := HealthStatus{
			InstanceID: instanceID,
		}

		// Get EC2 state
		ec2State, exists := ec2States[instanceID]
		if !exists {
			status.Healthy = false
			status.Reason = "instance not found in EC2"
			results = append(results, status)
			continue
		}
		status.EC2State = ec2State

		// Get heartbeat from registry (query for this specific instance)
		heartbeatAge, err := h.getHeartbeatAge(ctx, jobArrayID, instanceID)
		if err == nil {
			status.HeartbeatAge = heartbeatAge
		}

		// Check for spot interruption
		status.SpotInterruption = h.checkSpotInterruption(ctx, instanceID)

		// Evaluate overall health
		status.Healthy = h.evaluateHealth(status)
		if !status.Healthy && status.Reason == "" {
			status.Reason = h.getUnhealthyReason(status)
		}

		results = append(results, status)
	}

	return results, nil
}

// getEC2States retrieves EC2 instance states in batch
func (h *HealthChecker) getEC2States(ctx context.Context, instanceIDs []string) (map[string]string, error) {
	states := make(map[string]string)

	result, err := h.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return nil, err
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil && instance.State != nil && instance.State.Name != "" {
				states[aws.ToString(instance.InstanceId)] = string(instance.State.Name)
			}
		}
	}

	return states, nil
}

// checkSpotInterruption checks if instance has spot interruption notice
func (h *HealthChecker) checkSpotInterruption(ctx context.Context, instanceID string) bool {
	// Check instance metadata for spot interruption
	// This would require the instance to report its interruption status
	// For Phase 1, we'll implement a simpler version that checks instance state
	// In future phases, instances can report interruption via registry
	return false
}

// evaluateHealth determines if an instance is healthy based on criteria
func (h *HealthChecker) evaluateHealth(status HealthStatus) bool {
	// Unhealthy states
	if status.EC2State == "stopping" || status.EC2State == "stopped" ||
		status.EC2State == "shutting-down" || status.EC2State == "terminated" {
		return false
	}

	// Spot interruption notice
	if status.SpotInterruption {
		return false
	}

	// Stale heartbeat (only check if not pending)
	if status.EC2State == "running" && status.HeartbeatAge > 5*time.Minute {
		return false
	}

	// Pending is considered healthy (waiting for boot)
	if status.EC2State == "pending" {
		return true
	}

	// Running with recent heartbeat
	if status.EC2State == "running" && status.HeartbeatAge < 2*time.Minute {
		return true
	}

	// Running without heartbeat yet (recently launched)
	if status.EC2State == "running" && status.HeartbeatAge == 0 {
		return true
	}

	return false
}

// getUnhealthyReason returns a human-readable reason for unhealthy status
func (h *HealthChecker) getUnhealthyReason(status HealthStatus) string {
	if status.EC2State == "stopped" || status.EC2State == "stopping" {
		return "instance stopped"
	}
	if status.EC2State == "terminated" || status.EC2State == "shutting-down" {
		return "instance terminated"
	}
	if status.SpotInterruption {
		return "spot interruption notice"
	}
	if status.HeartbeatAge > 5*time.Minute {
		return "stale heartbeat"
	}
	return "unknown"
}

// getHeartbeatAge gets the heartbeat age for a specific instance
func (h *HealthChecker) getHeartbeatAge(ctx context.Context, jobArrayID, instanceID string) (time.Duration, error) {
	result, err := h.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(h.registryTable),
		Key: map[string]types.AttributeValue{
			"job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
			"instance_id":  &types.AttributeValueMemberS{Value: instanceID},
		},
	})

	if err != nil {
		return 0, err
	}

	if result.Item == nil {
		return 0, fmt.Errorf("instance not found in registry")
	}

	// Get last_heartbeat timestamp
	lastHeartbeat := getNumberValue(result.Item["last_heartbeat"])
	if lastHeartbeat == 0 {
		return 0, fmt.Errorf("no heartbeat recorded")
	}

	return time.Since(time.Unix(lastHeartbeat, 0)), nil
}

// getNumberValue extracts a number value from DynamoDB attribute
func getNumberValue(attr types.AttributeValue) int64 {
	if n, ok := attr.(*types.AttributeValueMemberN); ok {
		val, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil {
			log.Printf("warning: unexpected DynamoDB number value %q: %v", n.Value, err)
			return 0
		}
		return val
	}
	return 0
}

// CountPending counts instances in pending state
func CountPending(statuses []HealthStatus) int {
	count := 0
	for _, s := range statuses {
		if s.EC2State == "pending" {
			count++
		}
	}
	return count
}
