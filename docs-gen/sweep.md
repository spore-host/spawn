## `spawn sweep`

Manage parameter sweeps: list, check status, cancel, resume, and collect results.

Examples:
  spawn sweep list
  spawn sweep status sweep-20260116-abc123
  spawn sweep cancel sweep-20260116-abc123
  spawn sweep resume sweep-20260116-abc123 --max-concurrent 5
  spawn sweep collect sweep-20260116-abc123 --output results.json

```
spawn sweep
```

### `spawn sweep cancel`

Cancel a running parameter sweep and terminate its instances

```
spawn sweep cancel <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn sweep collect`

Download and aggregate results from a completed sweep

```
spawn sweep collect <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--best` |  | int |  | Show only top N results by metric (0 = all) |
| `--format` |  | string | `json` | Output format: json, csv, jsonl |
| `--metric` |  | string |  | Metric to rank results by (e.g. accuracy, loss) |
| `--output-file` | `-f` | string | `results.json` | Output file path |
| `--regions` | `-r` | stringSlice |  | Regions to collect from (comma-separated or repeated) |
| `--s3-prefix` |  | string |  | Custom S3 prefix for results (default: auto-detect) |

### `spawn sweep list`

List parameter sweeps

```
spawn sweep list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--last` |  | int | `20` | Show last N sweeps |
| `--region` |  | string |  | Filter by region |
| `--since` |  | string |  | Show sweeps created after date (YYYY-MM-DD) |
| `--status` |  | string |  | Filter by status (RUNNING, COMPLETED, FAILED, CANCELLED) |

### `spawn sweep resume`

Resume an interrupted parameter sweep from checkpoint

```
spawn sweep resume <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--detach` |  | bool |  | Run sweep orchestration in Lambda |
| `--max-concurrent` |  | int |  | Override max concurrent instances (0 = use original) |

### `spawn sweep status`

Show parameter sweep status and progress

```
spawn sweep status <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--check-complete` |  | bool |  | Exit with standardized codes: 0=complete 1=failed 2=running 3=error |

