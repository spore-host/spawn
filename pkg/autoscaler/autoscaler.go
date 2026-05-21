package autoscaler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AutoScaler is the main reconciliation engine
type AutoScaler struct {
	config             *Config
	dbClient           *Client
	healthChecker      *HealthChecker
	capacityReconciler *CapacityReconciler
	policyEvaluator    *PolicyEvaluator
	metricEvaluator    *MetricEvaluator
	drainManager       *DrainManager
	scheduleEvaluator  *ScheduleEvaluator
}

// NewAutoScaler creates a new autoscaler
func NewAutoScaler(config *Config) *AutoScaler {
	dbClient := NewClient(config.DynamoClient, config.TableName)
	healthChecker := NewHealthChecker(config.EC2Client, config.DynamoClient, config.RegistryTable)
	capacityReconciler := NewCapacityReconciler(config.EC2Client)
	policyEvaluator := NewPolicyEvaluator(config.SQSClient)
	metricEvaluator := NewMetricEvaluator(config.CloudWatchClient)
	drainManager := NewDrainManager(config.EC2Client, config.DynamoClient, config.RegistryTable)
	scheduleEvaluator := NewScheduleEvaluator()

	return &AutoScaler{
		config:             config,
		dbClient:           dbClient,
		healthChecker:      healthChecker,
		capacityReconciler: capacityReconciler,
		policyEvaluator:    policyEvaluator,
		metricEvaluator:    metricEvaluator,
		drainManager:       drainManager,
		scheduleEvaluator:  scheduleEvaluator,
	}
}

