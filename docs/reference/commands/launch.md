# spawn launch

Launch EC2 instances with comprehensive configuration options.

## Synopsis

```bash
spawn launch [flags]
```

## Description

Launch EC2 instances with smart defaults and extensive configuration options. spawn auto-detects AMIs based on instance type (architecture, GPU support), configures networking, installs the spored monitoring agent, and tags resources for automatic cleanup.

**Key Features:**
- Auto-detects appropriate AMI (AL2023 standard or GPU)
- Auto-creates VPC, subnet, security groups if needed
- Installs and configures spored agent for self-monitoring
- Supports TTL-based auto-termination
- Idle detection with configurable CPU/network thresholds
- Spot instance support with interruption handling
- Parameter sweeps for batch processing
- Job arrays for coordinated instance groups
- IAM instance profile creation and attachment

## Basic Usage

### Simple Launch
```bash
# Launch with minimal config (uses intelligent defaults)
spawn launch --instance-type m7i.large

# Launch in specific region
spawn launch --instance-type m7i.large --region us-west-2

# Launch with TTL (auto-terminate after 8 hours)
spawn launch --instance-type m7i.large --ttl 8h
```

### From truffle
```bash
# Find instance and launch
truffle search m7i.large | spawn launch

# Launch spot instance
truffle spot m7i.large --max-price 0.10 | spawn launch --spot
```

## Flags

### Instance Configuration

#### --instance-type
**Type:** String
**Required:** Yes (unless from stdin)
**Description:** EC2 instance type to launch.

```bash
spawn launch --instance-type m7i.large
spawn launch --instance-type p5.48xlarge  # GPU instance
spawn launch --instance-type m8g.xlarge   # Graviton (ARM)
```

#### --ami
**Type:** String (AMI ID)
**Default:** Auto-detected based on architecture and GPU
**Description:** AMI ID to use. If not specified, spawn selects the latest AL2023 AMI appropriate for the instance type.

```bash
spawn launch --instance-type m7i.large --ami ami-0c55b159cbfafe1f0
```

**Auto-Detection:**
- Standard x86_64: Latest AL2023 kernel default
- Graviton (ARM): Latest AL2023 ARM kernel default
- GPU instances: Latest AL2023 GPU kernel (NVIDIA drivers pre-installed)

#### --region
**Type:** String
**Default:** From `AWS_REGION`, config file, or `us-east-1`
**Description:** AWS region to launch instance.

```bash
spawn launch --instance-type m7i.large --region ap-south-1
```

#### --availability-zone, --az
**Type:** String
**Default:** Random AZ in selected region
**Description:** Specific availability zone.

```bash
spawn launch --instance-type m7i.large --az us-east-1a
```

### Lifecycle Management

#### --ttl
**Type:** Duration
**Default:** None (no auto-termination)
**Description:** Time-to-live. Instance auto-terminates after this duration.

```bash
spawn launch --instance-type m7i.large --ttl 24h
spawn launch --instance-type m7i.large --ttl 3h30m
```

**Format:** Go duration (`30m`, `2h`, `1d`, `3h30m`)

**Behavior:**
- spored agent monitors uptime
- Warns users 5 minutes before termination
- Terminates gracefully (SIGTERM before SIGKILL)

#### --idle-timeout
**Type:** Duration
**Default:** None
**Description:** Terminate if idle for this duration.

```bash
spawn launch --instance-type m7i.large --idle-timeout 1h
```

**Idle Criteria:**
- CPU < 5% (configurable via `--idle-cpu`)
- Network traffic < 10KB/min
- Both conditions must be met

#### --idle-cpu
**Type:** Integer (percentage)
**Default:** `5`
**Description:** CPU threshold for idle detection.

```bash
spawn launch --instance-type m7i.large --idle-timeout 1h --idle-cpu 10
# Considers idle if CPU < 10%
```

