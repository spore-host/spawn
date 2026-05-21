package alerts

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/google/uuid"
	"github.com/spore-host/spawn/pkg/security"
)

const (
	AlertsTableName       = "spawn-alerts"
	AlertHistoryTableName = "spawn-alert-history"
	AlertTTLDays          = 90
)

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
	Target string          `json:"target" dynamodbav:"target"` // email, slack URL, SNS ARN, webhook URL
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
	DurationMinutes int           `json:"duration_minutes,omitempty" dynamodbav:"duration_minutes,omitempty"` // For long_running trigger
	CreatedAt       time.Time     `json:"created_at" dynamodbav:"created_at"`
	TTL             int64         `json:"ttl,omitempty" dynamodbav:"ttl,omitempty"` // Unix timestamp for DynamoDB TTL
}

// AlertHistory represents a record of an alert that was sent
type AlertHistory struct {
	AlertID   string      `json:"alert_id" dynamodbav:"alert_id"`
	Timestamp time.Time   `json:"timestamp" dynamodbav:"timestamp"`
	UserID    string      `json:"user_id" dynamodbav:"user_id"`
	SweepID   string      `json:"sweep_id,omitempty" dynamodbav:"sweep_id,omitempty"`
	Trigger   TriggerType `json:"trigger" dynamodbav:"trigger"`
	Message   string      `json:"message" dynamodbav:"message"`
	Success   bool        `json:"success" dynamodbav:"success"`
	Error     string      `json:"error,omitempty" dynamodbav:"error,omitempty"`
	TTL       int64       `json:"ttl,omitempty" dynamodbav:"ttl,omitempty"`
}

// Client provides alert management operations
type Client struct {
	db                    *dynamodb.Client
	kms                   *kms.Client
	kmsKeyID              string
	encryptionEnabled     bool
	alertsTableName       string
	alertHistoryTableName string
}

// NewClient creates a new alerts client
func NewClient(db *dynamodb.Client) *Client {
	return &Client{
		db:                    db,
		encryptionEnabled:     false, // Backward compatible: encryption disabled by default
		alertsTableName:       AlertsTableName,
		alertHistoryTableName: AlertHistoryTableName,
	}
}

// NewClientWithEncryption creates a new alerts client with KMS encryption
func NewClientWithEncryption(db *dynamodb.Client, kmsClient *kms.Client, kmsKeyID string) *Client {
	return &Client{
		db:                    db,
		kms:                   kmsClient,
		kmsKeyID:              kmsKeyID,
		encryptionEnabled:     true,
		alertsTableName:       AlertsTableName,
		alertHistoryTableName: AlertHistoryTableName,
	}
}

// NewClientWithTableNames creates a new alerts client with custom table names
func NewClientWithTableNames(db *dynamodb.Client, alertsTable, historyTable string) *Client {
	return &Client{
		db:                    db,
		encryptionEnabled:     false,
		alertsTableName:       alertsTable,
		alertHistoryTableName: historyTable,
	}
}

// CreateAlert creates a new alert configuration
func (c *Client) CreateAlert(ctx context.Context, config *AlertConfig) error {
	// Generate alert ID if not provided
	if config.AlertID == "" {
		config.AlertID = uuid.New().String()
	}

	// Set created time
	if config.CreatedAt.IsZero() {
		config.CreatedAt = time.Now()
	}

	// Set TTL (90 days from now)
	config.TTL = time.Now().Add(AlertTTLDays * 24 * time.Hour).Unix()

	// Validate
	if err := config.Validate(); err != nil {
		return fmt.Errorf("validate alert config: %w", err)
	}

	// Encrypt webhook URLs if encryption is enabled
	if c.encryptionEnabled && c.kms != nil {
		for i, dest := range config.Destinations {
			if dest.Type == DestinationSlack || dest.Type == DestinationWebhook {
				// Only encrypt if not already encrypted
				if !security.IsEncrypted(dest.Target) {
					encrypted, err := security.EncryptSecret(ctx, c.kms, c.kmsKeyID, dest.Target)
					if err != nil {
						return fmt.Errorf("encrypt webhook URL: %w", err)
					}
					config.Destinations[i].Target = encrypted
				}
			}
		}
	}

	// Marshal to DynamoDB item
	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("marshal alert config: %w", err)
	}

	// Put item
	_, err = c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.alertsTableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put alert: %w", err)
	}

	return nil
}

// GetAlert retrieves an alert configuration by ID
func (c *Client) GetAlert(ctx context.Context, alertID string) (*AlertConfig, error) {
	result, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.alertsTableName),
		Key: map[string]types.AttributeValue{
			"alert_id": &types.AttributeValueMemberS{Value: alertID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get alert: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("alert not found: %s", alertID)
	}

	var config AlertConfig
	if err := attributevalue.UnmarshalMap(result.Item, &config); err != nil {
		return nil, fmt.Errorf("unmarshal alert: %w", err)
	}

	// Decrypt webhook URLs if encryption is enabled
	if err := c.decryptDestinations(ctx, &config); err != nil {
		return nil, fmt.Errorf("decrypt destinations: %w", err)
	}

	return &config, nil
}