// Reconcile reconciles a single autoscale group
func (a *AutoScaler) Reconcile(ctx context.Context, groupID string) error {
	// Load group
	group, err := a.dbClient.GetGroup(ctx, groupID)
	if err != nil {
		return fmt.Errorf("load group: %w", err)
	}

	// Skip non-active groups
	if group.Status != "active" {
		log.Printf("skipping group %s (status: %s)", group.GroupName, group.Status)
		return nil
	}

	// Declare hybrid policy variables (must be before goto)
	var queueDesired, metricDesired int
	var queueDepth int
	var metricValue float64
	var hasQueuePolicy, hasMetricPolicy bool

	// Evaluate scheduled actions first (highest priority - overrides desired capacity)
	if group.ScheduleConfig != nil && len(group.ScheduleConfig.Actions) > 0 {
		desired, min, max, actionName, shouldApply := a.scheduleEvaluator.EvaluateSchedule(
			group.ScheduleConfig,
			time.Now(),
		)
		if shouldApply {
			log.Printf("[%s] scheduled action %q triggered: desired=%d, min=%d, max=%d",
				group.GroupName, actionName, desired, min, max)

			// Apply scheduled capacity overrides
			group.DesiredCapacity = desired
			if min > 0 {
				group.MinCapacity = min
			}
			if max > 0 {
				group.MaxCapacity = max
			}

			// Update scaling state
			if group.ScalingState == nil {
				group.ScalingState = &ScalingState{}
			}
			group.ScalingState.LastCalculatedCapacity = desired

			// Skip other policy evaluations when schedule is active
			goto reconcile
		}
	}

	// Evaluate hybrid policies (queue + metric)
	// Both policies can be active; combine their recommendations intelligently

	// Evaluate queue policy (if present)
	if group.ScalingPolicy != nil {
		newDesired, depth, _, err := a.policyEvaluator.EvaluatePolicy(ctx, group)
		if err != nil {
			log.Printf("policy evaluation failed for %s: %v", group.GroupName, err)
		} else {
			queueDesired = newDesired
			queueDepth = depth
			hasQueuePolicy = true
		}
	}

reconcile:
	log.Printf("reconciling group %s (desired: %d)", group.GroupName, group.DesiredCapacity)

	// Discover current instances
	instances, err := a.discoverInstances(ctx, group)
	if err != nil {
		return fmt.Errorf("discover instances: %w", err)
	}

	log.Printf("found %d instances for group %s", len(instances), group.GroupName)

	// Evaluate metric policy (if present)
	// Metric policy requires existing instances to query metrics
	if group.MetricPolicy != nil {
		newDesired, value, _, err := a.metricEvaluator.EvaluateMetricPolicy(ctx, group, instances)
		if err != nil {
			log.Printf("metric policy evaluation failed for %s: %v", group.GroupName, err)
		} else {
			metricDesired = newDesired
			metricValue = value
			hasMetricPolicy = true
		}
	}

	// Combine policy recommendations using hybrid logic
	if hasQueuePolicy || hasMetricPolicy {
		oldDesired := group.DesiredCapacity
		newDesired := a.combineHybridPolicies(
			group.DesiredCapacity,
			queueDesired, hasQueuePolicy,
			metricDesired, hasMetricPolicy,
		)

		if newDesired != oldDesired {
			reason := a.getHybridScalingReason(
				oldDesired, newDesired,
				queueDesired, hasQueuePolicy,
				metricDesired, hasMetricPolicy,
				queueDepth, metricValue,
			)
			log.Printf("[%s] hybrid policy: %d → %d (%s)",
				group.GroupName, oldDesired, newDesired, reason)

			group.DesiredCapacity = newDesired

			// Update scaling state
			if group.ScalingState == nil {
				group.ScalingState = &ScalingState{}
			}
			if hasQueuePolicy {
				group.ScalingState.LastQueueDepth = queueDepth
			}
			group.ScalingState.LastCalculatedCapacity = newDesired
			if newDesired > oldDesired {
				group.ScalingState.LastScaleUp = time.Now()
			} else {
				group.ScalingState.LastScaleDown = time.Now()
			}
		}
	}

	// Handle draining instances (if drain enabled)
	if group.DrainConfig != nil && group.DrainConfig.Enabled {
		drainingInstances, err := a.drainManager.GetDrainingInstances(ctx, group.AutoScaleGroupID)
		if err != nil {
			log.Printf("error getting draining instances for %s: %v", group.GroupName, err)
		} else if len(drainingInstances) > 0 {
			log.Printf("found %d draining instances for %s", len(drainingInstances), group.GroupName)

			// Check which draining instances are ready to terminate
			readyToTerminate, err := a.drainManager.CheckDrainStatus(ctx, drainingInstances, group.DrainConfig)
			if err != nil {
				log.Printf("error checking drain status for %s: %v", group.GroupName, err)
			} else if len(readyToTerminate) > 0 {
				log.Printf("terminating %d drained instances for %s", len(readyToTerminate), group.GroupName)

				// Terminate drained instances
				if err := a.capacityReconciler.TerminateInstances(ctx, readyToTerminate); err != nil {
					log.Printf("error terminating drained instances: %v", err)
				}

				// Clear drain state
				if err := a.drainManager.ClearDrainState(ctx, readyToTerminate); err != nil {
					log.Printf("error clearing drain state: %v", err)
				}
			}
		}
	}

	// Check health
	health, err := a.healthChecker.CheckInstances(ctx, group.JobArrayID, instances)
	if err != nil {
		return fmt.Errorf("check health: %w", err)
	}

	// Plan capacity changes
	plan, err := a.capacityReconciler.PlanCapacity(ctx, group, health)
	if err != nil {
		return fmt.Errorf("plan capacity: %w", err)
	}

	log.Printf("capacity plan for %s: current=%d, desired=%d, healthy=%d, unhealthy=%d, launch=%d, terminate=%d",
		group.GroupName, plan.CurrentCapacity, plan.DesiredCapacity,
		plan.HealthyCount, plan.UnhealthyCount, plan.ToLaunch, len(plan.ToTerminate))

	// Execute plan if changes needed
	if plan.ToLaunch > 0 || len(plan.ToTerminate) > 0 {
		if err := a.capacityReconciler.ExecutePlanWithDrain(ctx, group, plan, a.drainManager, group.DrainConfig); err != nil {
			return fmt.Errorf("execute plan: %w", err)
		}

		// Update last scale event
		group.LastScaleEvent = time.Now()
	}

	// Save updated group
	if err := a.dbClient.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	return nil
}

// ReconcileAll reconciles all active autoscale groups
func (a *AutoScaler) ReconcileAll(ctx context.Context) error {
	groups, err := a.dbClient.ListActiveGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}

	log.Printf("found %d active autoscale groups", len(groups))

	for _, group := range groups {
		if err := a.Reconcile(ctx, group.AutoScaleGroupID); err != nil {
			log.Printf("failed to reconcile group %s: %v", group.GroupName, err)
		}
	}

	return nil
}

