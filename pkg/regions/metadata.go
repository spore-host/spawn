package regions

// RegionMetadata contains geographic and cost information for AWS regions
type RegionMetadata struct {
	Code        string  // AWS region code (e.g., "us-east-1")
	Name        string  // Human-readable name
	Geographic  string  // Geographic group: "us", "eu", "ap", "sa", "af", "me", "ca"
	Subregion   string  // Subregion: "north-america", "europe", "asia-pacific", etc.
	CostTier    string  // Cost tier: "low", "standard", "premium"
	Coordinates LatLong // Geographic coordinates for proximity calculations
}

// LatLong represents geographic coordinates
type LatLong struct {
	Latitude  float64
	Longitude float64
}

// AllAWSRegions contains metadata for all AWS regions
var AllAWSRegions = map[string]RegionMetadata{
	// US Regions
	"us-east-1": {
		Code:        "us-east-1",
		Name:        "US East (N. Virginia)",
		Geographic:  "us",
		Subregion:   "north-america",
		CostTier:    "standard",
		Coordinates: LatLong{38.13, -78.45},
	},
	"us-east-2": {
		Code:        "us-east-2",
		Name:        "US East (Ohio)",
		Geographic:  "us",
		Subregion:   "north-america",
		CostTier:    "standard",
		Coordinates: LatLong{40.42, -82.99},
	},
	"us-west-1": {
		Code:        "us-west-1",
		Name:        "US West (N. California)",
		Geographic:  "us",
		Subregion:   "north-america",
		CostTier:    "premium",
		Coordinates: LatLong{37.77, -122.42},
	},
	"us-west-2": {
		Code:        "us-west-2",
		Name:        "US West (Oregon)",
		Geographic:  "us",
		Subregion:   "north-america",
		CostTier:    "standard",
		Coordinates: LatLong{45.87, -119.69},
	},

	// Canada
	"ca-central-1": {
		Code:        "ca-central-1",
		Name:        "Canada (Central)",
		Geographic:  "ca",
		Subregion:   "north-america",
		CostTier:    "standard",
		Coordinates: LatLong{45.50, -73.57},
	},
	"ca-west-1": {
		Code:        "ca-west-1",
		Name:        "Canada West (Calgary)",
		Geographic:  "ca",
		Subregion:   "north-america",
		CostTier:    "standard",
		Coordinates: LatLong{51.05, -114.07},
	},

	// Europe
	"eu-west-1": {
		Code:        "eu-west-1",
		Name:        "Europe (Ireland)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{53.35, -6.26},
	},
	"eu-west-2": {
		Code:        "eu-west-2",
		Name:        "Europe (London)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{51.51, -0.13},
	},
	"eu-west-3": {
		Code:        "eu-west-3",
		Name:        "Europe (Paris)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{48.86, 2.35},
	},
	"eu-central-1": {
		Code:        "eu-central-1",
		Name:        "Europe (Frankfurt)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{50.11, 8.68},
	},
	"eu-central-2": {
		Code:        "eu-central-2",
		Name:        "Europe (Zurich)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "premium",
		Coordinates: LatLong{47.37, 8.54},
	},
	"eu-north-1": {
		Code:        "eu-north-1",
		Name:        "Europe (Stockholm)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "low",
		Coordinates: LatLong{59.33, 18.06},
	},
	"eu-south-1": {
		Code:        "eu-south-1",
		Name:        "Europe (Milan)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{45.46, 9.19},
	},
	"eu-south-2": {
		Code:        "eu-south-2",
		Name:        "Europe (Spain)",
		Geographic:  "eu",
		Subregion:   "europe",
		CostTier:    "standard",
		Coordinates: LatLong{40.42, -3.70},
	},

	// Asia Pacific
	"ap-southeast-1": {
		Code:        "ap-southeast-1",
		Name:        "Asia Pacific (Singapore)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "standard",
		Coordinates: LatLong{1.29, 103.85},
	},
	"ap-southeast-2": {
		Code:        "ap-southeast-2",
		Name:        "Asia Pacific (Sydney)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "standard",
		Coordinates: LatLong{-33.87, 151.21},
	},
	"ap-southeast-3": {
		Code:        "ap-southeast-3",
		Name:        "Asia Pacific (Jakarta)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "standard",
		Coordinates: LatLong{-6.21, 106.85},
	},
	"ap-southeast-4": {
		Code:        "ap-southeast-4",
		Name:        "Asia Pacific (Melbourne)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "standard",
		Coordinates: LatLong{-37.81, 144.96},
	},
	"ap-northeast-1": {
		Code:        "ap-northeast-1",
		Name:        "Asia Pacific (Tokyo)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "premium",
		Coordinates: LatLong{35.68, 139.77},
	},
	"ap-northeast-2": {
		Code:        "ap-northeast-2",
		Name:        "Asia Pacific (Seoul)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "standard",
		Coordinates: LatLong{37.57, 126.98},
	},
	"ap-northeast-3": {
		Code:        "ap-northeast-3",
		Name:        "Asia Pacific (Osaka)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "premium",
		Coordinates: LatLong{34.69, 135.50},
	},
	"ap-south-1": {
		Code:        "ap-south-1",
		Name:        "Asia Pacific (Mumbai)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "low",
		Coordinates: LatLong{19.08, 72.88},
	},
	"ap-south-2": {
		Code:        "ap-south-2",
		Name:        "Asia Pacific (Hyderabad)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "low",
		Coordinates: LatLong{17.39, 78.49},
	},
	"ap-east-1": {
		Code:        "ap-east-1",
		Name:        "Asia Pacific (Hong Kong)",
		Geographic:  "ap",
		Subregion:   "asia-pacific",
		CostTier:    "premium",
		Coordinates: LatLong{22.32, 114.17},
	},

	// South America
	"sa-east-1": {
		Code:        "sa-east-1",
		Name:        "South America (São Paulo)",
		Geographic:  "sa",
		Subregion:   "south-america",
		CostTier:    "premium",
		Coordinates: LatLong{-23.55, -46.64},
	},

	// Middle East
	"me-south-1": {
		Code:        "me-south-1",
		Name:        "Middle East (Bahrain)",
		Geographic:  "me",
		Subregion:   "middle-east",
		CostTier:    "premium",
		Coordinates: LatLong{26.07, 50.56},
	},
	"me-central-1": {
		Code:        "me-central-1",
		Name:        "Middle East (UAE)",
		Geographic:  "me",
		Subregion:   "middle-east",
		CostTier:    "premium",
		Coordinates: LatLong{25.28, 55.30},
	},

	// Africa
	"af-south-1": {
		Code:        "af-south-1",
		Name:        "Africa (Cape Town)",
		Geographic:  "af",
		Subregion:   "africa",
		CostTier:    "premium",
		Coordinates: LatLong{-33.92, 18.42},
	},

	// Israel
	"il-central-1": {
		Code:        "il-central-1",
		Name:        "Israel (Tel Aviv)",
		Geographic:  "me",
		Subregion:   "middle-east",
		CostTier:    "premium",
		Coordinates: LatLong{32.09, 34.78},
	},
}

