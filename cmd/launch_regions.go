package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/regions"
	"github.com/spore-host/spawn/pkg/sweep"
)

// instanceTypeOfferer is the one EC2 question the offerings filter asks. An
// interface rather than *aws.Client so the filter's behaviour — including what
// it does when the call FAILS, which is the part with a real decision in it —
// can be tested without AWS.
type instanceTypeOfferer interface {
	InstanceTypeOfferedInRegion(ctx context.Context, region, instanceType string) (bool, error)
}

// regionScore is one candidate region's ranking inputs. Package-scoped so the
// offerings filter can take a slice of them.
type regionScore struct {
	region         string
	latency        time.Duration
	continentMatch bool
}

// detectBestRegion selects the closest AWS region that actually OFFERS the
// requested instance type and is allowed by SCPs, preferring the user's own
// continent and then the lowest latency. It returns the region and a short
// human-readable reason for the choice.
//
// The doc comment has always claimed the instance type was honoured. It was not:
// instanceType was a parameter the body never referenced, so the ranking was
// latency and geolocation only (#732). The consequence was a launch placed in a
// region that does not offer the type at all, failing with EC2's opaque
// `Unsupported: The requested configuration is currently not supported` — and,
// because the ranking is a live latency measurement, the same command could
// choose a working region or a broken one on different runs.
//
// The offerings call is what makes the parameter real. It is one
// DescribeInstanceTypeOfferings per candidate, run concurrently, and it answers
// the question definitively rather than by inference.
func detectBestRegion(ctx context.Context, instanceType string) (string, string, error) {
	// First, get allowed regions from AWS (respects SCPs)
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to create AWS client: %w", err)
	}

	allowedRegions, err := awsClient.GetEnabledRegions(ctx)
	if err != nil || len(allowedRegions) == 0 {
		// Fallback to common regions if we can't get the list
		allowedRegions = []string{
			"us-east-1", "us-west-2", "eu-west-1",
			"ap-southeast-1", "us-east-2", "eu-central-1",
		}
	}

	// Try to detect user's location via IP geolocation
	userContinent := detectUserContinent()

	results := make([]regionScore, 0, len(allowedRegions))

	for _, region := range allowedRegions {
		start := time.Now()

		// Quick connectivity test to EC2 endpoint
		endpoint := fmt.Sprintf("ec2.%s.amazonaws.com", region)
		conn, err := net.DialTimeout("tcp", endpoint+":443", 2*time.Second)
		if err != nil {
			// Skip regions we can't reach (may be blocked by SCP or network)
			continue
		}
		_ = conn.Close()

		latency := time.Since(start)
		continentMatch := matchesContinent(region, userContinent)

		results = append(results, regionScore{
			region:         region,
			latency:        latency,
			continentMatch: continentMatch,
		})
	}

	if len(results) == 0 {
		return "", "", fmt.Errorf("could not connect to any allowed AWS region")
	}

	// Keep only regions that OFFER the instance type. Done after the latency
	// probe so it runs over the reachable set rather than all enabled regions,
	// and concurrently because it is one API call per candidate.
	if instanceType != "" {
		offered := regionsOfferingInstanceType(ctx, awsClient, results, instanceType)
		switch {
		case len(offered) == 0:
			// A hard error, not a fallback. Every candidate was reachable and
			// none can run this type, so any choice we made would fail at
			// RunInstances — which is exactly the failure #732 reported.
			var names []string
			for _, r := range results {
				names = append(names, r.region)
			}
			sort.Strings(names)
			return "", "", fmt.Errorf("no reachable region offers %s (checked %d: %s)",
				instanceType, len(names), strings.Join(names, ", "))
		default:
			results = offered
		}
	}

	// Sort by: continent match first, then latency
	sort.Slice(results, func(i, j int) bool {
		// Prioritize continent matches
		if results[i].continentMatch != results[j].continentMatch {
			return results[i].continentMatch
		}
		// Within same continent preference, choose lowest latency
		return results[i].latency < results[j].latency
	})

	// Return the best scored region, with the reason — a surprising choice must
	// be debuggable at the time, not three steps later from an Unsupported error.
	best := results[0]
	why := fmt.Sprintf("lowest latency of %d region(s) offering %s", len(results), instanceType)
	if instanceType == "" {
		why = fmt.Sprintf("lowest latency of %d reachable region(s)", len(results))
	}
	if best.continentMatch {
		why += ", on your continent"
	}
	return best.region, why, nil
}

