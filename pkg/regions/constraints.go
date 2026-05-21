package regions

// RegionConstraint defines constraints for region selection
type RegionConstraint struct {
	// Include is an explicit list of regions to include (supports wildcards)
	// Example: ["us-east-1", "us-west-*"]
	Include []string `json:"include,omitempty" dynamodbav:"include,omitempty"`

	// Exclude is a list of regions to exclude (supports wildcards)
	// Example: ["eu-*", "ap-*"]
	Exclude []string `json:"exclude,omitempty" dynamodbav:"exclude,omitempty"`

	// Geographic is a list of geographic groups to include
	// Valid values: "us", "eu", "ap", "sa", "af", "me", "ca"
	//               "north-america", "europe", "asia-pacific", etc.
	Geographic []string `json:"geographic,omitempty" dynamodbav:"geographic,omitempty"`

	// ProximityFrom specifies a region to calculate proximity scores from
	// Regions closer to this region will be prioritized
	ProximityFrom string `json:"proximity_from,omitempty" dynamodbav:"proximity_from,omitempty"`

	// CostTier specifies the preferred cost tier
	// Valid values: "low", "standard", "premium"
	CostTier string `json:"cost_tier,omitempty" dynamodbav:"cost_tier,omitempty"`

	// AllowEmpty allows the constraint to result in an empty region set
	// If false, an error will be returned if no regions match
	AllowEmpty bool `json:"allow_empty,omitempty" dynamodbav:"allow_empty,omitempty"`
}

// RegionPriority represents a region with its calculated priority score
type RegionPriority struct {
	Region   string         // AWS region code
	Score    float64        // Priority score (0.0-1.0, higher is better)
	Metadata RegionMetadata // Region metadata
}

// IsEmpty returns true if the constraint has no filters applied
func (c *RegionConstraint) IsEmpty() bool {
	return len(c.Include) == 0 &&
		len(c.Exclude) == 0 &&
		len(c.Geographic) == 0 &&
		c.ProximityFrom == "" &&
		c.CostTier == ""
}

// String returns a human-readable representation of the constraint
func (c *RegionConstraint) String() string {
	parts := []string{}

	if len(c.Include) > 0 {
		parts = append(parts, "include="+joinStrings(c.Include, ","))
	}
	if len(c.Exclude) > 0 {
		parts = append(parts, "exclude="+joinStrings(c.Exclude, ","))
	}
	if len(c.Geographic) > 0 {
		parts = append(parts, "geographic="+joinStrings(c.Geographic, ","))
	}
	if c.ProximityFrom != "" {
		parts = append(parts, "proximity_from="+c.ProximityFrom)
	}
	if c.CostTier != "" {
		parts = append(parts, "cost_tier="+c.CostTier)
	}

	if len(parts) == 0 {
		return "no constraints"
	}

	return joinStrings(parts, ", ")
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}
