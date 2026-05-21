# spawn burst

Launch cloud instances to join a hybrid (local + cloud) job array.

## Synopsis

```bash
spawn burst --job-array-id <id> --count <n> [flags]
```

## Description

`spawn burst` enables cloud bursting: local compute capacity can be extended with on-demand EC2 instances that join an existing hybrid job array. The launched instances register with the hybrid registry and coordinate with local instances to process workloads.

## Flags

### Required

#### --job-array-id
**Type:** String
**Description:** Job array ID to join.

```bash
spawn burst --job-array-id my-array --count 10
```

### Optional

#### --count
**Type:** Integer
**Default:** `1`
**Description:** Number of instances to launch.

#### --job-array-name
**Type:** String
**Default:** None
**Description:** Human-readable job array name (optional label).

#### --instance-type
**Type:** String
**Default:** `t3.micro`
**Description:** EC2 instance type for burst instances.

#### --ami
**Type:** String
**Default:** Auto-detected
**Description:** AMI ID. Defaults to auto-detection based on region.

#### --spot
**Type:** Boolean
**Default:** `false`
**Description:** Use Spot instances for cost savings.

#### --key-name
**Type:** String
**Default:** None
**Description:** SSH key pair name.

#### --subnet-id
**Type:** String
**Default:** None
**Description:** Subnet ID for instance placement.

#### --security-groups
**Type:** String slice
**Default:** None
**Description:** Security group IDs (comma-separated).

## Examples

```bash
# Burst 10 instances into a job array
spawn burst --count 10 --job-array-id genomics-run --instance-type c5.4xlarge

# Burst with Spot instances
spawn burst --count 20 --job-array-id simulation --spot --instance-type c5.xlarge

# Burst with specific AMI
spawn burst --count 5 --job-array-id analysis --ami ami-abc123
```

## See Also

- [spawn launch](launch.md) — Standard instance launch
- [spawn list](list.md) — List instances in a job array
