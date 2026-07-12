// Package bot provides a DynamoDB store for spawn's chat-bot registry and
// workspace records. It mirrors the pkg/alerts.Client pattern: a Client wraps a
// *dynamodb.Client (built by the caller from the correct account's config) and
// holds the registry + workspaces table names so tests can override them.
//
// The item structs (Registration, Workspace, ConnectCode) and their dynamodbav
// tags are the on-the-wire format shared with the separate spore-bot Lambda repo
// (the authoritative writer) and the dashboard-api Lambda, so the tag values and
// table names must not change.
package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// Registration is a bot registration item (registry table; key user_key+nickname).
type Registration struct {
	UserKey        string   `dynamodbav:"user_key" json:"user_key"`
	Nickname       string   `dynamodbav:"nickname" json:"nickname"`
	InstanceID     string   `dynamodbav:"instance_id" json:"instance_id"`
	AWSAccountID   string   `dynamodbav:"aws_account_id" json:"aws_account_id"`
	RoleARN        string   `dynamodbav:"role_arn,omitempty" json:"role_arn,omitempty"`
	DNSName        string   `dynamodbav:"dns_name,omitempty" json:"dns_name,omitempty"`
	TagPrefix      string   `dynamodbav:"tag_prefix" json:"tag_prefix"`
	AllowedActions []string `dynamodbav:"allowed_actions" json:"allowed_actions"`
	RegisteredBy   string   `dynamodbav:"registered_by" json:"registered_by"`
	Platform       string   `dynamodbav:"platform" json:"platform"`
	CreatedAt      string   `dynamodbav:"created_at" json:"created_at"`
	Enabled        bool     `dynamodbav:"enabled" json:"enabled"`
}

// Workspace is a chat-platform workspace registration (workspaces table; key
// workspace_key = "<platform>#<workspace-id>").
type Workspace struct {
	WorkspaceKey        string   `dynamodbav:"workspace_key" json:"workspace_key"`
	BotToken            string   `dynamodbav:"bot_token" json:"bot_token"`
	SigningSecret       string   `dynamodbav:"signing_secret" json:"signing_secret"`
	Platform            string   `dynamodbav:"platform" json:"platform"`
	WorkspaceName       string   `dynamodbav:"workspace_name,omitempty" json:"workspace_name,omitempty"`
	InstalledBy         string   `dynamodbav:"installed_by" json:"installed_by"`
	InstalledAt         string   `dynamodbav:"installed_at" json:"installed_at"`
	AllowedChannels     []string `dynamodbav:"allowed_channels,omitempty" json:"allowed_channels,omitempty"`
	ConnectCodeTTLHours int      `dynamodbav:"connect_code_ttl_hours,omitempty" json:"connect_code_ttl_hours,omitempty"`
	PublicKey           string   `dynamodbav:"public_key,omitempty" json:"public_key,omitempty"`
	IncomingWebhookURL  string   `dynamodbav:"incoming_webhook_url,omitempty" json:"incoming_webhook_url,omitempty"`
	RefreshToken        string   `dynamodbav:"refresh_token,omitempty" json:"refresh_token,omitempty"`
	TokenExpiresAt      int64    `dynamodbav:"token_expires_at,omitempty" json:"token_expires_at,omitempty"`
	TokenRotation       bool     `dynamodbav:"token_rotation,omitempty" json:"token_rotation,omitempty"`
}

// ConnectCode is a transient connect-code item stored in the workspaces table
// under workspace_key = "connect#<code>", with a DynamoDB TTL.
type ConnectCode struct {
	CodeKey     string `dynamodbav:"workspace_key"`
	Platform    string `dynamodbav:"platform"`
	WorkspaceID string `dynamodbav:"workspace_id"`
	UserID      string `dynamodbav:"user_id"`
	TTL         int64  `dynamodbav:"ttl"`
}

// Client provides bot registry + workspace operations backed by DynamoDB.
type Client struct {
	db              *dynamodb.Client
	registryTable   string
	workspacesTable string
}

// NewClient returns a Client using the env-resolved default table names
// (spawnconfig.GetBotRegistryTable / GetBotWorkspacesTable).
func NewClient(db *dynamodb.Client) *Client {
	return &Client{
		db:              db,
		registryTable:   spawnconfig.GetBotRegistryTable(),
		workspacesTable: spawnconfig.GetBotWorkspacesTable(),
	}
}

