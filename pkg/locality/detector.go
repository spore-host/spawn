package locality

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
)

// RegionMismatch represents a detected region mismatch between storage and compute
type RegionMismatch struct {
	ResourceType   string  // "EFS", "FSx Lustre", "S3"
	ResourceID     string  // fs-xxx, bucket name, etc.
	ResourceRegion string  // Region where storage lives
	LaunchRegion   string  // Region where instances will launch
	EstimatedCost  float64 // Cross-region transfer cost per GB
}

// DataLocalityWarning contains information about region mismatches
type DataLocalityWarning struct {
	Mismatches     []RegionMismatch
	HasMismatches  bool
	TotalCostPerGB float64
	AvgLatencyMs   int
	Recommendation string
}

// DetectEFSRegion detects the region of an EFS filesystem
func DetectEFSRegion(ctx context.Context, cfg aws.Config, efsID string) (string, error) {
	// Try common regions
	regions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
	}

	for _, region := range regions {
		regionalCfg := cfg.Copy()
		regionalCfg.Region = region
		efsClient := efs.NewFromConfig(regionalCfg)

		result, err := efsClient.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{
			FileSystemId: aws.String(efsID),
		})
		if err != nil {
			continue // Not in this region
		}

		if len(result.FileSystems) > 0 {
			return region, nil
		}
	}

	return "", fmt.Errorf("EFS filesystem %s not found in any region", efsID)
}

// DetectFSxRegion detects the region of an FSx filesystem
func DetectFSxRegion(ctx context.Context, cfg aws.Config, fsxID string) (string, error) {
	// Try common regions
	regions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
	}

	for _, region := range regions {
		regionalCfg := cfg.Copy()
		regionalCfg.Region = region
		fsxClient := fsx.NewFromConfig(regionalCfg)

		result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
			FileSystemIds: []string{fsxID},
		})
		if err != nil {
			continue // Not in this region
		}

		if len(result.FileSystems) > 0 {
			return region, nil
		}
	}

	return "", fmt.Errorf("FSx filesystem %s not found in any region", fsxID)
}

// CheckDataLocality checks for region mismatches between storage and compute
func CheckDataLocality(ctx context.Context, cfg aws.Config, launchRegion, efsID, fsxID string) (*DataLocalityWarning, error) {
	warning := &DataLocalityWarning{
		Mismatches:    []RegionMismatch{},
		HasMismatches: false,
	}

	// Check EFS region
	if efsID != "" {
		efsRegion, err := DetectEFSRegion(ctx, cfg, efsID)
		if err != nil {
			return nil, fmt.Errorf("failed to detect EFS region: %w", err)
		}

		if efsRegion != launchRegion {
			warning.HasMismatches = true
			warning.Mismatches = append(warning.Mismatches, RegionMismatch{
				ResourceType:   "EFS",
				ResourceID:     efsID,
				ResourceRegion: efsRegion,
				LaunchRegion:   launchRegion,
				EstimatedCost:  getCrossRegionCost(efsRegion, launchRegion),
			})
		}
	}

	// Check FSx region
	if fsxID != "" {
		fsxRegion, err := DetectFSxRegion(ctx, cfg, fsxID)
		if err != nil {
			return nil, fmt.Errorf("failed to detect FSx region: %w", err)
		}

		if fsxRegion != launchRegion {
			warning.HasMismatches = true
			warning.Mismatches = append(warning.Mismatches, RegionMismatch{
				ResourceType:   "FSx Lustre",
				ResourceID:     fsxID,
				ResourceRegion: fsxRegion,
				LaunchRegion:   launchRegion,
				EstimatedCost:  getCrossRegionCost(fsxRegion, launchRegion),
			})
		}
	}

	// Calculate total cost and recommendations
	if warning.HasMismatches {
		totalCost := 0.0
		for _, mismatch := range warning.Mismatches {
			totalCost += mismatch.EstimatedCost
		}
		warning.TotalCostPerGB = totalCost
		warning.AvgLatencyMs = estimateLatency(warning.Mismatches[0].ResourceRegion, launchRegion)
		warning.Recommendation = fmt.Sprintf("Launch instances in %s for best performance and lowest cost", warning.Mismatches[0].ResourceRegion)
	}

	return warning, nil
}

