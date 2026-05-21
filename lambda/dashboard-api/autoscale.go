package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqsTypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// AutoScaleGroup represents an auto-scaling group (minimal structure for reading from DynamoDB)
type AutoScaleGroup struct {
	AutoScaleGroupID      string    `dynamodbav:"autoscale_group_id"`
	GroupName             string    `dynamodbav:"group_name"`
	JobArrayID            string    `dynamodbav:"job_array_id"`
	DesiredCapacity       int       `dynamodbav:"desired_capacity"`
	MinCapacity           int       `dynamodbav:"min_capacity"`
	MaxCapacity           int       `dynamodbav:"max_capacity"`
	Status                string    `dynamodbav:"status"`
	CreatedAt             time.Time `dynamodbav:"created_at"`
	UpdatedAt             time.Time `dynamodbav:"updated_at"`
	LastScaleEvent        time.Time `dynamodbav:"last_scale_event"`
	PausedAt              time.Time `dynamodbav:"paused_at,omitempty"`
	PausedDesiredCapacity int       `dynamodbav:"paused_desired_capacity,omitempty"`
	TerminatedAt          time.Time `dynamodbav:"terminated_at,omitempty"`
	ScalingPolicy         *struct {
		QueueURL string `dynamodbav:"queue_url"`
	} `dynamodbav:"scaling_policy,omitempty"`
	MetricPolicy *struct {
		MetricName string `dynamodbav:"metric_name"`
	} `dynamodbav:"metric_policy,omitempty"`
	ScheduleConfig *struct {
		Schedules []interface{} `dynamodbav:"schedules"`
	} `dynamodbav:"schedule_config,omitempty"`
}

// AutoScaleGroupWithUserID extends AutoScaleGroup with user_id for access control
type AutoScaleGroupWithUserID struct {
	AutoScaleGroup
	UserID string `dynamodbav:"user_id"`
}

const (
	dynamoAutoscaleGroupsTable = "spawn-autoscale-groups-production"
	dynamoRegistryTable        = "spawn-registry-production"
)

// Instance type pricing (hourly, on-demand, us-east-1)
var instancePricing = map[string]float64{
	"t3.micro":    0.0104,
	"t3.small":    0.0208,
	"t3.medium":   0.0416,
	"t3.large":    0.0832,
	"t3.xlarge":   0.1664,
	"t3.2xlarge":  0.3328,
	"t3a.micro":   0.0094,
	"t3a.small":   0.0188,
	"t3a.medium":  0.0376,
	"t3a.large":   0.0752,
	"t3a.xlarge":  0.1504,
	"t3a.2xlarge": 0.3008,
	"m5.large":    0.096,
	"m5.xlarge":   0.192,
	"m5.2xlarge":  0.384,
	"m5.4xlarge":  0.768,
	"c5.large":    0.085,
	"c5.xlarge":   0.17,
	"c5.2xlarge":  0.34,
	"c5.4xlarge":  0.68,
	"r5.large":    0.126,
	"r5.xlarge":   0.252,
	"r5.2xlarge":  0.504,
}