// NewClientWithTableNames returns a Client with explicit table names. An empty
// name falls back to the env-resolved default, so a CLI flag override ("" when
// unset) behaves like NewClient.
func NewClientWithTableNames(db *dynamodb.Client, registryTable, workspacesTable string) *Client {
	if registryTable == "" {
		registryTable = spawnconfig.GetBotRegistryTable()
	}
	if workspacesTable == "" {
		workspacesTable = spawnconfig.GetBotWorkspacesTable()
	}
	return &Client{db: db, registryTable: registryTable, workspacesTable: workspacesTable}
}

// ── registry ──────────────────────────────────────────────────────────────

// UpsertRegistration writes a registration without resetting its enabled flag:
// all fields are overwritten except created_at (preserved via if_not_exists) and
// enabled (untouched, so re-registering an enabled instance stays enabled).
func (c *Client) UpsertRegistration(ctx context.Context, reg *Registration) error {
	actions := make([]types.AttributeValue, len(reg.AllowedActions))
	for i, a := range reg.AllowedActions {
		actions[i] = &types.AttributeValueMemberS{Value: a}
	}
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.registryTable),
		Key: map[string]types.AttributeValue{
			"user_key": &types.AttributeValueMemberS{Value: reg.UserKey},
			"nickname": &types.AttributeValueMemberS{Value: reg.Nickname},
		},
		UpdateExpression: aws.String(
			"SET instance_id = :iid, aws_account_id = :acct, role_arn = :role, " +
				"tag_prefix = :pfx, allowed_actions = :acts, registered_by = :by, " +
				"platform = :plat, created_at = if_not_exists(created_at, :cat)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":iid":  &types.AttributeValueMemberS{Value: reg.InstanceID},
			":acct": &types.AttributeValueMemberS{Value: reg.AWSAccountID},
			":role": &types.AttributeValueMemberS{Value: reg.RoleARN},
			":pfx":  &types.AttributeValueMemberS{Value: reg.TagPrefix},
			":acts": &types.AttributeValueMemberL{Value: actions},
			":by":   &types.AttributeValueMemberS{Value: reg.RegisteredBy},
			":plat": &types.AttributeValueMemberS{Value: reg.Platform},
			":cat":  &types.AttributeValueMemberS{Value: reg.CreatedAt},
		},
	})
	if err != nil {
		return fmt.Errorf("write registration: %w", err)
	}
	return nil
}

// DeleteRegistration removes a registration by user key + nickname.
func (c *Client) DeleteRegistration(ctx context.Context, userKey, nickname string) error {
	_, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.registryTable),
		Key: map[string]types.AttributeValue{
			"user_key": &types.AttributeValueMemberS{Value: userKey},
			"nickname": &types.AttributeValueMemberS{Value: nickname},
		},
	})
	if err != nil {
		return fmt.Errorf("delete registration: %w", err)
	}
	return nil
}

// SetRegistrationEnabled flips the enabled flag; fails if the registration does
// not exist (ConditionExpression attribute_exists(user_key)).
func (c *Client) SetRegistrationEnabled(ctx context.Context, userKey, nickname string, enabled bool) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.registryTable),
		Key: map[string]types.AttributeValue{
			"user_key": &types.AttributeValueMemberS{Value: userKey},
			"nickname": &types.AttributeValueMemberS{Value: nickname},
		},
		UpdateExpression: aws.String("SET enabled = :v"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":v": &types.AttributeValueMemberBOOL{Value: enabled},
		},
		ConditionExpression: aws.String("attribute_exists(user_key)"),
	})
	return err
}

// ListRegistrationsByWorkspace returns registrations whose user_key begins with
// "<platform>#<workspaceID>#".
func (c *Client) ListRegistrationsByWorkspace(ctx context.Context, platform, workspaceID string) ([]Registration, error) {
	prefix := platform + "#" + workspaceID + "#"
	result, err := c.db.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(c.registryTable),
		FilterExpression: aws.String("begins_with(user_key, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":prefix": &types.AttributeValueMemberS{Value: prefix},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("scan registrations: %w", err)
	}
	regs := make([]Registration, 0, len(result.Items))
	for _, item := range result.Items {
		var r Registration
		if err := attributevalue.UnmarshalMap(item, &r); err != nil {
			continue
		}
		regs = append(regs, r)
	}
	return regs, nil
}

