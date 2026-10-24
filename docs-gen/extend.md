## `spawn extend`

Extend the TTL (time-to-live) for a spawn-managed instance.

Prevents automatic termination by extending the TTL duration.

Duration format: 1h, 2h30m, 24h, etc.

Examples:
  # Extend by 2 hours
  spawn extend i-1234567890abcdef0 2h

  # Extend by name
  spawn extend my-instance 8h

```
spawn extend <instance-id-or-name> <duration> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--cost-limit` |  | float64 |  | Set the instance's cost limit to this amount in USD. Without it (or --raise-cost-limit) the existing cost limit is kept, and spored terminates the instance when it is reached, even before the new deadline. |
| `--job-array-id` |  | string |  | Extend TTL for all instances in job array by ID |
| `--job-array-name` |  | string |  | Extend TTL for all instances in job array by name |
| `--raise-cost-limit` |  | bool |  | Raise the instance's cost limit to what running until the new deadline costs, if it is lower. spored terminates the instance when the cost limit is reached. |

