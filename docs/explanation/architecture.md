# Architecture Overview

Understanding how spawn works under the hood.

## System Architecture

spawn is a CLI tool that orchestrates ephemeral AWS EC2 instances with automatic lifecycle management. The system consists of three main components:

```
┌─────────────────────────────────────────────────────────────┐
│                         User's Machine                       │
│                                                              │
│  ┌──────────────┐                                           │
│  │  spawn CLI   │  (Go binary)                              │
│  │              │  - Parse commands                         │
│  │              │  - AWS API calls                          │
│  │              │  - State tracking                         │
│  └──────┬───────┘                                           │
└─────────┼──────────────────────────────────────────────────┘
          │
          │ AWS API calls
          │
          ▼
┌─────────────────────────────────────────────────────────────┐
│                        AWS Account                           │
│                                                              │
│  ┌────────────┐  ┌──────────────┐  ┌─────────────┐         │
│  │ EC2        │  │  DynamoDB    │  │  Lambda     │         │
│  │ Instances  │  │  Tables      │  │  Functions  │         │
│  │            │  │              │  │             │         │
│  │ ┌────────┐ │  │ - Metadata  │  │ - DNS       │         │
│  │ │ spored │ │  │ - Queue     │  │   updater   │         │
│  │ │ agent  │ │  │ - Alerts    │  │ - Alerts    │         │
│  │ └────────┘ │  │             │  │             │         │
│  └────────────┘  └──────────────┘  └─────────────┘         │
│                                                              │
│  ┌────────────┐  ┌──────────────┐  ┌─────────────┐         │
│  │ S3         │  │  Route53     │  │  CloudWatch │         │
│  │ Buckets    │  │  DNS         │  │  Logs       │         │
│  │            │  │              │  │             │         │
│  │ - Binaries │  │ - spore.host │  │ - Metrics   │         │
│  │ - Results  │  │   zone       │  │ - Alarms    │         │
│  └────────────┘  └──────────────┘  └─────────────┘         │
└─────────────────────────────────────────────────────────────┘
```

## Component Details

### spawn CLI (User's Machine)

**Purpose:** Command-line interface for managing instances.

**Language:** Go

**Responsibilities:**
- Parse user commands and flags
- Validate input parameters
- Make AWS API calls (EC2, DynamoDB, S3, Lambda)
- Track instance state locally
- Format and display output

**Key files:**
- `cmd/*.go` - Command definitions
- `pkg/aws/` - AWS client wrapper
- `pkg/params/` - Parameter sweep parsing
- `pkg/queue/` - Batch queue management

### spored Agent (On EC2 Instances)

**Purpose:** Background daemon managing instance lifecycle.

**Language:** Go

**Runs as:** systemd service

**Responsibilities:**
- TTL enforcement (terminate on expiration)
- Idle detection (monitor CPU/network)
- Spot interruption monitoring (poll metadata endpoint)
- DNS registration (via Lambda)
- Cleanup on termination

**Installation:**
- Injected via user-data on instance launch
- Downloaded from S3 bucket
- Starts on boot via systemd

**Configuration:**
- `/etc/spored/config.yaml`
- Environment variables
- Instance metadata

### AWS Resources

#### EC2 Instances
**Purpose:** Compute resources for user workloads.

**Lifecycle:**
```
launch → running → [stopping] → stopped → [pending] → running
   ↓                   ↓
terminate           terminate
   ↓                   ↓
terminated          terminated
```

**Tags used:**
- `spawn=true` - Identifies spawn instances
- `Name` - Instance name
- `ttl` - Expiration timestamp
- `owner` - User identifier
- Custom tags from launch

#### DynamoDB Tables

**spawn-instances:**
- Stores instance metadata
- Tracks TTL, state, launch parameters
- Enables cross-session querying

**spawn-queues:**
- Batch queue state
- Job dependencies
- Execution status

**spawn-alerts:**
- Alert destinations
- Notification preferences

**spawn-availability-stats:**
- Historical launch success/failure rates
- Per instance type per region

#### S3 Buckets

**spawn-binaries-{region}:**
- spored binary distribution
- Version controlled
- Public read access

**spawn-results-{region}:**
- User data output
- Job results
- Checkpoints

#### Lambda Functions

**spawn-dns-updater:**
- Registers DNS records on instance launch
- Deregisters on termination
- Updates Route53 hosted zone