// BatchDeleteRegistrations deletes the given registrations in chunks of 25
// (DynamoDB's BatchWriteItem limit) and returns the count deleted.
func (c *Client) BatchDeleteRegistrations(ctx context.Context, regs []Registration) (int, error) {
	deleted := 0
	for i := 0; i < len(regs); i += 25 {
		end := i + 25
		if end > len(regs) {
			end = len(regs)
		}
		requests := make([]types.WriteRequest, 0, end-i)
		for _, r := range regs[i:end] {
			requests = append(requests, types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{
					Key: map[string]types.AttributeValue{
						"user_key": &types.AttributeValueMemberS{Value: r.UserKey},
						"nickname": &types.AttributeValueMemberS{Value: r.Nickname},
					},
				},
			})
		}
		if len(requests) == 0 {
			continue
		}
		if _, err := c.db.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{c.registryTable: requests},
		}); err != nil {
			return deleted, fmt.Errorf("batch delete: %w", err)
		}
		deleted += len(requests)
	}
	return deleted, nil
}

// ── workspaces ────────────────────────────────────────────────────────────

// PutWorkspace writes a workspace registration.
func (c *Client) PutWorkspace(ctx context.Context, ws *Workspace) error {
	item, err := attributevalue.MarshalMap(ws)
	if err != nil {
		return fmt.Errorf("marshal workspace: %w", err)
	}
	if _, err := c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.workspacesTable),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("write workspace: %w", err)
	}
	return nil
}

// DeleteWorkspace removes a workspace by "<platform>#<workspaceID>" key.
func (c *Client) DeleteWorkspace(ctx context.Context, platform, workspaceID string) error {
	_, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.workspacesTable),
		Key: map[string]types.AttributeValue{
			"workspace_key": &types.AttributeValueMemberS{Value: platform + "#" + workspaceID},
		},
	})
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

// GetWorkspace reads one workspace. Returns (nil, nil) when it does not exist.
func (c *Client) GetWorkspace(ctx context.Context, platform, workspaceID string) (*Workspace, error) {
	result, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.workspacesTable),
		Key: map[string]types.AttributeValue{
			"workspace_key": &types.AttributeValueMemberS{Value: platform + "#" + workspaceID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	if result.Item == nil {
		return nil, nil
	}
	var ws Workspace
	if err := attributevalue.UnmarshalMap(result.Item, &ws); err != nil {
		return nil, fmt.Errorf("unmarshal workspace: %w", err)
	}
	return &ws, nil
}

// ListWorkspaces returns all workspaces, optionally filtered to one platform
// (pass "" for all).
func (c *Client) ListWorkspaces(ctx context.Context, platform string) ([]Workspace, error) {
	input := &dynamodb.ScanInput{TableName: aws.String(c.workspacesTable)}
	if platform != "" {
		input.FilterExpression = aws.String("platform = :p")
		input.ExpressionAttributeValues = map[string]types.AttributeValue{
			":p": &types.AttributeValueMemberS{Value: platform},
		}
	}
	result, err := c.db.Scan(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("scan workspaces: %w", err)
	}
	wss := make([]Workspace, 0, len(result.Items))
	for _, item := range result.Items {
		var ws Workspace
		if err := attributevalue.UnmarshalMap(item, &ws); err != nil {
			continue
		}
		wss = append(wss, ws)
	}
	return wss, nil
}

// UpdateWorkspaceTokens persists rotated OAuth tokens for a workspace.
func (c *Client) UpdateWorkspaceTokens(ctx context.Context, platform, workspaceID, botToken, refreshToken string, expiresAt int64) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.workspacesTable),
		Key: map[string]types.AttributeValue{
			"workspace_key": &types.AttributeValueMemberS{Value: platform + "#" + workspaceID},
		},
		UpdateExpression: aws.String("SET bot_token = :t, refresh_token = :r, token_expires_at = :e"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberS{Value: botToken},
			":r": &types.AttributeValueMemberS{Value: refreshToken},
			":e": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiresAt)},
		},
	})
	return err
}

// ── connect codes ─────────────────────────────────────────────────────────

// RedeemConnectCode atomically deletes a connect code (workspace_key =
// "connect#<code>", case-insensitive, optional "SPORE-" prefix) and returns its
// stored identity. Returns (nil, nil) when the code is absent, or expired.
func (c *Client) RedeemConnectCode(ctx context.Context, code string) (*ConnectCode, error) {
	code = strings.TrimPrefix(strings.ToUpper(code), "SPORE-")
	key := "connect#" + code
	result, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.workspacesTable),
		Key: map[string]types.AttributeValue{
			"workspace_key": &types.AttributeValueMemberS{Value: key},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return nil, fmt.Errorf("redeem: %w", err)
	}
	if result.Attributes == nil {
		return nil, nil
	}
	var rec ConnectCode
	if err := attributevalue.UnmarshalMap(result.Attributes, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &rec, nil
}
