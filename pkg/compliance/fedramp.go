package compliance

// FedRAMPLevel represents FedRAMP authorization levels
type FedRAMPLevel string

const (
	FedRAMPLow      FedRAMPLevel = "fedramp-low"
	FedRAMPModerate FedRAMPLevel = "fedramp-moderate"
	FedRAMPHigh     FedRAMPLevel = "fedramp-high"
)

// FedRAMPControlSet returns the control set for a specific FedRAMP level
// FedRAMP is based on NIST 800-53, so we map to baselines
func FedRAMPControlSet(level FedRAMPLevel) *ControlSet {
	var baseline Baseline
	var name, description string

	switch level {
	case FedRAMPLow:
		baseline = BaselineLow
		name = "FedRAMP Low Authorization"
		description = "Cloud service providers offering low-impact SaaS"
	case FedRAMPModerate:
		baseline = BaselineModerate
		name = "FedRAMP Moderate Authorization"
		description = "Cloud service providers offering moderate-impact SaaS"
	case FedRAMPHigh:
		baseline = BaselineHigh
		name = "FedRAMP High Authorization"
		description = "Cloud service providers offering high-impact SaaS"
	default:
		baseline = BaselineLow
		name = "FedRAMP Low Authorization"
		description = "Cloud service providers offering low-impact SaaS"
	}

	// Get the corresponding NIST 800-53 baseline
	controlSet := NIST80053ControlSet(baseline)

	// Override name and description for FedRAMP branding
	controlSet.Name = name
	controlSet.Description = description

	return controlSet
}

// MapFedRAMPToBaseline maps FedRAMP levels to NIST 800-53 baselines
func MapFedRAMPToBaseline(level FedRAMPLevel) Baseline {
	switch level {
	case FedRAMPLow:
		return BaselineLow
	case FedRAMPModerate:
		return BaselineModerate
	case FedRAMPHigh:
		return BaselineHigh
	default:
		return BaselineLow
	}
}
