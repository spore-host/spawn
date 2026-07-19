## `spawn terminate`

Permanently terminate an instance. This is irreversible: the instance is
destroyed and any non-persisted volumes are deleted. Use stop or hibernate to
keep EBS volumes.

Terminate a single instance by ID or name, or an entire job array:

  spawn terminate i-0abc123
  spawn terminate my-instance --yes
  spawn terminate --job-array-name training

```
spawn terminate [instance-id-or-name] [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--job-array-id` |  | string |  | Terminate all instances in job array by ID |
| `--job-array-name` |  | string |  | Terminate all instances in job array by name |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

