package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	listRegion         string
	listRegions        []string
	listAZ             string
	listState          string
	listInstanceType   string
	listInstanceFamily string
	listTag            []string
	listJSON           bool // deprecated: use --output json
	listJobArrayID     string
	listJobArrayName   string
	listSweepID        string
	listSweepName      string
)

var listCmd = &cobra.Command{
	Use:     "list",
	RunE:    runList,
	Aliases: []string{"ls"},
	// Short and Long will be set after i18n initialization
}

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().StringVar(&listRegion, "region", "", "Filter by AWS region (default: all regions)")
	listCmd.Flags().StringSliceVarP(&listRegions, "regions", "r", nil, "Filter by regions (comma-separated, e.g. us-east-1,us-west-2)")
	listCmd.Flags().StringVar(&listAZ, "az", "", "Filter by availability zone")
	listCmd.Flags().StringVar(&listState, "state", "", "Filter by instance state (running, stopped, etc.)")
	listCmd.Flags().StringVar(&listInstanceType, "instance-type", "", "Filter by exact instance type (e.g., t3.micro)")
	listCmd.Flags().StringVar(&listInstanceFamily, "instance-family", "", "Filter by instance family (e.g., m7i, t3)")
	listCmd.Flags().StringArrayVar(&listTag, "tag", []string{}, "Filter by tag (key=value format, can be specified multiple times)")
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Output as JSON")
	_ = listCmd.Flags().MarkDeprecated("json", "use --output json instead")
	listCmd.Flags().StringVar(&listJobArrayID, "job-array-id", "", "Filter by job array ID")
	listCmd.Flags().StringVar(&listJobArrayName, "job-array-name", "", "Filter by job array name")
	listCmd.Flags().StringVar(&listSweepID, "sweep-id", "", "Filter by parameter sweep ID")
	listCmd.Flags().StringVar(&listSweepName, "sweep-name", "", "Filter by parameter sweep name")
}

func runList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Resolve effective region: --region takes precedence; --regions provides the first value as filter
	effectiveRegion := listRegion
	if effectiveRegion == "" && len(listRegions) > 0 {
		effectiveRegion = listRegions[0]
	}

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// List instances
	if effectiveRegion != "" {
		fmt.Fprintf(os.Stderr, "%s...\n", i18n.Tf("spawn.list.searching_region", map[string]interface{}{
			"Region": effectiveRegion,
		}))
	} else {
		fmt.Fprintf(os.Stderr, "%s...\n", i18n.T("spawn.list.searching_all_regions"))
	}

	instances, err := client.ListInstances(ctx, effectiveRegion, listState)
	if err != nil {
		return i18n.Te("spawn.list.error.list_failed", err)
	}

	if len(instances) == 0 {
		if listJSON || getOutputFormat() == "json" {
			fmt.Println("[]")
			return nil
		}
		fmt.Printf("\n%s\n", i18n.T("spawn.list.no_instances"))
		return nil
	}

	// Apply additional filters
	instances = filterInstances(instances)

	if len(instances) == 0 {
		if listJSON || getOutputFormat() == "json" {
			fmt.Println("[]")
			return nil
		}
		fmt.Printf("\n%s\n", i18n.T("spawn.list.no_instances_match"))
		return nil
	}

	// Sort by launch time (newest first)
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].LaunchTime.After(instances[j].LaunchTime)
	})

	// Output format: --json flag or global --output json
	if listJSON || getOutputFormat() == "json" {
		return outputJSON(instances)
	}

	return outputTable(instances)
}

