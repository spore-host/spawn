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

This is the first increment: only --dry-run is supported. It parses and validates
the spec, sizes the cheapest instance type that fits its resource request (via
truffle), and prints the plan — WITHOUT launching anything. Real launch and the
durable .exitcode-in-S3 completion record are a follow-up (see #386).

```
spawn task run --spec <file> --dry-run [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--dry-run` |  | bool |  | Size and preview the task without launching (currently required) |
| `--region` |  | string |  | Region to size against (default: the configured AWS region) |
| `--spec` |  | string |  | Path to a TaskSpec JSON file (required) |