// handleListAutoscaleGroups handles GET /api/autoscale-groups
// When teamID is non-empty, team groups are merged with personal groups.
func handleListAutoscaleGroups(ctx context.Context, cfg aws.Config, cliIamArn, teamID string) (events.APIGatewayProxyResponse, error) {
	startTime := time.Now()

	dynamodbClient := dynamodb.NewFromConfig(cfg)

	seen := make(map[string]struct{})
	var groupInfos []AutoScaleGroupInfo

	appendGroups := func(items []map[string]ddbTypes.AttributeValue) {
		for _, item := range items {
			var group AutoScaleGroup
			if err := attributevalue.UnmarshalMap(item, &group); err != nil {
				continue
			}
			if _, dup := seen[group.AutoScaleGroupID]; dup {
				continue
			}
			seen[group.AutoScaleGroupID] = struct{}{}

			count, err := getCurrentCapacity(ctx, cfg, group.AutoScaleGroupID, cliIamArn)
			if err != nil {
				log.Printf("warning: failed to get capacity for group %s: %v", group.AutoScaleGroupID, err)
			}

			policyType := "none"
			if group.ScalingPolicy != nil {
				policyType = "queue"
			} else if group.MetricPolicy != nil {
				policyType = "metric"
			} else if group.ScheduleConfig != nil {
				policyType = "schedule"
			}

			groupInfos = append(groupInfos, AutoScaleGroupInfo{
				AutoScaleGroupID: group.AutoScaleGroupID,
				GroupName:        group.GroupName,
				Status:           group.Status,
				DesiredCapacity:  group.DesiredCapacity,
				CurrentCapacity:  count,
				MinCapacity:      group.MinCapacity,
				MaxCapacity:      group.MaxCapacity,
				PolicyType:       policyType,
				LastScaleEvent:   group.LastScaleEvent,
				CreatedAt:        group.CreatedAt,
			})
		}
	}

	// Personal groups via user_id-index GSI
	result, err := dynamodbClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoAutoscaleGroupsTable),
		IndexName:              aws.String("user_id-index"),
		KeyConditionExpression: aws.String("user_id = :user_id"),
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":user_id": &ddbTypes.AttributeValueMemberS{Value: cliIamArn},
		},
		Limit: aws.Int32(200),
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to query autoscale groups: %v", err)), nil
	}
	appendGroups(result.Items)

	// Team groups via team_id-index GSI
	if teamID != "" {
		if _, err := resolveTeamContext(ctx, cfg, teamID, cliIamArn); err != nil {
			return errorResponse(403, "access denied"), nil
		}
		teamResult, err := dynamodbClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(dynamoAutoscaleGroupsTable),
			IndexName:              aws.String("team_id-index"),
			KeyConditionExpression: aws.String("team_id = :tid"),
			ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
				":tid": &ddbTypes.AttributeValueMemberS{Value: teamID},
			},
			Limit: aws.Int32(200),
		})
		if err != nil {
			log.Printf("warning: failed to query team autoscale groups: %v", err)
		} else {
			appendGroups(teamResult.Items)
		}
	}

	elapsed := time.Since(startTime)
	log.Printf("listed %d autoscale groups in %v", len(groupInfos), elapsed)

	if groupInfos == nil {
		groupInfos = []AutoScaleGroupInfo{}
	}

	response := AutoScaleGroupsAPIResponse{
		Success:         true,
		TotalGroups:     len(groupInfos),
		AutoScaleGroups: groupInfos,
	}

	return successResponse(response)
}

// handleGetAutoscaleGroup handles GET /api/autoscale-groups/{id}
func handleGetAutoscaleGroup(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Get group from DynamoDB
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	getInput := &dynamodb.GetItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
	}

	result, err := dynamodbClient.GetItem(ctx, getInput)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get group: %v", err)), nil
	}

	if result.Item == nil {
		return errorResponse(404, "Autoscale group not found"), nil
	}

	// Unmarshal group
	var group AutoScaleGroupWithUserID
	if err := attributevalue.UnmarshalMap(result.Item, &group); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal group: %v", err)), nil
	}

	// Verify group belongs to this user
	if group.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}

	// Enrich with health details
	detail, err := enrichGroupWithHealth(ctx, cfg, &group.AutoScaleGroup, cliIamArn)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to enrich group: %v", err)), nil
	}

	response := AutoScaleGroupDetailAPIResponse{
		Success: true,
		Group:   *detail,
	}

	return successResponse(response)
}

// handleGetCostSummary handles GET /api/cost-summary
func handleGetCostSummary(ctx context.Context, cfg aws.Config, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	cost, err := calculateInstanceCosts(ctx, cfg, cliIamArn)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to calculate costs: %v", err)), nil
	}

	response := CostSummaryAPIResponse{
		Success: true,
		Cost:    *cost,
	}

	return successResponse(response)
}

