package cost

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/libs/pricing"
)

const (
	dynamoTableName = "spawn-sweep-orchestration"
)

// RegionalCost represents cost for a specific region
type RegionalCost struct {
	Region        string
	InstanceHours float64
	EstimatedCost float64
	InstanceCount int
	InstanceType  string
}

// InstanceTypeCost represents cost for a specific instance type
type InstanceTypeCost struct {
	InstanceType  string
	InstanceHours float64
	EstimatedCost float64
	InstanceCount int
}

// CostBreakdown represents a detailed cost breakdown
type CostBreakdown struct {
	SweepID            string
	TotalCost          float64
	TotalInstanceHours float64
	Budget             float64
	BudgetRemaining    float64
	BudgetExceeded     bool
	ByRegion           []RegionalCost
	ByInstanceType     []InstanceTypeCost

	// Resource-level cost breakdown
	ComputeCost float64
	StorageCost float64
	NetworkCost float64

	// Time breakdown
	TotalHours   float64
	RunningHours float64
	StoppedHours float64
	Utilization  float64 // RunningHours / TotalHours * 100

	// Cloud economics
	StickerPriceAllIn    float64 // compute+storage+network if running 24/7
	EffectiveCostPerHour float64 // total / lifetime hours
	SavingsPercent       float64
}

// StateTransition records when an instance changed state
type StateTransition struct {
	Timestamp string `dynamodbav:"timestamp"`
	State     string `dynamodbav:"state"`
}

// EBSVolume represents an attached EBS volume
type EBSVolume struct {
	VolumeID   string `dynamodbav:"volume_id"`
	VolumeType string `dynamodbav:"volume_type"`
	SizeGB     int    `dynamodbav:"size_gb"`
	IOPS       int    `dynamodbav:"iops,omitempty"`
	IsRoot     bool   `dynamodbav:"is_root"`
}

// InstanceResources tracks resources attached to an instance
type InstanceResources struct {
	EBSVolumes []EBSVolume `dynamodbav:"ebs_volumes"`
	IPv4Count  int         `dynamodbav:"ipv4_count"`
}

// SweepInstance represents an instance in the sweep
type SweepInstance struct {
	Index              int                `dynamodbav:"index"`
	Region             string             `dynamodbav:"region"`
	InstanceID         string             `dynamodbav:"instance_id"`
	RequestedType      string             `dynamodbav:"requested_type,omitempty"`
	ActualType         string             `dynamodbav:"actual_type,omitempty"`
	State              string             `dynamodbav:"state"`
	LaunchedAt         string             `dynamodbav:"launched_at"`
	TerminatedAt       string             `dynamodbav:"terminated_at,omitempty"`
	ErrorMessage       string             `dynamodbav:"error_message,omitempty"`
	InstanceHours      float64            `dynamodbav:"instance_hours,omitempty"`
	EstimatedCost      float64            `dynamodbav:"estimated_cost,omitempty"`
	StateHistory       []StateTransition  `dynamodbav:"state_history,omitempty"`
	Resources          *InstanceResources `dynamodbav:"resources,omitempty"`
	HibernationEnabled bool               `dynamodbav:"hibernation_enabled,omitempty"`
}

// SweepRecord represents the minimal sweep record for cost calculation
type SweepRecord struct {
	SweepID       string          `dynamodbav:"sweep_id"`
	EstimatedCost float64         `dynamodbav:"estimated_cost,omitempty"`
	Budget        float64         `dynamodbav:"budget,omitempty"`
	Instances     []SweepInstance `dynamodbav:"instances"`
}

// Client provides cost tracking operations
type Client struct {
	db *dynamodb.Client
}

// NewClient creates a new cost client
func NewClient(db *dynamodb.Client) *Client {
	return &Client{db: db}
}

