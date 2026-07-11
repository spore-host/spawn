package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

const (
	// botCrossAccountRoleName is created automatically in the caller's AWS account
	// when registering an instance without an explicit --role-arn.
	botCrossAccountRoleName = "SpawnBotCrossAccount"
)

var (
	botPlatform           string
	botUser               string
	botUserID             string
	botWorkspaceID        string
	botInstance           string
	botNickname           string
	botAllow              []string
	botTagPrefix          string
	botTable              string
	botJSONOutput         bool // deprecated: use --output json
	botRoleARN            string
	botConnectCode        string   // for --connect-code self-registration
	botAllowedChannels    []string // for --allowed-channels channel restriction
	botConnectTTLHours    int      // for --connect-ttl workspace max connect code lifetime
	botWorkspaceRemoveYes bool     // --yes: skip the workspace-remove confirmation prompt
)

var botCmd = &cobra.Command{
	Use:     "notify",
	Short:   "Manage chat and SMS notification registrations for instances",
	Aliases: []string{"bot"},
	Long: `Register and manage Slack/Teams/SMS notifications for instances.

Lets authorized users receive lifecycle events (launch, idle stop, TTL warn,
termination) and control instances via chat slash commands without CLI access.

Examples:
  spawn notify register --platform slack --user professor@example.com \
    --instance i-0abc123 --nickname rstudio --allow start,stop,status
  spawn notify deregister --platform slack --user professor@example.com --nickname rstudio
  spawn notify list --platform slack --workspace T03NE3GTY`,
}

// ── register ─────────────────────────────────────────────────────────────────

var botRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register an instance for chat bot control",
	Long: `Register an EC2 instance so a chat user can control it via slash commands.

Supports specifying the user by email (--user) which resolves to a platform
user ID, or directly by platform ID (--user-id + --workspace-id).

The --nickname is the friendly name used in slash commands, e.g.:
  /prism stop rstudio
  /prism status jupyter

Both the instance ID and instance name (DNS name or spawn:name tag) are
accepted as the target in slash commands once registered.`,
	RunE: runBotRegister,
}