func outputTable(instances []aws.InstanceInfo) error {
	fmt.Println() // Blank line after search message

	// Separate instances into three groups: sweeps, standalone job arrays, and standalone instances
	sweepInstances := make(map[string][]aws.InstanceInfo)
	standaloneJobArrays := make(map[string][]aws.InstanceInfo)
	var standaloneInstances []aws.InstanceInfo

	for _, inst := range instances {
		if inst.SweepID != "" {
			sweepInstances[inst.SweepID] = append(sweepInstances[inst.SweepID], inst)
		} else if inst.JobArrayID != "" {
			standaloneJobArrays[inst.JobArrayID] = append(standaloneJobArrays[inst.JobArrayID], inst)
		} else {
			standaloneInstances = append(standaloneInstances, inst)
		}
	}

	// Display parameter sweeps
	if len(sweepInstances) > 0 {
		displayParameterSweeps(sweepInstances)
	}

	// Display standalone job arrays
	if len(standaloneJobArrays) > 0 {
		displayStandaloneJobArrays(standaloneJobArrays)
	}

	// Display standalone instances
	if len(standaloneInstances) > 0 {
		displayStandaloneInstances(standaloneInstances, len(sweepInstances) > 0 || len(standaloneJobArrays) > 0)
	}

	return nil
}

// displayParameterSweeps shows all parameter sweeps with stats and instances
func displayParameterSweeps(sweeps map[string][]aws.InstanceInfo) {
	if flagNoColor || flagAccessibility {
		fmt.Println("Parameter Sweeps:")
	} else {
		fmt.Println("\033[1mParameter Sweeps:\033[0m")
	}

	// Sort sweep IDs for consistent output
	var sweepIDs []string
	for id := range sweeps {
		sweepIDs = append(sweepIDs, id)
	}
	sort.Strings(sweepIDs)

	for _, sweepID := range sweepIDs {
		instances := sweeps[sweepID]

		// Sort instances by sweep index
		sort.Slice(instances, func(i, j int) bool {
			return instances[i].SweepIndex < instances[j].SweepIndex
		})

		// Calculate stats
		completed, running, pending := calculateSweepStats(instances)

		// Display sweep summary
		sweepName := instances[0].SweepName
		if sweepName == "" {
			sweepName = "unnamed"
		}
		fmt.Printf("\n  %s (%d instances, %d completed, %d running, %d pending)\n",
			sweepName, len(instances), completed, running, pending)
		fmt.Printf("  Sweep ID: %s\n", sweepID)

		// Display instances with parameters
		for _, inst := range instances {
			displayInstanceWithParams(inst, "    ")
		}
	}
	fmt.Println()
}

// displayStandaloneJobArrays shows standalone job arrays (not part of a sweep)
func displayStandaloneJobArrays(jobArrays map[string][]aws.InstanceInfo) {
	if flagNoColor || flagAccessibility {
		fmt.Println("Job Arrays:")
	} else {
		fmt.Println("\033[1mJob Arrays:\033[0m")
	}

	// Sort job array IDs for consistent output
	var jobArrayIDs []string
	for id := range jobArrays {
		jobArrayIDs = append(jobArrayIDs, id)
	}
	sort.Strings(jobArrayIDs)

	for _, arrayID := range jobArrayIDs {
		arrayInstances := jobArrays[arrayID]

		// Sort instances by index within array
		sort.Slice(arrayInstances, func(i, j int) bool {
			return arrayInstances[i].JobArrayIndex < arrayInstances[j].JobArrayIndex
		})

		// Count states
		completed, running, pending := calculateSweepStats(arrayInstances)

		// Display job array summary
		arrayName := arrayInstances[0].JobArrayName
		if arrayName == "" {
			arrayName = "unnamed"
		}
		fmt.Printf("\n  %s (%d instances, %d completed, %d running, %d pending)\n",
			arrayName, len(arrayInstances), completed, running, pending)
		fmt.Printf("  Array ID: %s\n", arrayID)

		// Display instances
		for _, inst := range arrayInstances {
			displayInstance(inst, "    ")
		}
	}
	fmt.Println()
}