#### --hibernate-on-idle
**Type:** Boolean
**Default:** `false`
**Description:** Hibernate instead of terminate when idle.

```bash
spawn launch --instance-type m7i.large --idle-timeout 1h --hibernate-on-idle
```

**Requirements:**
- Instance type must support hibernation
- Root volume must be encrypted
- Volume size >= instance RAM

#### --on-complete
**Type:** String
**Allowed Values:** `terminate`, `stop`, `hibernate`
**Default:** None
**Description:** Action to take when workload signals completion.

```bash
spawn launch --instance-type m7i.large --on-complete terminate --ttl 4h

# On instance, signal completion:
# spored complete --status success
# Instance terminates automatically after 30s grace period
```

**Completion Signal:**
- From CLI: `spored complete`
- From any language: `touch /tmp/SPAWN_COMPLETE`
- Optional metadata: `echo '{"status":"success"}' > /tmp/SPAWN_COMPLETE`

#### --completion-file
**Type:** Path
**Default:** `/tmp/SPAWN_COMPLETE`
**Description:** Custom completion file path.

```bash
spawn launch --instance-type m7i.large --on-complete terminate --completion-file /app/done
```

#### --completion-delay
**Type:** Duration
**Default:** `30s`
**Description:** Grace period after completion signal before action.

```bash
spawn launch --instance-type m7i.large --on-complete terminate --completion-delay 1m
```

#### --cost-limit
**Type:** Float64 (USD)
**Default:** `0` (disabled)
**Description:** Terminate the instance when accumulated compute spend reaches this amount. Uses on-demand pricing recorded at launch time; conservative for Spot instances. Can be used alone or combined with `--ttl` — whichever fires first wins.

```bash
# Terminate after spending $2.00
spawn launch --instance-type m7i.large --cost-limit 2.00

# Combined with TTL — whichever fires first
spawn launch --instance-type p3.2xlarge --cost-limit 5.00 --ttl 4h
```

**Note:** `spored status` shows current spend, percentage used, and remaining budget.

#### --pre-stop
**Type:** String (shell command)
**Default:** None
**Description:** Shell command to run before any lifecycle-triggered shutdown (TTL, idle, cost limit, Spot interruption, completion signal). Useful for flushing data, syncing files, or notifying external systems.

```bash
# Sync results to S3 before shutdown
spawn launch --instance-type c5.2xlarge \
  --pre-stop "aws s3 sync /results s3://my-bucket/results/" \
  --ttl 4h

# Save checkpoint and notify Slack
spawn launch --instance-type p3.8xlarge \
  --pre-stop "python save_checkpoint.py && curl -X POST $SLACK_WEBHOOK -d '{\"text\":\"Job complete\"}'" \
  --on-complete terminate
```

The hook runs with a configurable timeout (default 5 minutes). If it exits non-zero or times out, the shutdown proceeds anyway.

#### --pre-stop-timeout
**Type:** Duration
**Default:** `5m` (90s when triggered by Spot interruption)
**Description:** Maximum time to wait for the `--pre-stop` hook to complete before proceeding with shutdown.

```bash
spawn launch --instance-type c5.2xlarge \
  --pre-stop "python save_state.py" \
  --pre-stop-timeout 10m \
  --ttl 4h
```

**Note:** When triggered by a Spot interruption notice (2-minute warning), the timeout is automatically capped at 90 seconds regardless of this setting, to ensure the instance shuts down gracefully within the interruption window.

### Spot Instances

#### --spot
**Type:** Boolean
**Default:** `false`
**Description:** Launch as spot instance.

```bash
spawn launch --instance-type m7i.large --spot
```

#### --spot-max-price
**Type:** String (price per hour in USD)
**Default:** On-demand price
**Description:** Maximum spot price willing to pay.

```bash
spawn launch --instance-type m7i.large --spot --spot-max-price 0.10
```