func runBotRegister(cmd *cobra.Command, args []string) error {
	if botPlatform == "" {
		return fmt.Errorf("--platform is required (slack, teams, or discord)")
	}
	if botInstance == "" {
		return fmt.Errorf("--instance is required")
	}
	if botNickname == "" {
		botNickname = "default"
	}
	if len(botAllow) == 0 {
		botAllow = []string{"start", "stop", "status", "hibernate", "url"}
	}

	// Resolve tag prefix: flag > env > "spawn"
	tagpfx := botTagPrefix
	if tagpfx == "" {
		tagprefix.Init()
		tagpfx = tagprefix.Prefix()
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Resolve user ID from connect code, email, or direct user-id
	userID := botUserID
	workspaceID := botWorkspaceID

	if botConnectCode != "" {
		// Self-registration: redeem connect code to get user's Slack ID and workspace
		resolved, err := redeemConnectCode(ctx, cfg, botConnectCode, botTable)
		if err != nil {
			return fmt.Errorf("redeem connect code: %w", err)
		}
		if resolved == nil {
			return fmt.Errorf("connect code %q not found or expired (codes are valid for 15 minutes)", botConnectCode)
		}
		userID = resolved.UserID
		workspaceID = resolved.WorkspaceID
		if botPlatform == "" {
			botPlatform = resolved.Platform
		}
		fmt.Printf("Resolved connect code to user %s in workspace %s\n", userID, workspaceID)
	} else if userID == "" {
		if botUser == "" {
			return fmt.Errorf("one of --user (email), --user-id, or --connect-code is required")
		}
		if workspaceID == "" {
			return fmt.Errorf("--workspace-id is required when using --user (email)")
		}
		// Email → Slack user ID via Slack API using the workspace bot token
		resolved, err := lookupSlackUserByEmail(ctx, cfg, botPlatform, workspaceID, botUser, botTable)
		if err != nil {
			return fmt.Errorf("look up Slack user by email: %w", err)
		}
		userID = resolved
		fmt.Printf("Resolved %s → Slack user ID %s\n", botUser, userID)
	}

	// Get caller identity for registered_by
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, nil)
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	registeredBy := *identity.Arn

	// Auto-create cross-account role if --role-arn not provided
	roleARN := botRoleARN
	if roleARN == "" {
		fmt.Printf("No --role-arn provided — ensuring %s role exists in account %s...\n",
			botCrossAccountRoleName, *identity.Account)
		roleARN, err = ensureCrossAccountRole(ctx, cfg)
		if err != nil {
			return fmt.Errorf("create cross-account role: %w", err)
		}
		fmt.Printf("  ✓ Role: %s\n", roleARN)
	}

	// Build registry key
	userKey := strings.Join([]string{botPlatform, workspaceID, userID}, "#")

	reg := botRegistration{
		UserKey:        userKey,
		Nickname:       botNickname,
		InstanceID:     botInstance,
		AWSAccountID:   *identity.Account,
		RoleARN:        roleARN,
		TagPrefix:      tagpfx,
		AllowedActions: botAllow,
		RegisteredBy:   registeredBy,
		Platform:       botPlatform,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	tableName := botTable
	if tableName == "" {
		tableName = spawnconfig.GetBotRegistryTable()
	}

	client := dynamodb.NewFromConfig(cfg)
	// Use UpdateItem so re-registering an already-enabled instance doesn't reset
	// the enabled flag back to false. All other fields are overwritten.
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"user_key": &dynamodbtypes.AttributeValueMemberS{Value: reg.UserKey},
			"nickname": &dynamodbtypes.AttributeValueMemberS{Value: reg.Nickname},
		},
		UpdateExpression: aws.String(
			"SET instance_id = :iid, aws_account_id = :acct, role_arn = :role, " +
				"tag_prefix = :pfx, allowed_actions = :acts, registered_by = :by, " +
				"platform = :plat, created_at = if_not_exists(created_at, :cat)"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":iid":  &dynamodbtypes.AttributeValueMemberS{Value: reg.InstanceID},
			":acct": &dynamodbtypes.AttributeValueMemberS{Value: reg.AWSAccountID},
			":role": &dynamodbtypes.AttributeValueMemberS{Value: reg.RoleARN},
			":pfx":  &dynamodbtypes.AttributeValueMemberS{Value: reg.TagPrefix},
			":acts": &dynamodbtypes.AttributeValueMemberL{Value: func() []dynamodbtypes.AttributeValue {
				vals := make([]dynamodbtypes.AttributeValue, len(reg.AllowedActions))
				for i, a := range reg.AllowedActions {
					vals[i] = &dynamodbtypes.AttributeValueMemberS{Value: a}
				}
				return vals
			}()},
			":by":   &dynamodbtypes.AttributeValueMemberS{Value: reg.RegisteredBy},
			":plat": &dynamodbtypes.AttributeValueMemberS{Value: reg.Platform},
			":cat":  &dynamodbtypes.AttributeValueMemberS{Value: reg.CreatedAt},
		},
	})
	if err != nil {
		return fmt.Errorf("write registration: %w", err)
	}

	if botJSONOutput || getOutputFormat() == "json" {
		return json.NewEncoder(os.Stdout).Encode(reg)
	}
	fmt.Printf("Registered: %s → %s for %s/%s in %s/%s\n",
		reg.Nickname, reg.InstanceID, botPlatform, userID, botPlatform, workspaceID)
	fmt.Printf("  Allowed actions: %s\n", strings.Join(reg.AllowedActions, ", "))
	fmt.Printf("  Tag prefix: %s\n", reg.TagPrefix)
	return nil
}

// ensureCrossAccountRole creates the SpawnBotCrossAccount IAM role in the caller's
// AWS account if it doesn't already exist, and returns its ARN.
// The role trusts the spore-bot Lambda execution role to assume it, allowing the
// bot to call ec2:Describe/Start/Stop on instances in this account.
func ensureCrossAccountRole(ctx context.Context, cfg aws.Config) (string, error) {
	client := iam.NewFromConfig(cfg)

	// Check if role already exists
	existing, err := client.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(botCrossAccountRoleName),
	})
	if err == nil {
		return *existing.Role.Arn, nil
	}

	var notFound *iamtypes.NoSuchEntityException
	if !errors.As(err, &notFound) {
		return "", fmt.Errorf("get role: %w", err)
	}

	// Create the role
	trustPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": %q},
			"Action": "sts:AssumeRole",
			"Condition": {"StringEquals": {"sts:ExternalId": "spawn-bot"}}
		}]
	}`, spawnconfig.GetBotLambdaRoleARN())

	created, err := client.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(botCrossAccountRoleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
		Description:              aws.String("Allows spore-bot Lambda to control EC2 instances in this account via Slack/Teams commands"),
		// Tag so `spawn cleanup`/`resources` can find and attribute it (#258).
		Tags: []iamtypes.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:created-by"), Value: aws.String("spawn")},
			{Key: aws.String("spawn:created-at"), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
			{Key: aws.String("spawn:purpose"), Value: aws.String("bot-cross-account")},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create role: %w", err)
	}

	// Attach inline permission policy
	permPolicy := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"ec2:DescribeInstances",
				"ec2:DescribeTags",
				"ec2:StartInstances",
				"ec2:StopInstances",
				"ec2:CreateTags",
				"ec2:DeleteTags"
			],
			"Resource": "*"
		}]
	}`

	_, err = client.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(botCrossAccountRoleName),
		PolicyName:     aws.String("SpawnBotEC2Control"),
		PolicyDocument: aws.String(permPolicy),
	})
	if err != nil {
		return "", fmt.Errorf("attach role policy: %w", err)
	}

	return *created.Role.Arn, nil
}

