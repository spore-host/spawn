# spawn alerts

Manage alerts and notifications for sweeps, schedules, and cost thresholds.

## Synopsis

```bash
spawn alerts create <sweep-or-schedule-id> [flags]
spawn alerts list [flags]
spawn alerts delete <alert-id> [flags]
spawn alerts describe <alert-id> [flags]
```

## Description

Configure webhook-based alerts (Slack, Discord, generic webhooks) for parameter sweeps, scheduled executions, and cost tracking. Alerts can notify on sweep completion, failures, cost thresholds, and schedule triggers.

**Security:** Webhook URLs are encrypted at rest in DynamoDB using AWS KMS (alias: `spawn-webhook-encryption`).

## Subcommands

### create
Create a new alert configuration.

### list
List all alert configurations.

### delete
Delete an alert configuration.

### describe
Show details of a specific alert configuration.

## create - Create Alert

### Synopsis
```bash
spawn alerts create <sweep-or-schedule-id> [flags]
```

### Arguments

#### sweep-or-schedule-id
**Type:** String
**Required:** Yes
**Description:** Sweep ID, schedule ID, or "global" for account-wide alerts.

```bash
# Alert for specific sweep
spawn alerts create sweep-20260127-abc123 --slack https://hooks.slack.com/...

# Alert for schedule
spawn alerts create schedule-nightly --slack https://hooks.slack.com/...

# Global cost alert
spawn alerts create global --cost-threshold 100 --slack https://hooks.slack.com/...
```

### Flags

#### Notification Destinations

##### --slack
**Type:** URL
**Required:** One destination required
**Description:** Slack webhook URL.

```bash
spawn alerts create sweep-123 --slack https://hooks.slack.com/services/T.../B.../XXX
```

**Get Slack Webhook:**
1. Go to https://api.slack.com/apps
2. Create app or select existing
3. Add "Incoming Webhooks" feature
4. Create webhook for channel
5. Copy webhook URL

##### --discord
**Type:** URL
**Required:** One destination required
**Description:** Discord webhook URL.

```bash
spawn alerts create sweep-123 --discord https://discord.com/api/webhooks/...
```

**Get Discord Webhook:**
1. Server Settings → Integrations → Webhooks
2. Click "New Webhook"
3. Select channel
4. Copy webhook URL

##### --webhook
**Type:** URL
**Required:** One destination required
**Description:** Generic webhook URL (POST with JSON payload).

```bash
spawn alerts create sweep-123 --webhook https://example.com/webhook
```

**Webhook Payload Format:**
```json
{
  "alert_type": "sweep_complete",
  "sweep_id": "sweep-20260127-abc123",
  "status": "completed",
  "timestamp": "2026-01-27T15:30:00Z",
  "details": {
    "total": 50,
    "succeeded": 48,
    "failed": 2
  }
}
```

##### --email
**Type:** String (email address)
**Required:** One destination required
**Description:** Email address (via SNS).

```bash
spawn alerts create sweep-123 --email ops@example.com
```

**Note:** Email requires SNS topic subscription confirmation (check inbox).

#### Alert Triggers

##### --on-complete
**Type:** Boolean
**Default:** `false`
**Description:** Alert when sweep completes successfully.

```bash
spawn alerts create sweep-123 --slack https://... --on-complete
```

##### --on-failure
**Type:** Boolean
**Default:** `false`
**Description:** Alert on sweep failures or errors.

```bash
spawn alerts create sweep-123 --slack https://... --on-failure
```

##### --on-start
**Type:** Boolean
**Default:** `false`
**Description:** Alert when sweep starts.

```bash
spawn alerts create sweep-123 --slack https://... --on-start
```

##### --on-schedule
**Type:** Boolean
**Default:** `false`
**Description:** Alert when scheduled sweep triggers.

```bash
spawn alerts create schedule-nightly --slack https://... --on-schedule
```

##### --cost-threshold
**Type:** Float (USD)
**Default:** None
**Description:** Alert when cost exceeds threshold.

