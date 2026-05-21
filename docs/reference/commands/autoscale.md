# spawn autoscale

Launch and manage auto-scaling job arrays that maintain a target instance count.

## Synopsis

```bash
spawn autoscale <subcommand> [flags]
```

## Description

Auto-scaling job arrays automatically maintain a desired number of running instances. The reconciliation loop detects terminated or failed instances and launches replacements. Scaling can be driven by:

- **Manual capacity updates** — set desired/min/max directly
- **Queue-based scaling** — scale based on SQS message depth
- **Metric-based scaling** — scale based on CloudWatch metrics
- **Scheduled actions** — time-based capacity changes

## Global Flags (all subcommands)

| Flag | Default | Description |
|------|---------|-------------|
| `--table` | `spawn-autoscale-groups` | DynamoDB table name |
| `--env` | `production` | Environment (`production` or `staging`) |

## Subcommands

| Subcommand | Description |
|------------|-------------|
| `launch` | Launch a new auto-scaling group |
| `update <group-name>` | Update capacity |
| `status [group-name]` | Show group status |
| `health <group-name>` | Show per-instance health |
| `pause <group-name>` | Pause reconciliation |
| `resume <group-name>` | Resume reconciliation |
| `terminate <group-name>` | Terminate group and all instances |
| `set-policy <group-name>` | Set scaling policy |
| `scaling-activity <group-name>` | Show recent scaling events |
| `set-metric-policy <group-name>` | Set metric-based scaling policy |
| `metric-activity <group-name>` | Show recent metric-based scaling events |
| `add-schedule <group-name>` | Add a scheduled action |
| `remove-schedule <group-name> <schedule-name>` | Remove a scheduled action |
| `list-schedules <group-name>` | List scheduled actions |

## launch

Create a new auto-scaling group.

```bash
spawn autoscale launch \
  --name ml-workers \
  --instance-type c5.2xlarge \
  --ami ami-abc123 \
  --desired-capacity 5 \
  --min-capacity 2 \
  --max-capacity 10
```

**Required flags:** `--name`, `--instance-type`, `--ami`, `--desired-capacity`

**Optional flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--job-array-id` | Auto-generated | Job array ID |
| `--min-capacity` | `0` | Minimum instance count |
| `--max-capacity` | `desired * 2` | Maximum instance count |
| `--spot` | `false` | Use Spot instances |
| `--key-name` | None | SSH key pair |
| `--subnet-id` | None | Subnet ID |
| `--security-groups` | None | Security group IDs |
| `--iam-profile` | None | IAM instance profile |
| `--user-data` | None | User data (base64) |
| `--tags` | None | Additional tags (`key=value`) |
| `--scaling-policy` | None | `queue` or `metric` |
| `--queue-url` | None | SQS queue URL (for queue-based scaling) |
| `--target-messages-per-instance` | `10` | SQS messages per instance target |
| `--scale-up-cooldown` | `60` | Scale-up cooldown (seconds) |
| `--scale-down-cooldown` | `300` | Scale-down cooldown (seconds) |

## update

Change the desired capacity of a running group.

```bash
spawn autoscale update ml-workers --desired-capacity 8
```

## status / health

```bash
spawn autoscale status              # All groups
spawn autoscale status ml-workers   # Specific group
spawn autoscale health ml-workers   # Per-instance health
```

## Scaling Policies

### Queue-based (SQS)

```bash
spawn autoscale set-policy ml-workers \
  --scaling-policy queue \
  --queue-url https://sqs.us-east-1.amazonaws.com/123456789/my-queue \
  --target-messages-per-instance 5
```

### Metric-based (CloudWatch)

```bash
spawn autoscale set-metric-policy ml-workers \
  --metric-policy target-tracking \
  --metric-name CPUUtilization \
  --metric-namespace AWS/EC2 \
  --target-value 70 \
  --metric-period 300
```

## Scheduled Actions

```bash
# Scale up at 8am weekdays
spawn autoscale add-schedule ml-workers \
  --schedule-name morning-scale-up \
  --cron "0 8 * * 1-5" \
  --desired-capacity 10

# List schedules
spawn autoscale list-schedules ml-workers

# Remove schedule
spawn autoscale remove-schedule ml-workers morning-scale-up
```

## pause / resume / terminate

```bash
spawn autoscale pause ml-workers     # Stop reconciliation (instances keep running)
spawn autoscale resume ml-workers    # Resume reconciliation
spawn autoscale terminate ml-workers # Terminate all instances and delete group
```

## See Also

- [spawn launch](launch.md) — Launch a fixed set of instances
- [spawn list](list.md) — List instances (includes autoscale group members)
