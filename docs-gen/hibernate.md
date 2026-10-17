## `spawn hibernate`

Hibernate an instance to disk (saves RAM state).

The TTL deadline is absolute: it keeps counting while the instance is stopped, and stopping or starting does not move it. Only 'spawn extend' does.

```
spawn hibernate [instance-id-or-name] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--job-array-id` |  | string |  | Hibernate all instances in job array by ID |
| `--job-array-name` |  | string |  | Hibernate all instances in job array by name |

