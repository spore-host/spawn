package staging

import (
	"fmt"
)

// CostEstimate represents the estimated cost of different staging strategies
type CostEstimate struct {
	DataSizeGB         int
	NumRegions         int
	InstancesPerRegion int
	TotalInstances     int

	// Option A: Single-region storage
	SingleRegionStorageCost float64
	CrossRegionTransferCost float64
	SingleRegionTotalCost   float64

	// Option B: Regional replication
	MultiRegionStorageCost float64
	ReplicationCost        float64
	MultiRegionTotalCost   float64

	// Savings
	Savings        float64
	SavingsPercent float64
	Recommendation string
}

const (
	// S3 storage pricing (per GB per month)
	s3StorageCostPerGB = 0.023

	// Data transfer pricing (per GB)
	crossRegionTransferCost = 0.09 // Cross-region out
	s3ReplicationCost       = 0.02 // S3 replication
	internetTransferCost    = 0.09 // Internet out

)

// EstimateStagingCost calculates the cost of different staging strategies
func EstimateStagingCost(dataSizeGB, numRegions, instancesPerRegion int) *CostEstimate {
	totalInstances := numRegions * instancesPerRegion

	// Option A: Single-region storage with cross-region transfers
	// - Store data in 1 region
	// - Each instance in other regions downloads via cross-region transfer
	singleRegionStorage := float64(dataSizeGB) * s3StorageCostPerGB

	// Instances in primary region: free download (same region)
	// Instances in other regions: cross-region transfer cost
	instancesNeedingCrossRegion := instancesPerRegion * (numRegions - 1)
	crossRegionTransfer := float64(dataSizeGB) * float64(instancesNeedingCrossRegion) * crossRegionTransferCost
	optionATotal := singleRegionStorage + crossRegionTransfer

	// Option B: Regional replication
	// - Store data in all regions
	// - One-time replication cost
	// - All instances download from local region (free)
	multiRegionStorage := float64(dataSizeGB) * float64(numRegions) * s3StorageCostPerGB
	replication := float64(dataSizeGB) * float64(numRegions-1) * s3ReplicationCost
	optionBTotal := multiRegionStorage + replication

	// Calculate savings
	savings := optionATotal - optionBTotal
	savingsPercent := 0.0
	if optionATotal > 0 {
		savingsPercent = (savings / optionATotal) * 100
	}

	recommendation := "Single-region storage"
	if savings > 0 {
		recommendation = "Regional replication"
	}

	return &CostEstimate{
		DataSizeGB:              dataSizeGB,
		NumRegions:              numRegions,
		InstancesPerRegion:      instancesPerRegion,
		TotalInstances:          totalInstances,
		SingleRegionStorageCost: singleRegionStorage,
		CrossRegionTransferCost: crossRegionTransfer,
		SingleRegionTotalCost:   optionATotal,
		MultiRegionStorageCost:  multiRegionStorage,
		ReplicationCost:         replication,
		MultiRegionTotalCost:    optionBTotal,
		Savings:                 savings,
		SavingsPercent:          savingsPercent,
		Recommendation:          recommendation,
	}
}

// FormatCostEstimate returns a formatted string representation of the cost estimate
func (e *CostEstimate) FormatCostEstimate() string {
	var output string

	output += "Data Staging Cost Comparison\n"
	output += "=============================\n\n"
	output += fmt.Sprintf("Dataset: %d GB\n", e.DataSizeGB)
	output += fmt.Sprintf("Regions: %d\n", e.NumRegions)
	output += fmt.Sprintf("Instances per region: %d\n", e.InstancesPerRegion)
	output += fmt.Sprintf("Total instances: %d\n\n", e.TotalInstances)

	output += "Option A: Single-Region Storage\n"
	output += fmt.Sprintf("  S3 storage: $%.2f/month (1 region × %d GB × $%.3f/GB)\n",
		e.SingleRegionStorageCost, e.DataSizeGB, s3StorageCostPerGB)
	output += fmt.Sprintf("  Cross-region transfer: $%.2f\n", e.CrossRegionTransferCost)
	output += fmt.Sprintf("    (%d instances in remote regions × %d GB × $%.2f/GB)\n",
		e.InstancesPerRegion*(e.NumRegions-1), e.DataSizeGB, crossRegionTransferCost)
	output += fmt.Sprintf("  Total: $%.2f\n\n", e.SingleRegionTotalCost)

	output += "Option B: Regional Replication\n"
	output += fmt.Sprintf("  S3 storage: $%.2f/month (%d regions × %d GB × $%.3f/GB)\n",
		e.MultiRegionStorageCost, e.NumRegions, e.DataSizeGB, s3StorageCostPerGB)
	output += fmt.Sprintf("  Replication cost: $%.2f (one-time)\n", e.ReplicationCost)
	output += fmt.Sprintf("    (%d regions × %d GB × $%.2f/GB)\n",
		e.NumRegions-1, e.DataSizeGB, s3ReplicationCost)
	output += "  Cross-region transfer: $0.00 (instances download from local region)\n"
	output += fmt.Sprintf("  Total: $%.2f\n\n", e.MultiRegionTotalCost)

	if e.Savings > 0 {
		output += fmt.Sprintf("💰 Savings: $%.2f (%.0f%% reduction)\n", e.Savings, e.SavingsPercent)
		output += fmt.Sprintf("✓ Recommendation: %s\n", e.Recommendation)
	} else {
		output += fmt.Sprintf("Recommendation: %s (no savings with replication)\n", e.Recommendation)
	}

	return output
}

// BreakEvenAnalysis calculates when replication becomes cost-effective
func BreakEvenAnalysis(dataSizeGB, numRegions int) string {
	var output string

	output += "\nBreak-Even Analysis\n"
	output += "===================\n\n"

	// Calculate cost per instance
	crossRegionCostPerInstance := float64(dataSizeGB) * crossRegionTransferCost
	replicationCostTotal := float64(dataSizeGB) * float64(numRegions-1) * s3ReplicationCost

	// How many remote region instances needed to break even?
	if crossRegionCostPerInstance > 0 {
		breakEvenInstances := int(replicationCostTotal / crossRegionCostPerInstance)
		output += fmt.Sprintf("Replication cost: $%.2f (one-time)\n", replicationCostTotal)
		output += fmt.Sprintf("Cross-region cost per instance: $%.2f\n", crossRegionCostPerInstance)
		output += "\n"
		output += fmt.Sprintf("Break-even point: %d instances in remote regions\n", breakEvenInstances)
		output += "\n"
		output += "Replication is cost-effective when:\n"
		output += fmt.Sprintf("  - Running %d+ instances across %d regions\n", breakEvenInstances, numRegions-1)
		output += fmt.Sprintf("  - %d+ instances per remote region (on average)\n", breakEvenInstances/(numRegions-1))
	}

	return output
}
