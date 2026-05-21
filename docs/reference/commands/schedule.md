# spawn schedule

Manage scheduled parameter sweeps via AWS EventBridge Scheduler.

## Synopsis

```bash
spawn schedule create <param-file> [flags]
spawn schedule list [flags]
spawn schedule describe <schedule-id> [flags]
spawn schedule pause <schedule-id> [flags]
spawn schedule resume <schedule-id> [flags]
spawn schedule cancel <schedule-id> [flags]
```

## Description

Schedule parameter sweeps for one-time or recurring execution using AWS EventBridge Scheduler. Schedules run automatically without requiring the CLI to be running - EventBridge triggers Lambda to launch sweeps at specified times.

**Key Features:**
- One-time execution (specific date/time)
- Recurring execution (cron expressions)
- Timezone support
- Execution limits (max count or end date)
- Full execution history
- Parameter file uploaded to S3 once (reused for each execution)

## Subcommands

### create
Create a new schedule.

### list
List all schedules.

### describe
Show schedule details and execution history.

### pause
Pause a schedule (no new executions).

### resume
Resume a paused schedule.

### cancel
Delete a schedule (prevents future executions).

## create - Create Schedule

### Synopsis
```bash
spawn schedule create <param-file> [flags]
```

### Arguments

#### param-file
**Type:** Path (YAML/JSON file)
**Required:** Yes
**Description:** Parameter file to use for scheduled sweeps.

```bash
spawn schedule create sweep.yaml --at "2026-01-28T02:00:00"
```

### Flags

#### Scheduling

##### --at
**Type:** String (ISO 8601 datetime)
**Required:** One-time schedules
**Description:** Specific date and time for one-time execution.

```bash
# One-time schedule
spawn schedule create sweep.yaml --at "2026-01-28T02:00:00"

# With timezone
spawn schedule create sweep.yaml \
  --at "2026-01-28T02:00:00" \
  --timezone "America/New_York"
```

**Format:** ISO 8601: `YYYY-MM-DDTHH:MM:SS`

##### --cron
**Type:** String (cron expression)
**Required:** Recurring schedules
**Description:** Cron expression for recurring execution.

```bash
# Daily at 2 AM
spawn schedule create sweep.yaml --cron "0 2 * * *"

# Every 6 hours
spawn schedule create sweep.yaml --cron "0 */6 * * *"

# Weekdays at 9 AM
spawn schedule create sweep.yaml --cron "0 9 * * 1-5"
```

**Format:** Standard 5-field cron:
```
┌───────────── minute (0 - 59)
│ ┌───────────── hour (0 - 23)
│ │ ┌───────────── day of month (1 - 31)
│ │ │ ┌───────────── month (1 - 12)
│ │ │ │ ┌───────────── day of week (0 - 6) (Sunday to Saturday)
│ │ │ │ │
* * * * *
```

**Common Patterns:**
```
0 2 * * *       # Daily at 2 AM
0 */6 * * *     # Every 6 hours
0 0 * * 0       # Weekly on Sunday at midnight
0 9 * * 1-5     # Weekdays at 9 AM
*/30 * * * *    # Every 30 minutes
0 0 1 * *       # First day of month at midnight
```

##### --timezone
**Type:** String (IANA timezone)
**Default:** `UTC`
**Description:** Timezone for schedule interpretation.

```bash
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York"

# PST (Pacific Standard Time)
spawn schedule create sweep.yaml \
  --at "2026-01-28T02:00:00" \
  --timezone "America/Los_Angeles"
```

**Common Timezones:**
- `America/New_York` - Eastern (EST/EDT)
- `America/Chicago` - Central (CST/CDT)
- `America/Denver` - Mountain (MST/MDT)
- `America/Los_Angeles` - Pacific (PST/PDT)
- `Europe/London` - GMT/BST
- `Asia/Tokyo` - JST
- `UTC` - Coordinated Universal Time

#### Execution Limits

##### --max-executions
**Type:** Integer
**Default:** Unlimited
**Description:** Maximum number of executions before stopping.

```bash
# Run 30 times then stop
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --max-executions 30
```

##### --end-after
**Type:** String (ISO 8601 datetime)
**Default:** None
**Description:** Stop scheduling after this date/time.

```bash
# Schedule until March 1st
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --end-after "2026-03-01T00:00:00Z"
```

#### Configuration

##### --name
**Type:** String
**Default:** Auto-generated
**Description:** Human-readable schedule name.

```bash
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --name "nightly-training"
```

##### --description
**Type:** String
**Default:** None
**Description:** Schedule description.

```bash
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --name "nightly-training" \
  --description "Daily hyperparameter sweep for model training"
```

### Output

