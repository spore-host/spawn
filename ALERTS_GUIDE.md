# Alerts Guide

Get notified when your parameter sweeps complete, fail, exceed cost thresholds, or encounter issues.

## Table of Contents

- [Quick Start](#quick-start)
- [Alert Triggers](#alert-triggers)
- [Notification Destinations](#notification-destinations)
- [CLI Reference](#cli-reference)
- [Examples](#examples)
- [Architecture](#architecture)
- [Setup & Deployment](#setup--deployment)

## Quick Start

### 1. Create an Alert

Get notified via email when a sweep completes:

```bash
spawn alerts create sweep-abc123 \
  --on-complete \
  --email user@example.com
```

### 2. List Alerts

```bash
spawn alerts list
```

### 3. View Alert History

```bash
spawn alerts history alert-def456
```

### 4. Delete an Alert

```bash
spawn alerts delete alert-def456
```

## Alert Triggers

Alerts can be triggered by various events:

### Sweep Completion

Triggered when a parameter sweep completes successfully.

```bash
spawn alerts create sweep-123 \
  --on-complete \
  --email user@example.com
```

### Sweep Failure

Triggered when a parameter sweep fails.

```bash
spawn alerts create sweep-123 \
  --on-failure \
  --slack https://hooks.slack.com/services/YOUR/WEBHOOK/URL
```

### Cost Threshold

Triggered when total sweep cost exceeds a threshold (in dollars).

```bash
spawn alerts create sweep-123 \
  --cost-threshold 100 \
  --email finance@example.com
```

### Long-Running Sweep

Triggered when a sweep runs longer than expected (in minutes).

```bash
spawn alerts create sweep-123 \
  --long-running 120 \
  --email user@example.com
```

**Note**: Sweep duration is measured from start time to alert evaluation time.

### Instance Failure

Triggered when any instance in the sweep fails.

```bash
spawn alerts create sweep-123 \
  --instance-failed \
  --slack https://hooks.slack.com/services/YOUR/WEBHOOK/URL
```

## Notification Destinations

Alerts can be sent to multiple destinations:

### Email

Email notifications via Amazon SNS.

```bash
--email user@example.com
```

**Format**:
```
Subject: [spawn] Sweep Completed: ml-training-20260124-140530

Your parameter sweep has completed successfully!

Sweep ID: ml-training-20260124-140530
Status: Completed
Duration: 2h 34m
Instances: 10/10 completed
Cost: $45.67

View details: spawn status ml-training-20260124-140530
```

**Setup**: Subscribe to SNS topic `spawn-sweep-alerts` in your AWS account.

### Slack

Slack notifications via incoming webhooks.

```bash
--slack https://hooks.slack.com/services/YOUR/WEBHOOK/URL
```

**Format**:
```
✅ Sweep completed successfully: ml-training-20260124-140530

Sweep ID: ml-training-20260124-140530
Status: Completed
Cost: $45.67
Instances: 10
```

**Setup**:
1. Create Slack app: https://api.slack.com/apps
2. Enable Incoming Webhooks
3. Add webhook to workspace
4. Copy webhook URL

### SNS Topic

Send to custom Amazon SNS topic.

```bash
--sns arn:aws:sns:us-east-1:123456789012:my-alerts
```

### Webhook

POST JSON payload to custom HTTP endpoint.

```bash
--webhook https://api.example.com/alerts
```

**Payload**:
```json
{
  "message": "Sweep completed successfully: sweep-123",
  "sweep_id": "sweep-123",
  "status": "completed",
  "cost": 45.67
}
```

## CLI Reference

### `spawn alerts create`

Create a new alert configuration.

```bash
spawn alerts create <sweep-id> [flags]
```

**Flags**:
- `--on-complete` - Alert on sweep completion
- `--on-failure` - Alert on sweep failure
- `--cost-threshold <dollars>` - Alert when cost exceeds threshold
- `--long-running <minutes>` - Alert when duration exceeds threshold
- `--instance-failed` - Alert on instance failures
- `--email <address>` - Email destination
- `--slack <webhook-url>` - Slack destination
- `--sns <topic-arn>` - SNS topic destination
- `--webhook <url>` - HTTP webhook destination
- `--schedule-id <id>` - Alert for schedule instead of sweep

**Requirements**:
- At least one trigger flag (`--on-complete`, `--on-failure`, etc.)
- At least one destination flag (`--email`, `--slack`, etc.)
- Either `<sweep-id>` argument or `--schedule-id` flag

**Examples**:

```bash
# Single trigger, single destination
spawn alerts create sweep-123 \
  --on-complete \
  --email user@example.com

# Multiple triggers
spawn alerts create sweep-123 \
  --on-complete \
  --on-failure \
  --email user@example.com

# Multiple destinations
spawn alerts create sweep-123 \
  --on-failure \
  --email user@example.com \
  --slack https://hooks.slack.com/...

# Cost threshold
spawn alerts create sweep-123 \
  --cost-threshold 100 \
  --email finance@example.com

# Schedule alert
spawn alerts create --schedule-id sched-123 \
  --on-failure \
  --slack https://hooks.slack.com/...
```

### `spawn alerts list`

List all alert configurations.

```bash
spawn alerts list [flags]
```

**Flags**:
- `--sweep-id <id>` - Filter by sweep ID

**Output**:
```
ALERT ID          SWEEP/SCHEDULE     TRIGGERS              DESTINATIONS       CREATED
alert-abc123      sweep-123          complete, failure     email, slack       2026-01-24 14:30
alert-def456      sweep-456          cost_threshold        email              2026-01-24 15:45
```

### `spawn alerts delete`

Delete an alert configuration.

```bash
spawn alerts delete <alert-id>
```

### `spawn alerts history`

View alert notification history.

```bash
spawn alerts history <alert-id>
```

**Output**:
```
TIMESTAMP             TRIGGER       SUCCESS  MESSAGE
2026-01-24 14:35:12   complete      ✓        Sweep sweep-123: complete
2026-01-24 15:20:45   failure       ✗        Failed: connection timeout
```

## Examples

### ML Training Alerts

Get notified when training completes or costs exceed budget:

```bash
# Create sweep with queue
spawn launch --queue-template ml-pipeline \
  --template-var INPUT=/data/train.csv \
  --template-var S3_BUCKET=ml-results \
  --instance-type p3.2xlarge

# Capture sweep ID from output
SWEEP_ID="ml-pipeline-20260124-143052"

# Create alerts
spawn alerts create $SWEEP_ID \
  --on-complete \
  --on-failure \
  --cost-threshold 500 \
  --email team@example.com \
  --slack https://hooks.slack.com/services/...
```

### Scheduled Job Monitoring

Monitor nightly ETL pipeline:

```bash
# Create schedule
spawn schedule create etl-params.yaml \
  --cron "0 2 * * *" \
  --name "nightly-etl"

# Capture schedule ID from output
SCHED_ID="nightly-etl-20260124"

# Alert on failures
spawn alerts create --schedule-id $SCHED_ID \
  --on-failure \
  --email ops@example.com
```

### Cost Management

Monitor sweep costs across team:

```bash
# Low budget research sweep
spawn alerts create sweep-research-001 \
  --cost-threshold 50 \
  --email researcher@example.com

# Production training sweep
spawn alerts create sweep-prod-train-002 \
  --cost-threshold 1000 \
  --email finance@example.com \
  --email team-lead@example.com
```

### Instance Failure Monitoring

Get immediate notification of instance failures:

```bash
spawn alerts create sweep-critical-job \
  --instance-failed \
  --on-failure \
  --slack https://hooks.slack.com/services/... \
  --email oncall@example.com
```

## Architecture

### Components

```
┌─────────────┐
│  spawn CLI  │
└──────┬──────┘
       │ (1) Create Alert Config
       ↓
┌──────────────────┐
│ DynamoDB Table:  │
│  spawn-alerts    │
└──────────────────┘

┌──────────────────┐
│  Sweep Completes │
│   (DynamoDB)     │
└────────┬─────────┘
         │ (2) Stream Event
         ↓
┌──────────────────┐
│ Lambda:          │
│ alert-handler    │
└────────┬─────────┘
         │ (3) Query Alerts
         ↓
┌──────────────────┐
│ DynamoDB Table:  │
│  spawn-alerts    │
└────────┬─────────┘
         │ (4) Send Notifications
         ↓
┌──────────────────────────────────────┐
│  Email (SNS) │ Slack │ Webhook │ SNS │
└──────────────────────────────────────┘
         │ (5) Record History
         ↓
┌──────────────────┐
│ DynamoDB Table:  │
│ spawn-alert-     │
│   history        │
└──────────────────┘
```

### DynamoDB Tables

**spawn-alerts**:
- Primary key: `alert_id`
- GSI: `sweep_id-index` - Query alerts by sweep
- GSI: `user_id-index` - Query alerts by user
- TTL: 90 days (configurable)

**spawn-alert-history**:
- Primary key: `alert_id` (hash), `timestamp` (range)
- GSI: `user_id-timestamp-index` - Query history by user
- TTL: 90 days (configurable)

### SNS Topics

- `spawn-sweep-alerts` - Sweep completion/failure notifications
- `spawn-schedule-alerts` - Schedule execution notifications
- `spawn-cost-alerts` - Cost threshold notifications

### Lambda Functions

**alert-handler**:
- Triggered by: DynamoDB Streams, EventBridge
- Queries alert configurations
- Sends notifications to configured destinations
- Records notification history

## Setup & Deployment

### 1. Deploy DynamoDB Tables

```bash
aws cloudformation deploy \
  --template-file deployment/cloudformation/alerts-tables.yaml \
  --stack-name spawn-alerts \
  --parameter-overrides Environment=production
```

### 2. Deploy Lambda Function

```bash
# Build
cd lambda/alert-handler
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go
zip function.zip bootstrap

# Deploy
aws lambda create-function \
  --function-name alert-handler \
  --runtime provided.al2023 \
  --role arn:aws:iam::ACCOUNT_ID:role/alert-handler-role \
  --handler bootstrap \
  --zip-file fileb://function.zip

# Or update existing
aws lambda update-function-code \
  --function-name alert-handler \
  --zip-file fileb://function.zip
```

### 3. Configure DynamoDB Streams

Enable streams on `spawn-sweeps` table to trigger alert-handler on state changes.

```bash
aws dynamodb update-table \
  --table-name spawn-sweeps \
  --stream-specification StreamEnabled=true,StreamViewType=NEW_AND_OLD_IMAGES

# Add event source mapping
aws lambda create-event-source-mapping \
  --function-name alert-handler \
  --event-source-arn arn:aws:dynamodb:REGION:ACCOUNT:table/spawn-sweeps/stream/STREAM_ID \
  --starting-position LATEST \
  --batch-size 10
```

### 4. Subscribe to SNS Topics (Email)

```bash
# Subscribe email to sweep alerts topic
aws sns subscribe \
  --topic-arn arn:aws:sns:us-east-1:ACCOUNT_ID:spawn-sweep-alerts \
  --protocol email \
  --notification-endpoint user@example.com

# Confirm subscription in email
```

### 5. IAM Permissions

**CLI User**:
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:PutItem",
        "dynamodb:GetItem",
        "dynamodb:Query",
        "dynamodb:DeleteItem"
      ],
      "Resource": [
        "arn:aws:dynamodb:*:*:table/spawn-alerts",
        "arn:aws:dynamodb:*:*:table/spawn-alerts/index/*",
        "arn:aws:dynamodb:*:*:table/spawn-alert-history",
        "arn:aws:dynamodb:*:*:table/spawn-alert-history/index/*"
      ]
    }
  ]
}
```

**Lambda Function**:
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:Query",
        "dynamodb:PutItem",
        "dynamodb:GetRecords",
        "dynamodb:GetShardIterator",
        "dynamodb:DescribeStream",
        "dynamodb:ListStreams"
      ],
      "Resource": [
        "arn:aws:dynamodb:*:*:table/spawn-alerts",
        "arn:aws:dynamodb:*:*:table/spawn-alerts/index/*",
        "arn:aws:dynamodb:*:*:table/spawn-alert-history",
        "arn:aws:dynamodb:*:*:table/spawn-sweeps/stream/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "sns:Publish"
      ],
      "Resource": [
        "arn:aws:sns:*:*:spawn-sweep-alerts",
        "arn:aws:sns:*:*:spawn-schedule-alerts",
        "arn:aws:sns:*:*:spawn-cost-alerts"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:*:*:*"
    }
  ]
}
```

## Troubleshooting

### Alerts Not Being Sent

1. **Check alert configuration**:
   ```bash
   spawn alerts list --sweep-id sweep-123
   ```

2. **Check Lambda logs**:
   ```bash
   aws logs tail /aws/lambda/alert-handler --follow
   ```

3. **Verify DynamoDB streams enabled**:
   ```bash
   aws dynamodb describe-table --table-name spawn-sweeps | grep StreamArn
   ```

4. **Check SNS subscriptions**:
   ```bash
   aws sns list-subscriptions-by-topic \
     --topic-arn arn:aws:sns:us-east-1:ACCOUNT_ID:spawn-sweep-alerts
   ```

### Email Not Received

1. **Check SNS subscription confirmed**:
   - Look for confirmation email from AWS SNS
   - Click confirmation link

2. **Check spam folder**

3. **Verify subscription**:
   ```bash
   aws sns list-subscriptions-by-topic \
     --topic-arn arn:aws:sns:REGION:ACCOUNT:spawn-sweep-alerts
   ```

### Slack Webhook Failing

1. **Test webhook manually**:
   ```bash
   curl -X POST -H 'Content-type: application/json' \
     --data '{"text":"Test message"}' \
     https://hooks.slack.com/services/YOUR/WEBHOOK/URL
   ```

2. **Check Lambda logs** for Slack API errors

3. **Verify webhook URL** is correct and not expired

### Cost Threshold Not Triggering

Cost threshold alerts require periodic sweep status updates. The alert-handler must be invoked while the sweep is running to check costs.

**Note**: Current implementation triggers alerts only on completion/failure. Real-time cost monitoring requires additional EventBridge rule to periodically invoke alert-handler for running sweeps.

### Alert History Empty

History is only recorded when notifications are sent. If alert configuration exists but no notifications sent, history will be empty.

Check:
1. Has the trigger event occurred?
2. Are destinations configured correctly?
3. Check Lambda logs for errors

## Limitations

- Alerts are evaluated when sweep state changes (complete/failed)
- Cost threshold and long-running alerts require periodic evaluation (not yet implemented)
- Email delivery depends on SNS topic subscriptions
- Alert configurations expire after 90 days (TTL)
- Alert history expires after 90 days (TTL)

## Prometheus Alertmanager Integration (v0.19.0+)

Spawn now supports Prometheus Alertmanager for advanced, metric-based alerting beyond sweep-level alerts.

### Overview

The Alertmanager integration provides:
- **26 pre-built alert rules** across 4 categories
- **Real-time monitoring** via Prometheus metrics
- **Predictive alerts** (e.g., cost forecasting)
- **Flexible routing** to multiple destinations
- **Alert grouping and inhibition** rules

### Alert Categories

**Instance Lifecycle (6 rules):**
- InstanceHighIdleTime - Instance idle > 30 minutes
- InstanceTTLExpiringSoon - TTL < 5 minutes remaining
- SpotInstanceInterruptionWarning - Spot interruption detected
- InstanceNoActivity - No activity for 2+ hours
- InstanceIdleTimeoutExpiringSoon - Idle timeout < 5 minutes
- InstanceRecentlyStarted - Instance started < 5 minutes ago

**Cost Management (6 rules):**
- HighCostInstance - Instance > $5/hour
- DailyCostBudgetExceeded - Total cost > $200
- CostTrendingUp - Cost 50%+ higher than 24h ago
- HighRegionalCost - Regional cost > $50
- IdleHighCostInstance - Idle instance > $2/hour
- CostForecastExceeded - Forecasted daily cost > budget (predictive)

**Capacity (6 rules):**
- HighFleetSize - Fleet > 100 instances
- LowSpotAvailability - < 20% spot instances
- RegionalCapacityImbalance - One region has 3x average capacity
- JobArraySizeAnomaly - Job array > 50 instances
- FleetGrowthAnomaly - Fleet doubled in 1 hour
- ProviderImbalance - EC2 10x local instances

**Performance (8 rules):**
- HighCPUUsage - CPU > 95% for 5 minutes
- HighMemoryUsage - Memory > 90% for 5 minutes
- GPUHighUtilization - GPU > 95% for 10 minutes
- GPUHighTemperature - GPU > 80°C for 5 minutes
- HighNetworkThroughput - Network > 100MB/s for 10 minutes
- HighDiskIO - Disk I/O > 50MB/s for 10 minutes
- FleetAverageCPUHigh - Fleet avg CPU > 80% for 10 minutes
- RegionalPerformanceDegradation - Regional avg CPU > 90% for 15 minutes

### Setup

1. **Enable metrics on instances:**
   ```bash
   spawn launch --instance-type m7i.large \
     --tag spawn:metrics-enabled=true \
     --tag spawn:metrics-port=9090
   ```

2. **Install Prometheus and Alertmanager:**
   ```bash
   brew install prometheus alertmanager
   ```

3. **Configure Prometheus:**
   ```bash
   cp deployment/prometheus/prometheus.yaml /etc/prometheus/prometheus.yml
   cp deployment/prometheus/alerts/*.yaml /etc/prometheus/alerts/
   prometheus --config.file=/etc/prometheus/prometheus.yml
   ```

4. **Configure Alertmanager:**
   ```bash
   cp deployment/prometheus/alertmanager.yaml /etc/alertmanager/alertmanager.yml
   alertmanager --config.file=/etc/alertmanager/alertmanager.yml
   ```

### Integration with Existing Alerts

Alertmanager integrates with spawn's existing alert system via webhook bridge:

```
Prometheus → Alertmanager → Webhook Bridge → SNS/Slack/Email
```

The bridge converts Prometheus alerts to spawn format and routes them through your existing notification channels.

### Customization

Edit alert rules in `deployment/prometheus/alerts/*.yaml`:

```yaml
# Adjust CPU threshold
- alert: HighCPUUsage
  expr: spawn_cpu_usage_percent > 90  # Changed from 95
  for: 5m
  labels:
    severity: warning
```

Reload Prometheus:
```bash
curl -X POST http://localhost:9090/-/reload
```

### Documentation

- Complete setup guide: `docs/how-to/prometheus-alerting.md`
- Metrics reference: `docs/reference/metrics.md`
- Quick start: `deployment/prometheus/README.md`
- Grafana dashboards: `deployment/grafana/README.md`

### Comparison: Sweep Alerts vs Alertmanager

**Use Sweep Alerts for:**
- Parameter sweep completion/failure
- Sweep-level cost tracking
- Schedule-based alerts
- Simple email/Slack notifications

**Use Alertmanager for:**
- Real-time resource monitoring (CPU, memory, GPU)
- Fleet-wide visibility
- Predictive cost alerts
- Complex routing and grouping
- Integration with existing Prometheus/Grafana stack

Both systems can run concurrently and complement each other.

## Future Enhancements

- CloudWatch dashboard integration
- Periodic sweep monitoring for cost/duration thresholds (partially addressed by Alertmanager)
- Custom alert messages and templates
- Alert aggregation (don't spam on every instance failure)
- PagerDuty integration (possible via Alertmanager)
- Microsoft Teams integration
- SMS notifications via SNS
- Alert grouping and muting (supported in Alertmanager)
