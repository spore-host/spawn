package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// BotRegistration maps a chat user to an EC2 instance they can control.
type BotRegistration struct {
	// PK: {platform}#{workspace-id}#{user-id}
	UserKey string `dynamodbav:"user_key"`
	// SK: {nickname}
	Nickname       string   `dynamodbav:"nickname"`
	InstanceID     string   `dynamodbav:"instance_id"`
	AWSAccountID   string   `dynamodbav:"aws_account_id"`
	RoleARN        string   `dynamodbav:"role_arn"`
	DNSName        string   `dynamodbav:"dns_name,omitempty"`
	TagPrefix      string   `dynamodbav:"tag_prefix"`
	AllowedActions []string `dynamodbav:"allowed_actions"`
	RegisteredBy   string   `dynamodbav:"registered_by"`
	Platform       string   `dynamodbav:"platform"`
	CreatedAt      string   `dynamodbav:"created_at"`
	// Enabled must be explicitly set true before the bot will execute any EC2 command.
	// Registrations are created disabled (false) by default.
	Enabled bool `dynamodbav:"enabled" json:"enabled"`
	// NotifyOnly marks a self-service notification subscription created via /spore notify.
	// These entries receive DMs for lifecycle events but cannot execute EC2 operations.
	NotifyOnly bool `dynamodbav:"notify_only,omitempty" json:"notify_only,omitempty"`
	// TerminatedAt is set when the instance is detected as terminated in /spore list.
	TerminatedAt string `dynamodbav:"terminated_at,omitempty" json:"terminated_at,omitempty"`
	// TTL is the DynamoDB auto-expiry timestamp (Unix seconds). Set to 7 days after
	// TerminatedAt so the record disappears without manual cleanup.
	TTL int64 `dynamodbav:"ttl,omitempty" json:"ttl,omitempty"`
}

// WorkspaceConfig stores per-workspace OAuth tokens (bot token + signing secret).
type WorkspaceConfig struct {
	// PK: {platform}#{workspace-id}#{app-id}  (preferred)
	//  or {platform}#{workspace-id}            (legacy, single app per workspace)
	WorkspaceKey string `dynamodbav:"workspace_key"`
	// AppID is the Slack App ID (A...). Used to scope workspace keys when multiple
	// Slack apps are installed in the same workspace (e.g. spore-bot + prism-bot).
	AppID         string `dynamodbav:"app_id,omitempty"`
	BotToken      string `dynamodbav:"bot_token"`
	SigningSecret string `dynamodbav:"signing_secret"`
	Platform      string `dynamodbav:"platform"`
	WorkspaceName string `dynamodbav:"workspace_name"`
	InstalledBy   string `dynamodbav:"installed_by"`
	InstalledAt   string `dynamodbav:"installed_at"`
	// AllowedChannels restricts commands to specific channels. Empty = allow all channels.
	AllowedChannels []string `dynamodbav:"allowed_channels,omitempty"`
	// ConnectCodeTTLHours overrides the platform default TTL for /spore connect codes.
	// 0 = use platform default (BOT_CONNECT_CODE_TTL_HOURS env var, default 24h).
	// Workspace admins can only set this lower than the platform default, not higher.
	ConnectCodeTTLHours int `dynamodbav:"connect_code_ttl_hours,omitempty"`
	// Token rotation fields (populated when Slack token rotation is enabled for the app).
	RefreshToken   string `dynamodbav:"refresh_token,omitempty"`
	TokenExpiresAt int64  `dynamodbav:"token_expires_at,omitempty"`
	TokenRotation  bool   `dynamodbav:"token_rotation,omitempty"`
	// IncomingWebhookURL is a ready-to-use Slack webhook URL for channel notifications.
	// Populated automatically during OAuth when the user selects a channel.
	// Can also be set manually via spawn bot workspace-add --webhook-url.
	IncomingWebhookURL     string `dynamodbav:"incoming_webhook_url,omitempty"`
	IncomingWebhookChannel string `dynamodbav:"incoming_webhook_channel,omitempty"`
}

// ConnectCode is a short-lived token generated when a user types /spore connect.
// The user shares the code with their Instance Owner, who uses it to register them
// without needing to know their Slack user ID.
type ConnectCode struct {
	// PK: connect#{code}
	CodeKey     string `dynamodbav:"code_key"`
	Platform    string `dynamodbav:"platform"`
	WorkspaceID string `dynamodbav:"workspace_id"`
	UserID      string `dynamodbav:"user_id"`
	TTL         int64  `dynamodbav:"ttl"` // 15-minute expiry
}

// userKey builds the DynamoDB PK for a user: "{platform}#{workspace}#{user}".
func userKey(platform, workspaceID, userID string) string {
	return strings.Join([]string{platform, workspaceID, userID}, "#")
}