```bash
# Alert at $100 spend
spawn alerts create global --cost-threshold 100 --slack https://...

# Alert for specific sweep at $50
spawn alerts create sweep-123 --cost-threshold 50 --slack https://...
```

#### Alert Configuration

##### --name
**Type:** String
**Default:** Auto-generated
**Description:** Human-readable alert name.

```bash
spawn alerts create sweep-123 \
  --name "Training sweep completion alert" \
  --slack https://...
```

##### --throttle
**Type:** Duration
**Default:** `5m`
**Description:** Minimum time between alerts (prevent spam).

```bash
# Alert at most once per hour
spawn alerts create sweep-123 --slack https://... --throttle 1h
```

## list - List Alerts

### Synopsis
```bash
spawn alerts list [flags]
```

### Flags

#### --sweep-id
**Type:** String
**Default:** None
**Description:** Filter by sweep ID.

```bash
spawn alerts list --sweep-id sweep-20260127-abc123
```

#### --schedule-id
**Type:** String
**Default:** None
**Description:** Filter by schedule ID.

```bash
spawn alerts list --schedule-id schedule-nightly
```

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn alerts list --json
```

### Output

```
+----------------------------------+------------------------+-------------+---------------------------+
| Alert ID                         | Name                   | Triggers    | Destination               |
+----------------------------------+------------------------+-------------+---------------------------+
| alert-20260127-abc123            | Sweep completion       | on-complete | Slack: #ml-alerts         |
| alert-20260127-def456            | Cost threshold         | cost: $100  | Email: ops@example.com    |
| alert-20260127-ghi789            | Schedule notification  | on-schedule | Slack: #cron-jobs         |
+----------------------------------+------------------------+-------------+---------------------------+

Total: 3 alerts
```

## delete - Delete Alert

### Synopsis
```bash
spawn alerts delete <alert-id> [flags]
```

### Arguments

#### alert-id
**Type:** String
**Required:** Yes
**Description:** Alert ID to delete.

```bash
spawn alerts delete alert-20260127-abc123
```

### Flags

#### --force
**Type:** Boolean
**Default:** `false`
**Description:** Skip confirmation prompt.

```bash
spawn alerts delete alert-20260127-abc123 --force
```

## describe - Describe Alert

### Synopsis
```bash
spawn alerts describe <alert-id> [flags]
```

### Arguments

#### alert-id
**Type:** String
**Required:** Yes
**Description:** Alert ID to describe.

```bash
spawn alerts describe alert-20260127-abc123
```

### Output

```
Alert: alert-20260127-abc123
Name: Sweep completion alert
Created: 2026-01-27 14:00:00 PST

Resource:
  Type: sweep
  ID: sweep-20260127-abc123

Triggers:
  ✓ On completion
  ✓ On failure
  ✗ On start
  ✗ On schedule
  Cost threshold: $50

Destinations:
  • Slack: #ml-alerts (https://hooks.slack.com/.../***XXX)
  • Email: ops@example.com

Configuration:
  Throttle: 5 minutes
  Enabled: true
  Last triggered: 2026-01-27 15:30:00 PST

Statistics:
  Total triggers: 12
  Last 24 hours: 3
  Failed deliveries: 0
```

## Examples

### Slack Alert on Sweep Completion
```bash
spawn alerts create sweep-20260127-abc123 \
  --slack https://hooks.slack.com/services/T.../B.../XXX \
  --on-complete \
  --name "Training sweep completed"
```

### Multiple Trigger Conditions
```bash
spawn alerts create sweep-20260127-abc123 \
  --slack https://hooks.slack.com/services/T.../B.../XXX \
  --on-complete \
  --on-failure \
  --cost-threshold 100
```

### Cost Alert (Global)
```bash
spawn alerts create global \
  --email ops@example.com \
  --cost-threshold 500 \
  --name "Monthly cost alert"
```

### Discord Alert for Schedule
```bash
spawn alerts create schedule-nightly \
  --discord https://discord.com/api/webhooks/.../... \
  --on-schedule \
  --on-failure