**spawn-alert-handler:**
- Processes alert events
- Sends notifications (Slack, email, SNS)

#### Route53

**spore.host hosted zone:**
- Subdomain per instance: `{name}.{account-base36}.spore.host`
- A records point to instance public IPs
- Automatic cleanup on termination

## Data Flow

### Instance Launch Flow

```
1. User: spawn launch --instance-type c7i.xlarge --ttl 2h

2. CLI validates parameters
   └─> Check instance type exists
   └─> Parse TTL duration
   └─> Validate IAM permissions

3. CLI calls EC2 RunInstances API
   └─> User data script includes:
       - Download spored from S3
       - Configure spored (TTL, idle, etc.)
       - Start spored as systemd service
       - Run user's workload script

4. Instance boots
   └─> cloud-init runs user data
   └─> spored starts
   └─> spored registers DNS via Lambda

5. CLI writes metadata to DynamoDB
   └─> Instance ID, launch time, TTL
   └─> Parameters, tags

6. CLI returns control to user
   └─> Instance continues running
```

### TTL Enforcement Flow

```
1. spored checks TTL every 30 seconds
   └─> Read TTL from config or instance metadata
   └─> Calculate remaining time

2. If TTL < warn_minutes:
   └─> Send wall message to logged-in users
   └─> Log warning

3. If TTL expired:
   └─> Run cleanup actions:
       ├─> Deregister DNS (via Lambda)
       ├─> Send completion notifications
       └─> Upload final results to S3 (if configured)
   └─> Terminate instance via AWS API

4. Instance terminates
   └─> EC2 cleans up resources
   └─> EBS volumes deleted (unless configured otherwise)
```

### Spot Interruption Flow

```
1. spored polls metadata endpoint every 5 seconds:
   http://169.254.169.254/latest/meta-data/spot/instance-action

2. If interruption notice received:
   └─> Parse interruption time (typically 2 minutes warning)
   └─> Log alert: "Spot interruption in 2 minutes"

3. Run cleanup actions:
   ├─> Send wall message to users
   ├─> Deregister DNS
   ├─> Send notifications
   ├─> Run user-defined pre-termination hook
   └─> Write interruption marker file

4. Wait for AWS to terminate instance
   └─> No need to call TerminateInstances
   └─> AWS handles termination
```

## Security Model

### Authentication & Authorization

**User credentials:**
- AWS credentials from `~/.aws/credentials` or environment
- IAM user or assumed role
- Permissions validated per operation

**Instance credentials:**
- IAM instance profile attached at launch
- Temporary credentials via instance metadata service (IMDSv2)
- Scoped to minimum required permissions

### Network Security

**Default configuration:**
- Public IP assigned (customizable)
- Default security group (SSH from anywhere)
- **Security risk:** Users should use restrictive security groups

**Recommended configuration:**
- Private subnet + bastion host
- Security group allowing SSH only from known IPs
- Session Manager for SSH access (no public IP)

### Data Security

**Secrets:**
- Never log credentials or tokens
- Use AWS Secrets Manager or SSM Parameter Store
- Inject via environment variables, not user-data

**EBS encryption:**
- Optional at launch via `--encrypt-volumes`
- Uses default AWS KMS key or custom key
- All snapshots encrypted

## Scalability

### Concurrent Operations

**CLI operations:**
- Parallel launches supported
- AWS API rate limits apply (varies by region)
- Default: No client-side throttling

**Recommended limits:**
- < 100 concurrent launches
- Use `--array` for 100+ instances
- Use parameter sweeps with `max_concurrent` for control

### Data Storage Limits

**DynamoDB:**
- 400 KB per item limit
- Not a concern for instance metadata
- Queue configs may hit limit for very large DAGs

**S3:**
- No practical limits for spawn usage
- User data: max 16 KB (EC2 limit)

## Multi-Account Architecture

### Account Separation

**Infrastructure Account:**
- S3 buckets (binaries)
- Lambda functions
- Route53 hosted zone
- No EC2 instances

**Workload Accounts:**
- All EC2 instance provisioning
- DynamoDB tables (per-account state)
- Isolated blast radius

**Cross-account access:**
- IAM roles with trust relationships
- S3 bucket policies for binary access
- Lambda function invoke permissions

### DNS Across Accounts

**Approach:**
- VPC association authorization
- Private hosted zone shared across accounts
- Each instance gets: `{name}.{account-base36}.spore.host`

