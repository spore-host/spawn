## `spawn stage`

Stage data to regional S3 buckets for efficient multi-region parameter sweeps.

Data staging enables cost-optimized data movement by:
- Replicating data once to regional buckets
- Allowing instances to download from local region (free)
- Avoiding repeated cross-region transfers ($0.09/GB)

Cost savings example:
  100GB dataset, 2 regions, 10 instances each:
  - Without staging: $90.00 (cross-region transfers)
  - With staging: $6.60 (one-time replication)
  - Savings: $85.70 (93% reduction)

Commands:
  spawn stage upload &lt;path&gt;     Stage data to regional buckets
  spawn stage list              List staged data
  spawn stage estimate          Estimate staging cost savings
  spawn stage delete &lt;id&gt;       Delete staged data

```
spawn stage
```

### `spawn stage delete`

Delete staged data from all regions and remove metadata.

```
spawn stage delete <staging-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn stage estimate`

Estimate the cost difference between:
  A) Single-region storage with cross-region transfers
  B) Regional replication with local transfers

This helps determine if staging is cost-effective for your workload.

Example:
  spawn stage estimate \
    --data-size-gb 100 \
    --instances 10 \
    --regions us-east-1,us-west-2

```
spawn stage estimate [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--data-size-gb` |  | int | `100` | Dataset size in GB |
| `--instances` |  | int | `10` | Number of instances per region |
| `--regions` | `-r` | stringSlice | `[us-east-1,us-west-2]` | Regions to estimate for (comma-separated or repeated) |

### `spawn stage list`

List all data currently staged in regional buckets.

```
spawn stage list
```

### `spawn stage upload`

Upload a file or directory to spawn data staging buckets across regions.

The data will be:
1. Uploaded to the primary region
2. Replicated to additional regions
3. Tracked in DynamoDB for lifecycle management
4. Automatically deleted after 7 days

Example:
  spawn stage upload ./reference-genome.fasta \
    --regions us-east-1,us-west-2 \
    --dest /mnt/data/reference.fasta \
    --sweep-id sweep-abc123

```
spawn stage upload <local-path> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--dest` |  | string |  | Destination path on instances (default: /mnt/data/&lt;filename&gt;) |
| `--regions` | `-r` | stringSlice | `[us-east-1,us-west-2]` | Regions to replicate to (comma-separated or repeated) |
| `--sweep-id` |  | string |  | Associate with sweep ID for tracking |

