## `spawn stop`

Stop a running instance (preserves EBS volumes).

The TTL deadline is absolute: it keeps counting while the instance is stopped, and stopping or starting does not move it. Only 'spawn extend' does.

```
spawn stop [instance-id-or-name] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--job-array-id` |  | string |  | Stop all instances in job array by ID |
| `--job-array-name` |  | string |  | Stop all instances in job array by name |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

