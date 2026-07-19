## `spawn launch`

*Aliases: , run, create*

Launch an EC2 instance with smart defaults.

Three ways to use:
  1. Interactive wizard (default if no input)
  2. From truffle JSON via pipe
  3. Direct with flags

Examples:
  # Interactive wizard
  spawn launch

  # From truffle
  truffle search m7i.large | spawn launch

  # Direct
  spawn launch --instance-type m7i.large --region us-east-1

```
spawn launch <name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--active-ports` |  | string |  | TCP ports to monitor for active connections, prevents idle termination (e.g. '8787' for RStudio, '8787,8888' for RStudio+Jupyter) |
| `--active-processes` |  | string |  | Process names to monitor, prevents idle termination while any are running (e.g. 'rsession' for RStudio, 'rsession,jupyter' for multiple) |
| `--allow-cidr` |  | string |  | CIDR allowed to reach the managed Windows security group (RDP 3389 + SSH 22); default 0.0.0.0/0 |
| `--ami` |  | string |  | AMI ID (ami-...); omit or use 'auto' to auto-detect the latest AL2023 |
| `--attach-volume` |  | stringArray |  | Attach an EBS volume from a snapshot, mounted at a path: snap-xxx:/mount/point[:ro]. Repeatable. Read-only is the common case for shared reference data. |
| `--auto-placement-group` |  | bool | `true` | Automatically create placement group for MPI job arrays (default: true) |
| `--az` |  | string |  | Availability zone |
| `--batch-queue` |  | string |  | Batch job queue file (JSON) for sequential execution |
| `--budget` |  | float64 |  | Budget limit in dollars for parameter sweeps (0 = no limit) |
| `--capacity-block` |  | bool |  | The --reservation-id is a Capacity Block for ML (sets MarketType=capacity-block); mutually exclusive with --spot (#216) |
| `--cartesian` |  | bool |  | Generate cartesian product of parameter lists |
| `--command` |  | string |  | Command to run on all instances (executed after spored setup) |
| `--completion-delay` |  | string | `30s` | Grace period after completion signal |
| `--completion-file` |  | string | `/tmp/SPAWN_COMPLETE` | File to watch for completion signal |
| `--compliance-strict` |  | bool |  | Strict mode: fail on warnings (default: show warnings only) |
| `--config` |  | string |  | Launch config YAML file (supports plugins: list) |
| `--cost-limit` |  | float64 |  | Terminate/stop when compute spend reaches this amount in USD (compute cost only; 0 = disabled) |
| `--cost-tier` |  | string |  | Prefer cost tier: low, standard, premium |
| `--count` |  | int | `1` | Number of instances to launch (job array) |
| `--detach` |  | bool |  | Run sweep orchestration in Lambda (auto-enabled for parameter sweeps) |
| `--dns-api-endpoint` |  | string |  | Custom DNS API endpoint (overrides default) |
| `--dns-domain` |  | string |  | Custom DNS domain (overrides default) |
| `--dns` |  | string |  | Override DNS name if different from --name (advanced) |
| `--efa` |  | bool |  | Enable Elastic Fabric Adapter for ultra-low latency MPI (requires supported instance types) |
| `--efs-id` |  | string |  | EFS filesystem ID to mount (fs-xxx) |
| `--efs-mount-options` |  | string |  | Custom EFS mount options (overrides profile) |
| `--efs-mount-point` |  | string | `/efs` | EFS mount point (default: /efs) |
| `--efs-profile` |  | string | `general` | EFS performance profile: general, max-io, max-throughput, burst |
| `--estimate-only` |  | bool |  | Show cost estimate and exit without launching |
| `--fsx-create` |  | bool |  | Create new FSx Lustre filesystem with S3 backing (requires --fsx-lifecycle) |
| `--fsx-export-path` |  | string |  | S3 path to export to (e.g., s3://bucket/prefix) |
| `--fsx-id` |  | string |  | Existing FSx Lustre filesystem ID to mount (fs-xxx) |
| `--fsx-import-path` |  | string |  | S3 path to import from (e.g., s3://bucket/prefix) |
| `--fsx-lifecycle` |  | string |  | FSx lifetime (REQUIRED with --fsx-create): 'ephemeral' (reaped when this instance terminates) or 'durable' (persists; requires --fsx-ttl) |
| `--fsx-mount-point` |  | string | `/fsx` | FSx mount point (default: /fsx) |
| `--fsx-recall` |  | string |  | Recall FSx filesystem by stack name (recreate from S3) |
| `--fsx-s3-bucket` |  | string |  | S3 bucket for FSx import/export (required with --fsx-create) |
| `--fsx-skip-validate` |  | bool |  | Skip FSx filesystem validation (for testing) |
| `--fsx-storage-capacity` |  | int32 | `1200` | FSx storage capacity in GB (1200, 2400, or increments of 2400) |
| `--fsx-throughput` |  | int32 | `125` | FSx PERSISTENT_2 throughput in MB/s/TiB (125, 250, 500, or 1000; default: 125) |
| `--fsx-ttl` |  | string |  | FSx time-to-live, required for --fsx-lifecycle=durable (e.g. 7d, 720h) — the filesystem is reaped this long after creation once no instance is using it |
| `--hibernate` |  | bool |  | Enable hibernation |
| `--iam-allow-full-access` |  | bool |  | Permit wildcard *:FullAccess --iam-policy templates (s3:*/dynamodb:*/sqs:* on all resources) on the instance role; off by default — prefer scoped ReadOnly/WriteOnly |
| `--iam-managed-policies` |  | stringSlice |  | AWS managed policy ARNs |
| `--iam-policy-file` |  | string |  | Custom IAM policy JSON file |
| `--iam-policy` |  | stringSlice |  | Service-level policies (e.g., s3:ReadOnly,dynamodb:WriteOnly). Wildcard *:FullAccess templates require --iam-allow-full-access |
| `--iam-role-tags` |  | stringSlice |  | Tags for IAM role (key=value format) |
| `--iam-role` |  | string |  | IAM role name (creates if doesn't exist) |
| `--iam-trust-services` |  | stringSlice | `[ec2]` | Services that can assume role |
| `--idle-timeout` |  | string |  | Auto-terminate if idle (defaults to 1h if neither --ttl nor --idle-timeout set) |
| `--instance-names` |  | string |  | Instance name template (e.g., 'worker-{index}', default: '{job-array-name}-{index}') |
| `--instance-type` |  | string |  | Instance type |
| `--interactive` |  | bool |  | Force interactive wizard |
| `--job-array-name` |  | string |  | Job array group name (required if --count > 1) |
| `--key-name` |  | string |  | SSH key pair name (EC2 KeyName) |
| `--launch-delay` |  | string | `0s` | Delay between instance launches (e.g., 5s) |
| `--max-concurrent-per-region` |  | int |  | Max instances running simultaneously per region (0 = unlimited) |
| `--max-concurrent` |  | int |  | Max instances running simultaneously (0 = unlimited) |
| `--min-viable` |  | int | `1` | Job array: minimum members that must launch for success (default 1; ignored for --mpi) |
| `--mode` |  | string | `balanced` | Distribution mode: balanced (fair share) or opportunistic (prioritize available regions) |
| `--mpi-command` |  | string |  | Command to run via mpirun (alternative to --command) |
| `--mpi-processes-per-node` |  | int |  | MPI processes per node (default: vCPU count) |
| `--mpi` |  | bool |  | Enable MPI cluster setup (requires --count > 1) |
| `--name` |  | string |  | Name your spore, required (sets Name tag, DNS, and hostname) |
| `--nested-virtualization` |  | bool |  | Enable nested virtualization (run KVM/Hyper-V inside the instance). Requires a C8i/M8i/R8i instance type. |
| `--nist-800-171` |  | bool |  | Enable NIST 800-171 Rev 3 compliance mode |
| `--nist-800-53` |  | string |  | Enable NIST 800-53 compliance (low, moderate, high) |
| `--no-detach` |  | bool |  | Disable auto-detach for parameter sweeps (requires --ttl or --idle-timeout) |
| `--no-timeout` |  | bool |  | Disable automatic timeout (NOT RECOMMENDED: creates zombie risk) |
| `--notify-platform` |  | string |  | Chat platform for lifecycle notifications: slack (default), teams, or discord |
| `--on-complete` |  | string |  | Action when workload signals completion: terminate, stop, hibernate. Use 'terminate' for batch/headless workloads — 'stop' leaves EBS (and any attached EIP) billing indefinitely, which is easy to forget in accounts without a hosted reaper |
| `--on-idle` |  | string |  | Action when the instance goes idle: stop (default) or hibernate. Mirrors --on-complete. NOTE: a stopped/hibernated instance keeps billing for its EBS volumes (and any attached Elastic IP) — for batch/headless work prefer --on-complete terminate so cost is fully bounded |
| `--os` |  | string |  | Target OS: windows or linux. Omit to auto-detect from the AMI. Use to force the OS for a custom AMI whose platform metadata is unset. |
| `--output-id` |  | string |  | Write sweep/instance ID to file for scripting |
| `--param-file` |  | string |  | Path to parameter sweep file (JSON/YAML/CSV) |
| `--params` |  | string |  | Inline JSON parameters for sweep |
| `--placement-group` |  | string |  | AWS Placement Group for MPI instances (auto-created if not specified) |
| `--plugin` |  | stringArray |  | Plugin to install at launch (ref[@version], repeatable) |
| `--pre-stop-timeout` |  | string |  | Max time to wait for --pre-stop command (default: 5m, spot: 90s) |
| `--pre-stop` |  | string |  | Shell command to run on the instance before any lifecycle-triggered stop/terminate (e.g., "aws s3 sync /results s3://bucket/") |
| `--proximity-from` |  | string |  | Prefer regions close to this region (e.g., us-east-1) |
| `--queue-template` |  | string |  | Queue template name (use 'spawn queue template list' to see options) |
| `--quiet` |  | bool |  | Minimal output |
| `--region` |  | string |  | AWS region |
| `--regions-exclude` |  | stringSlice |  | Exclude these regions (supports wildcards: us-*, eu-*) |
| `--regions-geographic` |  | stringSlice |  | Geographic constraints: us, eu, ap, north-america, europe, asia-pacific |
| `--regions-include` |  | stringSlice |  | Only use these regions (supports wildcards: us-*, eu-*) |
| `--reservation-id` |  | string |  | Capacity Reservation / Capacity Block ID to launch into (fs-/cr-...) — instance must be in the reservation's AZ (#216) |
| `--security-group-ids` |  | stringSlice |  | Security group IDs (comma-separated or repeated) |
| `--session-timeout` |  | string | `30m` | Auto-logout idle shells (0 to disable) |
| `--skip-mpi-install` |  | bool |  | Skip MPI installation (use with custom AMIs that have MPI pre-installed) |
| `--skip-region-check` |  | bool |  | Skip data locality region mismatch warnings |
| `--slack-workspace` |  | string |  | Slack workspace ID for lifecycle notifications (e.g. T03NE3GTY) |
| `--spot-max-price` |  | string |  | Max Spot price |
| `--spot-webhook-url` |  | string |  | On spot interruption, spored POSTs a fire-once, best-effort notice to this URL within the ~2-min window (off-node consumers; empty = disabled) |
| `--spot` |  | bool |  | Launch as Spot instance |
| `--strata-formation` |  | string |  | Strata formation to activate (e.g. r-research@2024.03) |
| `--strata-profile` |  | string |  | Path to a Strata profile YAML file |
| `--strata-registry` |  | string | `s3://strata-registry` | Strata registry S3 URL |
| `--subnet-id` |  | string |  | Subnet ID |
| `--sweep-name` |  | string |  | Human-readable sweep identifier (auto-generated if empty) |
| `--tag` |  | stringArray |  | Custom tag key=value on the instance and its created volumes (repeatable). The spawn: prefix is reserved. |
| `--team` |  | string |  | Team ID: tag instance with spawn:team-id for team-shared access |
| `--template-var` |  | stringToString |  | Template variables (key=value) |
| `--terminate-on-error` |  | bool |  | If post-launch verification fails (e.g. spored didn't come up), terminate the instance instead of leaving it running |
| `--ttl` |  | string |  | Auto-terminate after duration (e.g., 8h, defaults to 1h idle if not set) |
| `--use-reservation` |  | bool |  | Use capacity reservation |
| `--user-data-file` |  | string |  | User data file |
| `--user-data` |  | string |  | User data (@file or inline) |
| `--volume-size` |  | int32 |  | Root EBS volume size in GiB (0 = use AMI default) |
| `--vpc` |  | string |  | VPC ID |
| `--wait-for-running` |  | bool | `true` | Wait until running |
| `--wait-for-ssh` |  | bool | `true` | Wait until SSH is ready |
| `--wait-timeout` |  | string |  | Timeout for --wait (e.g., 2h, 30m, 0=no timeout) |
| `--wait` |  | bool |  | Wait for sweep/launch to complete (requires --detach) |
| `--webhook-correlation` |  | string |  | Opaque blob echoed verbatim in the spot-webhook payload so a consumer can correlate the event to its own record (never parsed by spawn) |
| `--webhook-timeout` |  | string |  | Hard cap on the spot-webhook POST so it can't eat the reclamation window (default: 2s) |
| `--yes` | `-y` | bool |  | Auto-approve cost estimate (skip confirmation) |

