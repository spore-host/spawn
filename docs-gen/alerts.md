## `spawn alerts`

Create, list, and manage alert notifications for parameter sweeps and schedules.

Get notified via email, Slack, SNS, or webhooks when sweeps complete, fail,
exceed cost thresholds, or encounter issues.

Examples:
  # Create alert for sweep completion
  spawn alerts create &lt;sweep-id&gt; --on-complete --email user@example.com

  # Create alert for failures with Slack
  spawn alerts create &lt;sweep-id&gt; --on-failure --slack https://hooks.slack.com/...

  # Create cost threshold alert
  spawn alerts create &lt;sweep-id&gt; --cost-threshold 100 --email user@example.com

  # List all alerts
  spawn alerts list

  # Delete alert
  spawn alerts delete &lt;alert-id&gt;

```
spawn alerts
```

### `spawn alerts create`

Create a new alert for a parameter sweep or schedule.

At least one trigger (--on-complete, --on-failure, etc.) and one destination
(--email, --slack, --sns, --webhook) must be specified.

Examples:
  # Sweep completion via email
  spawn alerts create sweep-123 --on-complete --email user@example.com

  # Multiple triggers and destinations
  spawn alerts create sweep-123 \\
    --on-complete \\
    --on-failure \\
    --email user@example.com \\
    --slack https://hooks.slack.com/services/...

  # Cost threshold alert
  spawn alerts create sweep-123 \\
    --cost-threshold 100 \\
    --email finance@example.com

  # Long-running sweep alert (trigger after 2 hours)
  spawn alerts create sweep-123 \\
    --long-running 120 \\
    --email user@example.com

  # Schedule execution failure alert
  spawn alerts create --schedule-id sched-123 \\
    --on-failure \\
    --slack https://hooks.slack.com/...

```
spawn alerts create <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--cost-threshold` |  | float64 |  | Alert when cost exceeds threshold (dollars) |
| `--email` |  | string |  | Email address for notifications |
| `--instance-failed` |  | bool |  | Alert when any instance fails |
| `--long-running` |  | int |  | Alert when sweep runs longer than N minutes |
| `--on-complete` |  | bool |  | Alert when sweep/schedule completes |
| `--on-failure` |  | bool |  | Alert when sweep/schedule fails |
| `--schedule-id` |  | string |  | Schedule ID (alternative to sweep-id) |
| `--slack` |  | string |  | Slack webhook URL for notifications |
| `--sns` |  | string |  | SNS topic ARN for notifications |
| `--webhook` |  | string |  | Webhook URL for notifications |

### `spawn alerts delete`

Delete an alert configuration.

Example:
  spawn alerts delete alert-abc123

```
spawn alerts delete <alert-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn alerts history`

Show the history of notifications sent for an alert.

Example:
  spawn alerts history alert-abc123

```
spawn alerts history <alert-id>
```

### `spawn alerts list`

List all alert configurations for the current user.

Optionally filter by sweep ID.

Examples:
  # List all alerts
  spawn alerts list

  # List alerts for specific sweep
  spawn alerts list --sweep-id sweep-123

```
spawn alerts list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--sweep-id` |  | string |  | Filter by sweep ID |

