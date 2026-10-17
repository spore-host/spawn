## `spawn pipeline`

Manage multi-stage pipelines with DAG dependencies.

Pipelines allow you to orchestrate complex workflows where each stage runs on
separate instances with different instance types. Stages can depend on each other,
forming a directed acyclic graph (DAG).

Data can be passed between stages via:
- S3 (batch mode): Stage outputs uploaded to S3, downloaded by next stage
- Network streaming (real-time): Direct TCP/gRPC connections between stages

```
spawn pipeline
```

### `spawn pipeline cancel`

Cancel a running pipeline and terminate all instances.

Sets the cancellation flag in DynamoDB. The orchestrator Lambda will
terminate all running instances and mark the pipeline as CANCELLED.

```
spawn pipeline cancel <pipeline-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn pipeline collect`

Download all results from a completed pipeline.

Downloads outputs from all stages to a local directory.

```
spawn pipeline collect <pipeline-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-dir` |  | string | `./results` | Output directory for downloaded files |
| `--stage` |  | string |  | Download results from specific stage only |

### `spawn pipeline graph`

Display the pipeline dependency graph as ASCII art.

Shows:
- Stage names and instance types
- Dependencies between stages
- Fan-out and fan-in patterns
- Data passing modes (S3 or streaming)

```
spawn pipeline graph <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--simple` |  | bool |  | Show simplified graph |
| `--stats` |  | bool |  | Show graph statistics |

### `spawn pipeline launch`

Launch a multi-stage pipeline.

The pipeline definition will be uploaded to S3 and a Lambda orchestrator
will be invoked to manage the pipeline execution.

Each launch is a new run with its own run id. A run's stage markers, outputs
and config live under &lt;s3_prefix&gt;/runs/&lt;run-id&gt;/, so a run never reads
another run's results, even under the same s3_prefix.

pipeline_id comes from the definition file, so launching the same file again
reuses it. If that pipeline has a run still in flight, launch refuses rather
than replacing the running pipeline's record. --replace cancels the run in
flight (its instances are terminated) and then starts the new one.

```
spawn pipeline launch <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: from AWS config) |
| `--replace` |  | bool |  | If the pipeline has a run in flight, cancel it (terminating its instances) and start a new run |
| `--wait-timeout` |  | duration | `24h0m0s` | Give up waiting after this long (the pipeline keeps running; --wait then exits non-zero) |
| `--wait` |  | bool |  | Block until the pipeline reaches a terminal state, and exit non-zero if it did not succeed |

### `spawn pipeline list`

List all pipelines for the current user.

Shows pipeline ID, name, status, and cost.

```
spawn pipeline list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--status` |  | string |  | Filter by status (INITIALIZING, RUNNING, COMPLETED, FAILED, CANCELLED) |

### `spawn pipeline status`

Show the current status of a running or completed pipeline.

Displays:
- Overall pipeline status
- Per-stage progress
- Instance information
- Cost tracking

```
spawn pipeline status <pipeline-id>
```

### `spawn pipeline validate`

Validate a pipeline definition file.

Checks:
- JSON syntax and structure
- Required fields present
- Stage dependencies valid (no circular dependencies)
- Instance types, regions, and other configuration valid

```
spawn pipeline validate <file>
```

