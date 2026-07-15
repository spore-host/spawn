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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/bot"
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

// botClient builds a bot store Client from the given config, honoring the
// per-command table overrides (--table/--registry-table and --workspaces-table).
// Empty overrides fall back to the env-resolved defaults inside the store.
func botClient(cfg aws.Config) *bot.Client {
	return bot.NewClientWithTableNames(dynamodb.NewFromConfig(cfg), botTable, botWorkspacesTable)
}

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
		resolved, err := redeemConnectCode(ctx, cfg, botConnectCode)
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
		resolved, err := lookupSlackUserByEmail(ctx, cfg, botPlatform, workspaceID, botUser)
		if err != nil {
			return fmt.Errorf("look up Slack user by email: %w", err)
		}
		userID = resolved
		fmt.Printf("Resolved %s → Slack user ID %s\n", botUser, userID)
	}

	// Get caller identity for registered_by
	accountID, registeredBy, err := spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}

	// Auto-create cross-account role if --role-arn not provided
	roleARN := botRoleARN
	if roleARN == "" {
		fmt.Printf("No --role-arn provided — ensuring %s role exists in account %s...\n",
			botCrossAccountRoleName, accountID)
		roleARN, err = ensureCrossAccountRole(ctx, cfg)
		if err != nil {
			return fmt.Errorf("create cross-account role: %w", err)
		}
		fmt.Printf("  ✓ Role: %s\n", roleARN)
	}

	// Build registry key
	userKey := strings.Join([]string{botPlatform, workspaceID, userID}, "#")

	reg := bot.Registration{
		UserKey:        userKey,
		Nickname:       botNickname,
		InstanceID:     botInstance,
		AWSAccountID:   accountID,
		RoleARN:        roleARN,
		TagPrefix:      tagpfx,
		AllowedActions: botAllow,
		RegisteredBy:   registeredBy,
		Platform:       botPlatform,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	// Use UpsertRegistration so re-registering an already-enabled instance
	// doesn't reset the enabled flag back to false.
	if err := botClient(cfg).UpsertRegistration(ctx, &reg); err != nil {
		return err
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
		userKey := strings.Join([]string{botPlatform, botWorkspaceID, botUserID}, "#")
		if err := botClient(cfg).DeleteRegistration(ctx, userKey, botNickname); err != nil {
			return err
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
		userKey := strings.Join([]string{botPlatform, botWorkspaceID, botUserID}, "#")
		if err := botClient(cfg).SetRegistrationEnabled(ctx, userKey, botNickname, enabled); err != nil {
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
		regs, err := botClient(cfg).ListRegistrationsByWorkspace(ctx, botPlatform, botWorkspaceID)
		if err != nil {
			return err
		}
		if botJSONOutput || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(regs)
		}
		if len(regs) == 0 {
			fmt.Println("No registrations found.")
			return nil
		}
		w := newTableWriter(os.Stdout)
		fmt.Fprintln(w, "USER\tNICKNAME\tINSTANCE\tACTIONS\tTAG PREFIX")
		for _, r := range regs {
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

// botWorkspaceCmd groups the workspace management verbs under `notify workspace`.
var botWorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage chat-platform workspace registrations",
}

var botWorkspaceAddCmd = &cobra.Command{
	Use:   "add",
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
		_, callerARN, err := spawnaws.NewClientFromConfig(cfg).GetCallerIdentityInfo(ctx)
		if err != nil {
			return fmt.Errorf("get caller identity: %w", err)
		}
		ws := bot.Workspace{
			WorkspaceKey:        botPlatform + "#" + botWorkspaceID,
			BotToken:            botBotToken,
			SigningSecret:       botSigningSecret,
			PublicKey:           botPublicKey,
			IncomingWebhookURL:  botWebhookURL,
			Platform:            botPlatform,
			WorkspaceName:       botWorkspaceName,
			InstalledBy:         callerARN,
			InstalledAt:         time.Now().UTC().Format(time.RFC3339),
			AllowedChannels:     botAllowedChannels,
			ConnectCodeTTLHours: botConnectTTLHours,
		}
		if err := botClient(cfg).PutWorkspace(ctx, &ws); err != nil {
			return err
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
	Use:   "remove",
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
		if err := botClient(cfg).DeleteWorkspace(ctx, botPlatform, botWorkspaceID); err != nil {
			return err
		}
		fmt.Printf("Removed workspace: %s/%s\n", botPlatform, botWorkspaceID)
		return nil
	},
}

var botWorkspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered workspaces",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		wss, err := botClient(cfg).ListWorkspaces(ctx, botPlatform)
		if err != nil {
			return err
		}
		if botJSONOutput || getOutputFormat() == "json" {
			for i := range wss {
				wss[i].BotToken = "(redacted)"
				wss[i].SigningSecret = "(redacted)"
			}
			return json.NewEncoder(os.Stdout).Encode(wss)
		}
		if len(wss) == 0 {
			fmt.Println("No workspaces registered.")
			return nil
		}
		w := newTableWriter(os.Stdout)
		fmt.Fprintln(w, "PLATFORM\tWORKSPACE ID\tNAME\tINSTALLED BY\tINSTALLED AT")
		for _, ws := range wss {
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
	Use:   "destroy",
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

		client := botClient(cfg)

		// Scan all registrations for this workspace
		regs, err := client.ListRegistrationsByWorkspace(ctx, botPlatform, botWorkspaceID)
		if err != nil {
			return err
		}

		// Look up workspace record
		ws, err := client.GetWorkspace(ctx, botPlatform, botWorkspaceID)
		if err != nil {
			return err
		}
		wsFound := ws != nil

		// Dry-run: show what would be removed
		if !botDestroyConfirm {
			fmt.Println("Would remove:")
			if len(regs) == 0 {
				fmt.Println("  registrations: (none)")
			} else {
				fmt.Printf("  registrations: %d\n", len(regs))
				for _, r := range regs {
					parts := strings.SplitN(r.UserKey, "#", 3)
					userID := ""
					if len(parts) == 3 {
						userID = parts[2]
					}
					fmt.Printf("    %s/%s\n", userID, r.Nickname)
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

		// Execute: batch-delete all registrations, then the workspace record.
		deleted, err := client.BatchDeleteRegistrations(ctx, regs)
		if err != nil {
			return err
		}
		if wsFound {
			if err := client.DeleteWorkspace(ctx, botPlatform, botWorkspaceID); err != nil {
				return err
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
func lookupSlackUserByEmail(ctx context.Context, cfg aws.Config, platform, workspaceID, email string) (string, error) {
	client := botClient(cfg)
	ws, err := client.GetWorkspace(ctx, platform, workspaceID)
	if err != nil || ws == nil {
		return "", fmt.Errorf("workspace %s/%s not registered (run spawn notify workspace-add first)", platform, workspaceID)
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
			_ = client.UpdateWorkspaceTokens(ctx, platform, workspaceID, newToken, newRefresh, newExpiresAt)
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

// redeemConnectCode atomically deletes a connect code and returns the associated
// Slack identity. Returns nil if the code doesn't exist or has expired.
func redeemConnectCode(ctx context.Context, cfg aws.Config, code string) (*bot.ConnectCode, error) {
	rec, err := botClient(cfg).RedeemConnectCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	if time.Now().Unix() > rec.TTL {
		return nil, nil // expired
	}
	return rec, nil
}

// ── init ─────────────────────────────────────────────────────────────────────

func init() {
	rootCmd.AddCommand(botCmd)
	botCmd.AddCommand(botRegisterCmd, botDeregisterCmd, botListCmd,
		botEnableCmd, botDisableCmd, botWorkspaceCmd)
	// Workspace verbs now live under `notify workspace <verb>` (#305).
	botWorkspaceCmd.AddCommand(botWorkspaceAddCmd, botWorkspaceRemoveCmd,
		botWorkspaceListCmd, botWorkspaceDestroyCmd)

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

	// Back-compat: the workspace verbs used to be flat-hyphenated commands
	// (`notify workspace-add`, etc.). Keep hidden, deprecated shims that share
	// the real commands' RunE and flags (all bound to the same package vars) so
	// existing scripts keep working while `notify workspace <verb>` is canonical (#305).
	registerWorkspaceAlias(botWorkspaceAddCmd, "workspace-add")
	registerWorkspaceAlias(botWorkspaceRemoveCmd, "workspace-remove")
	registerWorkspaceAlias(botWorkspaceListCmd, "workspace-list")
	registerWorkspaceAlias(botWorkspaceDestroyCmd, "workspace-destroy")
}

// registerWorkspaceAlias adds a hidden, deprecated flat-name shim under `notify`
// that delegates to the given canonical `notify workspace <verb>` command. The
// shim shares the target's flag set (same underlying package vars), so parsing
// behaves identically.
func registerWorkspaceAlias(target *cobra.Command, oldName string) {
	shim := &cobra.Command{
		Use:                oldName,
		Short:              target.Short,
		Hidden:             true,
		Deprecated:         fmt.Sprintf("use `spawn notify workspace %s` instead", target.Name()),
		Args:               target.Args,
		RunE:               target.RunE,
		DisableFlagParsing: false,
	}
	shim.Flags().AddFlagSet(target.Flags())
	botCmd.AddCommand(shim)
}
