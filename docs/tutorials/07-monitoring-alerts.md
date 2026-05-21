# Tutorial 7: Monitoring & Alerts

**Duration:** 30 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md)

## What You'll Learn

In this tutorial, you'll learn how to monitor instances and set up notifications:
- Monitor instance status and health
- Set up Slack/Discord alerts
- Create cost threshold alerts
- Monitor parameter sweeps
- Configure alert triggers
- Debug failed instances

## Why Monitoring Matters

**Without Monitoring:**
- ❌ Don't know when sweep completes
- ❌ Discover failures hours later
- ❌ No visibility into instance health
- ❌ Manual checking required

**With Monitoring:**
- ✅ Instant notification when sweep completes
- ✅ Immediate alert on failures
- ✅ Real-time instance health metrics
- ✅ Cost alerts prevent overspending

## Monitoring Options

### Built-in Monitoring

spawn provides real-time monitoring via `status` command:

```bash
# Single instance
spawn status <instance-id>

# Parameter sweep
spawn status <sweep-id>

# Watch mode (auto-refresh)
spawn status <instance-id> --watch
```

### Alert Notifications

Get notifications via:
- **Slack** - Post to channel
- **Discord** - Post to server
- **Email** - Send via SNS
- **Webhook** - POST to custom endpoint

### Alert Triggers

Alert on:
- Sweep completion
- Sweep failure
- Cost threshold exceeded
- Schedule trigger
- Instance state changes

## Monitoring a Single Instance

### Check Instance Status

```bash
spawn status i-0abc123def456789
```

**Output:**
```
Instance: i-0abc123def456789
Name: ml-training-gpu
Region: us-east-1
State: running

Instance Type: g5.xlarge
AMI: ami-pytorch-cuda
Availability Zone: us-east-1a

Launch Time: 2026-01-27 10:00:00 PST
Uptime: 2h 15m
TTL: 8h (5h 45m remaining)
Auto-terminate at: 2026-01-27 18:00:00 PST

Network:
  Public IP: 54.123.45.67
  Private IP: 10.0.1.42
  DNS: i-0abc123def456789.c0zxr0ao.spore.host

Health:
  Status Checks: 2/2 passed
  System Status: ok
  Instance Status: ok

CPU:
  Current: 85%
  Average (15m): 72%

Memory:
  Used: 12.4 GB / 16 GB (78%)
  Available: 3.6 GB

Disk:
  Root Volume (100 GB): 45 GB used (45%)

Cost:
  Current: $2.26 (2.25 hours)
  Hourly: $1.006
  Projected 8h: $8.05

Tags:
  Name: ml-training-gpu
  project: cifar10-classification
  owner: alice
  spawn:managed: true
```

### Watch Mode

Auto-refresh every 10 seconds:

```bash
spawn status i-0abc123def456789 --watch
```

**Press Ctrl+C to exit.**

### JSON Output

For programmatic access:

```bash
spawn status i-0abc123def456789 --json | jq '.state'
# Output: "running"

spawn status i-0abc123def456789 --json | jq '.health.status_checks'
# Output: "ok"
```

## Monitoring Parameter Sweeps

### Check Sweep Status

```bash
spawn status sweep-20260127-abc123
```

**Output:**
```
Sweep: sweep-20260127-abc123
Parameters: 50
Region: us-east-1
Status: running

Progress: 32/50 completed (64%)

Instance States:
  Running: 18
  Terminated: 32 (completed successfully)
  Failed: 0

Duration:
  Started: 2026-01-27 08:00:00 PST
  Elapsed: 2h 15m
  Estimated completion: 2026-01-27 11:30:00 PST (1h 15m)

Costs:
  Current: $45.60
  Projected total: $71.25

Recent Completions:
  run-032 completed (exit code: 0) - 2 minutes ago
  run-031 completed (exit code: 0) - 3 minutes ago
  run-030 completed (exit code: 0) - 5 minutes ago

Next to complete (estimated):
  run-033 - 8 minutes remaining
  run-034 - 12 minutes remaining
```

### Watch Sweep Progress

```bash
spawn status sweep-20260127-abc123 --watch
```

Shows real-time updates as instances complete.

## Setting Up Slack Alerts

### Step 1: Create Slack Webhook

