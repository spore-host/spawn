package orchestrator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/spore-host/spawn/pkg/registry"
)

// Orchestrator manages automatic cloud bursting
type Orchestrator struct {
	config    *Config
	sqsClient *sqs.Client
	ec2Client *ec2.Client

	// State tracking
	managedInstances map[string]*ManagedInstance
	totalCost        float64
	lastBurstTime    time.Time
}

// ManagedInstance tracks a cloud instance launched by orchestrator
type ManagedInstance struct {
	InstanceID   string
	LaunchedAt   time.Time
	LastActivity time.Time
	CostPerHour  float64
}

// New creates a new orchestrator using the default AWS credential chain.
func New(ctx context.Context, cfg *Config) (*Orchestrator, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	return NewWithAWSConfig(ctx, cfg, awsCfg)
}

// NewWithAWSConfig creates a new orchestrator with an injected AWS config.
// Use this in tests to point the orchestrator at a Substrate emulator.
func NewWithAWSConfig(_ context.Context, cfg *Config, awsCfg aws.Config) (*Orchestrator, error) {
	if cfg.Region != "" {
		awsCfg.Region = cfg.Region
	}
	return &Orchestrator{
		config:           cfg,
		sqsClient:        sqs.NewFromConfig(awsCfg),
		ec2Client:        ec2.NewFromConfig(awsCfg),
		managedInstances: make(map[string]*ManagedInstance),
	}, nil
}

// Run starts the orchestrator main loop
func (o *Orchestrator) Run(ctx context.Context) error {
	log.Printf("Orchestrator started (mode: %s, job_array: %s)",
		o.config.BurstPolicy.Mode, o.config.JobArrayID)

	if o.config.BurstPolicy.Mode == "manual" {
		log.Printf("Manual mode - no automatic bursting")
		return nil
	}

	ticker := time.NewTicker(o.config.BurstPolicy.GetCheckInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Orchestrator shutting down")
			return ctx.Err()

		case <-ticker.C:
			if err := o.checkAndScale(ctx); err != nil {
				log.Printf("Error in scale check: %v", err)
			}
		}
	}
}

// checkAndScale checks queue depth and scales accordingly
func (o *Orchestrator) checkAndScale(ctx context.Context) error {
	// 1. Get queue depth
	queueDepth, err := o.getQueueDepth(ctx)
	if err != nil {
		return fmt.Errorf("failed to get queue depth: %w", err)
	}

	// 2. Count active instances
	localCount, cloudCount, err := o.countActiveInstances(ctx)
	if err != nil {
		return fmt.Errorf("failed to count instances: %w", err)
	}

	totalActive := localCount + cloudCount

	log.Printf("Queue depth: %d, Local: %d, Cloud: %d, Total: %d",
		queueDepth, localCount, cloudCount, totalActive)

	// 3. Decide if we need to scale
	if o.shouldScaleUp(queueDepth, localCount, cloudCount) {
		needed := o.calculateNeededInstances(queueDepth, totalActive)
		log.Printf("Scaling up: launching %d instances", needed)
		if err := o.scaleUp(ctx, needed); err != nil {
			return fmt.Errorf("failed to scale up: %w", err)
		}
	} else if o.shouldScaleDown(queueDepth, cloudCount) {
		toTerminate := o.calculateTerminateCount(queueDepth, cloudCount)
		log.Printf("Scaling down: terminating %d instances", toTerminate)
		if err := o.scaleDown(ctx, toTerminate); err != nil {
			return fmt.Errorf("failed to scale down: %w", err)
		}
	}

	// 4. Update cost tracking
	o.updateCostTracking()

	return nil
}

// getQueueDepth gets the approximate number of messages in the queue
func (o *Orchestrator) getQueueDepth(ctx context.Context) (int, error) {
	result, err := o.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(o.config.QueueURL),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})

	if err != nil {
		return 0, err
	}

	visible := 0
	notVisible := 0

	if val, ok := result.Attributes["ApproximateNumberOfMessages"]; ok {
		_, _ = fmt.Sscanf(val, "%d", &visible)
	}
	if val, ok := result.Attributes["ApproximateNumberOfMessagesNotVisible"]; ok {
		_, _ = fmt.Sscanf(val, "%d", &notVisible)
	}

	// Total is visible + in-flight
	return visible + notVisible, nil
}

