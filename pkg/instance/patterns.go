package instance

import (
	"fmt"
	"strings"
)

// ParseInstanceTypePattern parses instance type pattern into list
// Supports:
// - Single: "c5.large"
// - Pipe list: "c5.large|c5.xlarge|m5.large"
// - Wildcard: "c5.*" (expands to all c5 types, sorted small to large)
func ParseInstanceTypePattern(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty instance type pattern")
	}

	// If no wildcard or pipe, return as-is
	if !strings.Contains(pattern, "|") && !strings.Contains(pattern, "*") {
		return []string{pattern}, nil
	}

	// Pipe-separated list
	if strings.Contains(pattern, "|") {
		types := strings.Split(pattern, "|")
		result := make([]string, 0, len(types))
		for _, t := range types {
			trimmed := strings.TrimSpace(t)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("pipe-separated pattern resulted in empty list")
		}
		return result, nil
	}

	// Wildcard expansion (c5.*, m5.*)
	if strings.Contains(pattern, "*") {
		return expandWildcard(pattern)
	}

	return []string{pattern}, nil
}

// expandWildcard expands wildcard patterns to known instance types
func expandWildcard(pattern string) ([]string, error) {
	if !strings.HasSuffix(pattern, ".*") {
		return nil, fmt.Errorf("wildcard must be in format 'family.*' (e.g., 'c5.*')")
	}

	// Extract family (e.g., "c5" from "c5.*")
	family := strings.TrimSuffix(pattern, ".*")
	if family == "" {
		return nil, fmt.Errorf("empty instance family in wildcard pattern")
	}

	// Hardcoded list of common instance sizes (small to large)
	// This provides a reasonable fallback order for spot availability
	sizes := []string{
		"nano", "micro", "small", "medium", "large", "xlarge",
		"2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge",
		"18xlarge", "24xlarge", "32xlarge", "48xlarge", "56xlarge",
		"96xlarge", "112xlarge", "metal",
	}

	result := make([]string, 0, len(sizes))
	for _, size := range sizes {
		result = append(result, fmt.Sprintf("%s.%s", family, size))
	}

	return result, nil
}