#### --spot-interruption-behavior
**Type:** String
**Allowed Values:** `terminate`, `stop`, `hibernate`
**Default:** `terminate`
**Description:** Behavior on spot interruption.

```bash
spawn launch --instance-type m7i.large --spot --spot-interruption-behavior hibernate
```

### Networking

#### --vpc
**Type:** String (VPC ID)
**Default:** Auto-created or default VPC
**Description:** VPC to launch instance in.

```bash
spawn launch --instance-type m7i.large --vpc vpc-1234567890abcdef0
```

#### --subnet
**Type:** String (Subnet ID)
**Default:** Auto-created or random subnet in VPC
**Description:** Subnet to launch instance in.

```bash
spawn launch --instance-type m7i.large --subnet subnet-1234567890abcdef0
```

#### --security-groups
**Type:** String (comma-separated security group IDs)
**Default:** Auto-created with SSH access
**Description:** Security groups to attach.

```bash
spawn launch --instance-type m7i.large --security-groups sg-12345,sg-67890
```

#### --public-ip / --no-public-ip
**Type:** Boolean
**Default:** `true`
**Description:** Assign public IP address.

```bash
spawn launch --instance-type m7i.large --no-public-ip
```

#### --private-ip
**Type:** String (IP address)
**Default:** DHCP-assigned
**Description:** Specific private IP address.

```bash
spawn launch --instance-type m7i.large --private-ip 10.0.1.100
```

#### --dns-name
**Type:** String
**Default:** None
**Description:** DNS name for spore.host registration.

```bash
spawn launch --instance-type m7i.large --dns-name my-instance
# Accessible at: my-instance.<account-base36>.spore.host
```

### SSH Configuration

#### --key-name, --key-pair
**Type:** String
**Default:** Auto-created from `~/.ssh/id_rsa.pub`
**Description:** SSH key pair name in AWS.

```bash
spawn launch --instance-type m7i.large --key-name my-key
```

**Auto-Creation:**
- If `~/.ssh/id_rsa.pub` exists and no key specified
- Uploads public key to AWS as "default-ssh-key"
- Tagged with `spawn:imported=true` (never deleted)

#### --ssh-user
**Type:** String
**Default:** Detected from AMI (usually `ec2-user`)
**Description:** SSH username for connection.

```bash
spawn launch --instance-type m7i.large --ssh-user ubuntu
```

### Storage

#### --volume-size
**Type:** Integer (GB)
**Default:** AMI default (usually 8GB)
**Description:** Root EBS volume size.

```bash
spawn launch --instance-type m7i.large --volume-size 100
```

#### --volume-type
**Type:** String
**Allowed Values:** `gp2`, `gp3`, `io1`, `io2`, `st1`, `sc1`
**Default:** `gp3`
**Description:** EBS volume type.

```bash
spawn launch --instance-type m7i.large --volume-type gp3
```

#### --encrypt-volumes
**Type:** Boolean
**Default:** `false` (or `true` if hibernation enabled)
**Description:** Encrypt EBS volumes.

```bash
spawn launch --instance-type m7i.large --encrypt-volumes
```

#### --kms-key-id
**Type:** String (KMS key ID or alias)
**Default:** AWS-managed key
**Description:** KMS key for volume encryption.

```bash
spawn launch --instance-type m7i.large --encrypt-volumes --kms-key-id alias/my-key
```

### IAM Configuration

#### --iam-role
**Type:** String
**Default:** `spored-instance-role` (minimal permissions)
**Description:** IAM role name for instance profile.

```bash
spawn launch --instance-type m7i.large --iam-role my-app-role
```

#### --iam-policy
**Type:** String (comma-separated policy templates)
**Default:** None
**Description:** Policy templates to attach to IAM role.