```
Schedule created successfully

Schedule ID: schedule-20260127-abc123
Name: nightly-training
Type: recurring
Schedule: 0 2 * * * (daily at 2 AM)
Timezone: America/New_York

Next Execution: 2026-01-28 02:00:00 EST (in 14h 32m)
Max Executions: 30
Ends After: 2026-02-26 02:00:00 EST

Parameter File: s3://spawn-sweeps-us-east-1/schedule-20260127-abc123/params.yaml

Monitor: spawn schedule describe schedule-20260127-abc123
Cancel: spawn schedule cancel schedule-20260127-abc123
```

## list - List Schedules

### Synopsis
```bash
spawn schedule list [flags]
```

### Flags

#### --status
**Type:** String
**Allowed Values:** `active`, `paused`, `completed`, `cancelled`, `all`
**Default:** `active`
**Description:** Filter by schedule status.

```bash
spawn schedule list --status active
spawn schedule list --status paused
spawn schedule list --status all
```

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn schedule list --json
```

### Output

```
+------------------------------+------------------+----------+-----------+----------------+
| Schedule ID                  | Name             | Type     | Schedule  | Next Execution |
+------------------------------+------------------+----------+-----------+----------------+
| schedule-20260127-abc123     | nightly-training | recurring| 0 2 * * * | 14h 32m        |
| schedule-20260127-def456     | weekly-report    | recurring| 0 0 * * 0 | 3d 2h          |
| schedule-20260128-ghi789     | one-time-test    | one-time | -         | 22h 15m        |
+------------------------------+------------------+----------+-----------+----------------+

Total: 3 schedules (active)
```

## describe - Describe Schedule

### Synopsis
```bash
spawn schedule describe <schedule-id> [flags]
```

### Arguments

#### schedule-id
**Type:** String
**Required:** Yes
**Description:** Schedule ID to describe.

```bash
spawn schedule describe schedule-20260127-abc123
```

### Output

```
Schedule: schedule-20260127-abc123
Name: nightly-training
Status: active
Created: 2026-01-27 14:00:00 PST

Type: recurring
Schedule: 0 2 * * * (daily at 2 AM EST)
Timezone: America/New_York
Next Execution: 2026-01-28 02:00:00 EST (in 14h 32m)

Execution Limits:
  Max Executions: 30
  Executions Run: 5
  Remaining: 25
  Ends After: 2026-02-26 02:00:00 EST

Parameter File:
  Location: s3://spawn-sweeps-us-east-1/schedule-20260127-abc123/params.yaml
  Size: 4.2 KB
  Parameters: 50

Execution History (last 5):
  2026-01-27 02:00:00  sweep-20260127-xyz789  completed  50/50  $12.45
  2026-01-26 02:00:00  sweep-20260126-xyz788  completed  50/50  $11.89
  2026-01-25 02:00:00  sweep-20260125-xyz787  completed  50/50  $12.12
  2026-01-24 02:00:00  sweep-20260124-xyz786  failed     25/50  $6.23
  2026-01-23 02:00:00  sweep-20260123-xyz785  completed  50/50  $11.76

Statistics:
  Total Cost: $54.45
  Average Cost per Execution: $10.89
  Success Rate: 80% (4/5)
```

## pause - Pause Schedule

### Synopsis
```bash
spawn schedule pause <schedule-id> [flags]
```

### Arguments

#### schedule-id
**Type:** String
**Required:** Yes
**Description:** Schedule ID to pause.

```bash
spawn schedule pause schedule-20260127-abc123
```

### Output

```
Schedule paused successfully

Schedule: schedule-20260127-abc123 (nightly-training)
Status: active → paused

No new executions will run until resumed.
Resume: spawn schedule resume schedule-20260127-abc123
```

## resume - Resume Schedule

### Synopsis
```bash
spawn schedule resume <schedule-id> [flags]
```

### Arguments

#### schedule-id
**Type:** String
**Required:** Yes
**Description:** Schedule ID to resume.

```bash
spawn schedule resume schedule-20260127-abc123
```

### Output

```
Schedule resumed successfully

Schedule: schedule-20260127-abc123 (nightly-training)
Status: paused → active

Next Execution: 2026-01-28 02:00:00 EST (in 14h 32m)
```

## cancel - Cancel Schedule

### Synopsis
```bash
spawn schedule cancel <schedule-id> [flags]
```

### Arguments

#### schedule-id
**Type:** String
**Required:** Yes
**Description:** Schedule ID to cancel (delete).

```bash
spawn schedule cancel schedule-20260127-abc123
```

### Flags

#### --force
**Type:** Boolean
**Default:** `false`
**Description:** Skip confirmation prompt.

```bash
spawn schedule cancel schedule-20260127-abc123 --force
```

#### --delete-history
**Type:** Boolean
**Default:** `false`
**Description:** Delete execution history (keeps by default).

```bash
spawn schedule cancel schedule-20260127-abc123 --delete-history
```

### Output

```
⚠️  Confirm cancellation

Schedule: schedule-20260127-abc123 (nightly-training)
Executions Run: 5
Remaining: 25
Next Execution: 2026-01-28 02:00:00 EST