// regionsOfferingInstanceType filters candidates down to those where EC2 reports
// the instance type as offered.
//
// A region that cannot run the instance must never be a candidate. Errors are
// treated as "offered" on purpose: DescribeInstanceTypeOfferings can fail for
// reasons that say nothing about availability (throttling, a transient 5xx, a
// missing ec2:DescribeInstanceTypeOfferings grant), and excluding a region on
// that basis would silently narrow the choice — or empty it — for a caller whose
// only mistake was an IAM policy. Degrading to the previous latency-only
// behaviour is the safe direction.
func regionsOfferingInstanceType(ctx context.Context, client instanceTypeOfferer, candidates []regionScore, instanceType string) []regionScore {
	type verdict struct {
		idx     int
		offered bool
	}
	ch := make(chan verdict, len(candidates))
	for i, c := range candidates {
		go func(i int, region string) {
			offered, err := client.InstanceTypeOfferedInRegion(ctx, region, instanceType)
			ch <- verdict{idx: i, offered: offered || err != nil}
		}(i, c.region)
	}
	keep := make([]bool, len(candidates))
	for range candidates {
		v := <-ch
		keep[v.idx] = v.offered
	}
	// Rebuild in the original (already sorted) order rather than completion order,
	// so the ranking survives the concurrency.
	out := make([]regionScore, 0, len(candidates))
	for i, c := range candidates {
		if keep[i] {
			out = append(out, c)
		}
	}
	return out
}