**Built-in Templates:**
- S3: `s3:FullAccess`, `s3:ReadOnly`, `s3:WriteOnly`
- DynamoDB: `dynamodb:FullAccess`, `dynamodb:ReadOnly`, `dynamodb:WriteOnly`
- SQS: `sqs:FullAccess`, `sqs:ReadOnly`, `sqs:WriteOnly`
- Logs: `logs:WriteOnly`
- ECR: `ecr:ReadOnly`
- Secrets: `secretsmanager:ReadOnly`
- SSM: `ssm:ReadOnly`

```bash
spawn launch --instance-type m7i.large --iam-policy s3:ReadOnly,logs:WriteOnly

spawn launch --instance-type m7i.large --iam-policy s3:FullAccess,dynamodb:FullAccess
```

#### --iam-policy-file
**Type:** Path
**Default:** None
**Description:** Custom IAM policy JSON file.

```bash
spawn launch --instance-type m7i.large --iam-policy-file ./my-policy.json
```

#### --iam-managed-policies
**Type:** String (comma-separated ARNs)
**Default:** None
**Description:** AWS managed policy ARNs to attach.

```bash
spawn launch --instance-type m7i.large \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

### User Data

#### --user-data
**Type:** String or `@file`
**Default:** spored installation script
**Description:** User data script. Prefix with `@` to read from file.

```bash
# Inline
spawn launch --instance-type m7i.large --user-data "#!/bin/bash\necho 'Hello'"

# From file
spawn launch --instance-type m7i.large --user-data @setup.sh
```

**Note:** spawn always appends spored installation to user-data.

#### --user-data-file
**Type:** Path
**Default:** None
**Description:** User data script file (alternative to `--user-data @file`).

```bash
spawn launch --instance-type m7i.large --user-data-file setup.sh
```

### Parameter Sweeps

#### --param-file, --params
**Type:** Path (YAML/JSON file)
**Default:** None
**Description:** Parameter file for launching multiple instances.

```bash
spawn launch --param-file sweep.yaml
```

**Parameter File Format:**
```yaml
defaults:
  instance_type: t3.micro
  region: us-east-1
  ttl: 2h

params:
  - name: run-1
    learning_rate: 0.001
    batch_size: 32
  - name: run-2
    learning_rate: 0.01
    batch_size: 64