// handlePauseAutoscaleGroup handles POST /api/autoscale-groups/{id}/pause
func handlePauseAutoscaleGroup(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Fetch group and verify ownership
	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get group: %v", err)), nil
	}
	if result.Item == nil {
		return errorResponse(404, "Autoscale group not found"), nil
	}

	var group AutoScaleGroupWithUserID
	if err := attributevalue.UnmarshalMap(result.Item, &group); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal group: %v", err)), nil
	}
	if group.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}
	if group.Status == "paused" {
		return errorResponse(400, "Group is already paused"), nil
	}

	now := time.Now()
	_, err = dynamodbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
		UpdateExpression: aws.String("SET #status = :status, paused_at = :paused_at, paused_desired_capacity = desired_capacity, desired_capacity = :zero, updated_at = :updated_at"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":status":     &ddbTypes.AttributeValueMemberS{Value: "paused"},
			":paused_at":  &ddbTypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":zero":       &ddbTypes.AttributeValueMemberN{Value: "0"},
			":updated_at": &ddbTypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to pause group: %v", err)), nil
	}

	return successResponse(map[string]interface{}{
		"success": true,
		"message": "Autoscale group paused",
	})
}

// handleResumeAutoscaleGroup handles POST /api/autoscale-groups/{id}/resume
func handleResumeAutoscaleGroup(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Fetch group and verify ownership
	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get group: %v", err)), nil
	}
	if result.Item == nil {
		return errorResponse(404, "Autoscale group not found"), nil
	}

	var group AutoScaleGroupWithUserID
	if err := attributevalue.UnmarshalMap(result.Item, &group); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal group: %v", err)), nil
	}
	if group.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}
	if group.Status != "paused" {
		return errorResponse(400, "Group is not paused"), nil
	}

	// Restore previous desired capacity (default to 1 if not stored)
	desiredCapacity := group.PausedDesiredCapacity
	if desiredCapacity == 0 {
		desiredCapacity = 1
	}

	now := time.Now()
	_, err = dynamodbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
		UpdateExpression: aws.String("SET #status = :status, desired_capacity = :desired_capacity, updated_at = :updated_at REMOVE paused_at, paused_desired_capacity"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":status":           &ddbTypes.AttributeValueMemberS{Value: "active"},
			":desired_capacity": &ddbTypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", desiredCapacity)},
			":updated_at":       &ddbTypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to resume group: %v", err)), nil
	}

	return successResponse(map[string]interface{}{
		"success":          true,
		"message":          "Autoscale group resumed",
		"desired_capacity": desiredCapacity,
	})
}

// handleTerminateAutoscaleGroup handles DELETE /api/autoscale-groups/{id}
func handleTerminateAutoscaleGroup(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Fetch group and verify ownership
	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get group: %v", err)), nil
	}
	if result.Item == nil {
		return errorResponse(404, "Autoscale group not found"), nil
	}

	var group AutoScaleGroupWithUserID
	if err := attributevalue.UnmarshalMap(result.Item, &group); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal group: %v", err)), nil
	}
	if group.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}

	// Terminate all running instances
	terminatedCount := 0
	instances, _ := getGroupInstances(ctx, cfg, groupID, cliIamArn)
	for _, inst := range instances {
		if inst.State == "running" || inst.State == "pending" {
			// Find the region for this instance by querying across regions
			for _, region := range awsRegions {
				ec2Client, err := getEC2ClientForRegion(ctx, cfg, region)
				if err != nil {
					continue
				}
				_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
					InstanceIds: []string{inst.InstanceID},
				})
				if err == nil {
					terminatedCount++
					break
				}
			}
		}
	}

	now := time.Now()
	_, err = dynamodbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoAutoscaleGroupsTable),
		Key: map[string]ddbTypes.AttributeValue{
			"autoscale_group_id": &ddbTypes.AttributeValueMemberS{Value: groupID},
		},
		UpdateExpression: aws.String("SET #status = :status, terminated_at = :terminated_at, desired_capacity = :zero, updated_at = :updated_at"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":status":        &ddbTypes.AttributeValueMemberS{Value: "terminated"},
			":terminated_at": &ddbTypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":zero":          &ddbTypes.AttributeValueMemberN{Value: "0"},
			":updated_at":    &ddbTypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to update group status: %v", err)), nil
	}

	return successResponse(map[string]interface{}{
		"success":              true,
		"message":              "Autoscale group terminated",
		"instances_terminated": terminatedCount,
	})
}