This will:
  • Delete the schedule (no future executions)
  • Keep execution history (use --delete-history to remove)
  • Keep parameter file in S3 (manual cleanup required)

Confirm cancellation? [y/N]: y

Schedule cancelled successfully
```

## Examples

### One-Time Schedule
```bash
# Schedule for tomorrow at 2 AM PST
spawn schedule create sweep.yaml \
  --at "2026-01-28T02:00:00" \
  --timezone "America/Los_Angeles" \
  --name "one-time-test"
```

### Daily Recurring Schedule
```bash
# Daily at 2 AM EST, run 30 times
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York" \
  --max-executions 30 \
  --name "nightly-training"
```

### Weekly Schedule
```bash
# Every Sunday at midnight
spawn schedule create sweep.yaml \
  --cron "0 0 * * 0" \
  --name "weekly-report"
```

### Hourly Schedule with End Date
```bash
# Every 6 hours until March 1st
spawn schedule create sweep.yaml \
  --cron "0 */6 * * *" \
  --end-after "2026-03-01T00:00:00Z" \
  --name "frequent-sampling"
```

### List and Monitor
```bash
# List all active schedules
spawn schedule list

# List all schedules (including paused)
spawn schedule list --status all

# Watch schedule execution
spawn schedule describe schedule-123

# Monitor in loop
while true; do
    clear
    spawn schedule describe schedule-123
    sleep 60
done
```

### Pause and Resume
```bash
# Pause schedule temporarily
spawn schedule pause schedule-123

# Resume later
spawn schedule resume schedule-123
```

## Cron Expression Examples

```bash
# Every minute
--cron "* * * * *"

# Every 5 minutes
--cron "*/5 * * * *"

# Every hour
--cron "0 * * * *"

# Every day at 2 AM
--cron "0 2 * * *"

# Every weekday at 9 AM
--cron "0 9 * * 1-5"

# Every Monday at 9 AM
--cron "0 9 * * 1"

# First day of every month at midnight
--cron "0 0 1 * *"

# Every quarter (Jan, Apr, Jul, Oct)
--cron "0 0 1 1,4,7,10 *"

# Twice daily (9 AM and 5 PM)
--cron "0 9,17 * * *"

# Every 4 hours
--cron "0 */4 * * *"
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | Operation failed (EventBridge error, S3 upload failed) |
| 2 | Invalid schedule expression (malformed cron or datetime) |
| 3 | Schedule not found (schedule ID doesn't exist) |

## Troubleshooting

### "Invalid cron expression"
```bash
# Error: Invalid cron
spawn schedule create sweep.yaml --cron "invalid"

# Solution: Use 5-field cron
spawn schedule create sweep.yaml --cron "0 2 * * *"  # Correct

# Test cron: https://crontab.guru/
```

### "Invalid timezone"
```bash
# Error: Unknown timezone
spawn schedule create sweep.yaml --timezone "PST"

# Solution: Use IANA timezone names
spawn schedule create sweep.yaml --timezone "America/Los_Angeles"  # Correct
```

### Schedule Not Executing
```bash
# Check schedule status
spawn schedule describe schedule-123

# Verify schedule is active (not paused)
# Check next execution time
# Verify EventBridge rule exists:
aws events list-rules --name-prefix spawn-schedule-
```

### Execution Failed
```bash
# Check execution history
spawn schedule describe schedule-123

# Check Lambda logs
aws logs tail /aws/lambda/spawn-sweep-orchestrator --follow

# Check parameter file
aws s3 cp s3://spawn-sweeps-us-east-1/schedule-123/params.yaml -
```

## Cost Implications

- **EventBridge:** $1 per million rule invocations (~$0.03/month for hourly schedule)
- **Lambda:** $0.20 per million requests (~$0.001 per sweep trigger)
- **S3:** $0.023/GB/month for parameter file storage (typically < $0.01)
- **Sweep Costs:** Standard EC2 instance costs for each execution

**Total Overhead:** < $0.10/month for typical schedules

## Best Practices

### 1. Use Timezones Explicitly
```bash
# Good - explicit timezone
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --timezone "America/New_York"

# Bad - relies on UTC default
spawn schedule create sweep.yaml --cron "0 2 * * *"
```

### 2. Set Execution Limits
```bash
# Good - prevents runaway schedules
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --max-executions 30

# Or use end date
spawn schedule create sweep.yaml \
  --cron "0 2 * * *" \
  --end-after "2026-03-01T00:00:00Z"
```

### 3. Monitor Execution History
```bash
# Regular monitoring
spawn schedule describe schedule-123

# Alert on failures
spawn alerts create schedule-123 --on-failure --slack https://...
```

## See Also
- [spawn launch](launch.md) - Launch parameter sweeps
- [spawn status](status.md) - Check sweep status
- [spawn alerts](alerts.md) - Configure notifications
- [spawn cancel](cancel.md) - Cancel sweeps
- [Parameter Files](../parameter-files.md) - Parameter file format