// countActiveInstances counts local and cloud instances
func (o *Orchestrator) countActiveInstances(ctx context.Context) (local, cloud int, err error) {
	// Query DynamoDB registry directly
	peers, err := o.queryRegistry(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query registry: %w", err)
	}

	for _, peer := range peers {
		switch peer.Provider {
		case "local":
			local++
		case "ec2":
			cloud++
		}
	}

	return local, cloud, nil
}

// queryRegistry queries DynamoDB for peers (orchestrator doesn't have identity)
func (o *Orchestrator) queryRegistry(ctx context.Context) ([]registry.PeerInfo, error) {
	// Create a temporary registry client
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	// Use registry.DiscoverPeers directly
	return registry.DiscoverPeersForJobArray(ctx, awsCfg, o.config.JobArrayID)
}

// shouldScaleUp determines if we need to launch more instances
func (o *Orchestrator) shouldScaleUp(queueDepth, localCount, cloudCount int) bool {
	policy := o.config.BurstPolicy

	// Don't scale if at max
	if cloudCount >= policy.MaxCloudInstances {
		return false
	}

	// Don't scale if over budget
	if policy.CostBudget > 0 && o.totalCost >= policy.CostBudget {
		log.Printf("Budget limit reached: $%.2f/$%.2f", o.totalCost, policy.CostBudget)
		return false
	}

	// Scale if queue depth exceeds threshold and we have capacity
	totalCapacity := localCount + cloudCount
	return queueDepth > policy.QueueDepthThreshold && queueDepth > totalCapacity
}

// shouldScaleDown determines if we should terminate instances
func (o *Orchestrator) shouldScaleDown(queueDepth, cloudCount int) bool {
	policy := o.config.BurstPolicy

	// Keep minimum warm pool
	if cloudCount <= policy.MinCloudInstances {
		return false
	}

	// Scale down if queue is below threshold
	return queueDepth < policy.QueueDepthThreshold/2
}

// calculateNeededInstances calculates how many instances to launch
func (o *Orchestrator) calculateNeededInstances(queueDepth, currentTotal int) int {
	policy := o.config.BurstPolicy

	// Assume each instance can handle ~10 jobs concurrently
	neededTotal := (queueDepth / 10) + 1
	needed := neededTotal - currentTotal

	// Cap at max cloud instances
	maxNew := policy.MaxCloudInstances - len(o.managedInstances)
	if needed > maxNew {
		needed = maxNew
	}

	// Don't launch more than 10 at once
	if needed > 10 {
		needed = 10
	}

	return needed
}

// calculateTerminateCount calculates how many instances to terminate
func (o *Orchestrator) calculateTerminateCount(queueDepth, cloudCount int) int {
	policy := o.config.BurstPolicy

	// Assume each instance can handle ~10 jobs
	neededInstances := (queueDepth / 10) + 1
	excess := cloudCount - neededInstances

	// Keep minimum warm pool
	if cloudCount-excess < policy.MinCloudInstances {
		excess = cloudCount - policy.MinCloudInstances
	}

	if excess <= 0 {
		return 0
	}

	// Don't terminate more than 5 at once
	if excess > 5 {
		excess = 5
	}

	return excess
}