// displayStandaloneInstances shows standalone instances (not part of sweep or job array)
func displayStandaloneInstances(instances []aws.InstanceInfo, hasOtherGroups bool) {
	if hasOtherGroups {
		if flagNoColor || flagAccessibility {
			fmt.Println("Standalone Instances:")
		} else {
			fmt.Println("\033[1mStandalone Instances:\033[0m")
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	// Header
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		i18n.T("spawn.list.header.instance_id"),
		i18n.T("spawn.list.header.name"),
		i18n.T("spawn.list.header.type"),
		i18n.T("spawn.list.header.state"),
		i18n.T("spawn.list.header.iam_role"),
		i18n.T("spawn.list.header.az"),
		i18n.T("spawn.list.header.age"),
		i18n.T("spawn.list.header.ttl"),
		i18n.T("spawn.list.header.public_ip"),
		i18n.T("spawn.list.header.spot"),
	)

	for _, inst := range instances {
		age := formatDuration(time.Since(inst.LaunchTime))
		ttl := inst.TTL
		if ttl == "" {
			ttl = "none"
		}
		name := inst.Name
		if name == "" {
			name = "-"
		}
		spotIndicator := ""
		if inst.SpotInstance {
			spotIndicator = "✓"
		}
		state := colorizeState(inst.State)
		publicIP := inst.PublicIP
		if publicIP == "" {
			publicIP = "-"
		}
		iamRole := inst.IAMRole
		if iamRole == "" {
			iamRole = "-"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			inst.InstanceID,
			name,
			inst.InstanceType,
			state,
			iamRole,
			inst.AvailabilityZone,
			age,
			ttl,
			publicIP,
			spotIndicator,
		)
	}
}

// calculateSweepStats computes completed/running/pending counts
func calculateSweepStats(instances []aws.InstanceInfo) (completed, running, pending int) {
	for _, inst := range instances {
		switch inst.State {
		case "running":
			running++
		case "pending":
			pending++
		case "terminated", "stopped", "stopping", "shutting-down":
			completed++
		}
	}
	return
}

// displayInstanceWithParams shows instance details with parameters
func displayInstanceWithParams(inst aws.InstanceInfo, prefix string) {
	state := colorizeState(inst.State)
	name := inst.Name
	if name == "" {
		name = "-"
	}
	spotIndicator := ""
	if inst.SpotInstance {
		spotIndicator = " (spot)"
	}

	// Format parameters (max 3)
	paramsStr := formatParameters(inst.Parameters, 3)
	if paramsStr != "" {
		paramsStr = "  " + paramsStr
	}

	fmt.Printf("%s├─ %s  %s  %s  %s%s%s\n",
		prefix,
		name,
		inst.InstanceID,
		inst.InstanceType,
		state,
		paramsStr,
		spotIndicator,
	)
}

// formatParameters formats param key-value pairs for display (max maxParams)
func formatParameters(params map[string]string, maxParams int) string {
	if len(params) == 0 {
		return ""
	}

	// Sort parameter keys alphabetically
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Format up to maxParams
	var parts []string
	for i, k := range keys {
		if i >= maxParams {
			break
		}
		value := truncateValue(params[k], 20)
		parts = append(parts, fmt.Sprintf("%s=%s", k, value))
	}

	return strings.Join(parts, " ")
}

// truncateValue truncates string if > maxLen, adds "..."
func truncateValue(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen-3] + "..."
}

func displayInstance(inst aws.InstanceInfo, prefix string) {
	state := colorizeState(inst.State)
	publicIP := inst.PublicIP
	if publicIP == "" {
		publicIP = "-"
	}
	name := inst.Name
	if name == "" {
		name = "-"
	}
	spotIndicator := ""
	if inst.SpotInstance {
		spotIndicator = " (spot)"
	}
	iamRole := inst.IAMRole
	if iamRole == "" {
		iamRole = "-"
	}

	fmt.Printf("%s[%s] %s  %s  %s  %s  %s  %s  %s%s\n",
		prefix,
		inst.JobArrayIndex,
		name,
		inst.InstanceID,
		inst.InstanceType,
		state,
		iamRole,
		inst.AvailabilityZone,
		publicIP,
		spotIndicator,
	)
}