// ── deregister ────────────────────────────────────────────────────────────────

var botDeregisterCmd = &cobra.Command{
	Use:   "deregister",
	Short: "Remove a chat bot registration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botUserID == "" || botWorkspaceID == "" || botNickname == "" {
			return fmt.Errorf("--platform, --user-id, --workspace-id, and --nickname are all required")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		tableName := botTable
		if tableName == "" {
			tableName = spawnconfig.GetBotRegistryTable()
		}
		userKey := strings.Join([]string{botPlatform, botWorkspaceID, botUserID}, "#")
		client := dynamodb.NewFromConfig(cfg)
		_, err = client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(tableName),
			Key: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{Value: userKey},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: botNickname},
			},
		})
		if err != nil {
			return fmt.Errorf("delete registration: %w", err)
		}
		fmt.Printf("Deregistered: %s/%s/%s (%s)\n", botPlatform, botWorkspaceID, botUserID, botNickname)
		return nil
	},
}

// ── enable / disable ─────────────────────────────────────────────────────────

// botEnableDisable handles both enable and disable with a single implementation.
func botEnableDisable(enabled bool) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botUserID == "" || botWorkspaceID == "" || botNickname == "" {
			return fmt.Errorf("--platform, --user-id, --workspace-id, and --nickname are all required")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		tableName := botTable
		if tableName == "" {
			tableName = spawnconfig.GetBotRegistryTable()
		}
		userKey := strings.Join([]string{botPlatform, botWorkspaceID, botUserID}, "#")
		client := dynamodb.NewFromConfig(cfg)
		_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(tableName),
			Key: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{Value: userKey},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: botNickname},
			},
			UpdateExpression: aws.String("SET enabled = :v"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":v": &dynamodbtypes.AttributeValueMemberBOOL{Value: enabled},
			},
			ConditionExpression: aws.String("attribute_exists(user_key)"),
		})
		if err != nil {
			return fmt.Errorf("update enabled: %w", err)
		}
		action := "Enabled"
		if !enabled {
			action = "Disabled"
		}
		fmt.Printf("%s bot access for %s/%s (%s)\n", action, botPlatform, botNickname, botUserID)
		return nil
	}
}

var botEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable bot access for a registered instance",
	Long: `Grant bot access to a registered instance. Registrations are created
disabled by default — this command must be run before a chat user can
control the instance via slash commands.`,
	RunE: botEnableDisable(true),
}

var botDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Temporarily disable bot access for a registered instance",
	Long: `Suspend bot access without removing the registration. Use during
sensitive computation runs or maintenance. Re-enable with 'spawn notify enable'.`,
	RunE: botEnableDisable(false),
}

// ── list ──────────────────────────────────────────────────────────────────────

var botListCmd = &cobra.Command{
	Use:   "list",
	Short: "List chat bot registrations for a workspace",
	RunE: func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botWorkspaceID == "" {
			return fmt.Errorf("--platform and --workspace-id are required")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		tableName := botTable
		if tableName == "" {
			tableName = spawnconfig.GetBotRegistryTable()
		}
		// Scan with filter on platform+workspace prefix
		client := dynamodb.NewFromConfig(cfg)
		prefix := botPlatform + "#" + botWorkspaceID + "#"
		result, err := client.Scan(ctx, &dynamodb.ScanInput{
			TableName:        aws.String(tableName),
			FilterExpression: aws.String("begins_with(user_key, :prefix)"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":prefix": &dynamodbtypes.AttributeValueMemberS{Value: prefix},
			},
		})
		if err != nil {
			return fmt.Errorf("scan registrations: %w", err)
		}
		if botJSONOutput || getOutputFormat() == "json" {
			var regs []botRegistration
			for _, item := range result.Items {
				var r botRegistration
				if err := attributevalue.UnmarshalMap(item, &r); err == nil {
					regs = append(regs, r)
				}
			}
			return json.NewEncoder(os.Stdout).Encode(regs)
		}
		if len(result.Items) == 0 {
			fmt.Println("No registrations found.")
			return nil
		}
		w := newTableWriter(os.Stdout)
		fmt.Fprintln(w, "USER\tNICKNAME\tINSTANCE\tACTIONS\tTAG PREFIX")
		for _, item := range result.Items {
			var r botRegistration
			if err := attributevalue.UnmarshalMap(item, &r); err != nil {
				continue
			}
			parts := strings.SplitN(r.UserKey, "#", 3)
			userID := ""
			if len(parts) == 3 {
				userID = parts[2]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				userID, r.Nickname, r.InstanceID,
				strings.Join(r.AllowedActions, ","), r.TagPrefix)
		}
		return w.Flush()
	},
}