// workspaceKey builds the DynamoDB PK for a workspace.
// When appID is non-empty, uses "{platform}#{workspace}#{app}" to support
// multiple Slack apps installed in the same workspace.
// Falls back to "{platform}#{workspace}" for legacy records.
func workspaceKey(platform, workspaceID string, appID ...string) string {
	if len(appID) > 0 && appID[0] != "" {
		return platform + "#" + workspaceID + "#" + appID[0]
	}
	return platform + "#" + workspaceID
}

// GetWorkspaceForApp looks up a workspace by platform, workspace ID, and Slack App ID.
// Tries the app-scoped key first ({platform}#{workspace}#{app}), then falls back to
// the legacy key ({platform}#{workspace}) for backwards compatibility.
func (r *Registry) GetWorkspaceForApp(ctx context.Context, platform, workspaceID, appID string) (*WorkspaceConfig, error) {
	if appID != "" {
		ws, err := r.getWorkspaceByKey(ctx, workspaceKey(platform, workspaceID, appID))
		if err == nil {
			return ws, nil
		}
	}
	return r.getWorkspaceByKey(ctx, workspaceKey(platform, workspaceID))
}

// GetWorkspaceForSlash looks up a workspace for an incoming slash command.
// Slack does not send X-Slack-App-ID in slash command webhooks, so we use
// the slash command name (/spore, /prism) as a discriminator when multiple
// apps share a workspace. Lookup order:
//  1. {platform}#{workspace}#{command}  e.g. slack#T0AU2S6FU86#/spore
//  2. {platform}#{workspace}#{appID}    when appID is known (OAuth webhooks)
//  3. {platform}#{workspace}            legacy fallback
func (r *Registry) GetWorkspaceForSlash(ctx context.Context, platform, workspaceID, command, appID string) (*WorkspaceConfig, error) {
	if command != "" {
		ws, err := r.getWorkspaceByKey(ctx, workspaceKey(platform, workspaceID, command))
		if err == nil {
			return ws, nil
		}
	}
	return r.GetWorkspaceForApp(ctx, platform, workspaceID, appID)
}

func (r *Registry) getWorkspaceByKey(ctx context.Context, key string) (*WorkspaceConfig, error) {
	result, err := r.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(r.workspacesTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	if result.Item == nil {
		return nil, fmt.Errorf("workspace %s not registered", key)
	}
	var ws WorkspaceConfig
	if err := attributevalue.UnmarshalMap(result.Item, &ws); err != nil {
		return nil, fmt.Errorf("unmarshal workspace: %w", err)
	}
	return &ws, nil
}

// Registry handles DynamoDB operations for bot registrations and workspaces.
type Registry struct {
	client          *dynamodb.Client
	registryTable   string
	workspacesTable string
}

func newRegistry(cfg aws.Config) *Registry {
	return &Registry{
		client:          dynamodb.NewFromConfig(cfg),
		registryTable:   getEnv("BOT_REGISTRY_TABLE", "spore-bot-registry"),
		workspacesTable: getEnv("BOT_WORKSPACES_TABLE", "spore-bot-workspaces"),
	}
}

// GetWorkspace retrieves signing secret and bot token for a workspace.
// Use GetWorkspaceForApp when the Slack App ID is available.
func (r *Registry) GetWorkspace(ctx context.Context, platform, workspaceID string) (*WorkspaceConfig, error) {
	return r.getWorkspaceByKey(ctx, workspaceKey(platform, workspaceID))
}

// GetWorkspacesForPlatform returns ALL workspace configs whose key starts with
// "{platform}#{workspaceID}" — covering legacy, app-scoped, and command-scoped records.
// Used by handleNotify to fan out across multiple apps in the same workspace.
func (r *Registry) GetWorkspacesForPlatform(ctx context.Context, platform, workspaceID string) []WorkspaceConfig {
	prefix := platform + "#" + workspaceID
	result, err := r.client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(r.workspacesTable),
		FilterExpression: aws.String("begins_with(workspace_key, :pfx) AND attribute_exists(bot_token)"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":pfx": &dynamodbtypes.AttributeValueMemberS{Value: prefix},
		},
	})
	if err != nil || len(result.Items) == 0 {
		return nil
	}
	var workspaces []WorkspaceConfig
	for _, item := range result.Items {
		var ws WorkspaceConfig
		if err := attributevalue.UnmarshalMap(item, &ws); err == nil && ws.BotToken != "" {
			workspaces = append(workspaces, ws)
		}
	}
	return workspaces
}

