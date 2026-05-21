# spawn list-amis

List spawn-managed AMIs with filtering and search.

## Synopsis

```bash
spawn list-amis [flags]
```

## Description

List Amazon Machine Images (AMIs) created by spawn, with filtering by tags, architecture, and age. Useful for finding custom images for launching instances.

## Flags

### Filtering

#### --stack
**Type:** String
**Default:** None
**Description:** Filter by stack tag (e.g., pytorch, tensorflow).

```bash
spawn list-amis --stack pytorch
spawn list-amis --stack tensorflow
```

#### --region
**Type:** String
**Default:** Current region
**Description:** AWS region to search.

```bash
spawn list-amis --region us-east-1
```

#### --architecture
**Type:** String
**Allowed Values:** `x86_64`, `arm64`
**Default:** All
**Description:** Filter by CPU architecture.

```bash
spawn list-amis --architecture x86_64
spawn list-amis --architecture arm64
```

#### --tag
**Type:** String (key=value)
**Default:** None
**Description:** Filter by custom tag.

```bash
spawn list-amis --tag cuda-version=12.1
spawn list-amis --tag purpose=training
```

### Output

#### --format
**Type:** String
**Allowed Values:** `table`, `json`
**Default:** `table`
**Description:** Output format.

```bash
spawn list-amis --format json
```

#### --sort-by
**Type:** String
**Allowed Values:** `date`, `name`, `size`
**Default:** `date`
**Description:** Sort order.

```bash
spawn list-amis --sort-by date
spawn list-amis --sort-by name
```

## Output

### Table Format

```
+-------------------------+------------------+------------+--------+---------+
| AMI ID                  | Name             | Stack      | Arch   | Age     |
+-------------------------+------------------+------------+--------+---------+
| ami-0abc123def456789    | pytorch-20260127 | pytorch    | x86_64 | 2d      |
| ami-0def456abc789012    | pytorch-20260125 | pytorch    | x86_64 | 4d      |
| ami-0789012abc345def    | tf-20260120      | tensorflow | x86_64 | 9d      |
+-------------------------+------------------+------------+--------+---------+

Total: 3 AMIs
```

### JSON Format

```json
[
  {
    "ami_id": "ami-0abc123def456789",
    "name": "pytorch-2.2-cuda12-20260127",
    "description": "PyTorch 2.2 with CUDA 12.1 on AL2023",
    "state": "available",
    "architecture": "x86_64",
    "root_device_type": "ebs",
    "created_at": "2026-01-27T15:30:00Z",
    "age": "2d",
    "size_gb": 100,
    "tags": {
      "stack": "pytorch",
      "version": "2.2",
      "cuda-version": "12.1"
    }
  }
]
```

## Examples

### List All AMIs
```bash
spawn list-amis
```

### List PyTorch AMIs
```bash
spawn list-amis --stack pytorch
```

### List ARM64 AMIs
```bash
spawn list-amis --architecture arm64
```

### Find Latest AMI for Stack
```bash
# Get latest pytorch AMI ID
AMI=$(spawn list-amis --stack pytorch --format json | jq -r '.[0].ami_id')
spawn launch --instance-type g5.xlarge --ami "$AMI"
```

### List by Custom Tag
```bash
spawn list-amis --tag cuda-version=12.1
spawn list-amis --tag purpose=training
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | List completed successfully |
| 1 | API error |
| 2 | Invalid filter |

## See Also

- [spawn create-ami](create-ami.md) - Create AMIs
- [spawn launch](launch.md) - Launch from AMI
