package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/spore-host/spawn/pkg/security"
)

// Default configuration for shared infrastructure (with environment variable overrides)
const (
	defaultAlertsTable             = "spawn-alerts"
	defaultAlertHistoryTable       = "spawn-alert-history"
	defaultSweepAlertsTopicArnTmpl = "arn:aws:sns:%s:%s:spawn-sweep-alerts"
)

// defaultAccountID is the infra account ID; overridable via SPAWN_INFRA_ACCOUNT_ID.
var defaultAccountID = getEnv("SPAWN_INFRA_ACCOUNT_ID", "966362334030")

// TriggerType represents the type of event that triggers an alert
type TriggerType string

const (
	TriggerComplete       TriggerType = "complete"
	TriggerFailure        TriggerType = "failure"
	TriggerCostThreshold  TriggerType = "cost_threshold"
	TriggerLongRunning    TriggerType = "long_running"
	TriggerInstanceFailed TriggerType = "instance_failed"
)

// DestinationType represents the type of alert destination
type DestinationType string

const (
	DestinationEmail   DestinationType = "email"
	DestinationSlack   DestinationType = "slack"
	DestinationSNS     DestinationType = "sns"
	DestinationWebhook DestinationType = "webhook"
)

// Destination represents where to send alert notifications
type Destination struct {
	Type   DestinationType `json:"type" dynamodbav:"type"`
	Target string          `json:"target" dynamodbav:"target"`
}

// AlertConfig represents an alert configuration
type AlertConfig struct {
	AlertID         string        `json:"alert_id" dynamodbav:"alert_id"`
	SweepID         string        `json:"sweep_id,omitempty" dynamodbav:"sweep_id,omitempty"`
	ScheduleID      string        `json:"schedule_id,omitempty" dynamodbav:"schedule_id,omitempty"`
	UserID          string        `json:"user_id" dynamodbav:"user_id"`
	Triggers        []TriggerType `json:"triggers" dynamodbav:"triggers"`
	Destinations    []Destination `json:"destinations" dynamodbav:"destinations"`
	CostThreshold   float64       `json:"cost_threshold,omitempty" dynamodbav:"cost_threshold,omitempty"`
	DurationMinutes int           `json:"duration_minutes,omitempty" dynamodbav:"duration_minutes,omitempty"`
}

