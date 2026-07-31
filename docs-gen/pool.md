## `spawn pool`

Run a wide fan-out as a POOL of reusable worker instances instead of one
ephemeral instance per task.

At high fan-out, one-instance-per-task is dispatch-bound and pays a full instance
boot per task, so short tasks self-terminate faster than new ones launch and
concurrency can never reach N. A worker pool inverts this: N fungible workers pull
tasks from a run-scoped SQS queue and reuse across jobs (per-job cost is stage+run
only), degrading gracefully — ask for N workers, get M, and tasks drain through
whatever pool came up. Workers self-terminate on idle-timeout (scale to zero).

```
spawn pool
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--run-id` |  | string |  | Run id identifying the pool (queue) — shared by all commands |

### `spawn pool create`

Provision the worker pool + task queue for a run

```
spawn pool create --run-id R --workers N --instance-type T [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--idle-timeout` |  | duration | `5m0s` | Workers drain after this long with an empty queue |
| `--instance-type` |  | string |  | Worker instance type (e.g. c7i.large) |
| `--min-viable` |  | int | `1` | Minimum workers that must come up (best-effort: ask N, accept &gt;= this) |
| `--spot` |  | bool |  | Launch workers as Spot instances |
| `--ttl` |  | string | `4h` | Per-worker TTL backstop |
| `--visibility-timeout` |  | int | `900` | SQS claim window in seconds (must exceed the longest task runtime) |
| `--workers` |  | int |  | Number of worker instances to provision |

### `spawn pool drain`

Delete the pool's task queue (workers self-terminate on idle)

```
spawn pool drain --run-id R
```

### `spawn pool status`

Show the pool queue depth (visible + in-flight tasks)

```
spawn pool status --run-id R
```

### `spawn pool submit`

Stage a task spec to S3 and enqueue it for the pool

```
spawn pool submit --run-id R --spec spec.json [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--spec` |  | string |  | Path to a TaskSpec JSON file to enqueue |