// detectUserContinent attempts to determine the user's continent from their public IP
func detectUserContinent() string {
	// Try ipapi.co (free, no API key needed for moderate usage)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://ipapi.co/json/")
	if err != nil {
		return "" // Failed, will fall back to latency-only
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return ""
	}

	var result struct {
		CountryCode string `json:"country_code"`
		Continent   string `json:"continent_code"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Map continent codes: AF, AN, AS, EU, NA, OC, SA
	return result.Continent
}

// matchesContinent checks if an AWS region matches the user's continent
func matchesContinent(region, continentCode string) bool {
	if continentCode == "" {
		return false // Unknown continent, no preference
	}

	// Map AWS region prefixes to continent codes
	regionToContinentMap := map[string]string{
		"us-":      "NA", // North America
		"ca-":      "NA", // Canada
		"eu-":      "EU", // Europe
		"me-":      "AS", // Middle East (Asia)
		"af-":      "AF", // Africa
		"ap-":      "AS", // Asia Pacific
		"sa-":      "SA", // South America
		"il-":      "AS", // Israel (Middle East)
		"ap-south": "AS", // India
	}

	// Check region prefix
	for prefix, continent := range regionToContinentMap {
		if len(region) >= len(prefix) && region[:len(prefix)] == prefix {
			return continent == continentCode
		}
	}

	return false
}

// Job Array Helper Functions

// shouldApplyRegionConstraints checks if any region constraint flags are set
func shouldApplyRegionConstraints() bool {
	return len(regionsInclude) > 0 ||
		len(regionsExclude) > 0 ||
		len(regionsGeographic) > 0 ||
		proximityFrom != "" ||
		costTier != ""
}

// validateRegionConstraint validates region constraint parameters
func validateRegionConstraint(constraint *sweep.RegionConstraint) error {
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
	if constraint.ProximityFrom != "" {
		if !regions.IsValidRegion(constraint.ProximityFrom) {
			return fmt.Errorf("invalid proximity region: %s", constraint.ProximityFrom)
		}
	}

	// Validate geographic groups
	for _, group := range constraint.Geographic {
		if _, ok := regions.GeographicGroups[group]; !ok {
			return fmt.Errorf("invalid geographic group: %s", group)
		}
	}

	return nil
}

// applyRegionConstraints filters regions based on constraints
func applyRegionConstraints(allRegions []string, constraint *sweep.RegionConstraint) ([]string, error) {
	candidates := make([]string, len(allRegions))
	copy(candidates, allRegions)

	// Apply include filter
	if len(constraint.Include) > 0 {
		candidates = filterIncludeRegions(candidates, constraint.Include)
	}

	// Apply exclude filter
	if len(constraint.Exclude) > 0 {
		candidates = filterExcludeRegions(candidates, constraint.Exclude)
	}

	// Apply geographic filter
	if len(constraint.Geographic) > 0 {
		candidates = filterGeographicRegions(candidates, constraint.Geographic)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no regions match constraints: %s", formatConstraint(constraint))
	}

	return candidates, nil
}

// filterIncludeRegions keeps only regions matching include patterns
func filterIncludeRegions(allRegions []string, patterns []string) []string {
	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if matchesAnyPattern(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterExcludeRegions removes regions matching exclude patterns
func filterExcludeRegions(allRegions []string, patterns []string) []string {
	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if !matchesAnyPattern(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterGeographicRegions keeps only regions in specified geographic groups
func filterGeographicRegions(allRegions []string, groups []string) []string {
	allowed := make(map[string]bool)
	for _, group := range groups {
		if groupRegions, ok := regions.GeographicGroups[group]; ok {
			for _, r := range groupRegions {
				allowed[r] = true
			}
		}
	}

	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if allowed[region] {
			result = append(result, region)
		}
	}
	return result
}

// matchesAnyPattern checks if region matches any of the patterns
func matchesAnyPattern(region string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchesWildcard(region, pattern) {
			return true
		}
	}
	return false
}

// matchesWildcard matches region against pattern with wildcard support
func matchesWildcard(s, pattern string) bool {
	// Exact match
	if s == pattern {
		return true
	}

	// Prefix wildcard (us-*)
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(s, prefix)
	}

	// Suffix wildcard (*-1)
	if strings.HasPrefix(pattern, "*") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(s, suffix)
	}

	return false
}

// containsString checks if slice contains string
func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// formatConstraint returns human-readable constraint description
func formatConstraint(c *sweep.RegionConstraint) string {
	parts := []string{}

	if len(c.Include) > 0 {
		parts = append(parts, fmt.Sprintf("include=%s", strings.Join(c.Include, ",")))
	}
	if len(c.Exclude) > 0 {
		parts = append(parts, fmt.Sprintf("exclude=%s", strings.Join(c.Exclude, ",")))
	}
	if len(c.Geographic) > 0 {
		parts = append(parts, fmt.Sprintf("geographic=%s", strings.Join(c.Geographic, ",")))
	}
	if c.ProximityFrom != "" {
		parts = append(parts, fmt.Sprintf("proximity_from=%s", c.ProximityFrom))
	}
	if c.CostTier != "" {
		parts = append(parts, fmt.Sprintf("cost_tier=%s", c.CostTier))
	}

	if len(parts) == 0 {
		return "no constraints"
	}

	return strings.Join(parts, ", ")
}

// resolveLaunchRegion returns the region a launch should use, in precedence
// order, and is the ONLY place that order is expressed.
//
//	explicit --region  →  SPORE_REGION / AWS_REGION / AWS_DEFAULT_REGION  →
//	spore config file  →  auto-detect
//
// Auto-detect is the last resort. It used to be the only step: every caller
// checked `if region == "" { detectBestRegion(...) }`, so an ambient AWS_REGION
// and a configured default were both ignored and the instance landed wherever
// the lowest TCP latency pointed (#732).
//
// Extracted because that `if` was duplicated at five call sites — single
// launches, batch queues, and three sweep paths. Fixing the precedence in one of
// them would have left the other four wrong, which is the same shape as #732's
// other half: the root --region flag documents this exact ordering and binds a
// variable the launch path never reads.
//
// The returned reason is for the operator: a surprising region must be
// debuggable when it is chosen, not three steps later from an Unsupported error.
func resolveLaunchRegion(ctx context.Context, explicit, instanceType string) (region, why string, err error) {
	if explicit != "" {
		return explicit, "--region", nil
	}
	if shared := spawnconfig.SharedConfig().Region; shared != "" {
		return shared, "SPORE_REGION/AWS_REGION or your spore config", nil
	}
	return detectBestRegion(ctx, instanceType)
}
