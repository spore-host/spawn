# spawn list

List spawn-managed EC2 instances across all regions with powerful filtering.

## Synopsis

```bash
spawn list [flags]
```

## Description

List all EC2 instances tagged with `spawn:managed=true` across all AWS regions. Supports filtering by region, state, instance type/family, and custom tags.

## Basic Usage

```bash
# List all spawn-managed instances
spawn list

# List in specific region
spawn list --region us-east-1

# List running instances only
spawn list --state running

# List specific instance family
spawn list --instance-family m7i
```

## Flags

### Filtering

#### --region
**Type:** String
**Default:** All regions
**Description:** Filter by AWS region.

```bash
spawn list --region us-west-2
```

#### --az
**Type:** String
**Default:** None
**Description:** Filter by availability zone.

```bash
spawn list --az us-east-1a
```

#### --state
**Type:** String
**Allowed Values:** `running`, `stopped`, `stopping`, `terminated`, `terminating`, `pending`
**Default:** All states
**Description:** Filter by instance state.

```bash
spawn list --state running
spawn list --state stopped
```

#### --instance-type
**Type:** String
**Default:** None
**Description:** Filter by exact instance type.

```bash
spawn list --instance-type m7i.large
```

#### --instance-family
**Type:** String
**Default:** None
**Description:** Filter by instance family (all sizes in family).

```bash
spawn list --instance-family m7i
# Matches: m7i.large, m7i.xlarge, m7i.2xlarge, etc.
```

#### --tag
**Type:** String (key=value)
**Default:** None
**Description:** Filter by tag. Repeatable.

```bash
spawn list --tag env=prod
spawn list --tag env=prod --tag team=ml
# AND logic: both tags must match
```

#### --job-array-id
**Type:** String
**Default:** None
**Description:** Filter by job array ID.

```bash
spawn list --job-array-id abc123
```

#### --job-array-name
**Type:** String
**Default:** None
**Description:** Filter by job array name.

```bash
spawn list --job-array-name training-run
```

#### --sweep-id
**Type:** String
**Default:** None
**Description:** Filter by parameter sweep ID.

```bash
spawn list --sweep-id sweep-abc123
```

#### --sweep-name
**Type:** String
**Default:** None
**Description:** Filter by parameter sweep name.

```bash
spawn list --sweep-name hyperparameter-search
```

### Output Format

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output as JSON array.

```bash
spawn list --json
spawn list --state running --json
```

#### --quiet, -q
**Type:** Boolean
**Default:** `false`
**Description:** Output instance IDs only (one per line).

```bash
# Get all instance IDs
INSTANCES=$(spawn list --quiet)

# Get first running instance
INSTANCE=$(spawn list --state running --quiet | head -1)
```

## Output

### Table Format (Default)

```
Finding spawn-managed instances in all regions...

+------------------+------------+---------+----------------+-----------+-------+
| Instance ID      | Region     | State   | Public IP      | Type      | Age   |
+------------------+------------+---------+----------------+-----------+-------+
| i-0123456789abc  | us-east-1  | running | 54.123.45.67   | m7i.large | 2h30m |
| i-0987654321def  | us-west-2  | stopped | -              | t3.medium | 5d6h  |
+------------------+------------+---------+----------------+-----------+-------+

Total: 2 instances
```

### JSON Format

```json
[
  {
    "instance_id": "i-0123456789abcdef0",
    "name": "my-instance",
    "region": "us-east-1",
    "availability_zone": "us-east-1a",
    "state": "running",
    "instance_type": "m7i.large",
    "public_ip": "54.123.45.67",
    "private_ip": "10.0.1.100",
    "launch_time": "2026-01-27T10:00:00Z",
    "age": "2h30m",
    "tags": {
      "Name": "my-instance",
      "spawn:managed": "true",
      "spawn:ttl": "8h",
      "env": "prod"
    }
  }
]
```

### Quiet Format

```
i-0123456789abcdef0
i-0987654321fedcba
i-abcdef0123456789
```

## Examples

### List All Instances
```bash
spawn list
```

### List Running Instances in Specific Region
```bash
spawn list --region us-east-1 --state running
```

### List Specific Instance Family
```bash
spawn list --instance-family m7i
```

### List with Multiple Filters
```bash
spawn list \
  --region us-east-1 \
  --state running \
  --instance-family m7i \
  --tag env=prod
# Shows only running m7i instances in us-east-1 tagged env=prod
```

### JSON Output for Automation
```bash
spawn list --json | jq '.[] | select(.State == "running")'

# Get IPs of all running instances
spawn list --state running --json | jq -r '.[].public_ip'

# Count instances by region
spawn list --json | jq -r '.[].region' | sort | uniq -c
```

### Get First Running Instance ID
```bash
INSTANCE=$(spawn list --state running --quiet | head -1)
echo "Connecting to $INSTANCE"
spawn connect "$INSTANCE"
```

### List Specific Job Array
```bash
spawn list --job-array-name training-sweep
```

### List Specific Parameter Sweep
```bash
spawn list --sweep-name hyperparameter-search
```

### List Instances Older Than 1 Day
```bash
# JSON output with jq filtering
spawn list --json | \
  jq '.[] | select(.age | contains("d"))'
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | List completed successfully (even if no instances found) |
| 1 | API error (AWS API failure, network error) |
| 2 | Invalid filter (invalid state, malformed tag) |

## Notes

- Searches **all regions** by default unless `--region` specified
- Only lists instances with `spawn:managed=true` tag
- Age format: `2h30m` (2 hours 30 minutes), `5d6h` (5 days 6 hours)
- Multiple filters use **AND** logic (all must match)
- Empty results (no instances) exits with code 0

## Performance

- Parallel region scanning (fast even with many regions)
- Results cached for 30 seconds (repeated calls are instant)
- `--region` flag significantly faster (single region lookup)

## See Also
- [spawn launch](launch.md) - Launch instances
- [spawn status](status.md) - Check instance status
- [spawn connect](connect.md) - Connect to instance
- [spawn extend](extend.md) - Extend instance TTL
