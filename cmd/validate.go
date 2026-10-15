package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/compliance"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/infrastructure"
)

var (
	// Separate variables, one per flag. Both flags used to bind to a single
	// string, which is part of how the arity bug survived: --nist-800-171 takes no
	// value but was registered as a StringVar, so pflag REQUIRED one and the mode
	// was decided by Changed() while the value went unread (#808).
	validateNIST171        bool
	validateNIST53         string
	validateRegion         string
	validateInstanceID     string
	validateInfrastructure bool // Validate infrastructure resources
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate compliance and infrastructure configuration",
	Long: `Validate spawn instances and configuration against compliance controls.

This command can validate:
- Running instances against compliance controls (NIST 800-171, NIST 800-53)
- Infrastructure resources (DynamoDB, S3, Lambda, CloudWatch)
- Launch configuration before launching instances

Examples:
  # Validate all running instances against NIST 800-171
  spawn validate --nist-800-171

  # Validate specific instance
  spawn validate --instance-id i-0abc123 --nist-800-171

  # Validate infrastructure resources
  spawn validate --infrastructure

  # Output as JSON for automation
  spawn validate --nist-800-171 -o json`,
	RunE: runValidate,
}

func init() {
	rootCmd.AddCommand(validateCmd)

	// --nist-800-171 has nothing to supply, so it is a bool. As a StringVar pflag
	// demanded an argument and ate the next token: `spawn validate --nist-800-171
	// -o json` consumed "-o" as the flag's value, then "json" as a positional, and
	// printed a human table with exit 0 — the documented JSON path silently
	// producing the wrong format (#808).
	validateCmd.Flags().BoolVar(&validateNIST171, "nist-800-171", false,
		"Validate NIST 800-171 Rev 3 compliance")

	// --nist-800-53 does take a baseline. NoOptDefVal makes a bare --nist-800-53
	// mean "low", which is what the handler already assumed when the value was
	// empty, so the flag can be written either way without pflag demanding an
	// argument.
	validateCmd.Flags().StringVar(&validateNIST53, "nist-800-53", "",
		"Validate NIST 800-53 compliance at a baseline: "+strings.Join(nist53Baselines, ", ")+" (default low)")
	validateCmd.Flags().Lookup("nist-800-53").NoOptDefVal = nist53DefaultBaseline
	validateCmd.Flags().BoolVar(&validateInfrastructure, "infrastructure", false, "Validate infrastructure resources (DynamoDB, S3, Lambda)")
	validateCmd.Flags().StringVar(&validateRegion, "region", "", "AWS region to validate (default: all regions)")
	validateCmd.Flags().StringVar(&validateInstanceID, "instance-id", "", "Specific instance ID to validate")
}

func runValidate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Check if infrastructure validation requested
	if validateInfrastructure {
		return runInfrastructureValidation(ctx)
	}

	// Determine compliance mode.
	//
	// Both frameworks at once used to be accepted, with 800-171 silently winning.
	// Refusing is better than picking: a caller who passed both asked for
	// something this command cannot do, and quietly doing half of it is the
	// failure mode this whole issue is about.
	if validateNIST171 && cmd.Flags().Changed("nist-800-53") {
		return fmt.Errorf("--nist-800-171 and --nist-800-53 are mutually exclusive; pass one")
	}

	complianceMode := ""
	switch {
	case validateNIST171:
		complianceMode = "nist-800-171"
	case cmd.Flags().Changed("nist-800-53"):
		val := validateNIST53
		if val == "" {
			val = nist53DefaultBaseline
		}
		// Validate HERE, not twelve frames down. An unknown baseline used to
		// surface as "unknown compliance mode: nist-800-53-nonsense" from inside
		// the config loader, which names an internal string rather than the flag
		// the user typed or the values it accepts.
		if !isNIST53Baseline(val) {
			return fmt.Errorf("invalid --nist-800-53 baseline %q: want one of %s",
				val, strings.Join(nist53Baselines, ", "))
		}
		complianceMode = fmt.Sprintf("nist-800-53-%s", val)
	}

	if complianceMode == "" {
		return fmt.Errorf("compliance mode required: use --nist-800-171 or --nist-800-53=<low|moderate|high>, or --infrastructure")
	}

	// Load configuration
	complianceConfig, err := spawnconfig.LoadComplianceConfig(ctx, complianceMode, false)
	if err != nil {
		return fmt.Errorf("failed to load compliance config: %w", err)
	}

	infraConfig, err := spawnconfig.LoadInfrastructureConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load infrastructure config: %w", err)
	}

	// Create validator
	validator := compliance.NewValidator(complianceConfig, infraConfig)

	// Create AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create AWS client: %w", err)
	}

	// Query instances
	var instances []aws.InstanceInfo
	if validateInstanceID != "" {
		// Validate specific instance
		// Note: Need to implement GetInstance method
		return fmt.Errorf("single instance validation not yet implemented")
	} else {
		// Validate all spawn-managed instances
		instances, err = awsClient.ListInstances(ctx, validateRegion, "")
		if err != nil {
			return fmt.Errorf("failed to list instances: %w", err)
		}
	}

	if len(instances) == 0 {
		fmt.Fprintln(os.Stderr, "No spawn-managed instances found")
		return nil
	}

	// Validate instances
	results, err := validator.ValidateInstances(ctx, instances)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Output results
	if getOutputFormat() == "json" {
		return outputValidationJSON(results, instances, complianceConfig)
	}

	return outputText(results, instances, complianceConfig)
}

