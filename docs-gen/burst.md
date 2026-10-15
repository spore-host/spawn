## `spawn burst`

Launch EC2 instances that will register with the hybrid registry
and coordinate with local instances to process workloads.

This enables "cloud bursting" where local compute capacity can be
extended with on-demand cloud resources.

Every instance gets a TTL (--ttl, default 8h) and terminates when it expires.

```
spawn burst [flags]
```

**Examples:**

```sh
# Launch 10 instances to help process a job array
  spawn burst --count 10 --job-array-id my-array --instance-type c5.4xlarge

  # Launch with specific AMI
  spawn burst --count 5 --job-array-id genomics --ami ami-abc123

  # Launch Spot instances for cost savings, terminated after 2 hours
  spawn burst --count 20 --job-array-id simulation --spot --ttl 2h
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--ami` |  | string |  | AMI ID (auto-detect if not specified) |
| `--count` |  | int | `1` | Number of instances to launch |
| `--idle-timeout` |  | string |  | Also terminate an instance after it has been idle this long (e.g. 1h). Empty disables idle termination. |
| `--instance-profile` |  | string |  | Attach this EXISTING IAM instance profile by name instead of the default spored-instance-profile, which spawn creates or reuses as spawn launch does. Its role must let spored read and write its own tags, terminate itself, and use the spawn-hybrid-registry table. |
| `--instance-type` |  | string | `t3.micro` | EC2 instance type |
| `--job-array-id` |  | string |  | Job array ID to join (required) |
| `--job-array-name` |  | string |  | Job array name, used by spawn array status/cancel (defaults to the job array ID) |
| `--key-name` |  | string |  | SSH key pair name |
| `--security-group-ids` |  | stringSlice |  | Security group IDs (comma-separated or repeated) |
| `--spot` |  | bool |  | Use Spot instances |
| `--subnet-id` |  | string |  | Subnet ID |
| `--ttl` |  | string | `8h` | Terminate each instance after this duration (e.g. 30m, 2h, 36h). Required to be positive: burst instances always have a TTL. |

