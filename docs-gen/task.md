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

--dry-run sizes and prints the plan without launching. Container execution
(spec.container) is a follow-up increment — omit it to run on the host.

```
spawn task run --spec <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--dry-run` |  | bool |  | Size and preview the task without launching |
| `--region` |  | string |  | Region to size against (default: the configured AWS region) |
| `--spec` |  | string |  | Path to a TaskSpec JSON file (required) |