// ── workspace-add / workspace-remove / workspace-list ────────────────────────

var (
	botWorkspaceName   string
	botBotToken        string
	botSigningSecret   string
	botPublicKey       string // Discord application public key (Ed25519, hex) — #2
	botWebhookURL      string // channel webhook URL (Discord; or manual Slack) — #2
	botWorkspacesTable string
)

type botWorkspace struct {
	WorkspaceKey        string   `dynamodbav:"workspace_key" json:"workspace_key"`
	BotToken            string   `dynamodbav:"bot_token" json:"bot_token"`
	SigningSecret       string   `dynamodbav:"signing_secret" json:"signing_secret"`
	Platform            string   `dynamodbav:"platform" json:"platform"`
	WorkspaceName       string   `dynamodbav:"workspace_name,omitempty" json:"workspace_name,omitempty"`
	InstalledBy         string   `dynamodbav:"installed_by" json:"installed_by"`
	InstalledAt         string   `dynamodbav:"installed_at" json:"installed_at"`
	AllowedChannels     []string `dynamodbav:"allowed_channels,omitempty" json:"allowed_channels,omitempty"`
	ConnectCodeTTLHours int      `dynamodbav:"connect_code_ttl_hours,omitempty" json:"connect_code_ttl_hours,omitempty"`
	// PublicKey is the Discord application's Ed25519 public key (hex), used by the
	// spore-bot Lambda to verify inbound interaction requests. Discord has no
	// signing secret (#2).
	PublicKey string `dynamodbav:"public_key,omitempty" json:"public_key,omitempty"`
	// IncomingWebhookURL is a channel webhook for notifications (Discord channel
	// webhook, or a manually-supplied Slack incoming webhook).
	IncomingWebhookURL string `dynamodbav:"incoming_webhook_url,omitempty" json:"incoming_webhook_url,omitempty"`
	// Token rotation fields — managed automatically via OAuth; not set via CLI
	RefreshToken   string `dynamodbav:"refresh_token,omitempty" json:"refresh_token,omitempty"`
	TokenExpiresAt int64  `dynamodbav:"token_expires_at,omitempty" json:"token_expires_at,omitempty"`
	TokenRotation  bool   `dynamodbav:"token_rotation,omitempty" json:"token_rotation,omitempty"`
}

var botWorkspaceAddCmd = &cobra.Command{
	Use:   "workspace-add",
	Short: "Register a Slack/Teams workspace's bot token and signing secret",
	Long: `Store the Slack bot token and signing secret for a workspace so the
spore-bot Lambda can verify incoming slash command requests.

Run this once after installing the Slack app in a workspace:

  spawn notify workspace-add \
    --platform slack \
    --workspace-id T03NE3GTY \
    --workspace-name "My Workspace" \
    --bot-token xoxb-... \
    --signing-secret abc123...`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botWorkspaceID == "" {
			return fmt.Errorf("--platform and --workspace-id are required")
		}
		// Discord verifies interactions with an Ed25519 application public key, not
		// a signing secret; Slack/Teams require the signing secret (#2).
		if botPlatform == "discord" {
			if botPublicKey == "" {
				return fmt.Errorf("--public-key is required for discord (the application's Ed25519 public key)")
			}
		} else if botSigningSecret == "" {
			return fmt.Errorf("--signing-secret is required")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		stsClient := sts.NewFromConfig(cfg)
		identity, err := stsClient.GetCallerIdentity(ctx, nil)
		if err != nil {
			return fmt.Errorf("get caller identity: %w", err)
		}
		ws := botWorkspace{
			WorkspaceKey:        botPlatform + "#" + botWorkspaceID,
			BotToken:            botBotToken,
			SigningSecret:       botSigningSecret,
			PublicKey:           botPublicKey,
			IncomingWebhookURL:  botWebhookURL,
			Platform:            botPlatform,
			WorkspaceName:       botWorkspaceName,
			InstalledBy:         *identity.Arn,
			InstalledAt:         time.Now().UTC().Format(time.RFC3339),
			AllowedChannels:     botAllowedChannels,
			ConnectCodeTTLHours: botConnectTTLHours,
		}
		tableName := botWorkspacesTable
		if tableName == "" {
			tableName = spawnconfig.GetBotWorkspacesTable()
		}
		client := dynamodb.NewFromConfig(cfg)
		item, err := attributevalue.MarshalMap(ws)
		if err != nil {
			return fmt.Errorf("marshal workspace: %w", err)
		}
		_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(tableName),
			Item:      item,
		})
		if err != nil {
			return fmt.Errorf("write workspace: %w", err)
		}
		if botJSONOutput || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(ws)
		}
		fmt.Printf("Registered workspace: %s/%s", botPlatform, botWorkspaceID)
		if botWorkspaceName != "" {
			fmt.Printf(" (%s)", botWorkspaceName)
		}
		fmt.Println()
		return nil
	},
}