// decryptDestinations decrypts webhook URLs in alert destinations
func (c *Client) decryptDestinations(ctx context.Context, config *AlertConfig) error {
	if !c.encryptionEnabled || c.kms == nil {
		return nil
	}

	for i, dest := range config.Destinations {
		if dest.Type == DestinationSlack || dest.Type == DestinationWebhook {
			// Only decrypt if encrypted
			if security.IsEncrypted(dest.Target) {
				decrypted, err := security.DecryptSecret(ctx, c.kms, dest.Target)
				if err != nil {
					return fmt.Errorf("decrypt webhook URL: %w", err)
				}
				config.Destinations[i].Target = decrypted
			}
		}
	}

	return nil
}

// ListAlerts lists all alerts for a user, optionally filtered by sweep ID
func (c *Client) ListAlerts(ctx context.Context, userID string, sweepID string) ([]*AlertConfig, error) {
	var input *dynamodb.QueryInput

	if sweepID != "" {
		// Query by sweep_id
		input = &dynamodb.QueryInput{
			TableName:              aws.String(c.alertsTableName),
			IndexName:              aws.String("sweep_id-index"),
			KeyConditionExpression: aws.String("sweep_id = :sweep_id"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":sweep_id": &types.AttributeValueMemberS{Value: sweepID},
				":user_id":  &types.AttributeValueMemberS{Value: userID},
			},
			FilterExpression: aws.String("user_id = :user_id"),
		}
	} else {
		// Query by user_id
		input = &dynamodb.QueryInput{
			TableName:              aws.String(c.alertsTableName),
			IndexName:              aws.String("user_id-index"),
			KeyConditionExpression: aws.String("user_id = :user_id"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":user_id": &types.AttributeValueMemberS{Value: userID},
			},
		}
	}

	result, err := c.db.Query(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}

	var alerts []*AlertConfig
	for _, item := range result.Items {
		var config AlertConfig
		if err := attributevalue.UnmarshalMap(item, &config); err != nil {
			return nil, fmt.Errorf("unmarshal alert: %w", err)
		}

		// Decrypt webhook URLs if encryption is enabled
		if err := c.decryptDestinations(ctx, &config); err != nil {
			return nil, fmt.Errorf("decrypt destinations: %w", err)
		}

		alerts = append(alerts, &config)
	}

	return alerts, nil
}

// DeleteAlert deletes an alert configuration
func (c *Client) DeleteAlert(ctx context.Context, alertID string) error {
	_, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.alertsTableName),
		Key: map[string]types.AttributeValue{
			"alert_id": &types.AttributeValueMemberS{Value: alertID},
		},
	})
	if err != nil {
		return fmt.Errorf("delete alert: %w", err)
	}

	return nil
}

// RecordAlertHistory records that an alert was sent
func (c *Client) RecordAlertHistory(ctx context.Context, history *AlertHistory) error {
	if history.Timestamp.IsZero() {
		history.Timestamp = time.Now()
	}

	// Set TTL (90 days from now)
	history.TTL = time.Now().Add(AlertTTLDays * 24 * time.Hour).Unix()

	item, err := attributevalue.MarshalMap(history)
	if err != nil {
		return fmt.Errorf("marshal alert history: %w", err)
	}

	_, err = c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.alertHistoryTableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put alert history: %w", err)
	}

	return nil
}

// ListAlertHistory lists alert history for an alert ID
func (c *Client) ListAlertHistory(ctx context.Context, alertID string) ([]*AlertHistory, error) {
	result, err := c.db.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.alertHistoryTableName),
		KeyConditionExpression: aws.String("alert_id = :alert_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":alert_id": &types.AttributeValueMemberS{Value: alertID},
		},
		ScanIndexForward: aws.Bool(false), // Most recent first
	})
	if err != nil {
		return nil, fmt.Errorf("query alert history: %w", err)
	}

	var history []*AlertHistory
	for _, item := range result.Items {
		var h AlertHistory
		if err := attributevalue.UnmarshalMap(item, &h); err != nil {
			return nil, fmt.Errorf("unmarshal alert history: %w", err)
		}
		history = append(history, &h)
	}

	return history, nil
}

// Validate validates an alert configuration
func (a *AlertConfig) Validate() error {
	if a.UserID == "" {
		return fmt.Errorf("user_id is required")
	}

	if a.SweepID == "" && a.ScheduleID == "" {
		return fmt.Errorf("either sweep_id or schedule_id is required")
	}

	if len(a.Triggers) == 0 {
		return fmt.Errorf("at least one trigger is required")
	}

	if len(a.Destinations) == 0 {
		return fmt.Errorf("at least one destination is required")
	}

	// Validate triggers
	for _, trigger := range a.Triggers {
		switch trigger {
		case TriggerComplete, TriggerFailure, TriggerCostThreshold, TriggerLongRunning, TriggerInstanceFailed:
			// Valid
		default:
			return fmt.Errorf("invalid trigger type: %s", trigger)
		}

		// Validate trigger-specific requirements
		if trigger == TriggerCostThreshold && a.CostThreshold <= 0 {
			return fmt.Errorf("cost_threshold must be > 0 for cost_threshold trigger")
		}

		if trigger == TriggerLongRunning && a.DurationMinutes <= 0 {
			return fmt.Errorf("duration_minutes must be > 0 for long_running trigger")
		}
	}

	// Validate destinations
	for _, dest := range a.Destinations {
		switch dest.Type {
		case DestinationEmail, DestinationSlack, DestinationSNS, DestinationWebhook:
			// Valid
		default:
			return fmt.Errorf("invalid destination type: %s", dest.Type)
		}

		if dest.Target == "" {
			return fmt.Errorf("destination target is required")
		}
	}

	return nil
}