1. Go to https://api.slack.com/apps
2. Click "Create New App" → "From scratch"
3. Name: "spawn Alerts", select workspace
4. Click "Incoming Webhooks" → Enable
5. Click "Add New Webhook to Workspace"
6. Select channel (e.g., #ml-alerts)
7. Copy webhook URL

**Example webhook URL:**
```
https://hooks.slack.com/services/T{WORKSPACE}/B{CHANNEL}/{SECRET_TOKEN}
```

### Step 2: Create Alert

```bash
export SLACK_WEBHOOK="https://hooks.slack.com/services/T{WORKSPACE}/B{CHANNEL}/..."

spawn alerts create sweep-20260127-abc123 \
  --slack $SLACK_WEBHOOK \
  --on-complete \
  --on-failure
```

**Expected output:**
```
✓ Alert created successfully

Alert ID: alert-20260127-xyz789
Sweep: sweep-20260127-abc123
Destination: Slack (#ml-alerts)

Triggers:
  ✓ on-complete: Notify when sweep completes
  ✓ on-failure: Notify if any instance fails

Webhook URL: https://hooks.slack.com/services/T012.../B012.../xxxx****

Estimated notification time: 2026-01-27 11:30:00 PST
```

**Security Note:** Webhook URLs are encrypted at rest using AWS KMS.

### Step 3: Verify Alert

When sweep completes, you'll receive Slack message:

```
✅ Sweep Completed: sweep-20260127-abc123

Status: Completed
Duration: 3h 42m
Instances: 50

Results:
  ✓ Succeeded: 48
  ✗ Failed: 2

Cost: $72.45

View results:
  spawn collect-results sweep-20260127-abc123
```

## Setting Up Discord Alerts

### Step 1: Create Discord Webhook

1. Open Discord server
2. Server Settings → Integrations → Webhooks
3. Click "New Webhook"
4. Name: "spawn Alerts"
5. Select channel (e.g., #bot-alerts)
6. Copy webhook URL

**Example webhook URL:**
```
https://discord.com/api/webhooks/{WEBHOOK_ID}/{WEBHOOK_TOKEN}
```

### Step 2: Create Alert

```bash
export DISCORD_WEBHOOK="https://discord.com/api/webhooks/{WEBHOOK_ID}/{WEBHOOK_TOKEN}"

spawn alerts create sweep-20260127-abc123 \
  --discord $DISCORD_WEBHOOK \
  --on-complete
```

### Discord Message Format

```
🎉 Sweep Completed

**Sweep ID:** sweep-20260127-abc123
**Status:** Completed
**Duration:** 3h 42m
**Instances:** 50
**Succeeded:** 48
**Failed:** 2
**Cost:** $72.45

Run `spawn collect-results sweep-20260127-abc123` to download results.
```

## Setting Up Email Alerts

### Create Email Alert

```bash
spawn alerts create sweep-20260127-abc123 \
  --email ops@example.com \
  --on-complete \
  --on-failure
```

**Expected output:**
```
✓ Alert created successfully

Alert ID: alert-20260127-email123
Destination: Email (ops@example.com)

⚠️  Subscription Confirmation Required

A confirmation email has been sent to ops@example.com.
Please check your inbox and click "Confirm subscription" to activate alerts.

Once confirmed, you'll receive notifications for:
  ✓ on-complete: Sweep completion
  ✓ on-failure: Instance failures
```

**Check email:**
```
Subject: AWS Notification - Subscription Confirmation

You have chosen to subscribe to the topic:
arn:aws:sns:us-east-1:123456789012:spawn-alerts

To confirm, click: [Confirm subscription]
```

Click link to activate.

## Cost Threshold Alerts

### Create Budget Alert

```bash
spawn alerts create global \
  --cost-threshold 500.00 \
  --slack $SLACK_WEBHOOK
```

**What this does:**
- Monitors total monthly spending
- Alerts at 80%, 90%, 100% of threshold
- Posts to Slack when thresholds crossed

**Slack notifications:**

**At 80% ($400):**
```
⚠️ Budget Alert: 80% of Monthly Limit

Current spending: $400.00 / $500.00
Percentage: 80%
Days into month: 24 / 31

Projected end-of-month: $517 (over budget)

Top 5 instances:
  1. g5.xlarge (i-0abc123) - $125.40
  2. m7i.large (i-0abc234) - $89.20
  3. t3.medium (sweep-xyz) - $65.30
```

**At 100% ($500):**
```
🚨 Budget Alert: Monthly Limit Reached

Current spending: $500.00 / $500.00
Days remaining: 7

Consider:
  • Review running instances: spawn list
  • Terminate unused instances
  • Switch to spot instances for non-critical work
  • Check costs: spawn cost --breakdown
```

### Instance-Level Cost Alert

```bash
spawn alerts create i-0abc123def456789 \
  --cost-threshold 50.00 \
  --slack $SLACK_WEBHOOK
```

**Alerts when specific instance exceeds $50.**

Useful for long-running GPU instances.

## Alert Triggers

### On Completion

```bash
spawn alerts create sweep-123 --slack $WEBHOOK --on-complete
```

Alerts when all instances in sweep complete successfully.

### On Failure

```bash
spawn alerts create sweep-123 --slack $WEBHOOK --on-failure
```

Alerts immediately when any instance fails.

**Failure notification includes:**
- Instance ID and name
- Exit code
- Error message
- Log snippet
- Retry attempts

**Example:**
```
❌ Instance Failed: run-023

Instance ID: i-0abc345
Exit Code: 1
Attempts: 3/3 (all retries exhausted)

Error: CUDA out of memory

Last 10 log lines:
  [15:32:10] Loading model...
  [15:32:15] Allocating 12GB tensor...
  [15:32:16] RuntimeError: CUDA out of memory
  [15:32:16] Tried to allocate 12.00 GB
  [15:32:16] Process exited with code 1

Action: Check instance type (may need more GPU memory)
```

### On Start

```bash
spawn alerts create sweep-123 --slack $WEBHOOK --on-start
```

Alerts when sweep starts launching instances.

Useful for scheduled sweeps to confirm execution.

### Multiple Triggers

```bash
spawn alerts create sweep-123 \
  --slack $WEBHOOK \
  --on-start \
  --on-complete \
  --on-failure \
  --cost-threshold 100.00
```

One alert configuration, multiple triggers.

## Scheduled Alerts

### Daily Summary

```bash
spawn alerts create global \
  --email ops@example.com \
  --daily-summary \
  --summary-time "09:00"
```

**Daily email at 9 AM:**
```
Subject: spawn Daily Summary - January 27, 2026

Yesterday's Activity:
  • Instances launched: 45
  • Instances terminated: 42
  • Currently running: 8

Costs:
  • Yesterday: $32.45
  • Month-to-date: $456.78
  • Projected month-end: $526.00

Top 5 instances by cost:
  1. g5.xlarge (i-0abc123) - $8.45
  2. m7i.large (i-0abc234) - $5.20
  3. t3.medium (sweep-xyz) - $3.80

Running instances:
  • ml-training-gpu (g5.xlarge) - $1.006/hour
  • data-processing (m7i.large) - $0.1008/hour

Action items:
  ⚠️ 3 instances running > 24 hours
  ⚠️ Budget: 91% used (9 days until limit)
```

### Weekly Reports

```bash
spawn alerts create global \
  --email ops@example.com \
  --weekly-summary \
  --summary-day monday \
  --summary-time "09:00"
```

**Every Monday at 9 AM:**
```
Subject: spawn Weekly Summary - Week of January 20, 2026

This Week:
  • Instances launched: 234
  • Total compute hours: 1,456
  • Total cost: $187.45
  • Average daily: $26.78

Compared to last week:
  • Cost: +12% ($167.20 → $187.45)
  • Instances: +8% (217 → 234)

Cost by instance type:
  • g5.xlarge: $89.40 (48%)
  • m7i.large: $56.20 (30%)
  • t3.medium: $28.30 (15%)
  • t3.micro: $13.55 (7%)

Optimization opportunities:
  • Switch g5.xlarge workloads to spot: Save $62.58 (70%)
  • Use idle timeout on GPUs: Save $15.40
```

## Alert Management

### List All Alerts

```bash
spawn alerts list
```

**Output:**
```
+----------------------------------+------------------------+-------------+---------------------------+
| Alert ID                         | Name                   | Triggers    | Destination               |
+----------------------------------+------------------------+-------------+---------------------------+
| alert-20260127-abc123            | Sweep completion       | on-complete | Slack: #ml-alerts         |
| alert-20260127-def456            | Budget alert           | cost: $500  | Email: ops@example.com    |
| alert-20260127-ghi789            | Failure notifications  | on-failure  | Discord: #bot-alerts      |
+----------------------------------+------------------------+-------------+---------------------------+

Total: 3 alerts
```

### View Alert Details

```bash
spawn alerts describe alert-20260127-abc123
```

**Output:**
```
Alert ID: alert-20260127-abc123
Name: Sweep completion
Created: 2026-01-27 10:00:00 PST

Target:
  Type: sweep
  Sweep ID: sweep-20260127-abc123

Destination:
  Type: Slack
  Webhook: https://hooks.slack.com/services/T012.../B012.../xxxx****
  Channel: #ml-alerts

Triggers:
  ✓ on-complete: Yes
  ✓ on-failure: Yes
  ✗ on-start: No
  ✗ cost-threshold: Not set

Configuration:
  Throttle: 5 minutes
  Last triggered: Never
  Total notifications: 0

Status: Active
```

### Delete Alert

```bash
spawn alerts delete alert-20260127-abc123
```

**Output:**
```
✓ Alert deleted: alert-20260127-abc123

Notifications will stop immediately.
```

## Monitoring Best Practices

### 1. Alert on Failures

Always set up failure alerts:

```bash
spawn alerts create sweep-123 --slack $WEBHOOK --on-failure
```

Catch errors immediately rather than discovering hours later.

### 2. Budget Alerts

Set monthly budget alert:

```bash
spawn alerts create global --cost-threshold 500.00 --email ops@example.com
```

### 3. Completion Alerts for Long Jobs

For sweeps > 2 hours:

```bash
spawn alerts create sweep-123 --slack $WEBHOOK --on-complete
```

Get notified when done instead of checking manually.

### 4. Use Watch Mode

For active monitoring:

```bash
spawn status sweep-123 --watch
```

### 5. Tag Instances for Tracking

```bash
spawn launch --tags project=ml,owner=alice,experiment=v2
```

Makes filtering and monitoring easier:

```bash
spawn list --tag project=ml
spawn cost --tag project=ml
```

## Debugging Failed Instances

### Find Failed Instances

```bash
# List failed instances in sweep
spawn status sweep-123 --filter state=failed

# Or
spawn list --tag sweep:id=sweep-123 --state terminated --exit-code 1
```

### Check Logs

```bash
# Connect to instance (if still running)
spawn connect i-0abc123

# View cloud-init logs
tail -100 /var/log/cloud-init-output.log

# View spored logs
journalctl -u spored -n 100
```

### Common Failure Causes

**Out of Memory:**
```
RuntimeError: Cannot allocate memory
```
**Solution:** Use larger instance type

**CUDA Out of Memory:**
```
RuntimeError: CUDA out of memory
```
**Solution:** Reduce batch size or use instance with more GPU memory

**File Not Found:**
```
FileNotFoundError: [Errno 2] No such file or directory: '/data/input.csv'
```
**Solution:** Check S3 download step, verify file paths

**Timeout:**
```
Job exceeded timeout of 4h
```
**Solution:** Increase timeout or optimize code

### Retry Failed Instances

After fixing issue:

```bash
# Get failed instance IDs
FAILED=$(spawn status sweep-123 --json | jq -r '.instances[] | select(.exit_code != 0) | .parameter_name')

# Relaunch failed instances
spawn launch --param-file sweep.yaml --filter "$FAILED"
```

## Real-World Monitoring Setup

### ML Training Pipeline

```bash
# 1. Create sweep with long training jobs
spawn launch --param-file ml-sweep.yaml

# 2. Set up alerts
SWEEP_ID="sweep-20260127-abc123"

spawn alerts create $SWEEP_ID \
  --slack $SLACK_WEBHOOK \
  --on-complete \
  --on-failure \
  --cost-threshold 200.00

# 3. Monitor in terminal
spawn status $SWEEP_ID --watch

# 4. Let it run
# You'll get Slack notifications when:
#   - Any instance fails
#   - All instances complete
#   - Cost exceeds $200
```

## What You Learned

Congratulations! You now understand:

✅ Monitoring instance status and health
✅ Setting up Slack/Discord/Email alerts
✅ Creating cost threshold alerts
✅ Monitoring parameter sweeps
✅ Configuring alert triggers
✅ Debugging failed instances
✅ Alert management

## Practice Exercises

### Exercise 1: Basic Monitoring

1. Launch an instance
2. Check its status with `spawn status`
3. Use watch mode to monitor it
4. Check its cost

### Exercise 2: Slack Alert

1. Create Slack webhook
2. Launch a parameter sweep
3. Set up completion alert
4. Verify notification when sweep completes

### Exercise 3: Budget Alert

Set up a $100 budget alert and verify it triggers correctly.

## Next Steps

You've completed all Getting Started tutorials! 🎉

Continue learning:

🛠️ **[How-To Guides](../how-to/)** - Task-oriented recipes for specific scenarios

📚 **[Command Reference](../reference/)** - Complete command documentation

💡 **[FAQ](../FAQ.md)** - Common questions and troubleshooting

## Quick Reference

```bash
# Monitor instance
spawn status <instance-id>
spawn status <instance-id> --watch

# Monitor sweep
spawn status <sweep-id>
spawn status <sweep-id> --watch

# Create Slack alert
spawn alerts create <sweep-id> \
  --slack $SLACK_WEBHOOK \
  --on-complete \
  --on-failure

# Create cost alert
spawn alerts create global \
  --cost-threshold 500.00 \
  --email ops@example.com

# List alerts
spawn alerts list

# Delete alert
spawn alerts delete <alert-id>

# Debug failures
spawn list --state terminated --exit-code 1
spawn connect <instance-id>
tail -f /var/log/cloud-init-output.log
```

---

**Previous:** [← Tutorial 6: Cost Management](06-cost-management.md)
**Next:** Explore [How-To Guides](../how-to/) →