// getCrossRegionCost returns the estimated data transfer cost per GB for cross-region transfers
func getCrossRegionCost(sourceRegion, destRegion string) float64 {
	// Within same continent: ~$0.02/GB
	// Cross-continent: ~$0.05-0.10/GB
	// Simplified: use $0.02/GB for same continent, $0.08/GB for cross-continent

	sameContinentPairs := map[string][]string{
		"us-east-1":      {"us-east-2", "us-west-1", "us-west-2"},
		"us-east-2":      {"us-east-1", "us-west-1", "us-west-2"},
		"us-west-1":      {"us-east-1", "us-east-2", "us-west-2"},
		"us-west-2":      {"us-east-1", "us-east-2", "us-west-1"},
		"eu-west-1":      {"eu-central-1", "eu-west-2", "eu-north-1"},
		"eu-central-1":   {"eu-west-1", "eu-west-2", "eu-north-1"},
		"ap-southeast-1": {"ap-northeast-1", "ap-southeast-2", "ap-south-1"},
		"ap-northeast-1": {"ap-southeast-1", "ap-southeast-2", "ap-south-1"},
	}

	// Check if same continent
	if pairs, ok := sameContinentPairs[sourceRegion]; ok {
		for _, region := range pairs {
			if region == destRegion {
				return 0.02 // Same continent
			}
		}
	}

	// Cross-continent
	return 0.08
}

// estimateLatency estimates cross-region latency in milliseconds
func estimateLatency(sourceRegion, destRegion string) int {
	// Simplified latency estimates
	// Same region: <1ms
	// Same continent: 20-80ms
	// Cross-continent: 100-200ms

	if sourceRegion == destRegion {
		return 0
	}

	sameContinentPairs := map[string][]string{
		"us-east-1":      {"us-east-2", "us-west-1", "us-west-2"},
		"us-east-2":      {"us-east-1", "us-west-1", "us-west-2"},
		"us-west-1":      {"us-east-1", "us-east-2", "us-west-2"},
		"us-west-2":      {"us-east-1", "us-east-2", "us-west-1"},
		"eu-west-1":      {"eu-central-1"},
		"eu-central-1":   {"eu-west-1"},
		"ap-southeast-1": {"ap-northeast-1"},
		"ap-northeast-1": {"ap-southeast-1"},
	}

	if pairs, ok := sameContinentPairs[sourceRegion]; ok {
		for _, region := range pairs {
			if region == destRegion {
				return 50 // Same continent
			}
		}
	}

	return 150 // Cross-continent
}

// FormatWarning formats a data locality warning for display
func (w *DataLocalityWarning) FormatWarning() string {
	if !w.HasMismatches {
		return ""
	}

	msg := "\n⚠️  Data Locality Warning:\n\n"

	for _, mismatch := range w.Mismatches {
		msg += fmt.Sprintf("   %s %s is in %s\n", mismatch.ResourceType, mismatch.ResourceID, mismatch.ResourceRegion)
		msg += fmt.Sprintf("   You are launching instances in %s\n\n", mismatch.LaunchRegion)
	}

	msg += "   Cross-region data transfer costs:\n"
	msg += fmt.Sprintf("   - Per GB: $%.2f\n", w.TotalCostPerGB)
	msg += fmt.Sprintf("   - Example: 1 TB = $%.2f\n", w.TotalCostPerGB*1024)
	msg += fmt.Sprintf("   - Latency penalty: ~%dms\n\n", w.AvgLatencyMs)

	msg += fmt.Sprintf("   💡 Recommendation: %s\n\n", w.Recommendation)

	return msg
}
