## `spawn terminate`

Permanently terminate an instance. This is irreversible: the instance is
destroyed and any non-persisted volumes are deleted. Use stop or hibernate to
keep EBS volumes.

A running 'spawn task run' instance first has its command.log and a
terminal completion record (terminal_reason user_terminated) flushed to S3
over SSM, as a TTL or cost-limit kill does. The flush is bounded and
best-effort: if it fails, the instance is terminated anyway.

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