var botWorkspaceRemoveCmd = &cobra.Command{
	Use:   "workspace-remove",
	Short: "Remove a workspace registration",
	RunE: func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botWorkspaceID == "" {
			return fmt.Errorf("--platform and --workspace-id are required")
		}
		if !confirmYes(botWorkspaceRemoveYes, fmt.Sprintf("Remove workspace registration %s/%s?", botPlatform, botWorkspaceID)) {
			return fmt.Errorf("aborted")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		tableName := botWorkspacesTable
		if tableName == "" {
			tableName = spawnconfig.GetBotWorkspacesTable()
		}
		client := dynamodb.NewFromConfig(cfg)
		_, err = client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(tableName),
			Key: map[string]dynamodbtypes.AttributeValue{
				"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: botPlatform + "#" + botWorkspaceID},
			},
		})
		if err != nil {
			return fmt.Errorf("delete workspace: %w", err)
		}
		fmt.Printf("Removed workspace: %s/%s\n", botPlatform, botWorkspaceID)
		return nil
	},
}

var botWorkspaceListCmd = &cobra.Command{
	Use:   "workspace-list",
	Short: "List registered workspaces",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		tableName := botWorkspacesTable
		if tableName == "" {
			tableName = spawnconfig.GetBotWorkspacesTable()
		}
		client := dynamodb.NewFromConfig(cfg)
		input := &dynamodb.ScanInput{TableName: aws.String(tableName)}
		if botPlatform != "" {
			input.FilterExpression = aws.String("platform = :p")
			input.ExpressionAttributeValues = map[string]dynamodbtypes.AttributeValue{
				":p": &dynamodbtypes.AttributeValueMemberS{Value: botPlatform},
			}
		}
		result, err := client.Scan(ctx, input)
		if err != nil {
			return fmt.Errorf("scan workspaces: %w", err)
		}
		if botJSONOutput || getOutputFormat() == "json" {
			var wss []botWorkspace
			for _, item := range result.Items {
				var ws botWorkspace
				if err := attributevalue.UnmarshalMap(item, &ws); err == nil {
					ws.BotToken = "(redacted)"
					ws.SigningSecret = "(redacted)"
					wss = append(wss, ws)
				}
			}
			return json.NewEncoder(os.Stdout).Encode(wss)
		}
		if len(result.Items) == 0 {
			fmt.Println("No workspaces registered.")
			return nil
		}
		w := newTableWriter(os.Stdout)
		fmt.Fprintln(w, "PLATFORM\tWORKSPACE ID\tNAME\tINSTALLED BY\tINSTALLED AT")
		for _, item := range result.Items {
			var ws botWorkspace
			if err := attributevalue.UnmarshalMap(item, &ws); err != nil {
				continue
			}
			parts := strings.SplitN(ws.WorkspaceKey, "#", 2)
			wsID := ""
			if len(parts) == 2 {
				wsID = parts[1]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				ws.Platform, wsID, ws.WorkspaceName, ws.InstalledBy, ws.InstalledAt)
		}
		return w.Flush()
	},
}

// ── workspace-destroy ─────────────────────────────────────────────────────────

var botDestroyConfirm bool