```

### Multiple Destinations
```bash
spawn alerts create sweep-123 \
  --slack https://hooks.slack.com/.../... \
  --email ops@example.com \
  --discord https://discord.com/api/webhooks/.../... \
  --on-complete
```

### List All Alerts
```bash
spawn alerts list

# Filter by sweep
spawn alerts list --sweep-id sweep-123

# JSON for automation
spawn alerts list --json | jq '.[] | select(.triggers.cost_threshold > 0)'
```

### Delete Alert
```bash
# With confirmation
spawn alerts delete alert-20260127-abc123

# Skip confirmation
spawn alerts delete alert-20260127-abc123 --force
```

## Alert Message Examples

### Slack - Sweep Completion
```
🎉 Sweep Completed

Sweep: sweep-20260127-abc123
Status: ✅ Completed
Duration: 2h 15m

Results:
  • Total: 50 instances
  • Succeeded: 48
  • Failed: 2
  • Cost: $12.45

Failed instances:
  • run-027 (i-0987654321fed): Spot interruption
  • run-035 (i-543210fedcba9): Instance limit exceeded

View results: spawn collect-results --sweep-id sweep-20260127-abc123
```

### Slack - Cost Alert
```
⚠️ Cost Threshold Exceeded

Alert: Monthly cost alert
Threshold: $100.00
Current: $127.35 (+27%)

Top spenders:
  1. sweep-20260127-abc123: $45.20 (35%)
  2. sweep-20260126-def456: $32.10 (25%)
  3. Individual instances: $50.05 (40%)

View breakdown: spawn cost --breakdown
```

### Email - Schedule Trigger
```
Subject: Scheduled Sweep Started - schedule-nightly

Schedule: schedule-nightly
Sweep: sweep-20260127-xyz789
Started: 2026-01-27 02:00:00 PST

Parameters: 30 instances
Expected duration: ~45 minutes
Expected cost: ~$8.50

Monitor progress:
spawn status --sweep-id sweep-20260127-xyz789
```

## Webhook Security

### Encryption
- Webhook URLs encrypted at rest using AWS KMS
- KMS key: `alias/spawn-webhook-encryption`
- Decrypted only when sending notifications
- URLs masked in logs: `https://hooks.slack.com/.../***XXX`

### HTTPS Required
- All webhook URLs must use HTTPS
- Self-signed certificates rejected
- HTTP URLs rejected for security

### Authentication
Webhooks receive standard payload format - implement verification on receiving end if needed.

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | Operation failed (DynamoDB error, invalid webhook URL) |
| 2 | Invalid arguments (missing required flags, invalid format) |
| 3 | Alert not found (alert ID doesn't exist) |

## Troubleshooting

### "Invalid webhook URL"
```bash
# Must be HTTPS
spawn alerts create sweep-123 --slack https://hooks.slack.com/...  # ✓
spawn alerts create sweep-123 --slack http://hooks.slack.com/...   # ✗

# Must be valid URL format
spawn alerts create sweep-123 --slack "not a url"  # ✗
```

### Alerts Not Firing
```bash
# Check alert configuration
spawn alerts describe alert-20260127-abc123

# Check if alert is enabled
# Check throttle period (may be rate-limited)

# Test webhook manually
curl -X POST https://hooks.slack.com/... \
  -H 'Content-Type: application/json' \
  -d '{"text":"Test message"}'
```

### Email Not Received
```bash
# Check SNS subscription status
aws sns list-subscriptions-by-topic --topic-arn arn:aws:sns:...

# Confirm subscription (check email)
# Check spam folder
```

## See Also
- [spawn launch](launch.md) - Launch sweeps with alerts
- [spawn schedule](schedule.md) - Schedule sweeps with alerts
- [spawn cost](cost.md) - Track costs
- [spawn status](status.md) - Check sweep status
- [Environment Variables](../environment-variables.md) - `WEBHOOK_KMS_KEY_ID`