// ListUserInstances returns all registered instances for a user.
func (r *Registry) ListUserInstances(ctx context.Context, platform, workspaceID, userID string) ([]BotRegistration, error) {
	key := userKey(platform, workspaceID, userID)
	result, err := r.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(r.registryTable),
		KeyConditionExpression: aws.String("user_key = :uk"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":uk": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	regs := make([]BotRegistration, 0, len(result.Items))
	for _, item := range result.Items {
		var reg BotRegistration
		if err := attributevalue.UnmarshalMap(item, &reg); err != nil {
			continue
		}
		regs = append(regs, reg)
	}
	return regs, nil
}

// GetInstance retrieves a specific registered instance by nickname.
func (r *Registry) GetInstance(ctx context.Context, platform, workspaceID, userID, nickname string) (*BotRegistration, error) {
	key := userKey(platform, workspaceID, userID)
	result, err := r.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(r.registryTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"user_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
			"nickname": &dynamodbtypes.AttributeValueMemberS{Value: nickname},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if result.Item == nil {
		return nil, nil
	}
	var reg BotRegistration
	if err := attributevalue.UnmarshalMap(result.Item, &reg); err != nil {
		return nil, fmt.Errorf("unmarshal registration: %w", err)
	}
	return &reg, nil
}

// PutRegistration stores a new instance registration.
func (r *Registry) PutRegistration(ctx context.Context, reg *BotRegistration) error {
	if reg.CreatedAt == "" {
		reg.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	item, err := attributevalue.MarshalMap(reg)
	if err != nil {
		return fmt.Errorf("marshal registration: %w", err)
	}
	_, err = r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(r.registryTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put registration: %w", err)
	}
	return nil
}

// DeleteRegistration removes an instance registration.
func (r *Registry) DeleteRegistration(ctx context.Context, platform, workspaceID, userID, nickname string) error {
	key := userKey(platform, workspaceID, userID)
	_, err := r.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(r.registryTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"user_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
			"nickname": &dynamodbtypes.AttributeValueMemberS{Value: nickname},
		},
	})
	if err != nil {
		return fmt.Errorf("delete registration: %w", err)
	}
	return nil
}

// SetEnabled enables or disables bot access for a specific registration.
// Disabled registrations are visible in lists but the Lambda will refuse
// to execute EC2 commands on them until re-enabled.
func (r *Registry) SetEnabled(ctx context.Context, platform, workspaceID, userID, nickname string, enabled bool) error {
	_, err := r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &r.registryTable,
		Key: map[string]dynamodbtypes.AttributeValue{
			"user_key": &dynamodbtypes.AttributeValueMemberS{Value: userKey(platform, workspaceID, userID)},
			"nickname": &dynamodbtypes.AttributeValueMemberS{Value: nickname},
		},
		UpdateExpression: aws.String("SET enabled = :v"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":v": &dynamodbtypes.AttributeValueMemberBOOL{Value: enabled},
		},
		ConditionExpression: aws.String("attribute_exists(user_key)"),
	})
	if err != nil {
		return fmt.Errorf("set enabled: %w", err)
	}
	return nil
}

// PutWorkspace stores workspace credentials (bot token + signing secret).
func (r *Registry) PutWorkspace(ctx context.Context, ws *WorkspaceConfig) error {
	if ws.InstalledAt == "" {
		ws.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	}
	item, err := attributevalue.MarshalMap(ws)
	if err != nil {
		return fmt.Errorf("marshal workspace: %w", err)
	}
	_, err = r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(r.workspacesTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put workspace: %w", err)
	}
	return nil
}

// DeleteWorkspace removes a workspace record.
func (r *Registry) DeleteWorkspace(ctx context.Context, platform, workspaceID string) error {
	_, err := r.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(r.workspacesTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: workspaceKey(platform, workspaceID)},
		},
	})
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

// ListWorkspaceRegistrations returns ALL registrations across all users in a workspace.
// Used for workspace-destroy and workspace-list --all.
func (r *Registry) ListWorkspaceRegistrations(ctx context.Context, platform, workspaceID string) ([]BotRegistration, error) {
	prefix := platform + "#" + workspaceID + "#"
	var regs []BotRegistration
	var lastKey map[string]dynamodbtypes.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:        aws.String(r.registryTable),
			FilterExpression: aws.String("begins_with(user_key, :prefix)"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":prefix": &dynamodbtypes.AttributeValueMemberS{Value: prefix},
			},
		}
		if lastKey != nil {
			input.ExclusiveStartKey = lastKey
		}
		result, err := r.client.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("scan workspace registrations: %w", err)
		}
		for _, item := range result.Items {
			var reg BotRegistration
			if err := attributevalue.UnmarshalMap(item, &reg); err != nil {
				continue
			}
			regs = append(regs, reg)
		}
		if result.LastEvaluatedKey == nil {
			break
		}
		lastKey = result.LastEvaluatedKey
	}
	return regs, nil
}

