## `spawn availability`

Display availability statistics based on historical launch success/failure data.

This helps identify regions with proven capacity for specific instance types.
Statistics are passively collected from actual launch attempts.

```
spawn availability [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--instance-type` |  | string |  | Instance type to check (required) |
| `--regions` | `-r` | stringSlice |  | Regions to check (comma-separated or repeated; default: common regions) |

