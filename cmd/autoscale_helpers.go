package cmd

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdaTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/autoscaler"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// autoscaleConfig resolves the AWS config for every autoscale call site.
//
// This is spawn#774. Both sites used to call config.LoadDefaultConfig(ctx) with
// no options, which took the region from the ambient chain and IGNORED the
// root --region flag — a flag whose own help text says it "overrides
// SPORE_REGION/AWS_REGION and the shared config". It overrode nothing:
//
//	$ spawn autoscale status --region us-east-1   # probed us-west-2
//	$ AWS_REGION=us-east-1 … --region us-west-2   # used us-east-1
//
// Ignored in both directions, with the env winning over the flag that claims to
// beat it. The visible symptoms were a DynamoDB scan failing with
// ResourceNotFoundException in a region holding no groups table, and
// triggerLambda invoking a function by name in a region where it may not exist.
//
// Routing through NewClientWithRegion fixes the shared PROFILE too, which these
// sites also ignored, and uses the same resolution order as every other command
// (explicit region > sporeconfig shared region > ambient).
func autoscaleConfig(ctx context.Context) (aws.Config, error) {
	client, err := spawnaws.NewClientWithRegion(ctx, spawnRegion)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	return client.Config(), nil
}

func getAutoscaler(ctx context.Context) (*autoscaler.AutoScaler, error) {
	cfg, err := autoscaleConfig(ctx)
	if err != nil {
		return nil, err
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
	cfg, err := autoscaleConfig(ctx)
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
