package regions

import (
	"fmt"
	"strings"
)

// ApplyConstraints applies region constraints to a list of regions
// Returns the filtered list of regions that match all constraints
func ApplyConstraints(allRegions []string, constraint *RegionConstraint) ([]string, error) {
	if constraint == nil || constraint.IsEmpty() {
		return allRegions, nil
	}

	candidates := make([]string, len(allRegions))
	copy(candidates, allRegions)

	// Step 1: Apply include filter (if specified)
	if len(constraint.Include) > 0 {
		candidates = filterInclude(candidates, constraint.Include)
	}

	// Step 2: Apply exclude filter
	if len(constraint.Exclude) > 0 {
		candidates = filterExclude(candidates, constraint.Exclude)
	}

	// Step 3: Apply geographic filter
	if len(constraint.Geographic) > 0 {
		candidates = filterGeographic(candidates, constraint.Geographic)
	}

	// Step 4: Validate non-empty
	if len(candidates) == 0 && !constraint.AllowEmpty {
		return nil, fmt.Errorf("no regions match constraints: %s", constraint.String())
	}

	return candidates, nil
}

// filterInclude keeps only regions that match the include patterns
func filterInclude(regions []string, patterns []string) []string {
	result := make([]string, 0, len(regions))
	for _, region := range regions {
		if matchesAny(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterExclude removes regions that match the exclude patterns
func filterExclude(regions []string, patterns []string) []string {
	result := make([]string, 0, len(regions))
	for _, region := range regions {
		if !matchesAny(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterGeographic keeps only regions in the specified geographic groups
func filterGeographic(regions []string, groups []string) []string {
	// Build set of allowed regions from geographic groups
	allowed := make(map[string]bool)
	for _, group := range groups {
		if groupRegions, ok := GeographicGroups[group]; ok {
			for _, r := range groupRegions {
				allowed[r] = true
			}
		}
	}

	// Filter regions
	result := make([]string, 0, len(regions))
	for _, region := range regions {
		if allowed[region] {
			result = append(result, region)
		}
	}
	return result
}

// matchesAny returns true if the region matches any of the patterns
func matchesAny(region string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchWildcard(region, pattern) {
			return true
		}
	}
	return false
}

// matchWildcard matches a string against a pattern with wildcard support
// Supports patterns like: "us-*", "eu-*", "ap-southeast-*"
func matchWildcard(s, pattern string) bool {
	// Exact match
	if s == pattern {
		return true
	}

	// Wildcard suffix match
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(s, prefix)
	}

	// Wildcard prefix match
	if strings.HasPrefix(pattern, "*") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(s, suffix)
	}

	return false
}

// Contains checks if a slice contains a string
func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ValidateConstraint validates that a constraint is well-formed
func ValidateConstraint(constraint *RegionConstraint) error {
	if constraint == nil {
		return nil
	}

	// Validate cost tier
	if constraint.CostTier != "" {
		validTiers := map[string]bool{
			"low":      true,
			"standard": true,
			"premium":  true,
		}
		if !validTiers[constraint.CostTier] {
			return fmt.Errorf("invalid cost tier: %s (valid: low, standard, premium)", constraint.CostTier)
		}
	}

	// Validate proximity region
	if constraint.ProximityFrom != "" && !IsValidRegion(constraint.ProximityFrom) {
		return fmt.Errorf("invalid proximity region: %s", constraint.ProximityFrom)
	}

	// Validate geographic groups
	for _, group := range constraint.Geographic {
		if _, ok := GeographicGroups[group]; !ok {
			return fmt.Errorf("invalid geographic group: %s", group)
		}
	}

	return nil
}
