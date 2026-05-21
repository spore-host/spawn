package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	userAccountsTable = "spawn-user-accounts"
	costHistoryTable  = "spawn-cost-history"
	ttlDays           = 90
)

// aws regions to query for instances
var awsRegions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"eu-west-1", "eu-west-2", "eu-central-1",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
}

// instancePricing maps instance type to hourly on-demand rate
var instancePricing = map[string]float64{
	"t3.micro":   0.0104,
	"t3.small":   0.0208,
	"t3.medium":  0.0416,
	"t3.large":   0.0832,
	"t3.xlarge":  0.1664,
	"t3.2xlarge": 0.3328,
	"t3a.micro":  0.0094,
	"t3a.small":  0.0188,
	"t3a.medium": 0.0376,
	"t3a.large":  0.0752,
	"m5.large":   0.096,
	"m5.xlarge":  0.192,
	"m5.2xlarge": 0.384,
	"c5.large":   0.085,
	"c5.xlarge":  0.17,
	"c5.2xlarge": 0.34,
	"r5.large":   0.126,
	"r5.xlarge":  0.252,
}

// UserAccountRecord is a minimal record from spawn-user-accounts
type UserAccountRecord struct {
	UserID    string `dynamodbav:"user_id"`
	CliIamArn string `dynamodbav:"cli_iam_arn,omitempty"`
}

// CostComponents holds cost breakdown by type
type CostComponents struct {
	Compute float64 `dynamodbav:"compute" json:"compute"`
	Storage float64 `dynamodbav:"storage" json:"storage"`
	Network float64 `dynamodbav:"network" json:"network"`
}

// CostHistoryRecord is written to spawn-cost-history
type CostHistoryRecord struct {
	UserID          string         `dynamodbav:"user_id"`
	Timestamp       string         `dynamodbav:"timestamp"`
	HourlyCost      float64        `dynamodbav:"hourly_cost"`
	MonthlyEstimate float64        `dynamodbav:"monthly_estimate"`
	InstanceCount   int            `dynamodbav:"instance_count"`
	Breakdown       CostComponents `dynamodbav:"breakdown"`
	TTL             int64          `dynamodbav:"ttl"`
}

var (
	dynamodbClient *dynamodb.Client
	awsCfg         aws.Config
)

func init() {
	var err error
	awsCfg, err = config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}
	dynamodbClient = dynamodb.NewFromConfig(awsCfg)
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) error {
	// Scan all user accounts
	users, err := listAllUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	log.Printf("Collecting cost history for %d users", len(users))
	now := time.Now()

	for _, user := range users {
		userID := user.CliIamArn
		if userID == "" {
			userID = user.UserID
		}
		if userID == "" {
			continue
		}

		cost, count, err := calculateUserCost(ctx, userID)
		if err != nil {
			log.Printf("Failed to calculate cost for user %s: %v", userID, err)
			continue
		}

		record := CostHistoryRecord{
			UserID:          userID,
			Timestamp:       now.UTC().Format(time.RFC3339),
			HourlyCost:      cost.Compute + cost.Network,
			MonthlyEstimate: (cost.Compute + cost.Network) * 730,
			InstanceCount:   count,
			Breakdown:       cost,
			TTL:             now.AddDate(0, 0, ttlDays).Unix(),
		}

		if err := writeCostRecord(ctx, record); err != nil {
			log.Printf("Failed to write cost record for user %s: %v", userID, err)
		}
	}

	return nil
}

func listAllUsers(ctx context.Context) ([]UserAccountRecord, error) {
	var users []UserAccountRecord
	var lastKey map[string]ddbTypes.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:            aws.String(userAccountsTable),
			ProjectionExpression: aws.String("user_id, cli_iam_arn"),
		}
		if lastKey != nil {
			input.ExclusiveStartKey = lastKey
		}

		result, err := dynamodbClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan users: %w", err)
		}

		for _, item := range result.Items {
			var u UserAccountRecord
			if err := attributevalue.UnmarshalMap(item, &u); err == nil {
				users = append(users, u)
			}
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}

	return users, nil
}

func calculateUserCost(ctx context.Context, cliIamArn string) (CostComponents, int, error) {
	compute := 0.0
	network := 0.0
	totalCount := 0

	for _, region := range awsRegions {
		regionCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			continue
		}

		ec2Client := ec2.NewFromConfig(regionCfg)

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2Types.Filter{
				{Name: aws.String("tag:spawn:iam-user"), Values: []string{cliIamArn}},
				{Name: aws.String("instance-state-name"), Values: []string{"running"}},
			},
		})
		if err != nil {
			continue
		}

		for _, reservation := range result.Reservations {
			for _, instance := range reservation.Instances {
				instanceType := string(instance.InstanceType)
				isSpot := instance.InstanceLifecycle == ec2Types.InstanceLifecycleTypeSpot

				price, ok := instancePricing[instanceType]
				if !ok {
					price = 0.10
				}

				if isSpot {
					price *= 0.30
				}

				compute += price
				if instance.PublicIpAddress != nil && *instance.PublicIpAddress != "" {
					network += 0.005
				}
				totalCount++
			}
		}
	}

	return CostComponents{
		Compute: compute,
		Storage: 0, // EBS costs tracked separately
		Network: network,
	}, totalCount, nil
}

func writeCostRecord(ctx context.Context, record CostHistoryRecord) error {
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}

	_, err = dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(costHistoryTable),
		Item:      item,
	})
	return err
}
