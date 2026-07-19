## `spawn resume`

> **Deprecated:** use 'spawn sweep resume <sweep-id>' instead

Resume an interrupted parameter sweep from checkpoint.

Reads the sweep state from ~/.spawn/sweeps/<sweep-id>.json,
queries EC2 for current instance states, and continues launching
pending parameter sets with rolling queue orchestration.

Examples:
  # Resume sweep with original settings
  spawn resume --sweep-id hyperparam-20260115-abc123

  # Resume with different max-concurrent
  spawn resume --sweep-id <id> --max-concurrent 5

  # Resume in detached mode (Lambda)
  spawn resume --sweep-id <id> --detach

```
spawn resume [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--detach` |  | bool |  | Run sweep orchestration in Lambda |
| `--max-concurrent` |  | int |  | Override max concurrent instances (0 = use original) |
| `--sweep-id` |  | string |  | Sweep ID to resume (required) |