// getCurrentCapacity counts running/pending instances for a group
func getCurrentCapacity(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) (int, error) {
	count := 0

	for _, region := range awsRegions {
		ec2Client, err := getEC2ClientForRegion(ctx, cfg, region)
		if err != nil {
			continue
		}

		filters := []ec2Types.Filter{
			{
				Name:   aws.String("tag:spawn:autoscale-group"),
				Values: []string{groupID},
			},
			{
				Name:   aws.String("tag:spawn:iam-user"),
				Values: []string{cliIamArn},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running"},
			},
		}

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: filters,
		})
		if err != nil {
			continue
		}

		for _, reservation := range result.Reservations {
			count += len(reservation.Instances)
		}
	}

	return count, nil
}

// enrichGroupWithHealth adds health information to a group
func enrichGroupWithHealth(ctx context.Context, cfg aws.Config, group *AutoScaleGroup, cliIamArn string) (*GroupDetailInfo, error) {
	// Get instances for this group
	instances, err := getGroupInstances(ctx, cfg, group.AutoScaleGroupID, cliIamArn)
	if err != nil {
		return nil, fmt.Errorf("get instances: %w", err)
	}

	// Aggregate health stats
	healthyCount := 0
	unhealthyCount := 0
	pendingCount := 0

	for _, inst := range instances {
		if inst.State == "pending" {
			pendingCount++
		} else if inst.HealthStatus == "healthy" {
			healthyCount++
		} else {
			unhealthyCount++
		}
	}

	policyType := determinePolicyType(group)

	detail := &GroupDetailInfo{
		AutoScaleGroupInfo: AutoScaleGroupInfo{
			AutoScaleGroupID: group.AutoScaleGroupID,
			GroupName:        group.GroupName,
			Status:           group.Status,
			DesiredCapacity:  group.DesiredCapacity,
			CurrentCapacity:  len(instances),
			MinCapacity:      group.MinCapacity,
			MaxCapacity:      group.MaxCapacity,
			PolicyType:       policyType,
			LastScaleEvent:   group.LastScaleEvent,
			CreatedAt:        group.CreatedAt,
		},
		HealthyCount:   healthyCount,
		UnhealthyCount: unhealthyCount,
		PendingCount:   pendingCount,
		Instances:      instances,
	}

	// Add policy-specific metadata
	if group.ScalingPolicy != nil && group.ScalingPolicy.QueueURL != "" {
		// Fetch current queue depth from SQS
		if depth, err := getQueueDepth(ctx, cfg, group.ScalingPolicy.QueueURL); err == nil {
			detail.QueueDepth = aws.Int(depth)
		}
	}

	if group.MetricPolicy != nil {
		// Metric policy - could fetch current metric value here
		// For now, omit (would require CloudWatch permissions)
	}

	if group.ScheduleConfig != nil && len(group.ScheduleConfig.Schedules) > 0 {
		// Schedule policy exists - would need full cron parsing to determine next action
		// For now, omit next action (future enhancement)
	}

	return detail, nil
}

