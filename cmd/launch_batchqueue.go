package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/userdata"
)

// launchWithBatchQueue launches a single instance with a batch job queue
func launchWithBatchQueue(ctx context.Context, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	fmt.Fprintf(os.Stderr, "\n📦 Launching Batch Queue Instance\n\n")

	// Load and validate queue configuration
	var queueConfig *queue.QueueConfig
	var err error

	if queueTemplate != "" {
		// Generate from template
		fmt.Fprintf(os.Stderr, "📋 Loading template: %s\n", queueTemplate)
		tmpl, err := queue.LoadTemplate(queueTemplate)
		if err != nil {
			return fmt.Errorf("failed to load template: %w", err)
		}

		// Show required variables if none provided
		if len(templateVars) == 0 {
			var requiredVars []string
			for _, v := range tmpl.Variables {
				if v.Required {
					requiredVars = append(requiredVars, v.Name)
				}
			}
			if len(requiredVars) > 0 {
				return fmt.Errorf("template requires variables: %v\nUse --template-var KEY=VALUE", requiredVars)
			}
		}

		fmt.Fprintf(os.Stderr, "✓ Template loaded: %s (%d jobs)\n", tmpl.Description, len(tmpl.Config.Jobs))
		fmt.Fprintf(os.Stderr, "🔧 Substituting variables...\n")

		queueConfig, err = tmpl.Substitute(templateVars)
		if err != nil {
			return fmt.Errorf("failed to generate queue from template: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ Queue generated: %d jobs\n", len(queueConfig.Jobs))
	} else if batchQueueFile != "" {
		// Load from file
		fmt.Fprintf(os.Stderr, "📋 Loading queue configuration...\n")
		queueConfig, err = queue.LoadConfig(batchQueueFile)
		if err != nil {
			return fmt.Errorf("failed to load queue configuration: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Queue loaded: %d jobs\n", len(queueConfig.Jobs))
	} else {
		return fmt.Errorf("either --batch-queue or --queue-template is required")
	}

	// Generate queue ID if not set
	if queueConfig.QueueID == "" {
		queueConfig.QueueID = queue.GenerateQueueID()
	}

	// Validate required flags
	if instanceType == "" {
		return fmt.Errorf("--instance-type is required for batch queue mode")
	}

	// Auto-detect region if not specified
	queueRegion := region
	if queueRegion == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, instanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			queueRegion = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			queueRegion = detectedRegion
		}
	}

	// Load AWS config for spore-host-dev (where EC2 instances run)
	devCfg, err := spawnconfig.LoadComputeAWSConfig(ctx, queueRegion)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get AWS account ID
	accountID, err := aws.NewClientFromConfig(devCfg).GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}

	// Upload queue configuration to S3
	fmt.Fprintf(os.Stderr, "\n📤 Uploading queue configuration to S3...\n")

	// Create queue JSON
	queueJSON, err := json.MarshalIndent(queueConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal queue config: %w", err)
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "spawn-queue-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.Write(queueJSON); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	_ = tmpFile.Close()

	// Upload to S3
	stagingClient := staging.NewClient(devCfg, accountID)
	scheduleBucket, s3Key, size, _, err := stagingClient.UploadScheduleParams(ctx, tmpFile.Name(), queueConfig.QueueID, queueRegion)
	if err != nil {
		return fmt.Errorf("failed to upload queue config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Uploaded: %s (%.2f KB)\n", s3Key, float64(size)/1024)

	// Build combined user-data: standard spored installer + queue runner.
	// The queue runner script waits for spored to be ready before executing jobs.
	s3URL := fmt.Sprintf("s3://%s/%s", scheduleBucket, s3Key)
	queueRunnerScript := userdata.GenerateQueueRunnerUserData(s3URL, queueConfig.QueueID)

	// Build the standard spored userdata (SSH key setup + spored installer)
	stdScript, buildErr := buildUserData(plat, &aws.LaunchConfig{
		InstanceType: instanceType,
		Region:       queueRegion,
	}, "")
	var combinedScript string
	if buildErr == nil && stdScript != "" {
		// Append queue runner after spored installer (strip duplicate #!/bin/bash header)
		queuePart := queueRunnerScript
		if len(queuePart) > 3 && queuePart[:2] == "#!" {
			// Find end of first line to strip shebang
			nl := 0
			for i, c := range queuePart {
				if c == '\n' {
					nl = i + 1
					break
				}
			}
			queuePart = queuePart[nl:]
		}
		combinedScript = stdScript + "\n\n# === Batch queue runner ===\n" + queuePart
	} else {
		combinedScript = queueRunnerScript
	}
	queueUserData := encodeUserData(combinedScript)

	// Auto-detect AMI if not specified
	resolvedAMI := ami
	if resolvedAMI == "" {
		awsClientForAMI, amiErr := aws.NewClientWithRegion(ctx, queueRegion)
		if amiErr == nil {
			if detected, amiErr2 := awsClientForAMI.GetRecommendedAMI(ctx, queueRegion, instanceType); amiErr2 == nil {
				resolvedAMI = detected
			}
		}
	}

	// Build launch config
	instanceName := name
	if instanceName == "" {
		instanceName = fmt.Sprintf("%s-%s", queueConfig.QueueName, queueConfig.QueueID)
	}
	launchConfig := &aws.LaunchConfig{
		Name:         instanceName,
		InstanceType: instanceType,
		Region:       queueRegion,
		AMI:          resolvedAMI,
		KeyName:      keyPair,
		UserData:     queueUserData,
		Spot:         spot,
		SpotMaxPrice: spotMaxPrice,
		Hibernate:    hibernate,
		TTL:          queueConfig.GlobalTimeout, // Use global timeout as TTL
		DNSName:      instanceName,
	}

	// Add IAM role if specified
	if iamRole != "" {
		launchConfig.IamInstanceProfile = iamRole
	}

	// Add network config if specified (sgIDs may be empty — let spawn auto-create)
	if len(sgIDs) > 0 {
		launchConfig.SecurityGroupIDs = sgIDs
	}
	if subnetID != "" {
		launchConfig.SubnetID = subnetID
	}

	// CRITICAL SAFETY CHECK: Prevent zombie instances
	// If neither TTL nor idle timeout are set, default to 1h idle timeout
	if launchConfig.TTL == "" && launchConfig.IdleTimeout == "" && !noTimeout {
		launchConfig.IdleTimeout = "1h"
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h to prevent zombie instances\n")
		fmt.Fprintf(os.Stderr, "   Instance will terminate after 1 hour of inactivity.\n")
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	} else if noTimeout {
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified\n")
		fmt.Fprintf(os.Stderr, "   Instance will run indefinitely until manually terminated.\n\n")
	}

	// Initialize AWS client pinned to the resolved queue region (#276).
	awsClient, err := aws.NewClientWithRegion(ctx, queueRegion)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Set up SSH key pair if not specified
	if launchConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, queueRegion, launchConfig.AMI, plat)
		if err != nil {
			return fmt.Errorf("failed to setup SSH key: %w", err)
		}
		launchConfig.KeyName = keyName
	}

	// Set up IAM instance profile if not specified
	if launchConfig.IamInstanceProfile == "" {
		instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
		if err != nil {
			return fmt.Errorf("failed to setup IAM role: %w", err)
		}
		launchConfig.IamInstanceProfile = instanceProfile
	}

	// Launch instance
	fmt.Fprintf(os.Stderr, "\n🚀 Launching instance...\n")
	instance, err := awsClient.Launch(ctx, *launchConfig)
	if err != nil {
		return fmt.Errorf("failed to launch instance: %w", err)
	}

	// Write instance ID to file for workflow integration
	if err := writeOutputID(instance.InstanceID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write instance ID to file: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Batch queue instance launched!\n\n")
	fmt.Fprintf(os.Stderr, "Queue ID:       %s\n", queueConfig.QueueID)
	fmt.Fprintf(os.Stderr, "Instance ID:    %s\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "Instance Type:  %s\n", instanceType)
	fmt.Fprintf(os.Stderr, "Region:         %s\n", queueRegion)
	fmt.Fprintf(os.Stderr, "Total Jobs:     %d\n", len(queueConfig.Jobs))
	fmt.Fprintf(os.Stderr, "Global Timeout: %s\n", queueConfig.GlobalTimeout)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "The instance will execute jobs sequentially according to dependencies.\n")
	fmt.Fprintf(os.Stderr, "Results will be uploaded to: %s/%s/\n", queueConfig.ResultS3Bucket, queueConfig.ResultS3Prefix)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "To check queue status:\n")
	fmt.Fprintf(os.Stderr, "  spawn queue status %s\n\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "To download results:\n")
	fmt.Fprintf(os.Stderr, "  spawn queue results %s --output ./results/\n", queueConfig.QueueID)
	fmt.Fprintf(os.Stderr, "\n")

	return nil
}
