package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
)

// completeInstanceID provides completion for instance IDs
// Returns running spawn-managed instances
func completeInstanceID(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	// Get region from flag or default
	region, _ := cmd.Flags().GetString("region")
	if region != "" {
		cfg.Region = region
	}

	// Create EC2 client
	ec2Client := ec2.NewFromConfig(cfg)

	// List spawn-managed instances
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   stringPtr("tag:spawn:managed"),
				Values: []string{"true"},
			},
			{
				Name:   stringPtr("instance-state-name"),
				Values: []string{"running", "stopped"},
			},
		},
	}

	result, err := ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var suggestions []string
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := stringValue(instance.InstanceId)
			if instanceID == "" {
				continue
			}

			// Filter by prefix
			if toComplete != "" && !strings.HasPrefix(instanceID, toComplete) {
				continue
			}

			// Get instance name and state for description
			name := getInstanceName(instance.Tags)
			state := string(instance.State.Name)

			description := fmt.Sprintf("%s\t%s (%s)", instanceID, name, state)
			suggestions = append(suggestions, description)
		}
	}

	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

// completeRegion provides completion for AWS regions
func completeRegion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Common AWS regions
	regions := []string{
		"us-east-1\tUS East (N. Virginia)",
		"us-east-2\tUS East (Ohio)",
		"us-west-1\tUS West (N. California)",
		"us-west-2\tUS West (Oregon)",
		"eu-west-1\tEurope (Ireland)",
		"eu-west-2\tEurope (London)",
		"eu-west-3\tEurope (Paris)",
		"eu-central-1\tEurope (Frankfurt)",
		"eu-north-1\tEurope (Stockholm)",
		"ap-northeast-1\tAsia Pacific (Tokyo)",
		"ap-northeast-2\tAsia Pacific (Seoul)",
		"ap-southeast-1\tAsia Pacific (Singapore)",
		"ap-southeast-2\tAsia Pacific (Sydney)",
		"ap-south-1\tAsia Pacific (Mumbai)",
		"ca-central-1\tCanada (Central)",
		"sa-east-1\tSouth America (São Paulo)",
	}

	var filtered []string
	for _, region := range regions {
		if toComplete == "" || strings.HasPrefix(region, toComplete) {
			filtered = append(filtered, region)
		}
	}

	return filtered, cobra.ShellCompDirectiveNoFileComp
}

// completeInstanceType provides completion for common instance types
func completeInstanceType(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Common instance types with descriptions
	instanceTypes := []string{
		"t3.micro\tBurstable, 2 vCPU, 1 GB RAM",
		"t3.small\tBurstable, 2 vCPU, 2 GB RAM",
		"t3.medium\tBurstable, 2 vCPU, 4 GB RAM",
		"t3.large\tBurstable, 2 vCPU, 8 GB RAM",
		"m7i.large\tGeneral purpose, 2 vCPU, 8 GB RAM",
		"m7i.xlarge\tGeneral purpose, 4 vCPU, 16 GB RAM",
		"m7i.2xlarge\tGeneral purpose, 8 vCPU, 32 GB RAM",
		"m7i.4xlarge\tGeneral purpose, 16 vCPU, 64 GB RAM",
		"c7i.large\tCompute optimized, 2 vCPU, 4 GB RAM",
		"c7i.xlarge\tCompute optimized, 4 vCPU, 8 GB RAM",
		"c7i.2xlarge\tCompute optimized, 8 vCPU, 16 GB RAM",
		"r7i.large\tMemory optimized, 2 vCPU, 16 GB RAM",
		"r7i.xlarge\tMemory optimized, 4 vCPU, 32 GB RAM",
		"r7i.2xlarge\tMemory optimized, 8 vCPU, 64 GB RAM",
		"g5.xlarge\tGPU (1x A10G), 4 vCPU, 16 GB RAM",
		"g5.2xlarge\tGPU (1x A10G), 8 vCPU, 32 GB RAM",
		"g5.4xlarge\tGPU (1x A10G), 16 vCPU, 64 GB RAM",
		"p3.2xlarge\tGPU (1x V100), 8 vCPU, 61 GB RAM",
		"p3.8xlarge\tGPU (4x V100), 32 vCPU, 244 GB RAM",
	}

	var filtered []string
	for _, instanceType := range instanceTypes {
		if toComplete == "" || strings.HasPrefix(instanceType, toComplete) {
			filtered = append(filtered, instanceType)
		}
	}

	return filtered, cobra.ShellCompDirectiveNoFileComp
}

// Helper functions

func stringPtr(s string) *string {
	return &s
}

func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func getInstanceName(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if stringValue(tag.Key) == "Name" {
			return stringValue(tag.Value)
		}
	}
	return "unnamed"
}

func getTagValue(tags []ec2types.Tag, key string) string {
	for _, tag := range tags {
		if stringValue(tag.Key) == key {
			return stringValue(tag.Value)
		}
	}
	return ""
}