## Performance Characteristics

### Launch Time

**Typical times:**
- User runs `spawn launch`: 0-2s (CLI overhead)
- EC2 RunInstances API: 2-5s
- Instance pending → running: 30-60s
- cloud-init + user-data: 10-120s (depends on script)
- **Total:** 1-3 minutes for simple instances

**Optimizations:**
- Use custom AMIs to reduce user-data time
- Pre-install dependencies in AMI
- Use smaller instance types for faster boot

### Resource Usage

**CLI memory:** ~20-50 MB per invocation

**spored memory:** ~10-20 MB per instance

**Network bandwidth:**
- Initial spored download: ~10 MB
- DNS registration: < 1 KB
- Metadata polling: < 1 KB/minute

## Failure Modes

### Instance Launch Failures

**Causes:**
- Insufficient capacity
- Invalid parameters
- IAM permission denied
- Network errors

**Handling:**
- CLI returns error immediately
- No partial state created
- User can retry

### spored Failures

**If spored crashes:**
- systemd automatically restarts (configured)
- TTL enforcement continues after restart
- May miss spot interruption during downtime

**If spored fails to start:**
- Instance continues running
- No TTL enforcement
- No automatic termination
- **Mitigation:** Always set EC2 instance TTL as backup

### AWS Service Outages

**EC2 outage:**
- No new launches possible
- Running instances unaffected
- spored continues operating

**DynamoDB outage:**
- Instance metadata writes fail
- Instance launch succeeds
- `spawn list` may be incomplete
- Instances still terminate via spored

**S3 outage:**
- spored download fails
- User-data script fails
- Instance launches without spored
- **Mitigation:** Package spored in AMI

## Monitoring & Observability

### What to Monitor

**CLI-side:**
- Launch success/failure rates
- API error rates
- Command execution times

**Instance-side:**
- spored running status
- TTL remaining
- Idle state
- Spot interruption events

**AWS-side:**
- EC2 instance state changes
- DynamoDB read/write capacity
- Lambda invocation counts and errors
- Route53 query counts

### Logging

**CLI logs:**
- stderr for errors
- stdout for output
- `--debug` flag for verbose logging

**spored logs:**
- systemd journal: `journalctl -u spored`
- Optional CloudWatch Logs integration

**AWS CloudTrail:**
- All API calls logged
- Useful for audit and debugging

## Design Decisions

### Why Go?

**Pros:**
- Single binary distribution (no dependencies)
- Cross-platform compilation
- Excellent AWS SDK support
- Fast performance
- Strong concurrency primitives

### Why systemd for spored?

**Pros:**
- Automatic restart on failure
- Standard init system on modern Linux
- Easy log management via journalctl
- Well-understood by ops teams

**Cons:**
- Requires Linux (Amazon Linux, Ubuntu)
- Not available on Windows instances

### Why DynamoDB for State?

**Pros:**
- Serverless (no infrastructure to manage)
- Fast queries
- Auto-scaling
- Strong consistency

**Cons:**
- Cost for high-volume usage
- 400 KB item size limit

**Alternative considered:** S3 JSON files (too slow for queries)

### Why Route53 for DNS?

**Pros:**
- Integrated with AWS
- Automatic TTL management
- Subdomain per instance pattern

**Cons:**
- Cost per hosted zone ($0.50/month)
- DNS propagation delay (though minimal)

**Alternative considered:** /etc/hosts (requires SSH, not scalable)

## Extension Points

### Custom Workload Scripts

**user-data hook:**
- Bash script injected at launch
- Runs after spored starts
- Access to all environment variables

**Example:**
```bash
spawn launch --user-data @my-script.sh
```

### Alert Destinations

**spawn alerts system:**
- Slack webhooks
- SNS topics
- Email (via SES)
- Custom HTTP endpoints

**Example:**
```bash
spawn alerts add-destination --type slack --target <webhook-url>
```

### Batch Queue Hooks

**Pre/post job hooks:**
- Run script before job starts
- Run script after job completes
- Validation, cleanup, notifications

## Related Documentation

- [Core Concepts](core-concepts.md) - Deep dive into TTL, idle detection, spot
- [Security Model](security-model.md) - Security architecture
- [Multi-Account Architecture](multi-account.md) - Cross-account setup
- [spored Agent Design](spored-design.md) - Agent internals
