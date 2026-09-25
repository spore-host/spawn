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

Sizing picks the cheapest type that fits, and on a price TIE the smallest that
fits — which matters on a family like hpc7g where every size costs the same
because you rent the socket rather than the cores, so a 16-vCPU request returns
hpc7g.4xlarge and not the 64-vCPU hpc7g.16xlarge (spawn#610).

To choose the type yourself, set resources.instance_type in the spec: that pins
it exactly and skips candidate search and price ranking altogether (cpu,
memory_gib, architecture and families are then ignored). That is the way to sweep
several sizes of one family deliberately, since cpu/memory can't distinguish
equal-spec sizes. resources.families narrows sizing to an allow-list without
pinning a single type.

Once the type is chosen, spawn launches an ephemeral instance that stages inputs
from S3, runs the command, stages outputs back, and writes a durable completion
record to
s3://spawn-results-&lt;account&gt;-&lt;region&gt;/tasks/&lt;task_id&gt;/completion.json — the
signal workflow adapters poll. The instance self-terminates on completion (TTL +
on_complete).

If spec.container is set, the command runs inside that image (Docker is installed
on demand; the manifest dirs are bind-mounted; a private-ECR image is pulled with
an ecr:ReadOnly grant, GPUs passed with --gpus all). Otherwise it runs on the host.

The launch AMI is auto-selected from the sized instance type: a GPU family (g5,
g6, p4/p5, …) gets the AL2023 NVIDIA DLAMI so --gpus all lands on a host with a
driver (spawn#601), and everything else gets the standard AL2023 for the type's
architecture. Pin a specific AMI with --ami (or placement.ami in the spec); an
explicit AMI always wins over auto-selection.

Re-running the same task_id is safe with --wait: the results of the previous
attempt are cleared at launch, and every run stamps a run_id into its completion
record, so --wait ignores any record that isn't from the run it just launched
instead of reporting an earlier attempt's verdict (#608).

--dry-run sizes and prints the plan without launching.

```
spawn task run --spec <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--ami` |  | string |  | Pin a specific AMI (overrides auto-selection and placement.ami); empty = auto-select from the sized instance type (GPU DLAMI for GPU families) |
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

