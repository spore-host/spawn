## `spawn sweep`

Manage parameter sweeps: list, check status, cancel, resume, and collect results.

Examples:
  spawn sweep list
  spawn sweep status sweep-20260116-abc123
  spawn sweep cancel sweep-20260116-abc123
  spawn sweep resume sweep-20260116-abc123 --max-concurrent 5
  spawn sweep collect sweep-20260116-abc123 --output-file results.json

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
| `--regions` | `-r` | stringSlice |  | Regions to collect from (comma-separated or repeated; default: all) |
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
| `--completion-webhook-url` |  | string |  | The sweep's --completion-webhook-url, passed again: the checkpoint does not store it, since it may carry a token. Required when the sweep was launched with it. Not supported with --detach |
| `--cost-limit` |  | float64 |  | Cost limit in USD for each relaunched member, replacing the sweep's --cost-limit; may not be higher than it. A row's own cost_limit: still wins. Not supported with --detach |
| `--detach` |  | bool |  | Run sweep orchestration in Lambda |
| `--idle-timeout` |  | string |  | Idle timeout for each relaunched member, replacing the sweep's --idle-timeout; may not be longer than it. A row's own idle_timeout: still wins. Not supported with --detach |
| `--keep-on-error` |  | bool |  | Keep a relaunched instance whose spored never came up instead of terminating it. It then has no TTL/idle/cost-limit enforcement until spored is confirmed running |
| `--max-concurrent-auto` |  | bool |  | Re-derive max concurrent instances from the account's real AWS quota headroom for the pending parameter sets' instance type(s)/region, instead of the original or a user-supplied number (spawn#494). Not yet supported with --detach. |
| `--max-concurrent` |  | int |  | Override max concurrent instances (0 = use original) |
| `--spot-webhook-url` |  | string |  | The sweep's --spot-webhook-url, passed again: the checkpoint does not store it, since it may carry a token. Required when the sweep was launched with it. Not supported with --detach |
| `--ttl` |  | string |  | Time to live for each relaunched member (e.g. 4h, 1d), replacing the sweep's --ttl; may not be longer than it. A row's own ttl: still wins. Not supported with --detach |
| `--user-data` |  | string |  | The sweep's --user-data, passed again: the checkpoint does not store it, since it may carry a token. Required when the sweep was launched with it. Not supported with --detach |
| `--webhook-correlation` |  | string |  | The sweep's --webhook-correlation, passed again: the checkpoint does not store it, since it may carry a token. Required when the sweep was launched with it. Not supported with --detach |

### `spawn sweep status`

Show parameter sweep status and progress

```
spawn sweep status <sweep-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--check-complete` |  | bool |  | Exit with standardized codes: 0=complete 1=failed 2=running 3=error |

