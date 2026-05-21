package main

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/google/uuid"
)

// Audit result constants.
const (
	AuditResultSuccess    = "success"
	AuditResultDenied     = "denied"
	AuditResultNotEnabled = "not_enabled"
	AuditResultError      = "error"
)

// AuditEvent records a single bot action for compliance and troubleshooting.
type AuditEvent struct {
	AuditID     string `dynamodbav:"audit_id"`
	UserKey     string `dynamodbav:"user_key"` // {platform}#{workspace}#{user}
	TS          string `dynamodbav:"ts"`       // RFC3339
	Platform    string `dynamodbav:"platform"`
	WorkspaceID string `dynamodbav:"workspace_id"`
	UserID      string `dynamodbav:"user_id"`
	Command     string `dynamodbav:"command"` // status, start, stop, hibernate, url, enable, disable
	Nickname    string `dynamodbav:"nickname"`
	InstanceID  string `dynamodbav:"instance_id,omitempty"`
	AccountID   string `dynamodbav:"aws_account_id,omitempty"`
	Result      string `dynamodbav:"result"` // success, denied, not_enabled, error
	Detail      string `dynamodbav:"detail,omitempty"`
	TTL         int64  `dynamodbav:"ttl"` // Unix epoch; 90-day retention
}

// Auditor writes audit events to DynamoDB.
type Auditor struct {
	client    *dynamodb.Client
	tableName string
}

// NewAuditor creates an Auditor pointing at the audit log table.
func NewAuditor(cfg aws.Config) *Auditor {
	return &Auditor{
		client:    dynamodb.NewFromConfig(cfg),
		tableName: getEnv("BOT_AUDIT_TABLE", "spore-bot-audit"),
	}
}

// Log writes an audit event asynchronously (fire-and-forget).
// It never blocks the EC2 response path.
func (a *Auditor) Log(ctx context.Context, event AuditEvent) {
	if event.AuditID == "" {
		event.AuditID = uuid.New().String()
	}
	if event.TS == "" {
		event.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if event.TTL == 0 {
		event.TTL = time.Now().Add(90 * 24 * time.Hour).Unix()
	}

	go func() {
		item, err := attributevalue.MarshalMap(event)
		if err != nil {
			logf("audit marshal error: %v", err)
			return
		}
		if _, err := a.client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(a.tableName),
			Item:      item,
		}); err != nil {
			logf("audit write error: %v", err)
		}
	}()
}

// newAuditEvent builds a partially-populated AuditEvent from a BotAction.
func newAuditEvent(action *BotAction, result, detail string) AuditEvent {
	e := AuditEvent{
		Platform:    action.Platform,
		WorkspaceID: action.WorkspaceID,
		UserID:      action.UserID,
		Command:     action.Command,
		Result:      result,
		Detail:      detail,
	}
	e.UserKey = userKey(action.Platform, action.WorkspaceID, action.UserID)
	if action.Registration != nil {
		e.Nickname = action.Registration.Nickname
		e.InstanceID = action.Registration.InstanceID
		e.AccountID = action.Registration.AWSAccountID
	}
	return e
}
