package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

const (
	alertPreferencesTable = "spawn-alert-preferences"
	costHistoryTable      = "spawn-cost-history"
)

// AlertPreferences is the user's alert configuration
type AlertPreferences struct {
	UserID                 string  `dynamodbav:"user_id"`
	Enabled                bool    `dynamodbav:"enabled"`
	CostThresholdHourly    float64 `dynamodbav:"cost_threshold_hourly,omitempty"`
	CostThresholdDaily     float64 `dynamodbav:"cost_threshold_daily,omitempty"`
	InstanceCountThreshold int     `dynamodbav:"instance_count_threshold,omitempty"`
	QueueDepthHigh         int     `dynamodbav:"queue_depth_high,omitempty"`
	QueueDepthLow          int     `dynamodbav:"queue_depth_low,omitempty"`
	NotificationEmail      string  `dynamodbav:"notification_email,omitempty"`
	LastAlertedCost        string  `dynamodbav:"last_alerted_cost,omitempty"`
	LastAlertedInstance    string  `dynamodbav:"last_alerted_instance,omitempty"`
}

// CostHistoryRecord is a cost snapshot
type CostHistoryRecord struct {
	UserID        string  `dynamodbav:"user_id"`
	Timestamp     string  `dynamodbav:"timestamp"`
	HourlyCost    float64 `dynamodbav:"hourly_cost"`
	InstanceCount int     `dynamodbav:"instance_count"`
}

var (
	dynamodbClient *dynamodb.Client
	snsClient      *sns.Client
	snsTopicArn    string
	awsCfg         aws.Config
)

func init() {
	var err error
	awsCfg, err = config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}
	dynamodbClient = dynamodb.NewFromConfig(awsCfg)
	snsClient = sns.NewFromConfig(awsCfg)
	snsTopicArn = os.Getenv("COST_ALERTS_TOPIC_ARN")
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) error {
	// Scan all enabled alert preferences
	prefs, err := listEnabledPreferences(ctx)
	if err != nil {
		return fmt.Errorf("list preferences: %w", err)
	}

	log.Printf("Evaluating alerts for %d users", len(prefs))

	for _, pref := range prefs {
		if err := evaluateUserAlerts(ctx, pref); err != nil {
			log.Printf("Failed to evaluate alerts for user %s: %v", pref.UserID, err)
		}
	}

	return nil
}

func listEnabledPreferences(ctx context.Context) ([]AlertPreferences, error) {
	var prefs []AlertPreferences
	var lastKey map[string]ddbTypes.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:        aws.String(alertPreferencesTable),
			FilterExpression: aws.String("enabled = :enabled"),
			ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
				":enabled": &ddbTypes.AttributeValueMemberBOOL{Value: true},
			},
		}
		if lastKey != nil {
			input.ExclusiveStartKey = lastKey
		}

		result, err := dynamodbClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan preferences: %w", err)
		}

		for _, item := range result.Items {
			var p AlertPreferences
			if err := attributevalue.UnmarshalMap(item, &p); err == nil {
				prefs = append(prefs, p)
			}
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}

	return prefs, nil
}

func evaluateUserAlerts(ctx context.Context, pref AlertPreferences) error {
	var alerts []string
	now := time.Now()

	// Get latest cost record
	if pref.CostThresholdHourly > 0 || pref.CostThresholdDaily > 0 || pref.InstanceCountThreshold > 0 {
		latestCost, err := getLatestCostRecord(ctx, pref.UserID)
		if err == nil && latestCost != nil {
			// Hourly cost threshold
			if pref.CostThresholdHourly > 0 && latestCost.HourlyCost > pref.CostThresholdHourly {
				// Check cooldown (don't alert more than once per hour)
				if shouldAlert(pref.LastAlertedCost, 1*time.Hour) {
					alerts = append(alerts, fmt.Sprintf("Hourly cost $%.2f exceeds threshold $%.2f",
						latestCost.HourlyCost, pref.CostThresholdHourly))
				}
			}

			// Daily cost threshold
			if pref.CostThresholdDaily > 0 {
				dailyCost := latestCost.HourlyCost * 24
				if dailyCost > pref.CostThresholdDaily && shouldAlert(pref.LastAlertedCost, 4*time.Hour) {
					alerts = append(alerts, fmt.Sprintf("Projected daily cost $%.2f exceeds threshold $%.2f",
						dailyCost, pref.CostThresholdDaily))
				}
			}

			// Instance count threshold
			if pref.InstanceCountThreshold > 0 && latestCost.InstanceCount > pref.InstanceCountThreshold {
				if shouldAlert(pref.LastAlertedInstance, 1*time.Hour) {
					alerts = append(alerts, fmt.Sprintf("Instance count %d exceeds threshold %d",
						latestCost.InstanceCount, pref.InstanceCountThreshold))
				}
			}
		}
	}

	if len(alerts) == 0 {
		return nil
	}

	// Queue depth alerts - evaluated separately per autoscale group
	// (handled by the autoscale system directly)

	// Publish alerts to SNS
	if snsTopicArn == "" {
		log.Printf("SNS topic ARN not configured, skipping alert for user %s", pref.UserID)
		return nil
	}

	message := map[string]interface{}{
		"user_id":   pref.UserID,
		"alerts":    alerts,
		"timestamp": now.UTC().Format(time.RFC3339),
	}
	msgBytes, _ := json.Marshal(message)

	subject := fmt.Sprintf("Spawn Cost Alert for %s", pref.UserID)
	if pref.NotificationEmail != "" {
		subject = fmt.Sprintf("Spawn Alert - %d threshold(s) exceeded", len(alerts))
	}

	_, err := snsClient.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(snsTopicArn),
		Subject:  aws.String(subject),
		Message:  aws.String(string(msgBytes)),
	})
	if err != nil {
		return fmt.Errorf("publish alert: %w", err)
	}

	// Update last-alerted timestamps to prevent spam
	updateExpr := "SET last_alerted_cost = :ts, last_alerted_instance = :ts"
	_, err = dynamodbClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(alertPreferencesTable),
		Key: map[string]ddbTypes.AttributeValue{
			"user_id": &ddbTypes.AttributeValueMemberS{Value: pref.UserID},
		},
		UpdateExpression: aws.String(updateExpr),
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":ts": &ddbTypes.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339)},
		},
	})
	return err
}

func getLatestCostRecord(ctx context.Context, userID string) (*CostHistoryRecord, error) {
	result, err := dynamodbClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(costHistoryTable),
		KeyConditionExpression: aws.String("user_id = :uid"),
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":uid": &ddbTypes.AttributeValueMemberS{Value: userID},
		},
		ScanIndexForward: aws.Bool(false), // newest first
		Limit:            aws.Int32(1),
	})
	if err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	var record CostHistoryRecord
	if err := attributevalue.UnmarshalMap(result.Items[0], &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func shouldAlert(lastAlerted string, cooldown time.Duration) bool {
	if lastAlerted == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, lastAlerted)
	if err != nil {
		return true
	}
	return time.Since(t) > cooldown
}
