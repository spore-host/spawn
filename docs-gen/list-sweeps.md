## `spawn list-sweeps`

> **Deprecated:** use 'spawn sweep list' instead

List parameter sweeps from DynamoDB orchestration table.

Shows recent sweeps with their status, progress, and creation time.

Examples:
  # List recent sweeps
  spawn list-sweeps

  # Filter by status
  spawn list-sweeps --status RUNNING

  # Show last 5 sweeps
  spawn list-sweeps --last 5

  # Show sweeps since a date
  spawn list-sweeps --since 2026-01-15

  # JSON output
  spawn list-sweeps --json

```
spawn list-sweeps [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--last` |  | int | `20` | Show last N sweeps |
| `--region` |  | string |  | Filter by region |
| `--since` |  | string |  | Show sweeps created after date (YYYY-MM-DD) |
| `--status` |  | string |  | Filter by status (RUNNING, COMPLETED, FAILED, CANCELLED) |