```

See [Parameter Files](../parameter-files.md) for complete reference.

#### --max-concurrent
**Type:** Integer
**Default:** `5`
**Description:** Maximum concurrent instances for parameter sweeps.

```bash
spawn launch --param-file sweep.yaml --max-concurrent 10
```

#### --launch-delay
**Type:** Duration
**Default:** `5s`
**Description:** Delay between launching instances in sweep.

```bash
spawn launch --param-file sweep.yaml --launch-delay 10s
```

#### --detach
**Type:** Boolean
**Default:** `false`
**Description:** Launch sweep via Lambda (async, survives laptop disconnect).

```bash
spawn launch --param-file sweep.yaml --detach
# CLI exits immediately
# Lambda orchestrates sweep in cloud
# Check status: spawn status --sweep-id <id>
```

#### --wait
**Type:** Boolean
**Default:** `false`
**Description:** Wait for sweep completion.

```bash
spawn launch --param-file sweep.yaml --wait
```

#### --wait-timeout
**Type:** Duration
**Default:** `24h`
**Description:** Timeout for `--wait`.

```bash
spawn launch --param-file sweep.yaml --wait --wait-timeout 4h
```

#### --output-id
**Type:** Path
**Default:** None
**Description:** Write sweep/instance ID to file.

```bash
spawn launch --param-file sweep.yaml --output-id sweep-id.txt
```

### Job Arrays

#### --array, --count
**Type:** Integer
**Default:** `1`
**Description:** Launch N identical instances as job array.

```bash
spawn launch --instance-type t3.small --array 100
```

**Environment Variables on Instances:**
- `JOB_ARRAY_SIZE` - Total instance count
- `JOB_ARRAY_INDEX` - Instance index (0-based)
- `JOB_ARRAY_NAME` - Array name

#### --array-name, --job-array-name
**Type:** String
**Default:** Auto-generated
**Description:** Job array name.

```bash
spawn launch --instance-type t3.small --array 100 --array-name training-sweep
```

#### --group-dns
**Type:** Boolean
**Default:** `false`
**Description:** Enable group DNS for peer discovery.

```bash
spawn launch --instance-type m7i.large --array 8 --group-dns
```

**DNS Record:**
- `<array-name>.<account-base36>.spore.host` → all instance IPs
- Round-robin DNS for load distribution

### Batch Queues

#### --batch-queue, --queue
**Type:** Path (JSON file)
**Default:** None
**Description:** Batch queue configuration file.

```bash
spawn launch --instance-type m7i.large --batch-queue pipeline.json
```

**Queue Config Format:**
```json
{
  "queue_id": "pipeline-20260127",
  "jobs": [
    {
      "job_id": "preprocess",
      "command": "python preprocess.py",
      "retry_count": 3,
      "timeout": "30m"
    },
    {
      "job_id": "train",
      "command": "python train.py",
      "retry_count": 1,
      "timeout": "4h",
      "depends_on": ["preprocess"]
    }
  ]
}
```

See [Queue Configs](../queue-configs.md) for complete reference.

#### --queue-template
**Type:** String
**Default:** None
**Description:** Built-in queue template name.

```bash
spawn launch --instance-type m7i.large --queue-template ml-pipeline
```

### MPI and HPC

#### --mpi-size
**Type:** Integer
**Default:** None
**Description:** Launch MPI cluster with N nodes.

```bash
spawn launch --instance-type c7i.4xlarge --mpi-size 8
```

**Automatic Setup:**
- Passwordless SSH between nodes
- OpenMPI installation
- Hostfile generation
- Leader node designated (index 0)

#### --efa
**Type:** Boolean
**Default:** `false`
**Description:** Enable Elastic Fabric Adapter for low-latency networking.

```bash
spawn launch --instance-type c7i.metal-24xl --mpi-size 4 --efa
```

**Requirements:**
- Instance type must support EFA
- Placement group recommended

#### --placement-group
**Type:** String
**Allowed Values:** `cluster`, `partition`, `spread`
**Default:** None (or `cluster` if `--efa`)
**Description:** Placement group strategy.

```bash
spawn launch --instance-type c7i.4xlarge --mpi-size 8 --placement-group cluster
```

#### --fsx-lustre
**Type:** String (Filesystem ID)
**Default:** None
**Description:** Mount FSx Lustre filesystem.

```bash
spawn launch --instance-type m7i.large --fsx-lustre fs-0123456789abcdef0
```

**Mount Point:** `/fsx`

### Hibernation

#### --hibernate
**Type:** Boolean
**Default:** `false`
**Description:** Enable hibernation support.

```bash
spawn launch --instance-type m7i.large --hibernate
```

**Requirements:**
- Instance family must support hibernation (c5, m5, r5, c6i, m6i, r6i, m7i, m8g)
- Root volume must be encrypted
- Volume size >= instance RAM

**Automatic Configuration:**
- Enables hibernation flag
- Encrypts root volume
- Sets appropriate volume size

### Output and Monitoring

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output JSON format.

```bash
spawn launch --instance-type m7i.large --json
```

**Output:**
```json
{
  "instance_id": "i-0123456789abcdef0",
  "region": "us-east-1",
  "availability_zone": "us-east-1a",
  "public_ip": "54.123.45.67",
  "private_ip": "10.0.1.100",
  "dns_name": "my-instance.c0zxr0ao.spore.host",
  "state": "running"
}
```

#### --quiet
**Type:** Boolean
**Default:** `false`
**Description:** Minimal output (instance ID only).

```bash
INSTANCE_ID=$(spawn launch --instance-type m7i.large --quiet)
```

#### --verbose
**Type:** Boolean
**Default:** `false`
**Description:** Verbose output with detailed progress.

```bash
spawn launch --instance-type m7i.large --verbose
```

#### --wait-for-running
**Type:** Boolean
**Default:** `false`
**Description:** Wait for instance to reach running state.

```bash
spawn launch --instance-type m7i.large --wait-for-running
```

#### --wait-for-ssh
**Type:** Boolean
**Default:** `false`
**Description:** Wait for SSH to be available.

```bash
spawn launch --instance-type m7i.large --wait-for-ssh
```

### Naming and Tagging

#### --name
**Type:** String
**Default:** Auto-generated
**Description:** Instance name (sets `Name` tag).

```bash
spawn launch --instance-type m7i.large --name my-dev-instance
```

#### --tags
**Type:** String (comma-separated key=value pairs)
**Default:** None
**Description:** Additional tags for instance.

```bash
spawn launch --instance-type m7i.large --tags env=prod,team=ml,project=training
```

**Automatic Tags:**
- `spawn:managed=true`
- `spawn:root=true`
- `spawn:created-at=<timestamp>`
- `spawn:ttl=<duration>` (if `--ttl` specified)
- `spawn:idle-timeout=<duration>` (if `--idle-timeout` specified)
- `spawn:cost-limit=<amount>` (if `--cost-limit` specified)
- `spawn:price-per-hour=<price>` (if `--cost-limit` specified; on-demand rate at launch time)
- `spawn:pre-stop=<command>` (if `--pre-stop` specified)
- `spawn:pre-stop-timeout=<duration>` (if `--pre-stop-timeout` specified)

### Capacity Reservations

#### --use-reservation
**Type:** Boolean
**Default:** `false`
**Description:** Use capacity reservation if available.

```bash
spawn launch --instance-type p5.48xlarge --use-reservation
```

#### --capacity-reservation-id
**Type:** String
**Default:** None
**Description:** Specific capacity reservation ID.

```bash
spawn launch --instance-type p5.48xlarge \
  --capacity-reservation-id cr-0123456789abcdef0
