package cmd

import (
	"github.com/spf13/cobra"
)

// amiGroupCmd is the parent for AMI management subcommands.
var amiGroupCmd = &cobra.Command{
	Use:   "ami",
	Short: "Manage spawn-managed AMIs",
	Long: `Create and list AMIs created by spawn.

Examples:
  spawn ami list
  spawn ami list --stack pytorch --arch arm64
  spawn ami create my-instance --name pytorch-2.4-cuda12`,
}

var amiListCmd = &cobra.Command{
	Use:   "list",
	Short: "List spawn-managed AMIs",
	RunE:  runListAMIs,
}

var amiCreateCmd = &cobra.Command{
	Use:   "create <instance-id-or-name>",
	Short: "Create an AMI from a running instance",
	Args:  cobra.ExactArgs(1),
	RunE:  runCreateAMI,
}

func init() {
	rootCmd.AddCommand(amiGroupCmd)
	amiGroupCmd.AddCommand(amiListCmd, amiCreateCmd)

	// ami list shares flags with listAMIsCmd
	amiListCmd.Flags().StringVar(&listAMIsRegion, "region", "", "AWS region (default: current region from AWS config)")
	amiListCmd.Flags().StringVar(&listAMIsStack, "stack", "", "Filter by stack (spawn:stack tag)")
	amiListCmd.Flags().StringVar(&listAMIsVersion, "version", "", "Filter by version (spawn:version tag)")
	amiListCmd.Flags().StringVar(&listAMIsArch, "arch", "", "Filter by architecture (x86_64 or arm64)")
	amiListCmd.Flags().StringVar(&listAMIsGPU, "gpu", "", "Filter by GPU support (true or false)")
	amiListCmd.Flags().BoolVar(&listAMIsDeprecated, "deprecated", false, "Show deprecated AMIs")

	// ami create shares flags with createAMICmd
	amiCreateCmd.Flags().StringVar(&createAMIName, "name", "", "Name for the AMI (required)")
	_ = amiCreateCmd.MarkFlagRequired("name")
	amiCreateCmd.Flags().StringVar(&createAMIDescription, "description", "", "Description for the AMI")
	amiCreateCmd.Flags().StringArrayVar(&createAMITags, "tag", []string{}, "Tags in key=value format")
	amiCreateCmd.Flags().BoolVar(&createAMIReboot, "reboot", false, "Reboot instance before creating AMI (default: no-reboot)")
	amiCreateCmd.Flags().BoolVar(&createAMIWait, "wait", false, "Wait for AMI to become available")

	// Deprecate old top-level AMI commands
	listAMIsCmd.Deprecated = "use 'spawn ami list' instead"
	createAMICmd.Deprecated = "use 'spawn ami create' instead"
}
