## `spawn burst`

Launch EC2 instances that will register with the hybrid registry
and coordinate with local instances to process workloads.

This enables "cloud bursting" where local compute capacity can be
extended with on-demand cloud resources.

```
spawn burst [flags]
```

**Examples:**

```sh
# Launch 10 instances to help process a job array
  spawn burst --count 10 --job-array-id my-array --instance-type c5.4xlarge

  # Launch with specific AMI
  spawn burst --count 5 --job-array-id genomics --ami ami-abc123

  # Launch Spot instances for cost savings
  spawn burst --count 20 --job-array-id simulation --spot
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--ami` |  | string |  | AMI ID (auto-detect if not specified) |
| `--count` |  | int | `1` | Number of instances to launch |
| `--instance-type` |  | string | `t3.micro` | EC2 instance type |
| `--job-array-id` |  | string |  | Job array ID to join (required) |
| `--job-array-name` |  | string |  | Job array name (optional) |
| `--key-name` |  | string |  | SSH key pair name |
| `--security-group-ids` |  | stringSlice |  | Security group IDs (comma-separated or repeated) |
| `--spot` |  | bool |  | Use Spot instances |
| `--subnet-id` |  | string |  | Subnet ID |