```

## Examples

### Quick Development Instance
```bash
spawn launch --instance-type t3.medium --ttl 8h --name dev-box
```

### GPU Training Job
```bash
spawn launch \
  --instance-type g5.xlarge \
  --ttl 12h \
  --idle-timeout 1h \
  --user-data @train.sh \
  --name ml-training
```

### Spot Instance with Hibernation
```bash
spawn launch \
  --instance-type m7i.large \
  --spot \
  --spot-max-price 0.05 \
  --hibernate \
  --idle-timeout 1h \
  --hibernate-on-idle
```

### Instance with S3 Access
```bash
spawn launch \
  --instance-type m7i.large \
  --iam-policy s3:ReadOnly,logs:WriteOnly \
  --ttl 4h
```

### Parameter Sweep (Detached)
```bash
spawn launch \
  --param-file sweep.yaml \
  --max-concurrent 10 \
  --detach
```

### MPI Cluster
```bash
spawn launch \
  --instance-type c7i.4xlarge \
  --mpi-size 8 \
  --placement-group cluster \
  --ttl 24h
```

### Batch Pipeline
```bash
spawn launch \
  --instance-type m7i.large \
  --batch-queue pipeline.json \
  --on-complete terminate \
  --ttl 6h
```

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Instance launched successfully |
| 1 | Launch failed (AWS API error, network error) |
| 2 | Invalid arguments (missing required flags, invalid format) |
| 3 | No capacity available |
| 4 | Permission denied (insufficient IAM permissions) |

## See Also
- [spawn list](list.md) - List instances
- [spawn connect](connect.md) - Connect to instance
- [spawn extend](extend.md) - Extend instance TTL
- [Parameter Files](../parameter-files.md) - Parameter sweep format
- [Queue Configs](../queue-configs.md) - Batch queue format
- [IAM Policies](../iam-policies.md) - IAM policy templates
