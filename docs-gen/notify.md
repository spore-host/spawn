## `spawn notify`

*Aliases: bot*

Register and manage Slack/Teams/SMS notifications for instances.

Lets authorized users receive lifecycle events (launch, idle stop, TTL warn,
termination) and control instances via chat slash commands without CLI access.

Examples:
  spawn notify register --platform slack --user professor@example.com \
    --instance i-0abc123 --nickname rstudio --allow start,stop,status
  spawn notify deregister --platform slack --user professor@example.com --nickname rstudio
  spawn notify list --platform slack --workspace T03NE3GTY

```
spawn notify
```

### `spawn notify deregister`

Remove a chat bot registration

```
spawn notify deregister [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--nickname` |  | string |  | Nickname to deregister |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB registry table name |
| `--user-id` |  | string |  | Platform user ID |
| `--workspace-id` |  | string |  | Platform workspace ID |

### `spawn notify disable`

Suspend bot access without removing the registration. Use during
sensitive computation runs or maintenance. Re-enable with 'spawn notify enable'.

```
spawn notify disable [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--nickname` |  | string |  | Nickname of the registration to enable/disable |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB registry table name |
| `--user-id` |  | string |  | Platform user ID |
| `--workspace-id` |  | string |  | Platform workspace ID |

### `spawn notify enable`

Grant bot access to a registered instance. Registrations are created
disabled by default — this command must be run before a chat user can
control the instance via slash commands.

```
spawn notify enable [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--nickname` |  | string |  | Nickname of the registration to enable/disable |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB registry table name |
| `--user-id` |  | string |  | Platform user ID |
| `--workspace-id` |  | string |  | Platform workspace ID |

### `spawn notify list`

List chat bot registrations for a workspace

```
spawn notify list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB registry table name |
| `--workspace-id` |  | string |  | Platform workspace ID |

### `spawn notify register`

Register an EC2 instance so a chat user can control it via slash commands.

Supports specifying the user by email (--user) which resolves to a platform
user ID, or directly by platform ID (--user-id + --workspace-id).

The --nickname is the friendly name used in slash commands, e.g.:
  /prism stop rstudio
  /prism status jupyter

Both the instance ID and instance name (DNS name or spawn:name tag) are
accepted as the target in slash commands once registered.

```
spawn notify register [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--allow` |  | stringSlice |  | Allowed actions (default: start,stop,status,hibernate,url) |
| `--connect-code` |  | string |  | One-time code from /spore connect (alternative to --user-id) |
| `--instance` |  | string |  | Instance ID (i-...) or name |
| `--nickname` |  | string |  | Friendly name for slash commands (default: 'default') |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--role-arn` |  | string |  | Cross-account IAM role ARN for this instance's account (created automatically if omitted) |
| `--table` |  | string |  | Override DynamoDB registry table name |
| `--tag-prefix` |  | string |  | Tag prefix: spawn or prism (default: auto-detected) |
| `--user-id` |  | string |  | Platform-native user ID (e.g. Slack U04KZABCD) |
| `--user` |  | string |  | User email address (resolved to platform user ID) |
| `--workspace-id` |  | string |  | Platform workspace ID (e.g. Slack T03NE3GTY) |

### `spawn notify workspace`

Manage chat-platform workspace registrations

```
spawn notify workspace
```

#### `spawn notify workspace add`

Store the Slack bot token and signing secret for a workspace so the
spore-bot Lambda can verify incoming slash command requests.

Run this once after installing the Slack app in a workspace:

  spawn notify workspace-add \
    --platform slack \
    --workspace-id T03NE3GTY \
    --workspace-name "My Workspace" \
    --bot-token xoxb-... \
    --signing-secret abc123...

```
spawn notify workspace add [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--allowed-channels` |  | stringSlice |  | Restrict commands to specific channel IDs (e.g. C12345,C67890). Empty = all channels. |
| `--bot-token` |  | string |  | Bot token (Slack xoxb-..., or Discord bot token) |
| `--connect-ttl` |  | int |  | Max /spore connect code lifetime in hours (0 = use platform default, typically 24h). Can only lower the platform default. |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--public-key` |  | string |  | Discord application public key (Ed25519, hex; required for discord) |
| `--signing-secret` |  | string |  | Slack/Teams signing secret (required for slack/teams) |
| `--table` |  | string |  | Override DynamoDB workspaces table name |
| `--webhook-url` |  | string |  | Channel webhook URL for notifications (Discord channel webhook, or manual Slack incoming webhook) |
| `--workspace-id` |  | string |  | Platform workspace ID |
| `--workspace-name` |  | string |  | Human-friendly workspace name |

#### `spawn notify workspace destroy`

Permanently delete all instance registrations across all users in a workspace,
and remove the workspace's bot token and signing secret.

Preview with --dry-run; otherwise it prompts for confirmation (skip with --yes)
and then executes the full teardown.

Note: The SpawnBotCrossAccount IAM role in customer accounts is not
deleted automatically. Remove it separately with:
  aws cloudformation delete-stack --stack-name spawn-bot-cross-account

```
spawn notify workspace destroy [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--dry-run` |  | bool |  | Preview what would be removed without deleting anything |
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--registry-table` |  | string |  | Override DynamoDB registry table name |
| `--workspace-id` |  | string |  | Platform workspace ID (required) |
| `--workspaces-table` |  | string |  | Override DynamoDB workspaces table name |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

#### `spawn notify workspace list`

List registered workspaces

```
spawn notify workspace list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB workspaces table name |

#### `spawn notify workspace remove`

Remove a workspace registration

```
spawn notify workspace remove [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--platform` |  | string |  | Chat platform: slack, teams, or discord |
| `--table` |  | string |  | Override DynamoDB workspaces table name |
| `--workspace-id` |  | string |  | Platform workspace ID |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

