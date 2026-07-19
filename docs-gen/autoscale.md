## `spawn autoscale`

Launch and manage auto-scaling job arrays that maintain target capacity

```
spawn autoscale
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--env` |  | string | `production` | Environment (production or staging) |
| `--table` |  | string | `spawn-autoscale-groups` | DynamoDB table name |

### `spawn autoscale health`

Show instance health for auto-scaling group

```
spawn autoscale health <group-name>
```

### `spawn autoscale launch`

Launch a new auto-scaling job array with specified capacity and launch template

```
spawn autoscale launch [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--ami` |  | string |  | AMI ID (required) |
| `--desired-capacity` |  | int |  | Desired instance count (required) |
| `--iam-profile` |  | string |  | IAM instance profile |
| `--instance-type` |  | string |  | EC2 instance type (required) |
| `--job-array-id` |  | string |  | Job array ID (auto-generated if not specified) |
| `--key-name` |  | string |  | SSH key name |
| `--max-capacity` |  | int |  | Maximum instance count (default: desired * 2) |
| `--metric-name` |  | string |  | CloudWatch metric name (for custom metrics) |
| `--metric-namespace` |  | string |  | CloudWatch namespace (for custom metrics) |
| `--metric-period` |  | int | `300` | Metric evaluation period in seconds |
| `--metric-policy` |  | string |  | Metric policy type: 'cpu', 'memory', or 'custom' |
| `--metric-statistic` |  | string | `Average` | Metric statistic: Average, Maximum, or Minimum |
| `--min-capacity` |  | int |  | Minimum instance count (default: 0) |
| `--name` |  | string |  | Group name (required) |
| `--queue-url` |  | string |  | SQS queue URL for queue-depth policy (required if --scaling-policy=queue-depth) |
| `--scale-down-cooldown` |  | int | `300` | Scale-down cooldown in seconds |
| `--scale-up-cooldown` |  | int | `60` | Scale-up cooldown in seconds |
| `--scaling-policy` |  | string |  | Scaling policy type: 'queue-depth' (empty = manual mode) |
| `--security-group-ids` |  | stringSlice |  | Security group IDs (comma-separated or repeated) |
| `--spot` |  | bool |  | Use spot instances |
| `--subnet-id` |  | string |  | Subnet ID |
| `--tag` |  | stringArray |  | Additional tag key=value (repeatable) |
| `--target-messages-per-instance` |  | int | `10` | Target messages per instance for queue-depth scaling |
| `--target-value` |  | float64 |  | Target metric value (e.g., 70.0 for 70% CPU) |
| `--user-data` |  | string |  | User data script (base64 encoded) |

### `spawn autoscale list`

*Aliases: ls*

List all active auto-scaling groups

```
spawn autoscale list
```

### `spawn autoscale metric-activity`

Show recent metric-based scaling activity

```
spawn autoscale metric-activity <group-name>
```

### `spawn autoscale pause`

Pause auto-scaling (stop reconciliation)

```
spawn autoscale pause <group-name>
```

### `spawn autoscale resume`

Resume auto-scaling

```
spawn autoscale resume <group-name>
```

### `spawn autoscale scaling-activity`

Show recent scaling activity for an autoscale group

```
spawn autoscale scaling-activity <group-name>
```

### `spawn autoscale schedule`

Manage scheduled scaling actions for an autoscale group

```
spawn autoscale schedule
```

#### `spawn autoscale schedule add`

Add a scheduled action to an autoscale group

```
spawn autoscale schedule add <group-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--desired-capacity` |  | int |  | Desired capacity (required) |
| `--enabled` |  | bool | `true` | Enable the schedule immediately |
| `--max-capacity` |  | int |  | Maximum capacity override (optional) |
| `--min-capacity` |  | int |  | Minimum capacity override (optional) |
| `--name` |  | string |  | Schedule name (required) |
| `--schedule` |  | string |  | Cron expression: 'second minute hour day month weekday' (required) |
| `--timezone` |  | string | `UTC` | Timezone (e.g., America/New_York) |

#### `spawn autoscale schedule list`

List all scheduled actions for an autoscale group

```
spawn autoscale schedule list <group-name>
```

#### `spawn autoscale schedule remove`

Remove a scheduled action from an autoscale group

```
spawn autoscale schedule remove <group-name> <schedule-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn autoscale set-metric-policy`

Set or update metric-based scaling policy

```
spawn autoscale set-metric-policy <group-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--metric-name` |  | string |  | CloudWatch metric name (for custom metrics) |
| `--metric-namespace` |  | string |  | CloudWatch namespace (for custom metrics) |
| `--metric-period` |  | int | `300` | Metric evaluation period in seconds |
| `--metric-policy` |  | string |  | Metric policy type: 'cpu', 'memory', or 'custom' |
| `--metric-statistic` |  | string | `Average` | Metric statistic: Average, Maximum, or Minimum |
| `--none` |  | bool |  | Remove metric policy |
| `--target-value` |  | float64 |  | Target metric value (e.g., 70.0 for 70% CPU) |

### `spawn autoscale set-scaling-policy`

*Aliases: set-policy*

Set or update scaling policy for an autoscale group

```
spawn autoscale set-scaling-policy <group-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--none` |  | bool |  | Remove scaling policy (revert to manual mode) |
| `--queue-url` |  | string |  | SQS queue URL for single-queue policy (deprecated: use --queue) |
| `--queue-weight` |  | float64Slice |  | Queue weight 0.0-1.0 (must match number of --queue flags) |
| `--queue` |  | stringSlice |  | SQS queue URL (can be specified multiple times for multi-queue) |
| `--scale-down-cooldown` |  | int | `300` | Scale-down cooldown in seconds |
| `--scale-up-cooldown` |  | int | `60` | Scale-up cooldown in seconds |
| `--scaling-policy` |  | string |  | Scaling policy type: 'queue-depth' |
| `--target-messages-per-instance` |  | int | `10` | Target messages per instance for queue-depth scaling |

### `spawn autoscale status`

Show auto-scaling group status

```
spawn autoscale status [group-name]
```

### `spawn autoscale terminate`

Terminate auto-scaling group and all instances

```
spawn autoscale terminate <group-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn autoscale update`

Update auto-scaling group capacity

```
spawn autoscale update <group-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--desired-capacity` |  | int | `-1` | New desired capacity |
| `--max-capacity` |  | int | `-1` | New maximum capacity |
| `--min-capacity` |  | int | `-1` | New minimum capacity |

