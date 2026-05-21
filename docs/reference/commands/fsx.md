# spawn fsx

Manage FSx Lustre filesystems.

## Synopsis

```bash
spawn fsx create [flags]
spawn fsx list [flags]
spawn fsx delete <filesystem-id> [flags]
```

## Description

Manage FSx Lustre filesystems for high-performance parallel storage. Useful for HPC and ML workloads requiring shared filesystem access.

## Subcommands

### create
Create new FSx Lustre filesystem.

```bash
spawn fsx create --size 1200 --region us-east-1
```

### list
List FSx filesystems.

```bash
spawn fsx list
```

### delete
Delete FSx filesystem.

```bash
spawn fsx delete fs-0123456789abcdef0
```

## Flags

#### --size
**Type:** Integer (GB)
**Required:** Yes
**Valid Values:** 1200, 2400, or increments of 2400
**Description:** Filesystem size in GB.

```bash
spawn fsx create --size 1200  # 1.2 TB
spawn fsx create --size 2400  # 2.4 TB
```

#### --region
**Type:** String
**Default:** Current region
**Description:** AWS region.

```bash
spawn fsx create --size 1200 --region us-east-1
```

## Examples

### Create FSx Filesystem
```bash
spawn fsx create --size 1200 --region us-east-1
```

### List Filesystems
```bash
spawn fsx list
```

### Use with Instance
```bash
# Create filesystem
FS_ID=$(spawn fsx create --size 1200 --json | jq -r '.filesystem_id')

# Launch instance with filesystem
spawn launch --instance-type m7i.large --fsx-lustre "$FS_ID"

# Filesystem mounted at /fsx
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Operation successful |
| 1 | Operation failed |
| 2 | Invalid arguments |

## Cost

- **Storage:** ~$0.14/GB/month
- **1.2 TB:** ~$168/month
- **Throughput:** Included

## See Also

- [spawn launch](launch.md) - Mount FSx on launch
- [FSx Documentation](https://docs.aws.amazon.com/fsx/latest/LustreGuide/)