func colorizeState(state string) string {
	if flagNoColor || flagAccessibility {
		return state
	}
	switch state {
	case "running":
		return "\033[32m" + state + "\033[0m" // Green
	case "stopped":
		return "\033[33m" + state + "\033[0m" // Yellow
	case "stopping":
		return "\033[33m" + state + "\033[0m" // Yellow
	case "pending":
		return "\033[36m" + state + "\033[0m" // Cyan
	default:
		return state
	}
}

func outputJSON(instances []aws.InstanceInfo) error {
	// Build JSON objects for each instance
	output := make([]map[string]interface{}, len(instances))
	for i, inst := range instances {
		obj := map[string]interface{}{
			"instance_id":       inst.InstanceID,
			"name":              inst.Name,
			"instance_type":     inst.InstanceType,
			"state":             inst.State,
			"region":            inst.Region,
			"availability_zone": inst.AvailabilityZone,
			"public_ip":         inst.PublicIP,
			"private_ip":        inst.PrivateIP,
			"launch_time":       inst.LaunchTime.Format(time.RFC3339),
			"ttl":               inst.TTL,
			"idle_timeout":      inst.IdleTimeout,
			"key_name":          inst.KeyName,
			"spot":              inst.SpotInstance,
			"iam_role":          inst.IAMRole,
			"job_array_id":      inst.JobArrayID,
			"job_array_name":    inst.JobArrayName,
			"job_array_index":   inst.JobArrayIndex,
			"job_array_size":    inst.JobArraySize,
			"sweep_id":          inst.SweepID,
			"sweep_name":        inst.SweepName,
			"sweep_index":       inst.SweepIndex,
			"sweep_size":        inst.SweepSize,
			"parameters":        inst.Parameters,
		}
		output[i] = obj
	}

	// Marshal to JSON with indentation
	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	fmt.Println(string(jsonBytes))
	return nil
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes > 0 {
			return fmt.Sprintf("%dh%dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	if hours > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	return fmt.Sprintf("%dd", days)
}

func filterInstances(instances []aws.InstanceInfo) []aws.InstanceInfo {
	var filtered []aws.InstanceInfo

	for _, inst := range instances {
		// Filter by availability zone
		if listAZ != "" && inst.AvailabilityZone != listAZ {
			continue
		}

		// Filter by instance type (exact match)
		if listInstanceType != "" && inst.InstanceType != listInstanceType {
			continue
		}

		// Filter by instance family (prefix match)
		if listInstanceFamily != "" {
			// Extract family from instance type (e.g., "m7i" from "m7i.large")
			parts := strings.Split(inst.InstanceType, ".")
			if len(parts) == 0 || parts[0] != listInstanceFamily {
				continue
			}
		}

		// Filter by job array ID
		if listJobArrayID != "" && inst.JobArrayID != listJobArrayID {
			continue
		}

		// Filter by job array name
		if listJobArrayName != "" && inst.JobArrayName != listJobArrayName {
			continue
		}

		// Filter by sweep ID
		if listSweepID != "" && inst.SweepID != listSweepID {
			continue
		}

		// Filter by sweep name
		if listSweepName != "" && inst.SweepName != listSweepName {
			continue
		}

		// Filter by tags
		matchesTags := true
		for _, tagFilter := range listTag {
			// Parse tag filter in format "key=value"
			parts := strings.SplitN(tagFilter, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0]
			value := parts[1]

			// Check if instance has this tag with matching value
			// Special handling for common tags
			if key == "Name" {
				if inst.Name != value {
					matchesTags = false
					break
				}
			} else if key == "spawn:ttl" {
				if inst.TTL != value {
					matchesTags = false
					break
				}
			} else if key == "spawn:idle-timeout" {
				if inst.IdleTimeout != value {
					matchesTags = false
					break
				}
			} else {
				// Check in Tags map
				tagValue, exists := inst.Tags[key]
				if !exists || tagValue != value {
					matchesTags = false
					break
				}
			}
		}

		if !matchesTags {
			continue
		}

		// Instance passed all filters
		filtered = append(filtered, inst)
	}

	return filtered
}