var botWorkspaceDestroyCmd = &cobra.Command{
	Use:   "workspace-destroy",
	Short: "Completely remove a workspace: all registrations and credentials",
	Long: `Permanently delete all instance registrations across all users in a workspace,
and remove the workspace's bot token and signing secret.

Without --confirm, performs a dry-run showing what would be removed.
With --confirm, executes the full teardown.

Note: The SpawnBotCrossAccount IAM role in customer accounts is not
deleted automatically. Remove it separately with:
  aws cloudformation delete-stack --stack-name spawn-bot-cross-account`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if botPlatform == "" || botWorkspaceID == "" {
			return fmt.Errorf("--platform and --workspace-id are required")
		}
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}

		registryTable := botTable
		if registryTable == "" {
			registryTable = spawnconfig.GetBotRegistryTable()
		}
		workspacesTable := botWorkspacesTable
		if workspacesTable == "" {
			workspacesTable = spawnconfig.GetBotWorkspacesTable()
		}

		client := dynamodb.NewFromConfig(cfg)

		// Scan all registrations for this workspace
		prefix := botPlatform + "#" + botWorkspaceID + "#"
		scanResult, err := client.Scan(ctx, &dynamodb.ScanInput{
			TableName:        aws.String(registryTable),
			FilterExpression: aws.String("begins_with(user_key, :prefix)"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":prefix": &dynamodbtypes.AttributeValueMemberS{Value: prefix},
			},
		})
		if err != nil {
			return fmt.Errorf("scan registrations: %w", err)
		}

		// Look up workspace record
		wsResult, _ := client.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(workspacesTable),
			Key: map[string]dynamodbtypes.AttributeValue{
				"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: botPlatform + "#" + botWorkspaceID},
			},
		})
		wsFound := wsResult != nil && wsResult.Item != nil

		// Dry-run: show what would be removed
		if !botDestroyConfirm {
			fmt.Println("Would remove:")
			if len(scanResult.Items) == 0 {
				fmt.Println("  registrations: (none)")
			} else {
				fmt.Printf("  registrations: %d\n", len(scanResult.Items))
				for _, item := range scanResult.Items {
					var r botRegistration
					if err := attributevalue.UnmarshalMap(item, &r); err == nil {
						parts := strings.SplitN(r.UserKey, "#", 3)
						userID := ""
						if len(parts) == 3 {
							userID = parts[2]
						}
						fmt.Printf("    %s/%s\n", userID, r.Nickname)
					}
				}
			}
			if wsFound {
				fmt.Printf("  workspace: %s/%s\n", botPlatform, botWorkspaceID)
			} else {
				fmt.Printf("  workspace: %s/%s (not found)\n", botPlatform, botWorkspaceID)
			}
			fmt.Println("\nRun with --confirm to proceed.")
			return nil
		}

		// Execute: batch-delete all registrations
		deleted := 0
		items := scanResult.Items
		for i := 0; i < len(items); i += 25 {
			end := i + 25
			if end > len(items) {
				end = len(items)
			}
			requests := make([]dynamodbtypes.WriteRequest, 0, end-i)
			for _, item := range items[i:end] {
				var r botRegistration
				if err := attributevalue.UnmarshalMap(item, &r); err != nil {
					continue
				}
				requests = append(requests, dynamodbtypes.WriteRequest{
					DeleteRequest: &dynamodbtypes.DeleteRequest{
						Key: map[string]dynamodbtypes.AttributeValue{
							"user_key": &dynamodbtypes.AttributeValueMemberS{Value: r.UserKey},
							"nickname": &dynamodbtypes.AttributeValueMemberS{Value: r.Nickname},
						},
					},
				})
			}
			if len(requests) > 0 {
				if _, err := client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]dynamodbtypes.WriteRequest{
						registryTable: requests,
					},
				}); err != nil {
					return fmt.Errorf("batch delete: %w", err)
				}
				deleted += len(requests)
			}
		}

		// Delete workspace record
		if wsFound {
			if _, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(workspacesTable),
				Key: map[string]dynamodbtypes.AttributeValue{
					"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: botPlatform + "#" + botWorkspaceID},
				},
			}); err != nil {
				return fmt.Errorf("delete workspace: %w", err)
			}
		}

		fmt.Printf("Destroyed workspace %s/%s:\n", botPlatform, botWorkspaceID)
		fmt.Printf("  Removed %d instance registration(s)\n", deleted)
		if wsFound {
			fmt.Println("  Removed workspace credentials")
		}
		fmt.Println("\nNote: The SpawnBotCrossAccount IAM role in customer accounts must be")
		fmt.Println("deleted separately:")
		fmt.Println("  aws cloudformation delete-stack --stack-name spawn-bot-cross-account")
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// lookupSlackUserByEmail resolves a Slack user ID from an email address using
// the workspace's bot token stored in spore-bot-workspaces DynamoDB.
func lookupSlackUserByEmail(ctx context.Context, cfg aws.Config, platform, workspaceID, email, tableOverride string) (string, error) {
	// Fetch bot token from workspaces table
	workspacesTable := tableOverride
	if workspacesTable == "" {
		workspacesTable = spawnconfig.GetBotWorkspacesTable()
	}
	client := dynamodb.NewFromConfig(cfg)
	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(workspacesTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: platform + "#" + workspaceID},
		},
	})
	if err != nil || result.Item == nil {
		return "", fmt.Errorf("workspace %s/%s not registered (run spawn notify workspace-add first)", platform, workspaceID)
	}
	var ws botWorkspace
	if err := attributevalue.UnmarshalMap(result.Item, &ws); err != nil {
		return "", fmt.Errorf("unmarshal workspace: %w", err)
	}
	if ws.BotToken == "" {
		return "", fmt.Errorf("no bot token stored for workspace %s — re-run spawn notify workspace-add with --bot-token", workspaceID)
	}

	// Refresh token if rotation is enabled and token is expired
	botToken := ws.BotToken
	if ws.TokenRotation && ws.RefreshToken != "" && ws.TokenExpiresAt > 0 {
		if time.Now().Add(5*time.Minute).Unix() >= ws.TokenExpiresAt {
			newToken, newRefresh, expiresIn, err := exchangeRefreshTokenCLI(ctx, ws.RefreshToken)
			if err != nil {
				return "", fmt.Errorf("refresh Slack token: %w", err)
			}
			botToken = newToken
			// Update stored tokens
			newExpiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
			client.UpdateItem(ctx, &dynamodb.UpdateItemInput{ //nolint:errcheck
				TableName: aws.String(workspacesTable),
				Key: map[string]dynamodbtypes.AttributeValue{
					"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: platform + "#" + workspaceID},
				},
				UpdateExpression: aws.String("SET bot_token = :t, refresh_token = :r, token_expires_at = :e"),
				ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
					":t": &dynamodbtypes.AttributeValueMemberS{Value: newToken},
					":r": &dynamodbtypes.AttributeValueMemberS{Value: newRefresh},
					":e": &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", newExpiresAt)},
				},
			})
		}
	}

	// Call Slack users.lookupByEmail
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://slack.com/api/users.lookupByEmail?email="+email, nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Slack API request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &slackResp); err != nil {
		return "", fmt.Errorf("parse Slack response: %w", err)
	}
	if !slackResp.OK {
		if slackResp.Error == "users_not_found" {
			return "", fmt.Errorf("no Slack user found with email %q in workspace %s", email, workspaceID)
		}
		return "", fmt.Errorf("Slack API error: %s", slackResp.Error)
	}
	return slackResp.User.ID, nil
}