// discoverInstances finds all instances for an autoscale group
func (a *AutoScaler) discoverInstances(ctx context.Context, group *AutoScaleGroup) ([]string, error) {
	// Query EC2 for instances with autoscale-group tag
	result, err := a.config.EC2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
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
		return nil, err
	}

	instanceIDs := make([]string, 0)
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil {
				instanceIDs = append(instanceIDs, aws.ToString(instance.InstanceId))
			}
		}
	}

	return instanceIDs, nil
}

// GetGroup retrieves a group by ID
func (a *AutoScaler) GetGroup(ctx context.Context, groupID string) (*AutoScaleGroup, error) {
	return a.dbClient.GetGroup(ctx, groupID)
}

// GetGroupByName retrieves a group by name
func (a *AutoScaler) GetGroupByName(ctx context.Context, name string) (*AutoScaleGroup, error) {
	return a.dbClient.GetGroupByName(ctx, name)
}

// CreateGroup creates a new autoscale group
func (a *AutoScaler) CreateGroup(ctx context.Context, group *AutoScaleGroup) error {
	return a.dbClient.CreateGroup(ctx, group)
}

// UpdateGroup updates an existing autoscale group
func (a *AutoScaler) UpdateGroup(ctx context.Context, group *AutoScaleGroup) error {
	return a.dbClient.UpdateGroup(ctx, group)
}

// DeleteGroup deletes an autoscale group
func (a *AutoScaler) DeleteGroup(ctx context.Context, groupID string) error {
	return a.dbClient.DeleteGroup(ctx, groupID)
}

// ListActiveGroups lists all active groups
func (a *AutoScaler) ListActiveGroups(ctx context.Context) ([]*AutoScaleGroup, error) {
	return a.dbClient.ListActiveGroups(ctx)
}

// combineHybridPolicies combines queue and metric policy recommendations
// Strategy:
//   - Scale up: take maximum (respond to either work or resource pressure)
//   - Scale down: take maximum (more conservative, only scale down when both agree)
func (a *AutoScaler) combineHybridPolicies(
	current int,
	queueDesired int, hasQueue bool,
	metricDesired int, hasMetric bool,
) int {
	// Single policy: use it directly
	if hasQueue && !hasMetric {
		return queueDesired
	}
	if hasMetric && !hasQueue {
		return metricDesired
	}
	if !hasQueue && !hasMetric {
		return current
	}

	// Hybrid: both policies active
	scaleUp := queueDesired > current || metricDesired > current

	if scaleUp {
		// Scale up: take maximum (aggressive, respond to either signal)
		if queueDesired > metricDesired {
			return queueDesired
		}
		return metricDesired
	}

	// Scale down: take maximum (conservative, need both to agree)
	// This means we scale down to the HIGHER of the two recommendations
	// Example: queue says 5, metric says 3 → scale to 5 (more conservative)
	if queueDesired > metricDesired {
		return queueDesired
	}
	return metricDesired
}

// getHybridScalingReason returns a human-readable reason for hybrid scaling decision
func (a *AutoScaler) getHybridScalingReason(
	oldDesired, newDesired int,
	queueDesired int, hasQueue bool,
	metricDesired int, hasMetric bool,
	queueDepth int,
	metricValue float64,
) string {
	if !hasQueue && !hasMetric {
		return "no policy"
	}

	if hasQueue && !hasMetric {
		return fmt.Sprintf("queue: %d msgs", queueDepth)
	}

	if hasMetric && !hasQueue {
		return fmt.Sprintf("metric: %.2f", metricValue)
	}

	// Hybrid
	scaleUp := newDesired > oldDesired
	if scaleUp {
		if queueDesired >= metricDesired {
			return fmt.Sprintf("queue: %d msgs (queue: %d, metric: %d)", queueDepth, queueDesired, metricDesired)
		}
		return fmt.Sprintf("metric: %.2f (queue: %d, metric: %d)", metricValue, queueDesired, metricDesired)
	}

	// Scale down: both policies agreed
	return fmt.Sprintf("both policies (queue: %d msgs → %d, metric: %.2f → %d)",
		queueDepth, queueDesired, metricValue, metricDesired)
}