// DestroyWorkspace removes ALL instance registrations for a workspace and deletes
// the workspace credentials record. Returns the number of registrations deleted.
// This is irreversible — use for full integration teardown.
func (r *Registry) DestroyWorkspace(ctx context.Context, platform, workspaceID string) (int, error) {
	// List all registrations for this workspace
	regs, err := r.ListWorkspaceRegistrations(ctx, platform, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("list registrations: %w", err)
	}

	// Batch-delete all registrations (DynamoDB BatchWriteItem handles up to 25 per call)
	deleted := 0
	for i := 0; i < len(regs); i += 25 {
		end := i + 25
		if end > len(regs) {
			end = len(regs)
		}
		batch := regs[i:end]

		requests := make([]dynamodbtypes.WriteRequest, len(batch))
		for j, reg := range batch {
			requests[j] = dynamodbtypes.WriteRequest{
				DeleteRequest: &dynamodbtypes.DeleteRequest{
					Key: map[string]dynamodbtypes.AttributeValue{
						"user_key": &dynamodbtypes.AttributeValueMemberS{Value: reg.UserKey},
						"nickname": &dynamodbtypes.AttributeValueMemberS{Value: reg.Nickname},
					},
				},
			}
		}

		_, err := r.client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]dynamodbtypes.WriteRequest{
				r.registryTable: requests,
			},
		})
		if err != nil {
			return deleted, fmt.Errorf("batch delete registrations: %w", err)
		}
		deleted += len(batch)
	}

	// Delete the workspace record
	if err := r.DeleteWorkspace(ctx, platform, workspaceID); err != nil {
		return deleted, fmt.Errorf("delete workspace record: %w", err)
	}

	return deleted, nil
}

// IsChannelAllowed returns true if the workspace has no channel restriction,
// or if the given channel ID is in the allowed list.
func IsChannelAllowed(ws *WorkspaceConfig, channelID string) bool {
	if len(ws.AllowedChannels) == 0 {
		return true
	}
	for _, c := range ws.AllowedChannels {
		if c == channelID {
			return true
		}
	}
	return false
}

// PutConnectCode stores a short-lived connect code for self-registration.
func (r *Registry) PutConnectCode(ctx context.Context, code ConnectCode) error {
	item, err := attributevalue.MarshalMap(code)
	if err != nil {
		return fmt.Errorf("marshal connect code: %w", err)
	}
	_, err = r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &r.workspacesTable, // reuses workspaces table with "connect#" prefix key
		Item:      item,
	})
	return err
}

// RedeemConnectCode atomically deletes a connect code and returns it.
// Returns nil if the code does not exist or has expired (DynamoDB TTL is eventual
// so we also check the TTL field explicitly).
func (r *Registry) RedeemConnectCode(ctx context.Context, code string) (*ConnectCode, error) {
	result, err := r.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &r.workspacesTable,
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: "connect#" + code},
		},
		ReturnValues: dynamodbtypes.ReturnValueAllOld,
	})
	if err != nil {
		return nil, fmt.Errorf("redeem connect code: %w", err)
	}
	if result.Attributes == nil {
		return nil, nil
	}
	var cc ConnectCode
	if err := attributevalue.UnmarshalMap(result.Attributes, &cc); err != nil {
		return nil, fmt.Errorf("unmarshal connect code: %w", err)
	}
	if time.Now().Unix() > cc.TTL {
		return nil, nil // expired
	}
	return &cc, nil
}

// MarkTerminated stamps terminated_at and sets a 7-day DynamoDB TTL on a registration.
// Called by /spore list when EC2 reports the instance as terminated.
func (r *Registry) MarkTerminated(ctx context.Context, reg *BotRegistration) error {
	now := time.Now().UTC()
	expiry := now.Add(24 * time.Hour).Unix()
	_, err := r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &r.registryTable,
		Key: map[string]dynamodbtypes.AttributeValue{
			"user_key": &dynamodbtypes.AttributeValueMemberS{Value: reg.UserKey},
			"nickname": &dynamodbtypes.AttributeValueMemberS{Value: reg.Nickname},
		},
		UpdateExpression: aws.String("SET terminated_at = :t, #ttl = :e"),
		ExpressionAttributeNames: map[string]string{
			"#ttl": "ttl", // ttl is a reserved word in DynamoDB expressions
		},
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":t": &dynamodbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":e": &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", expiry)},
		},
	})
	return err
}

// isActionAllowed checks if an action is in the allowed list.
func isActionAllowed(reg *BotRegistration, action string) bool {
	for _, a := range reg.AllowedActions {
		if a == action {
			return true
		}
	}
	return false
}