func outputText(results map[string]*compliance.ValidationResult, instances []aws.InstanceInfo, cfg *spawnconfig.ComplianceConfig) error {
	fmt.Printf("Compliance Validation Report (%s)\n", cfg.GetModeDisplayName())
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	// Count compliant and non-compliant instances
	compliantCount := 0
	nonCompliantCount := 0
	totalViolations := 0

	for instanceID, result := range results {
		if result.Compliant {
			compliantCount++
		} else {
			nonCompliantCount++
			totalViolations += len(result.Violations)
		}

		// Find instance info
		var instanceInfo *aws.InstanceInfo
		for i := range instances {
			if instances[i].InstanceID == instanceID {
				instanceInfo = &instances[i]
				break
			}
		}

		if !result.Compliant && instanceInfo != nil {
			fmt.Printf("Instance: %s (%s)\n", instanceInfo.InstanceID, instanceInfo.Name)
			fmt.Printf("  Region: %s\n", instanceInfo.Region)
			fmt.Printf("  Type: %s\n", instanceInfo.InstanceType)
			fmt.Printf("  State: %s\n", instanceInfo.State)
			fmt.Println()

			for _, violation := range result.Violations {
				fmt.Printf("  ✗ [%s] %s\n", violation.ControlID, violation.ControlName)
				fmt.Printf("    %s\n", violation.Description)
				if violation.Remediation != "" {
					fmt.Printf("    Remediation: %s\n", violation.Remediation)
				}
				fmt.Println()
			}
		}
	}

	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Instances Scanned: %d\n", len(instances))
	fmt.Printf("Compliant: %d\n", compliantCount)
	fmt.Printf("Non-Compliant: %d\n", nonCompliantCount)
	fmt.Printf("Total Violations: %d\n", totalViolations)
	fmt.Println()

	if nonCompliantCount > 0 {
		fmt.Println("Recommendations:")
		fmt.Printf("  1. Terminate and relaunch non-compliant instances with --%s\n", cfg.Mode)
		fmt.Println("  2. Enable default EBS encryption: aws ec2 enable-ebs-encryption-by-default")
		fmt.Println("  3. Review networking configuration for compliance requirements")
		fmt.Println()
	}

	return nil
}

func outputValidationJSON(results map[string]*compliance.ValidationResult, instances []aws.InstanceInfo, cfg *spawnconfig.ComplianceConfig) error {
	// Build JSON structure
	output := map[string]interface{}{
		"compliance_mode":     cfg.GetModeDisplayName(),
		"instances_scanned":   len(instances),
		"compliant_count":     0,
		"non_compliant_count": 0,
		"total_violations":    0,
		"instances":           []map[string]interface{}{},
	}

	compliantCount := 0
	nonCompliantCount := 0
	totalViolations := 0

	for instanceID, result := range results {
		if result.Compliant {
			compliantCount++
		} else {
			nonCompliantCount++
			totalViolations += len(result.Violations)
		}

		// Find instance info
		var instanceInfo *aws.InstanceInfo
		for i := range instances {
			if instances[i].InstanceID == instanceID {
				instanceInfo = &instances[i]
				break
			}
		}

		if instanceInfo == nil {
			continue
		}

		instanceOutput := map[string]interface{}{
			"instance_id": instanceInfo.InstanceID,
			"name":        instanceInfo.Name,
			"region":      instanceInfo.Region,
			"type":        instanceInfo.InstanceType,
			"state":       instanceInfo.State,
			"compliant":   result.Compliant,
			"violations":  []map[string]string{},
		}

		for _, violation := range result.Violations {
			instanceOutput["violations"] = append(
				instanceOutput["violations"].([]map[string]string),
				map[string]string{
					"control_id":   violation.ControlID,
					"control_name": violation.ControlName,
					"description":  violation.Description,
					"severity":     violation.Severity,
					"remediation":  violation.Remediation,
				},
			)
		}

		output["instances"] = append(output["instances"].([]map[string]interface{}), instanceOutput)
	}

	output["compliant_count"] = compliantCount
	output["non_compliant_count"] = nonCompliantCount
	output["total_violations"] = totalViolations

	// Marshal to JSON
	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	fmt.Println(string(jsonBytes))
	return nil
}

