package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	listAMIsRegion     string
	listAMIsStack      string
	listAMIsVersion    string
	listAMIsArch       string
	listAMIsGPU        string
	listAMIsDeprecated bool
	listAMIsJSON       bool // deprecated: use --output json
)

var listAMIsCmd = &cobra.Command{
	Use:   "list-amis",
	Short: "List spawn-managed AMIs",
	Long: `List AMIs created and managed by spawn.

Filters AMIs by spawn tags to show only those created by spawn.
You can filter by stack, version, architecture, and other attributes.

Examples:
  # List all spawn AMIs
  spawn list-amis

  # Filter by stack
  spawn list-amis --stack pytorch

  # Filter by stack and version
  spawn list-amis --stack pytorch --version 2.2

  # Filter by architecture
  spawn list-amis --arch arm64

  # Show only GPU AMIs
  spawn list-amis --gpu true

  # Show deprecated AMIs
  spawn list-amis --deprecated

  # JSON output
  spawn list-amis --json`,
	RunE: runListAMIs,
}

func init() {
	rootCmd.AddCommand(listAMIsCmd)

	listAMIsCmd.Flags().StringVar(&listAMIsRegion, "region", "", "AWS region (default: current region from AWS config)")
	listAMIsCmd.Flags().StringVar(&listAMIsStack, "stack", "", "Filter by stack (spawn:stack tag)")
	listAMIsCmd.Flags().StringVar(&listAMIsVersion, "version", "", "Filter by version (spawn:version tag)")
	listAMIsCmd.Flags().StringVar(&listAMIsArch, "arch", "", "Filter by architecture (x86_64 or arm64)")
	listAMIsCmd.Flags().StringVar(&listAMIsGPU, "gpu", "", "Filter by GPU support (true or false)")
	listAMIsCmd.Flags().BoolVar(&listAMIsDeprecated, "deprecated", false, "Show deprecated AMIs (default: hide deprecated)")
	listAMIsCmd.Flags().BoolVar(&listAMIsJSON, "json", false, "Output as JSON")
	_ = listAMIsCmd.Flags().MarkDeprecated("json", "use --output json instead")
}

func runListAMIs(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Determine region
	region := listAMIsRegion
	if region == "" {
		cfg, err := client.GetConfig(ctx)
		if err != nil {
			return fmt.Errorf("failed to get AWS config: %w", err)
		}
		region = cfg.Region
	}

	// Search for AMIs
	fmt.Fprintf(os.Stderr, "Searching for AMIs in %s...\n", region)

	allAMIs, err := client.ListAMIs(ctx, region, nil)
	if err != nil {
		return fmt.Errorf("failed to list AMIs: %w", err)
	}

	// Filter spawn-managed AMIs and apply user filters
	spawnAMIs := []aws.AMIInfo{}
	for _, ami := range allAMIs {
		// Only include spawn-managed AMIs (those with spawn:managed tag or stack tag)
		if ami.Tags["spawn:managed"] != "true" && ami.Stack == "" {
			continue
		}

		// Filter deprecated AMIs unless explicitly requested
		if !listAMIsDeprecated && ami.Deprecated {
			continue
		}

		// Apply user filters
		if listAMIsStack != "" && ami.Stack != listAMIsStack {
			continue
		}
		if listAMIsVersion != "" && ami.Version != listAMIsVersion {
			continue
		}
		if listAMIsArch != "" && ami.Architecture != listAMIsArch {
			continue
		}
		if listAMIsGPU != "" {
			wantGPU := listAMIsGPU == "true"
			if ami.GPU != wantGPU {
				continue
			}
		}

		spawnAMIs = append(spawnAMIs, ami)
	}

	if len(spawnAMIs) == 0 {
		fmt.Printf("\nNo spawn-managed AMIs found.\n")
		fmt.Printf("\nCreate an AMI:\n")
		fmt.Printf("  spawn create-ami <instance-id> --name my-ami --tag stack=myapp\n")
		return nil
	}

	// Sort by creation date (newest first)
	sort.Slice(spawnAMIs, func(i, j int) bool {
		return spawnAMIs[i].CreationDate.After(spawnAMIs[j].CreationDate)
	})

	// Output
	if listAMIsJSON || getOutputFormat() == "json" {
		return outputAMIsJSON(spawnAMIs)
	}

	return outputAMIsTable(spawnAMIs)
}

