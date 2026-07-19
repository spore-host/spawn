## `spawn cancel`

> **Deprecated:** use 'spawn sweep cancel &lt;sweep-id&gt;' instead

Cancel a running parameter sweep and terminate all instances.

Queries DynamoDB for the sweep state, terminates all running/pending
instances via cross-account access, and updates the sweep status to CANCELLED.

Examples:
  # Cancel a running sweep
  spawn cancel --sweep-id sweep-20260116-abc123

```
spawn cancel [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--sweep-id` |  | string |  | Sweep ID to cancel (required) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

