package regions

import (
	"math"
	"sort"
)

// ScoreRegions calculates priority scores for regions based on constraints
// Returns a sorted list of regions with scores (highest score first)
func ScoreRegions(regions []string, constraint *RegionConstraint) []RegionPriority {
	priorities := make([]RegionPriority, 0, len(regions))

	for _, region := range regions {
		score := calculateScore(region, constraint)
		meta := AllAWSRegions[region]
		priorities = append(priorities, RegionPriority{
			Region:   region,
			Score:    score,
			Metadata: meta,
		})
	}

	// Sort by score descending (highest first)
	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i].Score > priorities[j].Score
	})

	return priorities
}

// calculateScore calculates a priority score for a region
// Score is between 0.0 and 1.0, with 1.0 being highest priority
func calculateScore(region string, constraint *RegionConstraint) float64 {
	if constraint == nil || constraint.IsEmpty() {
		return 0.5 // Neutral score
	}

	score := 0.5 // Base score

	// Factor 1: Proximity (if specified)
	if constraint.ProximityFrom != "" {
		proximityScore := calculateProximityScore(region, constraint.ProximityFrom)
		score = score*0.4 + proximityScore*0.6
	}

	// Factor 2: Cost tier (if specified)
	if constraint.CostTier != "" {
		costScore := calculateCostScore(region, constraint.CostTier)
		score = score*0.7 + costScore*0.3
	}

	return score
}

// calculateProximityScore calculates a score based on geographic proximity
// Closer regions get higher scores (1.0 = same region, 0.0 = opposite side of Earth)
func calculateProximityScore(region, fromRegion string) float64 {
	fromMeta, ok1 := AllAWSRegions[fromRegion]
	toMeta, ok2 := AllAWSRegions[region]

	if !ok1 || !ok2 {
		return 0.5 // Neutral if metadata unavailable
	}

	// Same region gets perfect score
	if region == fromRegion {
		return 1.0
	}

	// Calculate great circle distance
	distance := haversineDistance(fromMeta.Coordinates, toMeta.Coordinates)

	// Normalize distance to score
	// 0 km = 1.0, 20000 km (half Earth) = 0.0
	const maxDistance = 20000.0 // km
	score := 1.0 - (distance / maxDistance)

	if score < 0 {
		score = 0
	}

	return score
}

// calculateCostScore calculates a score based on cost tier preference
func calculateCostScore(region, preferredTier string) float64 {
	meta, ok := AllAWSRegions[region]
	if !ok {
		return 0.5 // Neutral if metadata unavailable
	}

	// Exact match gets full score
	if meta.CostTier == preferredTier {
		return 1.0
	}

	// Partial credit for adjacent tiers
	tierOrder := map[string]int{
		"low":      1,
		"standard": 2,
		"premium":  3,
	}

	preferredOrder, ok1 := tierOrder[preferredTier]
	actualOrder, ok2 := tierOrder[meta.CostTier]

	if !ok1 || !ok2 {
		return 0.5
	}

	// Distance between tiers
	distance := math.Abs(float64(preferredOrder - actualOrder))

	// 0 = 1.0, 1 = 0.5, 2 = 0.0
	score := 1.0 - (distance / 2.0)

	if score < 0 {
		score = 0
	}

	return score
}

// haversineDistance calculates the great circle distance between two points
// Returns distance in kilometers
func haversineDistance(from, to LatLong) float64 {
	const earthRadius = 6371.0 // km

	// Convert to radians
	lat1 := from.Latitude * math.Pi / 180
	lat2 := to.Latitude * math.Pi / 180
	deltaLat := (to.Latitude - from.Latitude) * math.Pi / 180
	deltaLon := (to.Longitude - from.Longitude) * math.Pi / 180

	// Haversine formula
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// GetRegionsByPriority returns regions sorted by priority
func GetRegionsByPriority(priorities []RegionPriority) []string {
	regions := make([]string, len(priorities))
	for i, p := range priorities {
		regions[i] = p.Region
	}
	return regions
}
