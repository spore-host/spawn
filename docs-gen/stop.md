## `spawn stop`

Stop a running instance (preserves EBS volumes)

```
spawn stop [instance-id-or-name] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--job-array-id` |  | string |  | Stop all instances in job array by ID |
| `--job-array-name` |  | string |  | Stop all instances in job array by name |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

