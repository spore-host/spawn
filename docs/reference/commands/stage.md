# spawn stage

Manage multi-region data staging for parameter sweeps.

## Synopsis

```bash
spawn stage create <source-path> [flags]
spawn stage list [flags]
spawn stage status <stage-id> [flags]
```

## Description

Stage data to multiple AWS regions for cost-efficient parameter sweeps. Eliminates cross-region data transfer costs by replicating data once to each region before launching instances.

**Cost Savings:** 90-99% reduction in data transfer costs for multi-region sweeps.

## Subcommands

### create
Stage data to multiple regions.

### list
List staging jobs.

### status
Check staging progress.

## create - Stage Data

### Synopsis
```bash
spawn stage create <source-path> [flags]
```

### Arguments

#### source-path
**Type:** String (S3 URI or local path)
**Required:** Yes
**Description:** Source data to stage.

```bash
# From S3
spawn stage create s3://my-bucket/dataset/ --regions us-east-1,us-west-2,eu-west-1

# From local
spawn stage create ./dataset/ --regions us-east-1,us-west-2
```

### Flags

#### --regions
**Type:** String (comma-separated)
**Required:** Yes
**Description:** Target regions for staging.

```bash
spawn stage create s3://bucket/data/ --regions us-east-1,us-west-2,ap-south-1
```

#### --name
**Type:** String
**Default:** Auto-generated
**Description:** Staging job name.

```bash
spawn stage create s3://bucket/data/ --regions us-east-1,us-west-2 --name training-dataset
```

## Output

```
Staging data to 3 regions

Source: s3://my-bucket/dataset/ (12.3 GB)
Regions: us-east-1, us-west-2, ap-south-1

Stage ID: stage-20260127-abc123

Progress:
  us-east-1:  [========================================] 100% (12.3 GB)
  us-west-2:  [========================>               ] 65% (8.0 GB)
  ap-south-1: [========>                               ] 23% (2.8 GB)

Staged Locations:
  us-east-1:  s3://spawn-staging-us-east-1/stage-20260127-abc123/
  us-west-2:  (in progress)
  ap-south-1: (in progress)

Estimated completion: 15 minutes
```

## list - List Staging Jobs

```bash
spawn stage list
```

**Output:**
```
+---------------------------+-----------+---------+----------+
| Stage ID                  | Name      | Status  | Regions  |
+---------------------------+-----------+---------+----------+
| stage-20260127-abc123     | dataset   | running | 3        |
| stage-20260126-def456     | models    | complete| 2        |
+---------------------------+-----------+---------+----------+
```

## status - Check Status

```bash
spawn stage status stage-20260127-abc123
```

## Examples

### Stage Dataset to 3 Regions
```bash
spawn stage create s3://ml-datasets/cifar10/ \
  --regions us-east-1,us-west-2,eu-west-1 \
  --name cifar10-dataset
```

### Use Staged Data in Sweep
```bash
# After staging completes
spawn launch --param-file sweep.yaml --use-staged-data stage-20260127-abc123
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | Staging failed (S3 error, permission denied) |
| 2 | Invalid arguments |

## See Also

- [spawn launch](launch.md) - Use staged data
- [Data Staging Guide](../../how-to/data-staging.md) - Complete workflow
