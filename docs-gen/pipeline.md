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

```
spawn pipeline launch <file> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region (default: from AWS config) |
| `--wait` |  | bool |  | Wait for pipeline to complete |

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

