package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdaTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

func getAutoscaler(ctx context.Context) (*autoscaler.AutoScaler, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Build table name with environment suffix
	tableName := fmt.Sprintf("%s-%s", autoscaleTableName, autoscaleEnv)

	ec2Client := ec2.NewFromConfig(cfg)
	dynamoClient := dynamodb.NewFromConfig(cfg)
	sqsClient := sqs.NewFromConfig(cfg)
	cloudwatchClient := cloudwatch.NewFromConfig(cfg)

	return autoscaler.NewAutoScaler(&autoscaler.Config{
		EC2Client:        ec2Client,
		DynamoClient:     dynamoClient,
		SQSClient:        sqsClient,
		CloudWatchClient: cloudwatchClient,
		TableName:        tableName,
		RegistryTable:    "spawn-hybrid-registry",
	}), nil
}

func triggerLambda(ctx context.Context, groupID string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	lambdaClient := lambda.NewFromConfig(cfg)
	functionName := fmt.Sprintf("spawn-autoscale-orchestrator-%s", autoscaleEnv)

	payload := fmt.Sprintf(`{"group_id":"%s"}`, groupID)

	_, err = lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(functionName),
		InvocationType: lambdaTypes.InvocationTypeEvent,
		Payload:        []byte(payload),
	})

	return err
}