// scaleUp launches new instances
func (o *Orchestrator) scaleUp(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}

	policy := o.config.BurstPolicy

	// Build tags
	tags := []ec2types.Tag{
		{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("spawn-auto-burst-%s", o.config.JobArrayID))},
		{Key: aws.String("spawn:job-array-id"), Value: aws.String(o.config.JobArrayID)},
		{Key: aws.String("spawn:auto-burst"), Value: aws.String("true")},
		{Key: aws.String("spawn:on-complete"), Value: aws.String("terminate")},
		{Key: aws.String("spawn:managed-by"), Value: aws.String("orchestrator")},
	}

	// Build launch request
	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(policy.AMI),
		InstanceType: ec2types.InstanceType(policy.InstanceType),
		MinCount:     aws.Int32(int32(count)),
		MaxCount:     aws.Int32(int32(count)),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
	}

	// Add optional parameters
	if policy.KeyName != "" {
		runInput.KeyName = aws.String(policy.KeyName)
	}
	if policy.SubnetID != "" {
		runInput.SubnetId = aws.String(policy.SubnetID)
	}
	if len(policy.SecurityGroups) > 0 {
		runInput.SecurityGroupIds = policy.SecurityGroups
	}

	// Handle Spot instances
	if policy.Spot {
		runInput.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
			SpotOptions: &ec2types.SpotMarketOptions{
				SpotInstanceType: ec2types.SpotInstanceTypeOneTime,
			},
		}
	}

	// Launch instances
	result, err := o.ec2Client.RunInstances(ctx, runInput)
	if err != nil {
		return fmt.Errorf("failed to run instances: %w", err)
	}

	// Track managed instances
	costPerHour := getInstanceCostPerHour(policy.InstanceType, policy.Spot)
	for _, instance := range result.Instances {
		instanceID := *instance.InstanceId
		o.managedInstances[instanceID] = &ManagedInstance{
			InstanceID:   instanceID,
			LaunchedAt:   time.Now(),
			LastActivity: time.Now(),
			CostPerHour:  costPerHour,
		}
		log.Printf("Launched instance: %s (cost: $%.3f/hour)", instanceID, costPerHour)
	}

	o.lastBurstTime = time.Now()
	return nil
}

// scaleDown terminates idle instances
func (o *Orchestrator) scaleDown(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}

	// Find oldest idle instances
	var toTerminate []string
	scaleDownDelay := o.config.BurstPolicy.GetScaleDownDelay()

	for id, instance := range o.managedInstances {
		if time.Since(instance.LastActivity) > scaleDownDelay {
			toTerminate = append(toTerminate, id)
			if len(toTerminate) >= count {
				break
			}
		}
	}

	if len(toTerminate) == 0 {
		log.Printf("No instances idle enough to terminate")
		return nil
	}

	// Terminate instances
	_, err := o.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: toTerminate,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	// Remove from tracking
	for _, id := range toTerminate {
		log.Printf("Terminated instance: %s", id)
		delete(o.managedInstances, id)
	}

	return nil
}

// updateCostTracking updates hourly cost estimate
func (o *Orchestrator) updateCostTracking() {
	var currentCost float64
	for _, instance := range o.managedInstances {
		currentCost += instance.CostPerHour
	}
	o.totalCost = currentCost
}

// getInstanceCostPerHour returns approximate cost per hour for an instance type
func getInstanceCostPerHour(instanceType string, spot bool) float64 {
	// Simplified cost mapping (US East 1 on-demand prices)
	costs := map[string]float64{
		"t3.micro":   0.0104,
		"t3.small":   0.0208,
		"t3.medium":  0.0416,
		"t3.large":   0.0832,
		"t3.xlarge":  0.1664,
		"c5.large":   0.085,
		"c5.xlarge":  0.17,
		"c5.2xlarge": 0.34,
		"c5.4xlarge": 0.68,
		"c5.9xlarge": 1.53,
		"m5.large":   0.096,
		"m5.xlarge":  0.192,
		"m5.2xlarge": 0.384,
		"m5.4xlarge": 0.768,
	}

	cost, ok := costs[instanceType]
	if !ok {
		cost = 0.10 // Default estimate
	}

	// Spot is typically 70% cheaper
	if spot {
		cost = cost * 0.3
	}

	return cost
}

// GetStats returns current orchestrator stats
func (o *Orchestrator) GetStats() Stats {
	return Stats{
		ManagedInstances: len(o.managedInstances),
		TotalCostPerHour: o.totalCost,
		LastBurstTime:    o.lastBurstTime,
	}
}

// Stats represents orchestrator statistics
type Stats struct {
	ManagedInstances int
	TotalCostPerHour float64
	LastBurstTime    time.Time
}