// GeographicGroups maps geographic identifiers to region codes
var GeographicGroups = map[string][]string{
	// US regions
	"us": {
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	},

	// North America (US + Canada)
	"north-america": {
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"ca-central-1", "ca-west-1",
	},

	// Canada
	"ca": {
		"ca-central-1", "ca-west-1",
	},

	// Europe
	"eu": {
		"eu-west-1", "eu-west-2", "eu-west-3",
		"eu-central-1", "eu-central-2",
		"eu-north-1",
		"eu-south-1", "eu-south-2",
	},
	"europe": {
		"eu-west-1", "eu-west-2", "eu-west-3",
		"eu-central-1", "eu-central-2",
		"eu-north-1",
		"eu-south-1", "eu-south-2",
	},

	// Asia Pacific
	"ap": {
		"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
		"ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
		"ap-south-1", "ap-south-2",
		"ap-east-1",
	},
	"asia-pacific": {
		"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
		"ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
		"ap-south-1", "ap-south-2",
		"ap-east-1",
	},

	// South America
	"sa": {
		"sa-east-1",
	},
	"south-america": {
		"sa-east-1",
	},

	// Middle East
	"me": {
		"me-south-1", "me-central-1", "il-central-1",
	},
	"middle-east": {
		"me-south-1", "me-central-1", "il-central-1",
	},

	// Africa
	"af": {
		"af-south-1",
	},
	"africa": {
		"af-south-1",
	},
}

// GetRegionMetadata returns metadata for a region, or nil if not found
func GetRegionMetadata(region string) *RegionMetadata {
	if meta, ok := AllAWSRegions[region]; ok {
		return &meta
	}
	return nil
}

// GetGeographicGroup returns all regions in a geographic group
func GetGeographicGroup(group string) []string {
	if regions, ok := GeographicGroups[group]; ok {
		return regions
	}
	return nil
}

// IsValidRegion checks if a region code is valid
func IsValidRegion(region string) bool {
	_, ok := AllAWSRegions[region]
	return ok
}
