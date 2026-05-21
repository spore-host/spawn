package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	lambdaSDK "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

// AutoScaleEvent is the Lambda event payload
type AutoScaleEvent struct {
	GroupID string `json:"group_id,omitempty"`
}

var (
	autoscalerInstance *autoscaler.AutoScaler
	lambdaClient       *lambdaSDK.Client
	functionName       string
)

func init() {
	ctx := context.Background()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Get environment variables
	autoscaleTableName := os.Getenv("AUTOSCALE_GROUPS_TABLE")
	if autoscaleTableName == "" {
		autoscaleTableName = "spawn-autoscale-groups"
	}

	registryTableName := os.Getenv("HYBRID_REGISTRY_TABLE")
	if registryTableName == "" {
		registryTableName = "spawn-hybrid-registry"
	}

	functionName = os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	ec2RoleARN := os.Getenv("EC2_ROLE_ARN")
	externalID := os.Getenv("EC2_EXTERNAL_ID")

	// Create clients
	var ec2Client *ec2.Client
	if ec2RoleARN != "" {
		// Use cross-account role for EC2 operations
		log.Printf("assuming cross-account role: %s", ec2RoleARN)
		stsClient := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, ec2RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "spawn-autoscale-orchestrator"
			if externalID != "" {
				o.ExternalID = aws.String(externalID)
			}
		})
		ec2Cfg, err := config.LoadDefaultConfig(ctx, config.WithCredentialsProvider(creds))
		if err != nil {
			log.Fatalf("failed to create cross-account config: %v", err)
		}
		ec2Client = ec2.NewFromConfig(ec2Cfg)
	} else {
		// Use default credentials
		ec2Client = ec2.NewFromConfig(cfg)
	}

	dynamoClient := dynamodb.NewFromConfig(cfg)
	sqsClient := sqs.NewFromConfig(cfg)
	cloudwatchClient := cloudwatch.NewFromConfig(cfg)
	lambdaClient = lambdaSDK.NewFromConfig(cfg)

	// Create autoscaler
	autoscalerInstance = autoscaler.NewAutoScaler(&autoscaler.Config{
		EC2Client:        ec2Client,
		DynamoClient:     dynamoClient,
		SQSClient:        sqsClient,
		CloudWatchClient: cloudwatchClient,
		TableName:        autoscaleTableName,
		RegistryTable:    registryTableName,
		EC2RoleARN:       ec2RoleARN,
		ExternalID:       externalID,
	})

	log.Printf("autoscale orchestrator initialized (table: %s, registry: %s)",
		autoscaleTableName, registryTableName)
}

func handler(ctx context.Context, event AutoScaleEvent) error {
	start := time.Now()
	log.Printf("autoscale orchestrator started (group_id: %s)", event.GroupID)

	// If specific group ID provided, reconcile just that group
	if event.GroupID != "" {
		if err := autoscalerInstance.Reconcile(ctx, event.GroupID); err != nil {
			log.Printf("failed to reconcile group %s: %v", event.GroupID, err)
			return err
		}
		log.Printf("reconciled group %s in %v", event.GroupID, time.Since(start))
		return nil
	}

	// Otherwise, reconcile all active groups
	groups, err := autoscalerInstance.ListActiveGroups(ctx)
	if err != nil {
		return fmt.Errorf("list active groups: %w", err)
	}

	log.Printf("found %d active groups to reconcile", len(groups))

	for i, group := range groups {
		if err := autoscalerInstance.Reconcile(ctx, group.AutoScaleGroupID); err != nil {
			log.Printf("failed to reconcile group %s: %v", group.GroupName, err)
		}

		// Check timeout (13-minute limit for Lambda)
		if time.Since(start) > 13*time.Minute {
			log.Printf("approaching timeout, self-invoking to continue from group %d/%d",
				i+1, len(groups))

			// Self-invoke to continue with remaining groups
			if i+1 < len(groups) {
				payload := fmt.Sprintf(`{"group_id":"%s"}`, groups[i+1].AutoScaleGroupID)
				_, err := lambdaClient.Invoke(ctx, &lambdaSDK.InvokeInput{
					FunctionName:   &functionName,
					InvocationType: types.InvocationTypeEvent,
					Payload:        []byte(payload),
				})
				if err != nil {
					log.Printf("failed to self-invoke: %v", err)
				}
			}
			break
		}
	}

	log.Printf("autoscale orchestrator completed in %v", time.Since(start))
	return nil
}

func main() {
	lambda.Start(handler)
}
