# spawn collect-results

Collect and aggregate results from parameter sweeps.

## Synopsis

```bash
spawn collect-results --sweep-id <sweep-id> [flags]
```

## Description

Download and aggregate results from all instances in a parameter sweep. Collects stdout/stderr logs, result files, and metadata for analysis.

## Flags

### Required

#### --sweep-id
**Type:** String
**Required:** Yes
**Description:** Sweep ID to collect results from.

```bash
spawn collect-results --sweep-id sweep-20260127-abc123
```

### Optional

#### --output, -o
**Type:** Path
**Default:** `./results/<sweep-id>/`
**Description:** Output directory.

```bash
spawn collect-results --sweep-id sweep-123 --output ./my-results/
```

#### --format
**Type:** String
**Allowed Values:** `raw`, `aggregated`, `csv`
**Default:** `raw`
**Description:** Result format.

```bash
# Raw files
spawn collect-results --sweep-id sweep-123 --format raw

# Aggregated JSON
spawn collect-results --sweep-id sweep-123 --format aggregated

# CSV summary
spawn collect-results --sweep-id sweep-123 --format csv
```

#### --include
**Type:** String
**Default:** `all`
**Allowed Values:** `logs`, `stdout`, `stderr`, `results`, `all`
**Description:** What to collect.

```bash
# Logs only
spawn collect-results --sweep-id sweep-123 --include logs

# Results only
spawn collect-results --sweep-id sweep-123 --include results
```

## Output

```
Collecting results for sweep: sweep-20260127-abc123

Source: s3://spawn-results-us-east-1/sweep-20260127-abc123/
Destination: ./results/sweep-20260127-abc123/

Progress:
  [========================================] 48/48 instances

Downloaded:
  Logs: 45 MB (stdout: 38 MB, stderr: 7 MB)
  Results: 1.2 GB
  Total: 1.245 GB

Time: 2m 34s
Average: 26 MB/instance

Results saved to: ./results/sweep-20260127-abc123/
```

### Directory Structure

```
results/sweep-20260127-abc123/
├── run-001/
│   ├── stdout.log
│   ├── stderr.log
│   ├── metadata.json
│   └── results/
│       └── output.txt
├── run-002/
│   └── ...
├── summary.json
└── results.csv
```

## Examples

### Collect All Results
```bash
spawn collect-results --sweep-id sweep-20260127-abc123
```

### Collect to Custom Directory
```bash
spawn collect-results --sweep-id sweep-123 --output ~/training-results/
```

### Logs Only
```bash
spawn collect-results --sweep-id sweep-123 --include logs
```

### Aggregated JSON
```bash
spawn collect-results --sweep-id sweep-123 --format aggregated
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Collection successful |
| 1 | Collection failed (S3 error, permission denied) |
| 2 | Invalid arguments |
| 3 | Sweep not found |

## See Also

- [spawn status](status.md) - Check sweep status
- [spawn launch](launch.md) - Launch sweeps
- [spawn list-sweeps](list-sweeps.md) - List sweeps