func outputAMIsTable(amis []aws.AMIInfo) error {
	ctx := context.Background()

	// Create AWS client for health checks
	client, err := aws.NewClient(ctx)
	if err != nil {
		// If we can't create client, just list without health checks
		return outputAMIsTableSimple(amis)
	}

	// Get region from first AMI
	region := ""
	if len(amis) > 0 {
		region = amis[0].Tags["spawn:source-region"]
	}
	if region == "" {
		cfg, err := client.GetConfig(ctx)
		if err == nil {
			region = cfg.Region
		}
	}

	fmt.Println() // Blank line

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	// Header
	_, _ = fmt.Fprintf(w, "NAME\tAMI ID\tSTACK\tVERSION\tARCH\tSIZE\tAGE\tSTATUS\n")

	hasWarnings := false

	for _, ami := range amis {
		// Format age
		age := formatDuration(time.Since(ami.CreationDate))

		// Format size
		size := fmt.Sprintf("%dGB", ami.Size)

		// Stack and version
		stack := ami.Stack
		if stack == "" {
			stack = "-"
		}
		version := ami.Version
		if version == "" {
			version = "-"
		}

		// Status
		status := ""
		if ami.GPU {
			status = "GPU"
		}
		if ami.Deprecated {
			if status != "" {
				status += " "
			}
			status += "[deprecated]"
		}

		// Health check
		if region != "" {
			health, err := client.CheckAMIHealth(ctx, ami, region)
			if err == nil && len(health.Warnings) > 0 {
				if status != "" {
					status += " "
				}
				status += "⚠️"
				hasWarnings = true
			}
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ami.Name,
			ami.AMIID,
			stack,
			version,
			ami.Architecture,
			size,
			age,
			status,
		)
	}

	_ = w.Flush()

	// Show warnings in detail after table
	if hasWarnings && region != "" {
		fmt.Println()
		fmt.Println("Warnings:")
		for _, ami := range amis {
			health, err := client.CheckAMIHealth(ctx, ami, region)
			if err == nil && len(health.Warnings) > 0 {
				fmt.Printf("  %s:\n", ami.Name)
				for _, warning := range health.Warnings {
					fmt.Printf("    - %s\n", warning)
				}
			}
		}
	}

	return nil
}

func outputAMIsTableSimple(amis []aws.AMIInfo) error {
	fmt.Println() // Blank line

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	// Header
	_, _ = fmt.Fprintf(w, "NAME\tAMI ID\tSTACK\tVERSION\tARCH\tSIZE\tAGE\tSTATUS\n")

	for _, ami := range amis {
		// Format age
		age := formatDuration(time.Since(ami.CreationDate))

		// Format size
		size := fmt.Sprintf("%dGB", ami.Size)

		// Stack and version
		stack := ami.Stack
		if stack == "" {
			stack = "-"
		}
		version := ami.Version
		if version == "" {
			version = "-"
		}

		// Status
		status := ""
		if ami.GPU {
			status = "GPU"
		}
		if ami.Deprecated {
			if status != "" {
				status += " "
			}
			status += "[deprecated]"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ami.Name,
			ami.AMIID,
			stack,
			version,
			ami.Architecture,
			size,
			age,
			status,
		)
	}

	fmt.Println() // Blank line

	// Summary
	gpuCount := 0
	deprecatedCount := 0
	for _, ami := range amis {
		if ami.GPU {
			gpuCount++
		}
		if ami.Deprecated {
			deprecatedCount++
		}
	}

	fmt.Printf("Total: %d AMIs", len(amis))
	if gpuCount > 0 {
		fmt.Printf(" (%d GPU)", gpuCount)
	}
	if deprecatedCount > 0 {
		fmt.Printf(" (%d deprecated)", deprecatedCount)
	}
	fmt.Println()

	return nil
}

func outputAMIsJSON(amis []aws.AMIInfo) error {
	// Convert to JSON-friendly format
	type JSONOutput struct {
		AMIID        string            `json:"ami_id"`
		Name         string            `json:"name"`
		Description  string            `json:"description"`
		Architecture string            `json:"architecture"`
		CreationDate string            `json:"creation_date"`
		Size         int64             `json:"size_gb"`
		Stack        string            `json:"stack,omitempty"`
		Version      string            `json:"version,omitempty"`
		GPU          bool              `json:"gpu"`
		BaseOS       string            `json:"base_os,omitempty"`
		Deprecated   bool              `json:"deprecated"`
		Tags         map[string]string `json:"tags"`
	}

	output := make([]JSONOutput, len(amis))
	for i, ami := range amis {
		output[i] = JSONOutput{
			AMIID:        ami.AMIID,
			Name:         ami.Name,
			Description:  ami.Description,
			Architecture: ami.Architecture,
			CreationDate: ami.CreationDate.Format(time.RFC3339),
			Size:         ami.Size,
			Stack:        ami.Stack,
			Version:      ami.Version,
			GPU:          ami.GPU,
			BaseOS:       ami.BaseOS,
			Deprecated:   ami.Deprecated,
			Tags:         ami.Tags,
		}
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