// GetCostBreakdown retrieves and calculates cost breakdown for a sweep
func (c *Client) GetCostBreakdown(ctx context.Context, sweepID string) (*CostBreakdown, error) {
	result, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoTableName),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get sweep record: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("sweep not found: %s", sweepID)
	}

	var sweep SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &sweep); err != nil {
		return nil, fmt.Errorf("unmarshal sweep: %w", err)
	}

	breakdown := &CostBreakdown{
		SweepID:        sweepID,
		TotalCost:      sweep.EstimatedCost,
		Budget:         sweep.Budget,
		ByRegion:       make([]RegionalCost, 0),
		ByInstanceType: make([]InstanceTypeCost, 0),
	}

	regionMap := make(map[string]*RegionalCost)
	typeMap := make(map[string]*InstanceTypeCost)

	now := time.Now()

	for _, inst := range sweep.Instances {
		if inst.InstanceID == "" {
			continue
		}

		instType := inst.ActualType
		if instType == "" {
			instType = inst.RequestedType
		}

		var endTime time.Time
		if inst.TerminatedAt != "" {
			if t, err := time.Parse(time.RFC3339, inst.TerminatedAt); err == nil {
				endTime = t
			} else {
				endTime = now
			}
		} else {
			endTime = now
		}

		totalHrs := totalHours(inst.LaunchedAt, endTime)
		runningHrs := calculateRunningHours(inst.StateHistory, inst.LaunchedAt, endTime)
		computeCost := calculateComputeCost(inst, runningHrs)
		storageCost := calculateStorageCost(inst, totalHrs)
		networkCost := calculateNetworkCost(inst, totalHrs)

		breakdown.TotalInstanceHours += runningHrs
		breakdown.TotalHours += totalHrs
		breakdown.RunningHours += runningHrs
		breakdown.ComputeCost += computeCost
		breakdown.StorageCost += storageCost
		breakdown.NetworkCost += networkCost

		// Aggregate by region
		if regionMap[inst.Region] == nil {
			regionMap[inst.Region] = &RegionalCost{Region: inst.Region}
		}
		regionMap[inst.Region].InstanceHours += runningHrs
		regionMap[inst.Region].EstimatedCost += computeCost + storageCost + networkCost
		regionMap[inst.Region].InstanceCount++

		// Aggregate by instance type
		if typeMap[instType] == nil {
			typeMap[instType] = &InstanceTypeCost{InstanceType: instType}
		}
		typeMap[instType].InstanceHours += runningHrs
		typeMap[instType].EstimatedCost += computeCost + storageCost + networkCost
		typeMap[instType].InstanceCount++
	}

	// Recalculate total cost from components
	breakdown.TotalCost = breakdown.ComputeCost + breakdown.StorageCost + breakdown.NetworkCost
	if breakdown.TotalCost == 0 && sweep.EstimatedCost > 0 {
		// Fall back to stored estimate if we computed zero (no state history)
		breakdown.TotalCost = sweep.EstimatedCost
	}

	// Time metrics
	breakdown.StoppedHours = breakdown.TotalHours - breakdown.RunningHours
	if breakdown.TotalHours > 0 {
		breakdown.Utilization = (breakdown.RunningHours / breakdown.TotalHours) * 100
	}
	if breakdown.TotalHours > 0 {
		breakdown.EffectiveCostPerHour = breakdown.TotalCost / breakdown.TotalHours
	}

	// Convert maps to slices
	for _, rc := range regionMap {
		breakdown.ByRegion = append(breakdown.ByRegion, *rc)
	}
	for _, tc := range typeMap {
		breakdown.ByInstanceType = append(breakdown.ByInstanceType, *tc)
	}

	// Sort by cost (highest first)
	sort.Slice(breakdown.ByRegion, func(i, j int) bool {
		return breakdown.ByRegion[i].EstimatedCost > breakdown.ByRegion[j].EstimatedCost
	})
	sort.Slice(breakdown.ByInstanceType, func(i, j int) bool {
		return breakdown.ByInstanceType[i].EstimatedCost > breakdown.ByInstanceType[j].EstimatedCost
	})

	// Budget status
	if breakdown.Budget > 0 {
		breakdown.BudgetRemaining = breakdown.Budget - breakdown.TotalCost
		breakdown.BudgetExceeded = breakdown.TotalCost > breakdown.Budget
	}

	return breakdown, nil
}

// calculateComputeCost returns compute cost for an instance given running hours
func calculateComputeCost(inst SweepInstance, runningHrs float64) float64 {
	instType := inst.ActualType
	if instType == "" {
		instType = inst.RequestedType
	}
	if instType == "" {
		instType = "t3.micro"
	}
	hourlyRate := pricing.GetEC2HourlyRate(inst.Region, instType)
	return runningHrs * hourlyRate
}

// calculateStorageCost returns EBS storage cost for lifetime hours
func calculateStorageCost(inst SweepInstance, lifetimeHours float64) float64 {
	if inst.Resources == nil || len(inst.Resources.EBSVolumes) == 0 {
		return 0
	}
	lifetimeMonths := lifetimeHours / (24 * 30) // approximate hours per month
	total := 0.0
	for _, vol := range inst.Resources.EBSVolumes {
		if vol.SizeGB <= 0 {
			continue
		}
		monthlyRatePerGB := pricing.GetEBSMonthlyRate(inst.Region, vol.VolumeType, vol.SizeGB, vol.IOPS)
		total += monthlyRatePerGB * float64(vol.SizeGB) * lifetimeMonths
	}
	return total
}

// calculateNetworkCost returns IPv4 address cost for lifetime hours
func calculateNetworkCost(inst SweepInstance, lifetimeHours float64) float64 {
	if inst.Resources == nil || inst.Resources.IPv4Count == 0 {
		return 0
	}
	return float64(inst.Resources.IPv4Count) * pricing.GetIPv4HourlyRate() * lifetimeHours
}

// calculateRunningHours returns hours the instance was in "running" state
func calculateRunningHours(history []StateTransition, launchedAt string, end time.Time) float64 {
	if len(history) == 0 {
		// No history: use total elapsed time as fallback
		launched, err := time.Parse(time.RFC3339, launchedAt)
		if err != nil {
			return 0
		}
		hours := end.Sub(launched).Hours()
		if hours < 0 {
			return 0
		}
		return hours
	}

	totalRunning := 0.0
	for i, transition := range history {
		if transition.State != "running" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, transition.Timestamp)
		if err != nil {
			continue
		}
		var endTs time.Time
		if i+1 < len(history) {
			if nextTs, err := time.Parse(time.RFC3339, history[i+1].Timestamp); err == nil {
				endTs = nextTs
			} else {
				endTs = end
			}
		} else {
			endTs = end
		}
		if hrs := endTs.Sub(ts).Hours(); hrs > 0 {
			totalRunning += hrs
		}
	}
	return totalRunning
}

// totalHours returns elapsed hours from launch to end
func totalHours(launchedAt string, end time.Time) float64 {
	launched, err := time.Parse(time.RFC3339, launchedAt)
	if err != nil {
		return 0
	}
	hours := end.Sub(launched).Hours()
	if hours < 0 {
		return 0
	}
	return hours
}
