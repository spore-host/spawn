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
| `--cost-limit` |  | float64 |  | Set the instance's cost limit to this amount in USD, instead of raising it to what the new TTL needs |
| `--job-array-id` |  | string |  | Extend TTL for all instances in job array by ID |
| `--job-array-name` |  | string |  | Extend TTL for all instances in job array by name |
| `--keep-cost-limit` |  | bool |  | Extend the TTL and leave the cost limit alone. The instance will still stop when the existing cap is reached, which may be before the new deadline. |

