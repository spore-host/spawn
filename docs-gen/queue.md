## `spawn queue`

Commands for managing and monitoring batch job queues.

Batch queues execute jobs sequentially on a single instance, with
dependency management and automatic result collection.

Examples:
  # Check queue status on instance
  spawn queue status i-1234567890abcdef0

  # Download queue results
  spawn queue results queue-20260122-140530 --output ./results/

```
spawn queue
```

### `spawn queue results`

Download all job results from S3 for a completed or running queue.

Results include job outputs, logs, and the final queue state.

Examples:
  # Download to current directory
  spawn queue results queue-20260122-140530

  # Download to specific directory
  spawn queue results queue-20260122-140530 --output ./my-results/

```
spawn queue results <queue-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-dir` |  | string | `.` | Output directory for results |

### `spawn queue status`

Show the execution status of a batch queue running on an instance.

Connects to the instance via SSH and reads the queue state file.

Examples:
  spawn queue status i-1234567890abcdef0

```
spawn queue status <instance-id>
```

### `spawn queue template`

Manage pre-built queue configuration templates.

Templates provide ready-to-use queue configurations for common workflows
with variable substitution for customization.

Available commands:
  list      - List available templates
  show      - Show template details
  generate  - Generate queue config from template

```
spawn queue template
```

#### `spawn queue template generate`

Generate a queue configuration file from a template with variable substitution.

Variables can be provided via --var flags or use template defaults.

Examples:
  # Generate with defaults, output to file
  spawn queue template generate ml-pipeline --output pipeline.json

  # Provide required variables
  spawn queue template generate ml-pipeline \
    --var INPUT=/data/train.csv \
    --var S3_BUCKET=my-results \
    --output pipeline.json

  # Output to stdout (for piping)
  spawn queue template generate simple-sequential \
    --var S3_BUCKET=results

```
spawn queue template generate <template-name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-file` |  | string |  | Output file (default: stdout) |
| `--var` |  | stringToString |  | Template variables (key=value) |

#### `spawn queue template init`

Launch an interactive wizard to create a custom queue configuration.

Guides you through creating a queue by asking questions about:
- Workflow type and name
- Number of jobs and commands
- Job dependencies
- Timeouts and retry policies
- Result collection
- S3 bucket configuration

Examples:
  spawn queue template init
  spawn queue template init --output my-queue.json

```
spawn queue template init [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output-file` |  | string |  | Output file (default: queue.json) |

#### `spawn queue template list`

List all available queue configuration templates.

Shows template names, descriptions, and required/optional variables.

Examples:
  spawn queue template list

```
spawn queue template list
```

#### `spawn queue template show`

Show detailed information about a queue template.

Displays template description, jobs, and all variables with their defaults.

Examples:
  spawn queue template show ml-pipeline
  spawn queue template show etl

```
spawn queue template show <template-name>
```

