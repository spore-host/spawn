## `spawn collect-results`

> **Deprecated:** use 'spawn sweep collect &lt;sweep-id&gt;' instead

Collect results from all instances in a parameter sweep.

This command downloads result files from S3 (uploaded by sweep instances),
aggregates them into a single output file, and optionally identifies the
best performing parameters based on a metric.

Result File Convention:
Instances should upload results to: s3://spawn-results-&lt;account&gt;-&lt;region&gt;/sweeps/&lt;sweep-id&gt;/&lt;index&gt;/results.json

The result file should be a JSON object with metrics, e.g.:
{
  "accuracy": 0.95,
  "loss": 0.12,
  "duration": 120.5,
  "params": {...}
}

Examples:
  # Collect all results to JSON
  spawn collect-results --sweep-id sweep-123 --output results.json

  # Collect to CSV format
  spawn collect-results --sweep-id sweep-123 --output results.csv --format csv

  # Find top 5 runs by accuracy (descending)
  spawn collect-results --sweep-id sweep-123 --metric accuracy --best 5

  # Custom S3 prefix (if instances uploaded to different location)
  spawn collect-results --sweep-id sweep-123 --s3-prefix s3://my-bucket/results/

```
spawn collect-results [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--best` |  | int |  | Show only top N results by metric (0 = all) |
| `--format` |  | string | `json` | Output format: json, csv, jsonl |
| `--metric` |  | string |  | Metric to use for ranking results (e.g., accuracy, loss) |
| `--output` | `-o` | string | `results.json` | Output file path |
| `--regions` | `-r` | stringSlice |  | Regions to collect from (comma-separated or repeated; default: all) |
| `--s3-prefix` |  | string |  | Custom S3 prefix for results (default: auto-detect) |
| `--sweep-id` |  | string |  | Sweep ID to collect results from (required) |

