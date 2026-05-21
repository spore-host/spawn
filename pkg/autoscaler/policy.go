package autoscaler

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSClient defines the interface for SQS operations needed by the PolicyEvaluator
type SQSClient interface {
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// QueueConfig defines a single queue with optional weight
type QueueConfig struct {
	QueueURL string  `dynamodbav:"queue_url"`        // SQS queue URL
	Weight   float64 `dynamodbav:"weight,omitempty"` // 0.0-1.0, defaults to 1.0
}

// ScalingPolicy defines a queue-depth based scaling policy
type ScalingPolicy struct {
	PolicyType                string        `dynamodbav:"policy_type"`                  // "queue-depth"
	QueueURL                  string        `dynamodbav:"queue_url,omitempty"`          // DEPRECATED: use Queues
	Queues                    []QueueConfig `dynamodbav:"queues,omitempty"`             // Multi-queue support
	TargetMessagesPerInstance int           `dynamodbav:"target_messages_per_instance"` // e.g., 10
	ScaleUpCooldownSeconds    int           `dynamodbav:"scale_up_cooldown_seconds"`    // default: 60
	ScaleDownCooldownSeconds  int           `dynamodbav:"scale_down_cooldown_seconds"`  // default: 300
}

// ScalingState tracks recent scaling activity
type ScalingState struct {
	LastScaleUp            time.Time `dynamodbav:"last_scale_up,omitempty"`
	LastScaleDown          time.Time `dynamodbav:"last_scale_down,omitempty"`
	LastQueueDepth         int       `dynamodbav:"last_queue_depth"`
	LastCalculatedCapacity int       `dynamodbav:"last_calculated_capacity"`
}

// PolicyEvaluator evaluates scaling policies and calculates desired capacity
type PolicyEvaluator struct {
	sqsClient SQSClient
}

// NewPolicyEvaluator creates a new policy evaluator
func NewPolicyEvaluator(sqsClient SQSClient) *PolicyEvaluator {
	return &PolicyEvaluator{
		sqsClient: sqsClient,
	}
}

// EvaluatePolicy calculates new desired capacity based on scaling policy
// Returns: (newDesiredCapacity, queueDepth, changed, error)
func (p *PolicyEvaluator) EvaluatePolicy(
	ctx context.Context,
	group *AutoScaleGroup,
) (int, int, bool, error) {
	if group.ScalingPolicy == nil {
		return group.DesiredCapacity, 0, false, nil
	}

	// Normalize policy: convert single queue to multi-queue format
	queues := p.normalizeQueues(group.ScalingPolicy)
	if len(queues) == 0 {
		return group.DesiredCapacity, 0, false, fmt.Errorf("no queues configured")
	}

	// Query SQS for weighted queue depth
	weightedDepth, err := p.getWeightedQueueDepth(ctx, queues)
	if err != nil {
		return 0, 0, false, fmt.Errorf("get weighted queue depth: %w", err)
	}

	// Calculate needed capacity
	needed := p.calculateDesiredCapacity(
		weightedDepth,
		group.ScalingPolicy.TargetMessagesPerInstance,
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
		return needed, weightedDepth, false, nil
	}

	// Check cooldown
	scaleUp := needed > group.DesiredCapacity
	if p.inCooldown(group.ScalingState, group.ScalingPolicy, scaleUp) {
		return group.DesiredCapacity, weightedDepth, false, nil
	}

	return needed, weightedDepth, true, nil
}

// normalizeQueues converts policy to normalized queue list
// Handles backward compatibility with single QueueURL field
func (p *PolicyEvaluator) normalizeQueues(policy *ScalingPolicy) []QueueConfig {
	// If Queues is populated, use it
	if len(policy.Queues) > 0 {
		queues := make([]QueueConfig, len(policy.Queues))
		for i, q := range policy.Queues {
			queues[i] = q
			// Default weight to 1.0 if not specified
			if queues[i].Weight == 0 {
				queues[i].Weight = 1.0
			}
		}
		return queues
	}

	// Backward compatibility: convert single QueueURL to Queues
	if policy.QueueURL != "" {
		return []QueueConfig{
			{
				QueueURL: policy.QueueURL,
				Weight:   1.0,
			},
		}
	}

	return nil
}

// getWeightedQueueDepth queries multiple queues and returns weighted total
func (p *PolicyEvaluator) getWeightedQueueDepth(ctx context.Context, queues []QueueConfig) (int, error) {
	var weightedDepth float64

	for _, queue := range queues {
		depth, err := p.getQueueDepth(ctx, queue.QueueURL)
		if err != nil {
			return 0, fmt.Errorf("queue %s: %w", queue.QueueURL, err)
		}

		weightedDepth += float64(depth) * queue.Weight
	}

	// Round to nearest integer
	return int(weightedDepth + 0.5), nil
}

// getQueueDepth queries SQS for total queue depth (visible + in-flight messages)
func (p *PolicyEvaluator) getQueueDepth(ctx context.Context, queueURL string) (int, error) {
	result, err := p.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return 0, err
	}

	visible := parseIntAttr(result.Attributes, string(types.QueueAttributeNameApproximateNumberOfMessages))
	inFlight := parseIntAttr(result.Attributes, string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible))

	return visible + inFlight, nil
}

// maxReasonableQueueDepth caps queue depth before ceiling-division to prevent integer overflow.
const maxReasonableQueueDepth = 1_000_000

// calculateDesiredCapacity computes needed instances using ceiling division
func (p *PolicyEvaluator) calculateDesiredCapacity(queueDepth, targetMessagesPerInstance int) int {
	if queueDepth == 0 || targetMessagesPerInstance <= 0 {
		return 0
	}

	if queueDepth > maxReasonableQueueDepth {
		log.Printf("warning: queueDepth %d exceeds max %d, capping", queueDepth, maxReasonableQueueDepth)
		queueDepth = maxReasonableQueueDepth
	}

	// Ceiling division: (depth + target - 1) / target
	return (queueDepth + targetMessagesPerInstance - 1) / targetMessagesPerInstance
}

// inCooldown checks if scaling action is in cooldown period
func (p *PolicyEvaluator) inCooldown(
	state *ScalingState,
	policy *ScalingPolicy,
	scaleUp bool,
) bool {
	if state == nil {
		return false
	}

	var lastScale time.Time
	var cooldownSeconds int

	if scaleUp {
		lastScale = state.LastScaleUp
		cooldownSeconds = policy.ScaleUpCooldownSeconds
	} else {
		lastScale = state.LastScaleDown
		cooldownSeconds = policy.ScaleDownCooldownSeconds
	}

	if lastScale.IsZero() {
		return false
	}

	elapsed := time.Since(lastScale)
	cooldown := time.Duration(cooldownSeconds) * time.Second

	return elapsed < cooldown
}

// parseIntAttr safely parses integer attribute from SQS response
func parseIntAttr(attrs map[string]string, key string) int {
	val, ok := attrs[key]
	if !ok {
		return 0
	}

	n, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}

	return n
}
