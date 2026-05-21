# Scheduled Executions Guide

Schedule parameter sweeps for future execution using AWS EventBridge Scheduler. Execute sweeps automatically on a recurring schedule or at a specific time without keeping your CLI running.

## Table of Contents

- [Overview](#overview)
- [Quick Start](#quick-start)
- [Schedule Types](#schedule-types)
- [Command Reference](#command-reference)
- [Cron Expressions](#cron-expressions)
- [Timezone Handling](#timezone-handling)
- [Execution Limits](#execution-limits)
- [Monitoring & Management](#monitoring--management)
- [Architecture](#architecture)
- [Troubleshooting](#troubleshooting)
- [Best Practices](#best-practices)

## Overview

Scheduled executions enable you to:

- **Set and forget**: Schedule sweeps to run automatically
- **Recurring schedules**: Run nightly training, weekly experiments, etc.
- **One-time execution**: Schedule a future sweep at a specific time
- **Timezone support**: Handle DST transitions automatically
- **Execution history**: Track all past executions
- **Flexible control**: Pause, resume, or cancel schedules

**Use Cases:**
- Nightly training runs with fresh data
- Weekly model retraining and evaluation
- Monthly batch processing jobs
- Scheduled hyperparameter sweeps
- Continuous experimentation

## Quick Start

### Create a Recurring Schedule

Schedule a nightly training run at 2 AM EST:

```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --name "nightly-training"
```

### Create a One-Time Schedule

Schedule a sweep for tomorrow at 3 PM:

```bash
spawn schedule create params.yaml \
  --at "2026-01-25T15:00:00" \
  --timezone "America/New_York" \
  --name "afternoon-experiment"
```

### Monitor Your Schedules

```bash
# List all schedules
spawn schedule list

# View details and execution history
spawn schedule describe sched-20260122-140530

# Pause temporarily
spawn schedule pause sched-20260122-140530

# Resume
spawn schedule resume sched-20260122-140530

# Cancel permanently
spawn schedule cancel sched-20260122-140530
```

## Schedule Types

### One-Time Schedules

Execute a sweep once at a specific time.

**Syntax:**
```bash
spawn schedule create params.yaml \
  --at "<timestamp>" \
  --timezone "<timezone>"
```

**Timestamp formats:**
- ISO 8601: `2026-01-25T15:00:00`
- Date only: `2026-01-25` (defaults to midnight)

**Example:**
```bash
spawn schedule create hyperparameters.yaml \
  --at "2026-01-25T14:30:00" \
  --timezone "America/Los_Angeles" \
  --name "weekend-experiment"
```

### Recurring Schedules

Execute sweeps on a regular schedule using cron expressions.

**Syntax:**
```bash
spawn schedule create params.yaml \
  --cron "<expression>" \
  --timezone "<timezone>"
```

**Common patterns:**
```bash
# Every day at 2 AM
--cron "0 2 * * *"

# Every Monday at 9 AM
--cron "0 9 * * 1"

# Every 6 hours
--cron "0 */6 * * *"

# First day of every month at midnight
--cron "0 0 1 * *"

# Weekdays at 8 AM
--cron "0 8 * * 1-5"
```

**Example:**
```bash
spawn schedule create nightly-params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --name "nightly-training" \
  --max-executions 30
```

## Command Reference

### `spawn schedule create`

Create a new scheduled execution.

**Syntax:**
```bash
spawn schedule create <params-file> [options]
```

**Required Options (one of):**
- `--at <timestamp>` - One-time execution at specified time
- `--cron <expression>` - Recurring execution with cron expression

**Common Options:**
- `--timezone <tz>` - IANA timezone (e.g., "America/New_York")
- `--name <name>` - Friendly name for the schedule
- `--region <region>` - AWS region (default: us-east-1)

**Execution Limit Options:**
- `--max-executions <n>` - Maximum number of executions (recurring only)
- `--end-after <timestamp>` - Stop executing after this date

**Examples:**

One-time schedule:
```bash
spawn schedule create params.yaml \
  --at "2026-01-26T15:00:00" \
  --timezone "America/New_York" \
  --name "afternoon-run"
```

Recurring with limits:
```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --max-executions 30 \
  --name "30-day-experiment"
```

End after specific date:
```bash
spawn schedule create params.yaml \
  --cron "0 */6 * * *" \
  --timezone "UTC" \
  --end-after "2026-02-01T00:00:00" \
  --name "january-runs"
```

### `spawn schedule list`

List all your schedules.

**Syntax:**
```bash
spawn schedule list [options]
```

**Options:**
- `--status <status>` - Filter by status: active, paused, cancelled
- `--region <region>` - AWS region (default: us-east-1)

**Output:**
```
SCHEDULE ID              NAME                STATUS    TYPE       NEXT EXECUTION
sched-20260122-140530    nightly-training    active    recurring  2026-01-23T02:00:00-05:00
sched-20260122-150000    afternoon-run       active    one-time   2026-01-26T15:00:00-05:00
sched-20260120-100000    old-experiment      paused    recurring  -
```

**Examples:**

List all active schedules:
```bash
spawn schedule list --status active
```

List all schedules (any status):
```bash
spawn schedule list
```

### `spawn schedule describe`

View detailed information about a schedule including execution history.

**Syntax:**
```bash
spawn schedule describe <schedule-id>
```

**Output:**
```
Schedule ID: sched-20260122-140530
Name: nightly-training
Status: active
Type: recurring
Schedule: 0 2 * * * (every day at 2 AM)
Timezone: America/New_York
Created: 2026-01-22T14:05:30-05:00
Next Execution: 2026-01-23T02:00:00-05:00

Execution Limits:
  Max Executions: 30
  Executions So Far: 5
  Remaining: 25

Last 10 Executions:
TIME                       SWEEP ID                  STATUS
2026-01-22T02:00:00-05:00  sweep-20260122-020000     success
2026-01-21T02:00:00-05:00  sweep-20260121-020000     success
2026-01-20T02:00:00-05:00  sweep-20260120-020000     success
2026-01-19T02:00:00-05:00  sweep-20260119-020000     failed
2026-01-18T02:00:00-05:00  sweep-20260118-020000     success
```

**Example:**
```bash
spawn schedule describe sched-20260122-140530
```

### `spawn schedule pause`

Temporarily disable a schedule without deleting it.

**Syntax:**
```bash
spawn schedule pause <schedule-id>
```

**Use cases:**
- Temporary maintenance or debugging
- Preserving configuration while stopping executions
- Quick disable without losing execution history

**Example:**
```bash
spawn schedule pause sched-20260122-140530
```

### `spawn schedule resume`

Re-enable a paused schedule.

**Syntax:**
```bash
spawn schedule resume <schedule-id>
```

**Example:**
```bash
spawn schedule resume sched-20260122-140530
```

### `spawn schedule cancel`

Permanently delete a schedule.

**Syntax:**
```bash
spawn schedule cancel <schedule-id>
```

**Warning:** This action is permanent. The schedule and its configuration will be deleted, but execution history is preserved for 30 days.

**Example:**
```bash
spawn schedule cancel sched-20260122-140530
```

## Cron Expressions

Scheduled executions use standard cron syntax with 5 fields.

### Syntax

```
┌───────────── minute (0 - 59)
│ ┌───────────── hour (0 - 23)
│ │ ┌───────────── day of month (1 - 31)
│ │ │ ┌───────────── month (1 - 12)
│ │ │ │ ┌───────────── day of week (0 - 6) (Sunday = 0)
│ │ │ │ │
* * * * *
```

### Special Characters

- `*` - Any value
- `,` - List (e.g., `1,15` = 1st and 15th)
- `-` - Range (e.g., `1-5` = Monday through Friday)
- `/` - Step values (e.g., `*/2` = every 2 units)

### Common Patterns

**Daily:**
```bash
# Every day at 2 AM
0 2 * * *

# Every day at noon
0 12 * * *

# Every day at 6 AM and 6 PM
0 6,18 * * *
```

**Hourly:**
```bash
# Every hour at minute 0
0 * * * *

# Every 2 hours
0 */2 * * *

# Every 6 hours
0 */6 * * *

# Every hour from 9 AM to 5 PM
0 9-17 * * *
```

**Weekly:**
```bash
# Every Monday at 9 AM
0 9 * * 1

# Every Friday at 5 PM
0 17 * * 5

# Weekdays at 8 AM
0 8 * * 1-5

# Weekends at 10 AM
0 10 * * 0,6
```

**Monthly:**
```bash
# First day of every month at midnight
0 0 1 * *

# Last day of every month (requires special syntax)
# Use 28-31 and check in your script

# 15th of every month at noon
0 12 15 * *

# First Monday of every month at 9 AM (complex, needs calculation)
```

**Custom:**
```bash
# Every 15 minutes
*/15 * * * *

# Every 30 minutes during business hours
*/30 9-17 * * 1-5

# Twice daily (8 AM and 8 PM)
0 8,20 * * *

# First and last hour of every day
0 0,23 * * *
```

### Testing Cron Expressions

Use online tools to test cron expressions:
- https://crontab.guru
- https://crontab.cronhub.io

## Timezone Handling

Always specify a timezone to ensure consistent execution across daylight saving time transitions.

### IANA Timezone Database

Use standard IANA timezone names:

**US Timezones:**
```bash
America/New_York      # Eastern Time
America/Chicago       # Central Time
America/Denver        # Mountain Time
America/Los_Angeles   # Pacific Time
America/Anchorage     # Alaska Time
Pacific/Honolulu      # Hawaii Time
```

**Other Common Timezones:**
```bash
UTC                   # Coordinated Universal Time
Europe/London         # British Time
Europe/Paris          # Central European Time
Asia/Tokyo            # Japan Standard Time
Australia/Sydney      # Australian Eastern Time
```

**Find your timezone:**
```bash
# Linux/macOS
timedatectl | grep "Time zone"

# Or check /usr/share/zoneinfo/
ls /usr/share/zoneinfo/America/
```

### Daylight Saving Time

EventBridge Scheduler automatically handles DST transitions:

**Example:** Schedule for 2 AM EST (America/New_York)
- During EST (winter): Executes at 2:00 AM (UTC-5)
- During EDT (summer): Executes at 2:00 AM (UTC-4)
- **No action needed** - timezone handles it automatically

**Best practice:** Always use timezone-aware scheduling rather than UTC offsets.

### UTC Scheduling

For globally distributed teams or when DST is not a concern:

```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --timezone "UTC" \
  --name "utc-schedule"
```

## Execution Limits

Control how long a recurring schedule runs.

### Maximum Executions

Limit the total number of executions:

```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --max-executions 30 \
  --name "30-day-trial"
```

**Use cases:**
- Time-limited experiments (e.g., 30 days)
- Trial periods before committing long-term
- Preventing runaway costs

**Behavior:** Schedule automatically transitions to "cancelled" status after reaching the limit.

### End Date

Stop executing after a specific date:

```bash
spawn schedule create params.yaml \
  --cron "0 */6 * * *" \
  --end-after "2026-02-01T00:00:00" \
  --timezone "America/New_York" \
  --name "january-only"
```

**Use cases:**
- Project-based schedules
- Seasonal experiments
- Budget control

**Behavior:** No executions occur after the end date. Schedule remains in "active" status but doesn't execute.

### Combining Limits

Use both limits for extra safety:

```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --max-executions 30 \
  --end-after "2026-03-01T00:00:00" \
  --timezone "America/New_York" \
  --name "controlled-experiment"
```

**Behavior:** Whichever limit is reached first stops the schedule.

### No Limits

For ongoing production schedules:

```bash
spawn schedule create params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --name "continuous-training"
```

**Warning:** Monitor costs and execution history regularly.

## Monitoring & Management

### Viewing Execution History

```bash
spawn schedule describe <schedule-id>
```

**Shows:**
- Last 10 executions
- Sweep IDs for each execution
- Success/failure status
- Execution timestamps

**Example output:**
```
Last 10 Executions:
TIME                       SWEEP ID                  STATUS
2026-01-22T02:00:00-05:00  sweep-20260122-020000     success
2026-01-21T02:00:00-05:00  sweep-20260121-020000     success
2026-01-20T02:00:00-05:00  sweep-20260120-020000     failed
```

### Tracking Sweep Results

Each execution creates a standard sweep that can be monitored:

```bash
# Get sweep ID from schedule history
spawn schedule describe sched-20260122-140530

# Monitor the sweep
spawn list --sweep-id sweep-20260122-020000

# Collect results
spawn collect-results --sweep-id sweep-20260122-020000 --output results.csv
```

### Handling Failures

If a scheduled execution fails:

1. **Check execution history:**
   ```bash
   spawn schedule describe <schedule-id>
   ```

2. **Investigate the failed sweep:**
   ```bash
   spawn list --sweep-id <failed-sweep-id>
   ```

3. **Common causes:**
   - Invalid parameter file
   - Insufficient quotas
   - Network issues
   - Lambda timeout (rare)

4. **Resolution:**
   - Fix parameter file
   - Request quota increase
   - Schedule continues automatically for next execution

### Cost Monitoring

**View schedule metadata:**
```bash
spawn schedule describe <schedule-id>
```

**Estimate monthly cost:**
```
Executions per month: 30 (daily)
Instances per execution: 10
Instance hours per execution: 2h
Instance type: c5.xlarge ($0.17/hour)

Monthly cost: 30 × 10 × 2 × $0.17 = $102
```

**Set billing alerts:**
```bash
# AWS CLI
aws budgets create-budget \
  --account-id 123456789012 \
  --budget file://budget.json
```

## Architecture

Understanding the architecture helps with troubleshooting and optimization.

### Components

```
┌─────────────┐
│  spawn CLI  │
│             │
│  schedule   │
│   create    │
└─────┬───────┘
      │
      │ 1. Upload params to S3
      │ 2. Create schedule in DynamoDB
      │ 3. Create EventBridge schedule
      │
      ▼
┌─────────────────────────────────────┐
│  AWS EventBridge Scheduler          │
│                                     │
│  Triggers at specified time/cron    │
└─────────────┬───────────────────────┘
              │
              │ 4. Invoke Lambda
              │
              ▼
┌─────────────────────────────────────┐
│  scheduler-handler Lambda           │
│                                     │
│  - Load schedule from DynamoDB      │
│  - Download params from S3          │
│  - Create sweep record              │
│  - Invoke sweep-orchestrator        │
│  - Record execution history         │
└─────────────┬───────────────────────┘
              │
              │ 5. Invoke sweep orchestrator
              │
              ▼
┌─────────────────────────────────────┐
│  sweep-orchestrator Lambda          │
│                                     │
│  - Launch EC2 instances             │
│  - Standard sweep execution         │
└─────────────────────────────────────┘
```

### Data Storage

**S3 (spawn-schedules-{region}):**
- Parameter files: `schedules/{schedule-id}/params.yaml`
- Retention: 90 days (automatic deletion)

**DynamoDB (spawn-schedules):**
- Schedule metadata
- Configuration
- Next execution time
- Retention: 90 days TTL

**DynamoDB (spawn-schedule-history):**
- Execution records
- Sweep IDs
- Success/failure status
- Retention: 30 days TTL

### Execution Flow

1. **EventBridge triggers** at scheduled time
2. **Lambda loads** schedule from DynamoDB
3. **Lambda checks** execution limits
4. **Lambda downloads** parameter file from S3
5. **Lambda creates** sweep record
6. **Lambda invokes** sweep-orchestrator asynchronously
7. **Lambda records** execution in history
8. **Lambda updates** schedule metadata (execution count, last sweep ID)
9. **For recurring:** Lambda calculates next execution time

### IAM Permissions

**EventBridge Scheduler:**
- `lambda:InvokeFunction` on scheduler-handler

**scheduler-handler Lambda:**
- `dynamodb:GetItem`, `PutItem`, `UpdateItem` on schedule tables
- `s3:GetObject` on spawn-schedules-* buckets
- `lambda:InvokeFunction` on sweep-orchestrator

## Troubleshooting

### Schedule Not Executing

**Symptom:** No sweep launches at scheduled time.

**Check:**
1. Verify schedule status:
   ```bash
   spawn schedule describe <schedule-id>
   ```

2. Ensure status is "active" (not "paused" or "cancelled")

3. Check execution limits:
   - max_executions not reached
   - end_after not passed

4. Verify timezone and time calculation:
   ```bash
   # Use online tool to verify cron expression
   # https://crontab.guru
   ```

5. Check EventBridge Scheduler:
   ```bash
   aws scheduler list-schedules --region us-east-1
   ```

**Resolution:**
- Resume if paused: `spawn schedule resume <schedule-id>`
- Recreate if cancelled
- Fix cron expression if incorrect

### Executions Failing

**Symptom:** Schedule executes but sweeps fail.

**Check:**
1. View execution history:
   ```bash
   spawn schedule describe <schedule-id>
   ```

2. Check error message in history

3. Common causes:
   - Invalid parameter file
   - Insufficient EC2 quotas
   - Invalid AMI ID
   - Network configuration issues

**Resolution:**
- Fix parameter file and let schedule retry on next execution
- Request quota increase if needed
- Update AMI ID in parameter file

### Wrong Execution Time

**Symptom:** Schedule executes at unexpected time.

**Check:**
1. Verify timezone:
   ```bash
   spawn schedule describe <schedule-id>
   ```

2. Check for DST transition

3. Verify cron expression matches intent

**Resolution:**
- Cancel and recreate with correct timezone
- Adjust cron expression if needed

### Missing Execution History

**Symptom:** Old executions not visible.

**Explanation:** Execution history has 30-day TTL and is automatically deleted.

**Resolution:** Export history if long-term retention needed:
```bash
# Custom script to export before deletion
spawn schedule describe <schedule-id> > history-backup.txt
```

### Lambda Timeout

**Symptom:** Schedule shows "failed" with timeout error.

**Rare occurrence** - scheduler-handler has 5-minute timeout.

**Check:**
- Very large parameter files (>100MB)
- DynamoDB throttling
- Network issues

**Resolution:**
- Reduce parameter file size
- Contact support if persistent

## Best Practices

### Naming Conventions

Use descriptive names that include:
- Purpose
- Frequency
- Owner/team

**Examples:**
```bash
--name "ml-team-nightly-training"
--name "weekly-monday-retraining"
--name "john-experiment-daily"
```

### Schedule Design

**Do:**
- ✅ Start with one-time schedule to test configuration
- ✅ Use timezone-aware scheduling
- ✅ Set execution limits for experiments
- ✅ Monitor execution history regularly
- ✅ Document schedule purpose and parameters

**Don't:**
- ❌ Schedule more frequently than needed (increases cost)
- ❌ Forget to set max_executions for trials
- ❌ Use UTC without considering team timezones
- ❌ Leave failed schedules running unmonitored

### Parameter File Management

**Version control:**
```bash
# Keep parameter files in git
git add params/nightly-training.yaml
git commit -m "Update learning rate for nightly runs"

# Reference specific commit
spawn schedule create params/nightly-training.yaml \
  --name "nightly-training-v2"
```

**Testing:**
```bash
# Always test manually first
spawn launch params.yaml

# Then schedule
spawn schedule create params.yaml --at "tomorrow-2pm"
```

### Cost Optimization

**Right-size execution frequency:**
```bash
# Instead of every 6 hours (4x/day = 120x/month)
--cron "0 */6 * * *"

# Consider daily (1x/day = 30x/month)
--cron "0 2 * * *"
```

**Use execution limits:**
```bash
# Limit trial to 30 days
--max-executions 30

# Or set end date
--end-after "2026-02-01T00:00:00"
```

**Monitor and pause unused schedules:**
```bash
# Pause during holidays/downtime
spawn schedule pause <schedule-id>

# Resume when needed
spawn schedule resume <schedule-id>
```

### Security

**Parameter file access:**
- Stored in S3 with encryption at rest
- Accessible only to your AWS account
- Automatically deleted after 90 days

**Credentials:**
- Never put credentials in parameter files
- Use IAM roles for EC2 instances
- Use AWS Secrets Manager for sensitive data

**Access control:**
- Use IAM policies to restrict schedule management
- Audit schedule creation with CloudTrail

### Maintenance

**Regular reviews:**
```bash
# Weekly: Check execution status
spawn schedule list --status active

# Monthly: Review and cleanup unused schedules
spawn schedule list | grep old | xargs -I {} spawn schedule cancel {}
```

**Update parameter files:**
```bash
# Can't modify existing schedule parameters
# Cancel and recreate:
spawn schedule cancel old-schedule-id
spawn schedule create new-params.yaml --cron "0 2 * * *"
```

**Monitor AWS limits:**
- EventBridge Scheduler: 1 million schedules per account
- Lambda concurrent executions: Check account limits
- EC2 instance quotas: Request increases as needed

## Examples

### Example 1: Daily Training with Fresh Data

```bash
# Schedule nightly training at 2 AM EST
# Runs for 30 days
spawn schedule create training-params.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --max-executions 30 \
  --name "daily-training-jan2026"
```

**Parameter file (training-params.yaml):**
```yaml
sweep_name: daily-training
region: us-east-1
instance_type: g5.xlarge
max_concurrent: 5

defaults:
  data_path: s3://my-bucket/latest-data/
  epochs: 100

params:
  - learning_rate: 0.001
    batch_size: 32
  - learning_rate: 0.0001
    batch_size: 32
```

### Example 2: Weekly Model Retraining

```bash
# Every Monday at 9 AM PST
spawn schedule create weekly-retrain.yaml \
  --cron "0 9 * * 1" \
  --timezone "America/Los_Angeles" \
  --name "weekly-model-update"
```

### Example 3: Hourly Monitoring During Business Hours

```bash
# Every hour from 9 AM to 5 PM on weekdays
spawn schedule create monitoring.yaml \
  --cron "0 9-17 * * 1-5" \
  --timezone "America/New_York" \
  --name "business-hours-monitoring"
```

### Example 4: One-Time Large Experiment

```bash
# Schedule for weekend when resources are cheaper
spawn schedule create large-experiment.yaml \
  --at "2026-01-25T02:00:00" \
  --timezone "UTC" \
  --name "weekend-large-sweep"
```

## FAQ

**Q: Can I modify a schedule after creation?**

A: No, schedules are immutable. Cancel and recreate with new parameters.

**Q: What happens if an execution fails?**

A: The schedule continues to run on schedule. Check execution history and fix parameter file for future runs.

**Q: Can I schedule batch queues?**

A: Not directly. Scheduled sweeps launch multiple independent instances. For sequential jobs, use batch queues separately.

**Q: How much does scheduling cost?**

A: EventBridge Scheduler is free for the first 14 million invocations per month, then $1.00 per million. For typical usage (daily schedules), cost is negligible.

**Q: What's the maximum schedule duration?**

A: No hard limit, but set execution limits to control costs and avoid runaway schedules.

**Q: Can I share schedules between AWS accounts?**

A: No, schedules are account-specific. Recreate in each account as needed.

**Q: How do I back up schedule configurations?**

A: Export schedule details to version control:
```bash
spawn schedule describe <schedule-id> > schedule-backup.json
```

## See Also

- [Parameter Sweep Guide](PARAMETER_SWEEP_GUIDE.md)
- [Batch Queue Guide](BATCH_QUEUE_GUIDE.md)
- [Examples](examples/README.md)
- [Troubleshooting](TROUBLESHOOTING.md)