// getGroupInstances gets all instances for a group across all regions
func getGroupInstances(ctx context.Context, cfg aws.Config, groupID, cliIamArn string) ([]GroupInstanceInfo, error) {
	var wg sync.WaitGroup
	instancesChan := make(chan []GroupInstanceInfo, len(awsRegions))

	for _, region := range awsRegions {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()

			ec2Client, err := getEC2ClientForRegion(ctx, cfg, r)
			if err != nil {
				return
			}

			filters := []ec2Types.Filter{
				{
					Name:   aws.String("tag:spawn:autoscale-group"),
					Values: []string{groupID},
				},
				{
					Name:   aws.String("tag:spawn:iam-user"),
					Values: []string{cliIamArn},
				},
			}

			result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				Filters: filters,
			})
			if err != nil {
				return
			}

			var regionInstances []GroupInstanceInfo
			for _, reservation := range result.Reservations {
				for _, instance := range reservation.Instances {
					state := string(instance.State.Name)
					healthStatus := "unknown"

					// Simple health check based on state
					if state == "running" {
						healthStatus = "healthy"
					} else if state == "pending" {
						healthStatus = "pending"
					} else {
						healthStatus = "unhealthy"
					}

					inst := GroupInstanceInfo{
						InstanceID:       aws.ToString(instance.InstanceId),
						State:            state,
						HealthStatus:     healthStatus,
						HeartbeatAge:     0, // Would need registry query
						SpotInterruption: false,
						LaunchedAt:       aws.ToTime(instance.LaunchTime),
					}

					regionInstances = append(regionInstances, inst)
				}
			}

			if len(regionInstances) > 0 {
				instancesChan <- regionInstances
			}
		}(region)
	}

	go func() {
		wg.Wait()
		close(instancesChan)
	}()

	var allInstances []GroupInstanceInfo
	for instances := range instancesChan {
		allInstances = append(allInstances, instances...)
	}

	// Sort by launch time (newest first)
	sort.Slice(allInstances, func(i, j int) bool {
		return allInstances[i].LaunchedAt.After(allInstances[j].LaunchedAt)
	})

	return allInstances, nil
}

// calculateInstanceCosts calculates current hourly and monthly costs
func calculateInstanceCosts(ctx context.Context, cfg aws.Config, cliIamArn string) (*CostSummary, error) {
	breakdown := make(map[string]TypeCost)
	totalHourly := 0.0
	totalCount := 0

	for _, region := range awsRegions {
		ec2Client, err := getEC2ClientForRegion(ctx, cfg, region)
		if err != nil {
			continue
		}

		filters := []ec2Types.Filter{
			{
				Name:   aws.String("tag:spawn:iam-user"),
				Values: []string{cliIamArn},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running"},
			},
		}

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: filters,
		})
		if err != nil {
			continue
		}

		for _, reservation := range result.Reservations {
			for _, instance := range reservation.Instances {
				instanceType := string(instance.InstanceType)
				isSpot := instance.InstanceLifecycle == ec2Types.InstanceLifecycleTypeSpot

				// Get base price
				price, exists := instancePricing[instanceType]
				if !exists {
					// Default estimate for unknown types
					price = 0.10
				}

				// Apply spot discount (70% savings estimate)
				if isSpot {
					price = price * 0.30
				}

				// Update breakdown
				entry := breakdown[instanceType]
				entry.Count++
				entry.HourlyCost += price
				breakdown[instanceType] = entry

				totalHourly += price
				totalCount++
			}
		}
	}

	// Calculate monthly (730 hours/month average)
	totalMonthly := totalHourly * 730

	return &CostSummary{
		TotalHourlyCost:      totalHourly,
		EstimatedMonthlyCost: totalMonthly,
		InstanceCount:        totalCount,
		BreakdownByType:      breakdown,
	}, nil
}

// determinePolicyType determines the policy type for a group
func determinePolicyType(group *AutoScaleGroup) string {
	if group.ScalingPolicy != nil && group.ScalingPolicy.QueueURL != "" {
		return "queue"
	}
	if group.MetricPolicy != nil {
		return "metric"
	}
	if group.ScheduleConfig != nil && len(group.ScheduleConfig.Schedules) > 0 {
		return "schedule"
	}
	return "none"
}

// getQueueDepth fetches the approximate number of messages in an SQS queue
func getQueueDepth(ctx context.Context, cfg aws.Config, queueURL string) (int, error) {
	sqsClient := sqs.NewFromConfig(cfg)
	result, err := sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqsTypes.QueueAttributeName{"ApproximateNumberOfMessages"},
	})
	if err != nil {
		return 0, fmt.Errorf("get queue attributes: %w", err)
	}
	if v, ok := result.Attributes["ApproximateNumberOfMessages"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("parse queue depth: %w", err)
		}
		return n, nil
	}
	return 0, nil
}
