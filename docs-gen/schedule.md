## `spawn schedule`

Create, list, and manage scheduled executions of parameter sweeps.

Schedules run parameter sweeps at specified times via EventBridge Scheduler.
No CLI running required - sweeps launch automatically at scheduled times.

Examples:
  # One-time execution
  spawn schedule create params.yaml --at "2026-01-23T02:00:00" --timezone "America/New_York"

  # Recurring daily at 2 AM
  spawn schedule create params.yaml --cron "0 2 * * *" --name "nightly-training"

  # Recurring with execution limit
  spawn schedule create params.yaml --cron "0 */4 * * *" --max-executions 100

  # List all schedules
  spawn schedule list

  # Cancel a schedule
  spawn schedule cancel <schedule-id>

```
spawn schedule
```

### `spawn schedule cancel`

Cancel a scheduled execution. This will delete the EventBridge schedule
and update the DynamoDB record. No further executions will occur.

Examples:
  spawn schedule cancel sched-20260122-140530

```
spawn schedule cancel <schedule-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn schedule create`

Create a new scheduled execution of a parameter sweep.

Either --at (one-time) or --cron (recurring) is required.

Time formats:
  --at:       ISO 8601 format (2026-01-23T02:00:00)
  --cron:     Standard cron expression (minute hour day month weekday)

Examples:
  # One-time at specific time
  spawn schedule create params.yaml --at "2026-01-23T14:30:00"

  # Every day at 2 AM Eastern
  spawn schedule create params.yaml --cron "0 2 * * *" --timezone "America/New_York"

  # Every 6 hours for 30 days
  spawn schedule create params.yaml --cron "0 */6 * * *" --max-executions 120

  # Weekdays only at 9 AM
  spawn schedule create params.yaml --cron "0 9 * * 1-5"

```
spawn schedule create <params-file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--at` |  | string |  | One-time execution time (ISO 8601 format) |
| `--cron` |  | string |  | Cron expression for recurring execution |
| `--end-after` |  | string |  | Stop executing after this time (ISO 8601 format) |
| `--max-executions` |  | int |  | Maximum number of executions (0 = unlimited) |
| `--name` |  | string |  | Friendly name for this schedule |
| `--region` |  | string | `us-east-1` | AWS region for sweep execution |
| `--timezone` |  | string | `UTC` | IANA timezone (e.g., America/New_York) |

### `spawn schedule list`

List all scheduled executions for the current user.

Examples:
  # List all schedules
  spawn schedule list

  # List only active schedules
  spawn schedule list --status active

```
spawn schedule list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--status` |  | string |  | Filter by status (active\|paused\|cancelled) |

### `spawn schedule pause`

Pause a scheduled execution temporarily. The schedule remains but
executions are disabled. Use 'resume' to re-enable.

Examples:
  spawn schedule pause sched-20260122-140530

```
spawn schedule pause <schedule-id>
```

### `spawn schedule resume`

Resume a paused scheduled execution. Executions will continue
according to the schedule.

Examples:
  spawn schedule resume sched-20260122-140530

```
spawn schedule resume <schedule-id>
```

### `spawn schedule show`

*Aliases: describe*

Show detailed information about a scheduled execution including
configuration, execution history, and next run time.

Examples:
  spawn schedule show sched-20260122-140530

```
spawn schedule show <schedule-id>
```

