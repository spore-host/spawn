## `spawn task`

Work with a single launched instance as a "task".

  spawn task diagnose &lt;name|instance-id&gt;   one-screen summary + likely cause

```
spawn task
```

### `spawn task diagnose`

Summarize an instance's state, cost, and likely failure cause

```
spawn task diagnose <name|instance-id>
```

### `spawn task run`

Run a task described by a TaskSpec JSON file (the shared workflow-adapter
contract, spawn#386).

Sizes the cheapest instance type that fits the resource request (via truffle),
then launches an ephemeral instance that stages inputs from S3, runs the command,
stages outputs back, and writes a durable completion record to
s3://spawn-results-&lt;account&gt;-&lt;region&gt;/tasks/&lt;task_id&gt;/completion.json — the
signal workflow adapters poll. The instance self-terminates on completion (TTL +
on_complete).

If spec.container is set, the command runs inside that image (Docker is installed
on demand; the manifest dirs are bind-mounted; a private-ECR image is pulled with
an ecr:ReadOnly grant, GPUs passed with --gpus all). Otherwise it runs on the host.

--dry-run sizes and prints the plan without launching.

```
spawn task run --spec <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--dry-run` |  | bool |  | Size and preview the task without launching |
| `--poll-interval` |  | duration | `15s` | How often to poll for completion when --wait is set |
| `--region` |  | string |  | Region to size against (default: the configured AWS region) |
| `--spec` |  | string |  | Path to a TaskSpec JSON file (required) |
| `--wait` |  | bool |  | Block until the task's completion record appears, then exit with its exit code |

### `spawn task status`

Read the completion record a 'spawn task run' task wrote to
s3://spawn-results-&lt;account&gt;-&lt;region&gt;/tasks/&lt;task-id&gt;/completion.json.

If the record isn't there yet the task is still running. With --check-complete,
exit codes mirror 'spawn status': 0=completed, 1=failed, 2=running, 3=error.

```
spawn task status <task-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--check-complete` |  | bool |  | Exit 0=completed, 1=failed, 2=running, 3=error instead of printing |
| `--region` |  | string |  | Region the task ran in (default: the configured AWS region) |