// exchangeRefreshTokenCLI calls Slack's oauth.v2.exchange to get new tokens.
// Used by the CLI when a workspace token has expired (token rotation enabled).
func exchangeRefreshTokenCLI(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, expiresIn int, err error) {
	clientID := os.Getenv("SLACK_CLIENT_ID")
	clientSecret := os.Getenv("SLACK_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return "", "", 0, fmt.Errorf("SLACK_CLIENT_ID and SLACK_CLIENT_SECRET must be set to refresh tokens")
	}
	vals := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	resp, err := http.PostForm("https://slack.com/api/oauth.v2.exchange", vals)
	if err != nil {
		return "", "", 0, fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error,omitempty"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", 0, fmt.Errorf("parse: %w", err)
	}
	if !result.OK {
		return "", "", 0, fmt.Errorf("Slack API: %s", result.Error)
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

// connectCodeRecord mirrors the Lambda's ConnectCode struct for DynamoDB operations.
type connectCodeRecord struct {
	CodeKey     string `dynamodbav:"workspace_key"`
	Platform    string `dynamodbav:"platform"`
	WorkspaceID string `dynamodbav:"workspace_id"`
	UserID      string `dynamodbav:"user_id"`
	TTL         int64  `dynamodbav:"ttl"`
}

// redeemConnectCode atomically deletes a connect code and returns the associated
// Slack identity. Returns nil if the code doesn't exist or has expired.
func redeemConnectCode(ctx context.Context, cfg aws.Config, code, tableOverride string) (*connectCodeRecord, error) {
	workspacesTable := tableOverride
	if workspacesTable == "" {
		workspacesTable = spawnconfig.GetBotWorkspacesTable()
	}
	// Normalize: accept with or without "SPORE-" prefix
	code = strings.TrimPrefix(strings.ToUpper(code), "SPORE-")
	key := "connect#" + code

	client := dynamodb.NewFromConfig(cfg)
	result, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(workspacesTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
		ReturnValues: dynamodbtypes.ReturnValueAllOld,
	})
	if err != nil {
		return nil, fmt.Errorf("redeem: %w", err)
	}
	if result.Attributes == nil {
		return nil, nil
	}
	var rec connectCodeRecord
	if err := attributevalue.UnmarshalMap(result.Attributes, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if time.Now().Unix() > rec.TTL {
		return nil, nil // expired
	}
	return &rec, nil
}

// ── types ────────────────────────────────────────────────────────────────────

type botRegistration struct {
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
	// Enabled tracks whether the bot may execute EC2 commands for this registration.
	// Stored explicitly so re-registering an enabled instance doesn't silently disable it.
	Enabled bool `dynamodbav:"enabled" json:"enabled"`
}

// ── init ─────────────────────────────────────────────────────────────────────

func init() {
	rootCmd.AddCommand(botCmd)
	botCmd.AddCommand(botRegisterCmd, botDeregisterCmd, botListCmd,
		botEnableCmd, botDisableCmd,
		botWorkspaceAddCmd, botWorkspaceRemoveCmd, botWorkspaceListCmd,
		botWorkspaceDestroyCmd)

	// Shared flags across all subcommands
	allSubs := []*cobra.Command{
		botRegisterCmd, botDeregisterCmd, botListCmd,
		botEnableCmd, botDisableCmd,
		botWorkspaceAddCmd, botWorkspaceRemoveCmd, botWorkspaceListCmd,
		botWorkspaceDestroyCmd,
	}
	for _, sub := range allSubs {
		sub.Flags().StringVar(&botPlatform, "platform", "", "Chat platform: slack, teams, or discord")
		sub.Flags().BoolVar(&botJSONOutput, "json", false, "Output as JSON")
		_ = sub.Flags().MarkDeprecated("json", "use --output json instead")
	}

	// Registry table override (register/deregister/list)
	for _, sub := range []*cobra.Command{botRegisterCmd, botDeregisterCmd, botListCmd} {
		sub.Flags().StringVar(&botTable, "table", "", "Override DynamoDB registry table name")
	}

	// Workspaces table override + workspace-id for workspace commands
	for _, sub := range []*cobra.Command{botWorkspaceAddCmd, botWorkspaceRemoveCmd, botWorkspaceListCmd} {
		sub.Flags().StringVar(&botWorkspacesTable, "table", "", "Override DynamoDB workspaces table name")
	}
	botWorkspaceAddCmd.Flags().StringVar(&botWorkspaceName, "workspace-name", "", "Human-friendly workspace name")
	botWorkspaceAddCmd.Flags().StringVar(&botBotToken, "bot-token", "", "Bot token (Slack xoxb-..., or Discord bot token)")
	botWorkspaceAddCmd.Flags().StringVar(&botSigningSecret, "signing-secret", "", "Slack/Teams signing secret (required for slack/teams)")
	botWorkspaceAddCmd.Flags().StringVar(&botPublicKey, "public-key", "", "Discord application public key (Ed25519, hex; required for discord)")
	botWorkspaceAddCmd.Flags().StringVar(&botWebhookURL, "webhook-url", "", "Channel webhook URL for notifications (Discord channel webhook, or manual Slack incoming webhook)")
	botWorkspaceAddCmd.Flags().StringSliceVar(&botAllowedChannels, "allowed-channels", nil, "Restrict commands to specific channel IDs (e.g. C12345,C67890). Empty = all channels.")
	botWorkspaceAddCmd.Flags().IntVar(&botConnectTTLHours, "connect-ttl", 0, "Max /spore connect code lifetime in hours (0 = use platform default, typically 24h). Can only lower the platform default.")

	// Register-specific flags
	botRegisterCmd.Flags().StringVar(&botUser, "user", "", "User email address (resolved to platform user ID)")
	botRegisterCmd.Flags().StringVar(&botUserID, "user-id", "", "Platform-native user ID (e.g. Slack U04KZABCD)")
	botRegisterCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID (e.g. Slack T03NE3GTY)")
	botRegisterCmd.Flags().StringVar(&botInstance, "instance", "", "Instance ID (i-...) or name")
	botRegisterCmd.Flags().StringVar(&botNickname, "nickname", "", "Friendly name for slash commands (default: 'default')")
	botRegisterCmd.Flags().StringSliceVar(&botAllow, "allow", nil, "Allowed actions (default: start,stop,status,hibernate,url)")
	botRegisterCmd.Flags().StringVar(&botTagPrefix, "tag-prefix", "", "Tag prefix: spawn or prism (default: auto-detected)")
	botRegisterCmd.Flags().StringVar(&botRoleARN, "role-arn", "", "Cross-account IAM role ARN for this instance's account (created automatically if omitted)")
	botRegisterCmd.Flags().StringVar(&botConnectCode, "connect-code", "", "One-time code from /spore connect (alternative to --user-id)")

	// Deregister flags
	botDeregisterCmd.Flags().StringVar(&botUserID, "user-id", "", "Platform user ID")
	botDeregisterCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID")
	botDeregisterCmd.Flags().StringVar(&botNickname, "nickname", "", "Nickname to deregister")

	// List flags
	botListCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID")

	// enable/disable flags
	for _, sub := range []*cobra.Command{botEnableCmd, botDisableCmd} {
		sub.Flags().StringVar(&botUserID, "user-id", "", "Platform user ID")
		sub.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID")
		sub.Flags().StringVar(&botNickname, "nickname", "", "Nickname of the registration to enable/disable")
		sub.Flags().StringVar(&botTable, "table", "", "Override DynamoDB registry table name")
	}

	// workspace-add/remove/destroy share workspace-id
	botWorkspaceAddCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID")
	botWorkspaceRemoveCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID")
	botWorkspaceRemoveCmd.Flags().BoolVarP(&botWorkspaceRemoveYes, "yes", "y", false, "Skip the confirmation prompt")
	botWorkspaceDestroyCmd.Flags().StringVar(&botWorkspaceID, "workspace-id", "", "Platform workspace ID (required)")
	botWorkspaceDestroyCmd.Flags().StringVar(&botWorkspacesTable, "workspaces-table", "", "Override DynamoDB workspaces table name")
	botWorkspaceDestroyCmd.Flags().StringVar(&botTable, "registry-table", "", "Override DynamoDB registry table name")
	botWorkspaceDestroyCmd.Flags().BoolVar(&botDestroyConfirm, "confirm", false, "Execute destroy (default: dry-run)")
}