func runInfrastructureValidation(ctx context.Context) error {
	// Load infrastructure configuration
	infraConfig, err := spawnconfig.LoadInfrastructureConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load infrastructure config: %w", err)
	}

	// Load AWS config
	awsCfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Determine region
	region := validateRegion
	if region == "" {
		region = awsCfg.Region
		if region == "" {
			region = "us-east-1" // Fallback
		}
	}

	// Get account ID
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create AWS client: %w", err)
	}
	accountID, err := awsClient.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get account ID: %w", err)
	}

	// Create resolver and validator
	resolver := infrastructure.NewResolver(infraConfig, region, accountID)
	validator := infrastructure.NewValidator(resolver, awsCfg)

	// Validate all resources
	result, err := validator.ValidateAll(ctx)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Output results
	if getOutputFormat() == "json" {
		return outputInfrastructureJSON(result, resolver)
	}

	return outputInfrastructureText(result, resolver, validator)
}

func outputInfrastructureText(result *infrastructure.ValidationResult, resolver *infrastructure.Resolver, validator *infrastructure.Validator) error {
	fmt.Println("Infrastructure Validation Report")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	// Show infrastructure mode
	if resolver.IsSelfHosted() {
		fmt.Println("Mode: Self-hosted infrastructure (customer account)")
	} else {
		fmt.Println("Mode: Shared infrastructure (spore-host-infra account)")
	}
	fmt.Println()

	// Group resources by type
	dynamoDBResources := []infrastructure.ResourceStatus{}
	s3Resources := []infrastructure.ResourceStatus{}
	lambdaResources := []infrastructure.ResourceStatus{}

	for name, status := range result.Resources {
		if strings.HasPrefix(name, "dynamodb_") {
			dynamoDBResources = append(dynamoDBResources, status)
		} else if strings.HasPrefix(name, "s3_") {
			s3Resources = append(s3Resources, status)
		} else if strings.HasPrefix(name, "lambda_") {
			lambdaResources = append(lambdaResources, status)
		}
	}

	// Print DynamoDB tables
	fmt.Println("DynamoDB Tables:")
	accessibleCount := 0
	for _, res := range dynamoDBResources {
		if res.Accessible {
			fmt.Printf("  ✓ %s\n", res.Name)
			accessibleCount++
		} else {
			fmt.Printf("  ✗ %s: %s\n", res.Name, res.Error)
		}
	}
	if len(dynamoDBResources) > 0 {
		fmt.Printf("  (%d/%d accessible)\n", accessibleCount, len(dynamoDBResources))
	}
	fmt.Println()

	// Print S3 buckets
	fmt.Println("S3 Buckets:")
	accessibleCount = 0
	for _, res := range s3Resources {
		if res.Accessible {
			fmt.Printf("  ✓ %s\n", res.Name)
			accessibleCount++
		} else {
			fmt.Printf("  ✗ %s: %s\n", res.Name, res.Error)
		}
	}
	if len(s3Resources) > 0 {
		fmt.Printf("  (%d/%d accessible)\n", accessibleCount, len(s3Resources))
	}
	fmt.Println()

	// Print Lambda functions
	fmt.Println("Lambda Functions:")
	accessibleCount = 0
	for _, res := range lambdaResources {
		if res.Accessible {
			// Extract function name from ARN for cleaner display
			funcName := res.Name
			if strings.Contains(funcName, ":function:") {
				parts := strings.Split(funcName, ":function:")
				if len(parts) > 1 {
					funcName = parts[1]
				}
			}
			fmt.Printf("  ✓ %s\n", funcName)
			accessibleCount++
		} else {
			fmt.Printf("  ✗ %s: %s\n", res.Name, res.Error)
		}
	}
	if len(lambdaResources) > 0 {
		fmt.Printf("  (%d/%d accessible)\n", accessibleCount, len(lambdaResources))
	}
	fmt.Println()

	// Overall status
	fmt.Println(strings.Repeat("=", 50))
	if result.Valid {
		fmt.Println("Status: ✓ All resources accessible")
	} else {
		fmt.Printf("Status: ✗ %d errors found\n", len(result.Errors))
		fmt.Println()

		// Print recommendations
		recommendations := validator.GetRecommendations(result)
		if len(recommendations) > 0 {
			fmt.Println("Recommendations:")
			for i, rec := range recommendations {
				fmt.Printf("  %d. %s\n", i+1, rec)
			}
		}
	}

	return nil
}

func outputInfrastructureJSON(result *infrastructure.ValidationResult, resolver *infrastructure.Resolver) error {
	output := map[string]interface{}{
		"valid":      result.Valid,
		"mode":       resolver.IsSelfHosted(),
		"errors":     result.Errors,
		"warnings":   result.Warnings,
		"resources":  result.Resources,
		"region":     resolver.GetRegion(),
		"account_id": resolver.GetAccountID(),
	}

	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	fmt.Println(string(jsonBytes))
	return nil
}

// nist53Baselines are the NIST 800-53 baselines --nist-800-53 accepts, in
// increasing order of strictness. Exported as a slice rather than a set so the
// flag's help text and its error message list them in the same order.
var nist53Baselines = []string{"low", "moderate", "high"}

// nist53DefaultBaseline is what a bare --nist-800-53 means.
const nist53DefaultBaseline = "low"

func isNIST53Baseline(v string) bool {
	for _, b := range nist53Baselines {
		if b == v {
			return true
		}
	}
	return false
}