// SweepEvent is the input from DynamoDB stream or EventBridge
type SweepEvent struct {
	SweepID      string  `json:"sweep_id"`
	Status       string  `json:"status"` // "completed", "failed", "running"
	TotalCost    float64 `json:"total_cost,omitempty"`
	Duration     int     `json:"duration_minutes,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Instances    int     `json:"instances,omitempty"`
	Failed       int     `json:"failed,omitempty"`
}

// SlackMessage represents a Slack webhook payload
type SlackMessage struct {
	Text        string            `json:"text"`
	Attachments []SlackAttachment `json:"attachments,omitempty"`
}

// SlackAttachment represents a Slack message attachment
type SlackAttachment struct {
	Color  string       `json:"color"`
	Fields []SlackField `json:"fields"`
}

// SlackField represents a field in a Slack attachment
type SlackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

var (
	dynamoClient        *dynamodb.Client
	snsClient           *sns.Client
	kmsClient           *kms.Client
	httpClient          *http.Client
	region              string
	accountID           string
	kmsKeyID            string
	encryptionEnabled   bool
	alertsTable         string
	alertHistoryTable   string
	sweepAlertsTopicArn string
)

func init() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	dynamoClient = dynamodb.NewFromConfig(cfg)
	snsClient = sns.NewFromConfig(cfg)
	kmsClient = kms.NewFromConfig(cfg)
	httpClient = &http.Client{Timeout: 10 * time.Second}

	region = cfg.Region
	accountID = getEnv("SPAWN_ACCOUNT_ID", defaultAccountID)

	// Load table names from environment variables with fallbacks
	alertsTable = getEnv("SPAWN_ALERTS_TABLE", defaultAlertsTable)
	alertHistoryTable = getEnv("SPAWN_ALERT_HISTORY_TABLE", defaultAlertHistoryTable)

	// Build SNS topic ARN
	sweepAlertsTopicArn = fmt.Sprintf(
		getEnv("SPAWN_SWEEP_ALERTS_TOPIC_ARN_TEMPLATE", defaultSweepAlertsTopicArnTmpl),
		region,
		accountID,
	)

	// Enable webhook encryption if KMS key is configured
	kmsKeyID = os.Getenv("WEBHOOK_KMS_KEY_ID")
	encryptionEnabled = kmsKeyID != ""
	if encryptionEnabled {
		log.Printf("Webhook encryption enabled with KMS key: %s", kmsKeyID)
	} else {
		log.Printf("Webhook encryption disabled (no KMS key configured)")
	}

	log.Printf("Configuration: account=%s, alerts_table=%s, alert_history_table=%s",
		accountID, alertsTable, alertHistoryTable)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func handler(ctx context.Context, event SweepEvent) error {
	log.Printf("Processing alert event for sweep: %s, status: %s", event.SweepID, event.Status)

	// Determine trigger type based on event
	var trigger TriggerType
	switch event.Status {
	case "completed":
		trigger = TriggerComplete
	case "failed":
		trigger = TriggerFailure
	case "running":
		// Check for cost threshold or long-running
		// This would be called periodically for running sweeps
		return handleRunningAlerts(ctx, event)
	default:
		log.Printf("Unknown status: %s", event.Status)
		return nil
	}

	// Get alerts for this sweep
	alerts, err := getAlertsForSweep(ctx, event.SweepID)
	if err != nil {
		return fmt.Errorf("get alerts: %w", err)
	}

	if len(alerts) == 0 {
		log.Printf("No alerts configured for sweep: %s", event.SweepID)
		return nil
	}

	// Process each alert
	for _, alert := range alerts {
		// Check if alert has this trigger
		if !hasTrigger(alert, trigger) {
			continue
		}

		// Send notifications
		if err := sendNotifications(ctx, alert, event, trigger); err != nil {
			log.Printf("Error sending notifications for alert %s: %v", alert.AlertID, err)
			recordAlertHistory(ctx, alert.AlertID, alert.UserID, event.SweepID, trigger, fmt.Sprintf("Failed: %v", err), false, err.Error())
			continue
		}

		// Record success
		message := fmt.Sprintf("Sweep %s: %s", event.SweepID, trigger)
		recordAlertHistory(ctx, alert.AlertID, alert.UserID, event.SweepID, trigger, message, true, "")
	}

	return nil
}

func handleRunningAlerts(ctx context.Context, event SweepEvent) error {
	alerts, err := getAlertsForSweep(ctx, event.SweepID)
	if err != nil {
		return err
	}

	for _, alert := range alerts {
		// Check cost threshold
		if hasTrigger(alert, TriggerCostThreshold) && event.TotalCost > alert.CostThreshold {
			sendNotifications(ctx, alert, event, TriggerCostThreshold)
			recordAlertHistory(ctx, alert.AlertID, alert.UserID, event.SweepID, TriggerCostThreshold,
				fmt.Sprintf("Cost threshold exceeded: $%.2f > $%.2f", event.TotalCost, alert.CostThreshold), true, "")
		}

		// Check long-running
		if hasTrigger(alert, TriggerLongRunning) && event.Duration > alert.DurationMinutes {
			sendNotifications(ctx, alert, event, TriggerLongRunning)
			recordAlertHistory(ctx, alert.AlertID, alert.UserID, event.SweepID, TriggerLongRunning,
				fmt.Sprintf("Duration exceeded: %dm > %dm", event.Duration, alert.DurationMinutes), true, "")
		}

		// Check instance failures
		if hasTrigger(alert, TriggerInstanceFailed) && event.Failed > 0 {
			sendNotifications(ctx, alert, event, TriggerInstanceFailed)
			recordAlertHistory(ctx, alert.AlertID, alert.UserID, event.SweepID, TriggerInstanceFailed,
				fmt.Sprintf("Instance failures detected: %d", event.Failed), true, "")
		}
	}

	return nil
}

func getAlertsForSweep(ctx context.Context, sweepID string) ([]*AlertConfig, error) {
	result, err := dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(alertsTable),
		IndexName:              aws.String("sweep_id-index"),
		KeyConditionExpression: aws.String("sweep_id = :sweep_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		return nil, err
	}

	var alerts []*AlertConfig
	for _, item := range result.Items {
		var alert AlertConfig
		if err := attributevalue.UnmarshalMap(item, &alert); err != nil {
			log.Printf("Error unmarshaling alert: %v", err)
			continue
		}

		// Decrypt webhook URLs if encryption is enabled
		if encryptionEnabled {
			for i, dest := range alert.Destinations {
				if dest.Type == DestinationSlack || dest.Type == DestinationWebhook {
					// Only decrypt if encrypted
					if security.IsEncrypted(dest.Target) {
						decrypted, err := security.DecryptSecret(ctx, kmsClient, dest.Target)
						if err != nil {
							log.Printf("Error decrypting webhook URL for alert %s: %v", alert.AlertID, err)
							continue
						}
						alert.Destinations[i].Target = decrypted
					}
				}
			}
		}

		alerts = append(alerts, &alert)
	}

	return alerts, nil
}

func hasTrigger(alert *AlertConfig, trigger TriggerType) bool {
	for _, t := range alert.Triggers {
		if t == trigger {
			return true
		}
	}
	return false
}

func sendNotifications(ctx context.Context, alert *AlertConfig, event SweepEvent, trigger TriggerType) error {
	message := formatMessage(event, trigger)

	var lastErr error
	for _, dest := range alert.Destinations {
		var err error
		switch dest.Type {
		case DestinationEmail:
			err = sendEmailNotification(ctx, dest.Target, message, event)
		case DestinationSlack:
			err = sendSlackNotification(ctx, dest.Target, message, event, trigger)
		case DestinationSNS:
			err = sendSNSNotification(ctx, dest.Target, message)
		case DestinationWebhook:
			err = sendWebhookNotification(ctx, dest.Target, message, event)
		default:
			log.Printf("Unknown destination type: %s", dest.Type)
			continue
		}

		if err != nil {
			// Mask webhook URLs in logs
			target := dest.Target
			if dest.Type == DestinationSlack || dest.Type == DestinationWebhook {
				target = security.MaskURL(dest.Target)
			}
			log.Printf("Error sending to %s (%s): %v", target, dest.Type, err)
			lastErr = err
		}
	}

	return lastErr
}

func sendEmailNotification(ctx context.Context, email string, message string, event SweepEvent) error {
	subject := fmt.Sprintf("[spawn] Sweep %s: %s", event.SweepID, event.Status)

	body := fmt.Sprintf(`%s

Sweep ID: %s
Status: %s
`, message, event.SweepID, event.Status)

	if event.TotalCost > 0 {
		body += fmt.Sprintf("Cost: $%.2f\n", event.TotalCost)
	}
	if event.Instances > 0 {
		body += fmt.Sprintf("Instances: %d\n", event.Instances)
	}
	if event.Failed > 0 {
		body += fmt.Sprintf("Failed: %d\n", event.Failed)
	}

	body += "\nView details: spawn status " + event.SweepID + "\n"

	// Send via SNS topic with email subscription
	_, err := snsClient.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(sweepAlertsTopicArn),
		Subject:  aws.String(subject),
		Message:  aws.String(body),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"email": {
				DataType:    aws.String("String"),
				StringValue: aws.String(email),
			},
		},
	})

	return err
}

func sendSlackNotification(ctx context.Context, webhookURL string, message string, event SweepEvent, trigger TriggerType) error {
	color := "good"
	emoji := "✅"
	if trigger == TriggerFailure {
		color = "danger"
		emoji = "❌"
	} else if trigger == TriggerCostThreshold {
		color = "warning"
		emoji = "⚠️"
	}

	slackMsg := SlackMessage{
		Text: fmt.Sprintf("%s %s", emoji, message),
		Attachments: []SlackAttachment{
			{
				Color: color,
				Fields: []SlackField{
					{Title: "Sweep ID", Value: event.SweepID, Short: true},
					{Title: "Status", Value: event.Status, Short: true},
				},
			},
		},
	}

	if event.TotalCost > 0 {
		slackMsg.Attachments[0].Fields = append(slackMsg.Attachments[0].Fields,
			SlackField{Title: "Cost", Value: fmt.Sprintf("$%.2f", event.TotalCost), Short: true})
	}
	if event.Instances > 0 {
		slackMsg.Attachments[0].Fields = append(slackMsg.Attachments[0].Fields,
			SlackField{Title: "Instances", Value: fmt.Sprintf("%d", event.Instances), Short: true})
	}

	payload, err := json.Marshal(slackMsg)
	if err != nil {
		return fmt.Errorf("marshal slack message: %w", err)
	}

	resp, err := httpClient.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack webhook returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func sendSNSNotification(ctx context.Context, topicArn string, message string) error {
	_, err := snsClient.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(topicArn),
		Message:  aws.String(message),
	})
	return err
}

func sendWebhookNotification(ctx context.Context, webhookURL string, message string, event SweepEvent) error {
	payload := map[string]interface{}{
		"message":  message,
		"sweep_id": event.SweepID,
		"status":   event.Status,
		"cost":     event.TotalCost,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	resp, err := httpClient.Post(webhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("post to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func formatMessage(event SweepEvent, trigger TriggerType) string {
	switch trigger {
	case TriggerComplete:
		return fmt.Sprintf("Sweep completed successfully: %s", event.SweepID)
	case TriggerFailure:
		msg := fmt.Sprintf("Sweep failed: %s", event.SweepID)
		if event.ErrorMessage != "" {
			msg += fmt.Sprintf(" - %s", event.ErrorMessage)
		}
		return msg
	case TriggerCostThreshold:
		return fmt.Sprintf("Cost threshold exceeded for sweep %s: $%.2f", event.SweepID, event.TotalCost)
	case TriggerLongRunning:
		return fmt.Sprintf("Sweep %s running longer than expected: %dm", event.SweepID, event.Duration)
	case TriggerInstanceFailed:
		return fmt.Sprintf("Instance failures detected in sweep %s: %d failed", event.SweepID, event.Failed)
	default:
		return fmt.Sprintf("Sweep alert: %s - %s", event.SweepID, trigger)
	}
}

func recordAlertHistory(ctx context.Context, alertID, userID, sweepID string, trigger TriggerType, message string, success bool, errMsg string) {
	history := map[string]types.AttributeValue{
		"alert_id":  &types.AttributeValueMemberS{Value: alertID},
		"timestamp": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		"user_id":   &types.AttributeValueMemberS{Value: userID},
		"sweep_id":  &types.AttributeValueMemberS{Value: sweepID},
		"trigger":   &types.AttributeValueMemberS{Value: string(trigger)},
		"message":   &types.AttributeValueMemberS{Value: message},
		"success":   &types.AttributeValueMemberBOOL{Value: success},
		"ttl":       &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().Add(90*24*time.Hour).Unix())},
	}

	if errMsg != "" {
		history["error"] = &types.AttributeValueMemberS{Value: errMsg}
	}

	_, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(alertHistoryTable),
		Item:      history,
	})
	if err != nil {
		log.Printf("Error recording alert history: %v", err)
	}
}

func main() {
	lambda.Start(handler)
}
