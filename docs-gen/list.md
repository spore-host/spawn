## `spawn list`

*Aliases: ls*

List spawn-managed EC2 instances across regions.

Shows all instances with the spawn:managed tag.

Examples:
  # List all instances
  spawn list

  # Filter by region
  spawn list --region us-east-1

  # Filter by state
  spawn list --state running

  # JSON output
  spawn list --output json

```
spawn list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--az` |  | string |  | Filter by availability zone |
| `--instance-family` |  | string |  | Filter by instance family (e.g., m7i, t3) |
| `--instance-type` |  | string |  | Filter by exact instance type (e.g., t3.micro) |
| `--job-array-id` |  | string |  | Filter by job array ID |
| `--job-array-name` |  | string |  | Filter by job array name |
| `--region` |  | string |  | Filter by AWS region (default: all regions) |
| `--regions` | `-r` | stringSlice |  | Filter by regions (comma-separated, e.g. us-east-1,us-west-2) |
| `--state` |  | string |  | Filter by instance state (running, stopped, etc.) |
| `--sweep-id` |  | string |  | Filter by parameter sweep ID |
| `--sweep-name` |  | string |  | Filter by parameter sweep name |
| `--tag` |  | stringArray |  | Filter by tag (key=value format, can be specified multiple times) |

