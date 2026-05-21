# Detached Mode Architecture

Technical guide to Lambda-orchestrated parameter sweeps in spawn CLI.

---

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Components](#components)
- [Data Flow](#data-flow)
- [Implementation Details](#implementation-details)
- [Cost & Performance](#cost--performance)
- [Infrastructure Setup](#infrastructure-setup)
- [Troubleshooting](#troubleshooting)
- [Development Guide](#development-guide)

---

## Overview

**Detached mode** enables parameter sweeps to run independently of the CLI by orchestrating launches via AWS Lambda. The Lambda function polls EC2 instance states and launches new instances as capacity becomes available, implementing a rolling queue pattern.

> **⚠️ DEFAULT BEHAVIOR**: As of v0.15.0, detached mode is **automatically enabled** for all parameter sweeps to prevent zombie instances. Users launching parameter sweeps without `--detach` will see it auto-enabled. To opt-out, use `--no-detach` (requires `--ttl` or `--idle-timeout` for safety).

### Key Benefits

- **Survives disconnection** - Sweep continues if laptop sleeps, network drops, or terminal closes
- **Remote monitoring** - Check status from any machine with AWS credentials
- **Multi-hour sweeps** - Self-reinvoking Lambda handles unlimited duration
- **Resume capability** - Continue from checkpoint on any machine
- **Low cost** - ~$0.005 per sweep

### Use Cases

- Hyperparameter tuning overnight
- Multi-region latency testing
- Long-running batch processing
- Production ML training sweeps
- Any sweep >30 minutes

---

## Architecture

### Component Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│ User Workstation                                                        │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  spawn CLI                                                               │
│    │                                                                     │
│    ├─> 1. Load parameter file (local)                                   │
│    ├─> 2. Get AWS account ID (STS)                                      │
│    ├─> 3. Validate parameters (optional)                                │
│    ├─> 4. Upload params to S3                                           │
│    ├─> 5. Create DynamoDB record                                        │
│    ├─> 6. Invoke Lambda                                                 │
│    └─> 7. Exit (disconnection OK)                                       │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ spore-host-infra Account (966362334030)                                  │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  S3: spawn-sweeps-us-east-1                                              │
│    └─> sweeps/<sweep-id>/params.json  (unlimited size)                  │
│                                                                          │
│  DynamoDB: spawn-sweep-orchestration                                     │
│    ├─> Primary Key: sweep_id (HASH)                                     │
│    ├─> GSI: user_id-created_at-index                                    │
│    └─> Stores: status, progress, instances, errors                      │
│                                                                          │
│  Lambda: spawn-sweep-orchestrator                                        │
│    ├─> Runtime: Go (custom provided.al2023)                             │
│    ├─> Timeout: 15 minutes (13min active + 2min buffer)                 │
│    ├─> Memory: 512 MB                                                   │
│    ├─> Concurrency: Unlimited (on-demand)                               │
│    │                                                                     │
│    └─> Execution flow:                                                  │
│         1. Load sweep state from DynamoDB                                │
│         2. Download params from S3 (cache in /tmp)                       │
│         3. If INITIALIZING: Setup resources, set status=RUNNING          │
│         4. Polling loop (every 10s, up to 13 minutes):                   │
│            ├─> AssumeRole to spore-host-dev                                │
│            ├─> Query EC2 instance states                                 │
│            ├─> Calculate available slots                                 │
│            ├─> Launch next batch (available slots)                       │
│            ├─> Update DynamoDB state                                     │
│            ├─> Check timeout → Re-invoke self                            │
│            └─> Check completion → Set status=COMPLETED                   │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    │ AssumeRole(SpawnSweepCrossAccountRole)
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ spore-host-dev Account (435415984226)                                    │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  EC2 Instances                                                           │
│    ├─> Launched by Lambda via cross-account role                        │
│    ├─> Tagged with spawn:sweep-id                                       │
│    ├─> Tagged with spawn:sweep-index                                    │
│    └─> IAM role: spawnd-role (for spored agent)                         │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

### Cross-Account Pattern

**Why cross-account?**
- **Security isolation** - Infrastructure (Lambda, S3, DynamoDB) separate from workloads (EC2)
- **Cost tracking** - Separate billing per environment
- **Resource organization** - Infrastructure account vs compute accounts
- **Scalability** - Support multiple compute accounts

**Trust relationship:**
```
spore-host-infra (Lambda) → AssumeRole → spore-host-dev (EC2 launches)
```

**Alternative:** Single account setup possible (use same account ID for both).

---

## Components

### 1. S3 Bucket (spawn-sweeps-us-east-1)

**Purpose:** Store parameter files (unlimited size).

**Structure:**
```
spawn-sweeps-us-east-1/
└── sweeps/
    └── <sweep-id>/
        └── params.json
```

**Lifecycle:**
- Retention: 30 days
- Auto-delete after 30 days
- Versioning: Disabled

**Permissions:**
- Lambda: Read access
- CLI: Write access

### 2. DynamoDB Table (spawn-sweep-orchestration)

**Purpose:** Store sweep state for remote access.

**Schema:**
```go
type SweepRecord struct {
    // Primary key
    SweepID string `dynamodbav:"sweep_id"` // HASH

    // Metadata
    SweepName     string `dynamodbav:"sweep_name"`
    UserID        string `dynamodbav:"user_id"`      // IAM ARN
    CreatedAt     string `dynamodbav:"created_at"`   // ISO8601
    UpdatedAt     string `dynamodbav:"updated_at"`   // ISO8601
    CompletedAt   string `dynamodbav:"completed_at,omitempty"`

    // Configuration
    S3ParamsKey     string `dynamodbav:"s3_params_key"`     // s3://bucket/key
    MaxConcurrent   int    `dynamodbav:"max_concurrent"`
    LaunchDelay     string `dynamodbav:"launch_delay"`
    TotalParams     int    `dynamodbav:"total_params"`
    Region          string `dynamodbav:"region"`
    AWSAccountID    string `dynamodbav:"aws_account_id"`

    // State
    Status        string `dynamodbav:"status"` // INITIALIZING | RUNNING | COMPLETED | FAILED
    NextToLaunch  int    `dynamodbav:"next_to_launch"`
    Launched      int    `dynamodbav:"launched"`
    Failed        int    `dynamodbav:"failed"`
    ErrorMessage  string `dynamodbav:"error_message,omitempty"`

    // Instance tracking
    Instances []SweepInstance `dynamodbav:"instances"`
}

type SweepInstance struct {
    Index        int    `dynamodbav:"index"`
    InstanceID   string `dynamodbav:"instance_id"`
    State        string `dynamodbav:"state"`
    LaunchedAt   string `dynamodbav:"launched_at"`
    TerminatedAt string `dynamodbav:"terminated_at,omitempty"`
    ErrorMessage string `dynamodbav:"error_message,omitempty"`
}
```

**GSI:** `user_id-created_at-index`
- Query all sweeps for a user
- Sort by creation time (newest first)

**Billing:** Pay-per-request (no provisioned capacity).

### 3. Lambda Function (spawn-sweep-orchestrator)

**Runtime:** Go 1.21 (custom provided.al2023)

**Configuration:**
- Timeout: 15 minutes (900 seconds)
- Memory: 512 MB
- Concurrency: Unlimited (on-demand)
- Package size: ~7.4 MB

**Permissions:**
- DynamoDB: Read/Write to spawn-sweep-orchestration
- S3: Read from spawn-sweeps-us-east-1
- Lambda: Invoke self (for re-invocation)
- STS: AssumeRole to cross-account role

**Environment:**
- None (stateless)

**Invocation:**
```json
{
  "sweep_id": "sweep-20260116-abc123",
  "force_download": false
}
```

### 4. IAM Roles

**SpawnSweepOrchestratorRole** (spore-host-infra):
- Trust: lambda.amazonaws.com
- Permissions: DynamoDB, S3, Lambda invoke, STS AssumeRole

**SpawnSweepCrossAccountRole** (spore-host-dev):
- Trust: arn:aws:iam::966362334030:role/SpawnSweepOrchestratorRole
- Permissions: EC2 RunInstances, DescribeInstances, TerminateInstances, IAM PassRole

**spawnd-role** (spore-host-dev):
- Trust: ec2.amazonaws.com
- Permissions: S3 GetObject (spored binary), Lambda invoke (DNS updater), EC2 describe/terminate (self)

---

## Data Flow

### Launch Sequence

```
1. CLI → STS GetCallerIdentity
   └─> Get AWS account ID (435415984226)

2. CLI → Validate parameters (optional)
   └─> Check instance types exist in regions

3. CLI → S3 PutObject
   └─> Upload params.json to s3://spawn-sweeps-us-east-1/sweeps/<id>/params.json

4. CLI → DynamoDB PutItem
   └─> Create sweep record with status=INITIALIZING

5. CLI → Lambda Invoke (RequestResponse)
   └─> Synchronous invoke with sweep_id

6. CLI → Exit
   └─> User can disconnect

7. Lambda → DynamoDB GetItem
   └─> Load sweep state

8. Lambda → S3 GetObject
   └─> Download params.json (cache in /tmp)

9. Lambda → Update status=RUNNING
   └─> DynamoDB PutItem

10. Lambda → Polling Loop (every 10s):
    ├─> STS AssumeRole (SpawnSweepCrossAccountRole)
    ├─> EC2 DescribeInstances (query active count)
    ├─> Calculate available = maxConcurrent - activeCount
    ├─> For each available slot:
    │   ├─> EC2 RunInstances (with tags, user data, IAM role)
    │   └─> Append to state.Instances
    ├─> DynamoDB PutItem (save state)
    ├─> Check timeout (13 minutes):
    │   └─> If approaching: Lambda Invoke self + exit
    └─> Check completion (NextToLaunch >= TotalParams && activeCount == 0):
        └─> Set status=COMPLETED + exit
```

### Status Query Sequence

```
1. CLI → DynamoDB GetItem
   └─> Query sweep record by sweep_id

2. CLI → Format output
   └─> Display progress, instances, errors

3. User sees real-time progress
```

### Resume Sequence

```
1. CLI → DynamoDB GetItem
   └─> Load current sweep state

2. CLI → Validate resumable
   └─> Check status != COMPLETED

3. CLI → Lambda Invoke (RequestResponse)
   └─> Re-invoke Lambda with same sweep_id

4. Lambda → Continue from NextToLaunch
   └─> Picks up where it left off
```

### Cancel Sequence

```
1. CLI → DynamoDB GetItem
   └─> Load sweep state

2. CLI → STS AssumeRole (optional, if cross-account)
   └─> Get temporary credentials

3. CLI → EC2 TerminateInstances
   └─> Terminate all pending/running instances

4. CLI → DynamoDB PutItem
   └─> Update status=CANCELLED, set CompletedAt

5. CLI → Exit
```

---

## Implementation Details

### Self-Reinvoking Lambda Pattern

**Problem:** Lambda timeout is 15 minutes, but sweeps can run for hours.

**Solution:** Lambda re-invokes itself before timeout.

**Implementation:**
```go
func runPollingLoop(ctx, state, params, ec2Client, sweepID) error {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    deadline := time.Now().Add(13 * time.Minute) // 13min (2min buffer)

    for {
        // Check timeout first
        if time.Now().After(deadline) {
            log.Println("Approaching Lambda timeout, re-invoking...")
            saveSweepState(ctx, state) // Save checkpoint
            return reinvokeSelf(ctx, sweepID)
        }

        // Polling logic...
        <-ticker.C
    }
}

func reinvokeSelf(ctx, sweepID) error {
    _, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
        FunctionName:   aws.String("spawn-sweep-orchestrator"),
        InvocationType: types.InvocationTypeEvent, // Async
        Payload:        json.Marshal(SweepEvent{SweepID: sweepID}),
    })
    return err
}
```

**Why it works:**
- Lambda A saves state to DynamoDB
- Lambda A invokes Lambda B asynchronously
- Lambda A exits (no longer counts toward timeout)
- Lambda B loads state, continues polling
- Process repeats as needed

**Cost:** Each invocation costs ~$0.004, so 10 invocations = $0.04 for multi-hour sweep.

### Rolling Queue Algorithm

**Goal:** Launch N instances concurrently, launch new instance when old instance terminates.

**Implementation:**
```go
for {
    // 1. Query active instance count
    activeCount := countActiveInstances(ec2Client, state)

    // 2. Calculate available slots
    available := state.MaxConcurrent - activeCount

    // 3. Launch available instances
    if available > 0 && state.NextToLaunch < state.TotalParams {
        toLaunch := min(available, state.TotalParams - state.NextToLaunch)

        for i := 0; i < toLaunch; i++ {
            paramIndex := state.NextToLaunch
            config := params.Params[paramIndex]

            // Launch instance
            launchInstance(ec2Client, state, config, paramIndex)

            state.NextToLaunch++
            state.Launched++

            // Save state after each launch
            saveSweepState(ctx, state)

            // Optional delay
            time.Sleep(launchDelay)
        }
    }

    // 4. Check completion
    if state.NextToLaunch >= state.TotalParams && activeCount == 0 {
        state.Status = "COMPLETED"
        saveSweepState(ctx, state)
        return nil
    }

    // 5. Wait for next poll
    <-ticker.C
}
```

**Why polling?**
- Simple to implement
- No event subscriptions needed
- Easy to debug
- Low cost (1 DynamoDB read + 1 EC2 DescribeInstances per 10s)

### Error Handling

**Launch failures:**
```go
result, err := ec2Client.RunInstances(ctx, input)
if err != nil {
    // Don't stop sweep - track failure and continue
    state.Failed++
    state.Instances = append(state.Instances, SweepInstance{
        Index:        paramIndex,
        State:        "failed",
        ErrorMessage: err.Error(),
        LaunchedAt:   time.Now().Format(time.RFC3339),
    })
    continue // Try next parameter
}
```

**DynamoDB write failures:**
```go
func saveSweepState(ctx, state) error {
    for retries := 0; retries < 3; retries++ {
        err = dynamodbClient.PutItem(ctx, input)
        if err == nil {
            return nil
        }
        log.Printf("DynamoDB write failed (attempt %d/3): %v", retries+1, err)
        time.Sleep(time.Duration(retries+1) * time.Second)
    }
    return fmt.Errorf("failed to save state after 3 retries: %w", err)
}
```

**Timeout recovery:**
- Save state before timeout
- Re-invoke asynchronously
- New invocation loads state and continues

### S3 Parameter Caching

**Problem:** Downloading parameters every poll wastes bandwidth.

**Solution:** Cache in Lambda /tmp directory.

**Implementation:**
```go
func downloadParams(ctx, s3Key, forceDownload) (*ParamFileFormat, error) {
    cacheFile := "/tmp/params-" + sweepID + ".json"

    // Check cache
    if !forceDownload {
        if data, err := os.ReadFile(cacheFile); err == nil {
            var params ParamFileFormat
            json.Unmarshal(data, &params)
            return &params, nil
        }
    }

    // Download from S3
    result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{...})
    data, _ := io.ReadAll(result.Body)

    // Cache to /tmp
    os.WriteFile(cacheFile, data, 0644)

    var params ParamFileFormat
    json.Unmarshal(data, &params)
    return &params, nil
}
```

**Why /tmp?**
- Persists across invocations in same container
- Up to 512 MB available (configurable to 10 GB)
- Automatic cleanup when container destroyed

### Cross-Account EC2 Launching

**Problem:** Lambda in account A needs to launch EC2 in account B.

**Solution:** AssumeRole pattern.

**Implementation:**
```go
func createCrossAccountEC2Client(ctx, region, accountID) (*ec2.Client, error) {
    roleArn := fmt.Sprintf("arn:aws:iam::%s:role/SpawnSweepCrossAccountRole", accountID)

    // Assume role
    result, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
        RoleArn:         aws.String(roleArn),
        RoleSessionName: aws.String("spawn-sweep-" + time.Now().Format("20060102-150405")),
        DurationSeconds: aws.Int32(900), // 15 minutes
    })

    creds := result.Credentials

    // Create EC2 client with assumed role credentials
    cfg, err := config.LoadDefaultConfig(ctx,
        config.WithRegion(region),
        config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx) (aws.Credentials, error) {
            return aws.Credentials{
                AccessKeyID:     *creds.AccessKeyId,
                SecretAccessKey: *creds.SecretAccessKey,
                SessionToken:    *creds.SessionToken,
            }, nil
        })),
    )

    return ec2.NewFromConfig(cfg), nil
}
```

**Session duration:** 15 minutes matches Lambda timeout.

---

## Cost & Performance

### Cost Breakdown

**Per sweep (100 instances, max_concurrent=5, 40min duration):**

**Lambda:**
- Invocations: 4 (40min / 13min per invocation)
- Duration: 52 minutes total (13min × 4)
- Cost: $0.000016 per GB-second × 512MB/1024 × 52min × 60s = **$0.004**

**DynamoDB:**
- Writes: 100 (1 per instance launch)
- Reads: 240 (1 per poll × 10s × 40min = 240 reads)
- Cost: $1.25 per million writes + $0.25 per million reads = **$0.0003**

**S3:**
- Upload: 1 (parameter file)
- Download: 1 (cached in Lambda /tmp)
- Storage: <1 MB for 30 days
- Cost: Negligible = **$0.000005**

**Total: ~$0.005 per sweep**

**Cost per instance:** $0.00005 (5 hundredths of a cent)

### Performance Characteristics

**Latency:**
- CLI to Lambda invoke: <1 second
- Lambda cold start: 2-3 seconds
- Lambda warm start: <500ms
- Instance launch: 30-60 seconds (EC2)
- Polling interval: 10 seconds
- Detection latency: 0-10 seconds (average 5s)

**Throughput:**
- Max concurrent: Configurable (tested up to 100)
- Launch rate: ~6 instances/minute (10s polling interval)
- S3 upload: 100 MB/s
- DynamoDB writes: 1000 writes/second (on-demand)

**Scalability:**
- Parameter sets: 1000+ (tested)
- File size: Unlimited (S3)
- Sweep duration: Unlimited (self-reinvocation)
- Concurrent sweeps: Unlimited (Lambda scales automatically)

**Limits:**
- Lambda timeout: 15 minutes per invocation
- Lambda memory: 512 MB (configurable to 10 GB)
- DynamoDB item size: 400 KB (rarely hit)
- S3 object size: 5 TB (never hit)

---

## Infrastructure Setup

### Prerequisites

- AWS Organization with 2+ accounts (or single account)
- AWS CLI configured with profiles
- Terraform or manual setup

### Manual Setup Scripts

All scripts in `scripts/` directory:

**1. S3 Bucket:**
```bash
AWS_PROFILE=spore-host-infra ./scripts/setup-sweep-s3-bucket.sh
```

**2. DynamoDB Table:**
```bash
AWS_PROFILE=spore-host-infra ./scripts/setup-sweep-dynamodb.sh
```

**3. Lambda IAM Role:**
```bash
AWS_PROFILE=spore-host-infra ./scripts/setup-sweep-orchestrator-lambda-role.sh
```

**4. Cross-Account IAM Role:**
```bash
AWS_PROFILE=spore-host-dev ./scripts/setup-sweep-cross-account-role.sh
```

**5. Deploy Lambda:**
```bash
AWS_PROFILE=spore-host-infra ./scripts/deploy-sweep-orchestrator.sh
```

**6. EC2 Instance IAM Role:**
```bash
AWS_PROFILE=spore-host-dev ./scripts/setup-spawnd-iam-role.sh
```

### Verification

**Test infrastructure:**
```bash
# 1. Check S3 bucket
AWS_PROFILE=spore-host-infra aws s3 ls spawn-sweeps-us-east-1

# 2. Check DynamoDB table
AWS_PROFILE=spore-host-infra aws dynamodb describe-table \
  --table-name spawn-sweep-orchestration

# 3. Check Lambda function
AWS_PROFILE=spore-host-infra aws lambda get-function \
  --function-name spawn-sweep-orchestrator

# 4. Check IAM roles
AWS_PROFILE=spore-host-infra aws iam get-role \
  --role-name SpawnSweepOrchestratorRole

AWS_PROFILE=spore-host-dev aws iam get-role \
  --role-name SpawnSweepCrossAccountRole
```

**Test end-to-end:**
```bash
# Launch small sweep
spawn launch --param-file test-sweep.json --max-concurrent 2 --detach

# Check status
spawn status --sweep-id <id>

# Cancel
spawn cancel --sweep-id <id>
```

---

## Troubleshooting

### Lambda Not Launching Instances

**Check Lambda logs:**
```bash
AWS_PROFILE=spore-host-infra aws logs tail /aws/lambda/spawn-sweep-orchestrator --follow
```

**Common issues:**
- Cross-account role trust policy incorrect
- Lambda doesn't have AssumeRole permission
- IAM role propagation delay (wait 10 seconds)

**Fix:**
- Verify trust policy in SpawnSweepCrossAccountRole
- Verify SpawnSweepOrchestratorRole has `sts:AssumeRole` permission
- Re-deploy Lambda if permissions changed

### DynamoDB Access Denied

**Symptom:** Lambda logs show "AccessDenied" on DynamoDB

**Fix:**
```bash
# Check Lambda role has DynamoDB permissions
AWS_PROFILE=spore-host-infra aws iam get-role-policy \
  --role-name SpawnSweepOrchestratorRole \
  --policy-name SpawnSweepOrchestratorPolicy
```

Should include:
```json
{
  "Effect": "Allow",
  "Action": [
    "dynamodb:GetItem",
    "dynamodb:PutItem",
    "dynamodb:Query"
  ],
  "Resource": "arn:aws:dynamodb:us-east-1:966362334030:table/spawn-sweep-orchestration"
}
```

### S3 Access Denied

**Symptom:** Lambda can't download parameters

**Fix:**
```bash
# Check Lambda role has S3 permissions
AWS_PROFILE=spore-host-infra aws iam get-role-policy \
  --role-name SpawnSweepOrchestratorRole \
  --policy-name SpawnSweepOrchestratorPolicy
```

Should include:
```json
{
  "Effect": "Allow",
  "Action": ["s3:GetObject"],
  "Resource": "arn:aws:s3:::spawn-sweeps-us-east-1/sweeps/*"
}
```

### Sweep Stuck in RUNNING

**Check if Lambda is still running:**
```bash
# Check recent invocations
AWS_PROFILE=spore-host-infra aws lambda list-invocations \
  --function-name spawn-sweep-orchestrator \
  --max-items 10
```

**Possible causes:**
- Lambda exhausted retries and stopped
- All instances at max concurrent (check sweep has pending params)
- EC2 launch failures (check Failed count in status)

**Fix:**
- Check Lambda logs for errors
- Resume sweep to re-invoke Lambda: `spawn resume --sweep-id <id> --detach`

### Race Condition on Cancel

**Symptom:** Cancelled sweep shows status=RUNNING after cancel

**Cause:** Lambda overwrites status on next state save.

**Workaround:** Cancel command terminates instances, Lambda will eventually mark as COMPLETED when activeCount=0.

**Permanent fix:** See [Issue #26](https://github.com/spore-host/spore-host/issues/26) - add cancellation flag check in Lambda polling loop.

### High Costs

**Check sweep configuration:**
```bash
spawn status --sweep-id <id>
```

**Common causes:**
- Too many instances (check TotalParams)
- Long-running instances (check TTL)
- Expensive instance types (check parameter file)

**Mitigation:**
- Set aggressive TTLs: `"ttl": "30m"`
- Use Spot instances: `"spot": true`
- Use smaller instance types for testing

---

## Development Guide

### Local Testing

**Test Lambda locally:**
```bash
cd spawn/lambda/sweep-orchestrator

# Build
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

# Run with test event
sam local invoke -e test-event.json
```

**Test event:**
```json
{
  "sweep_id": "test-sweep-123",
  "force_download": false
}
```

### Debugging

**Enable verbose logging:**
```go
// In main.go
log.SetOutput(os.Stdout)
log.Printf("Debug: sweep_id=%s, status=%s, nextToLaunch=%d", ...)
```

**Check CloudWatch logs:**
```bash
aws logs tail /aws/lambda/spawn-sweep-orchestrator --follow --format short
```

**Query DynamoDB directly:**
```bash
aws dynamodb get-item \
  --table-name spawn-sweep-orchestration \
  --key '{"sweep_id":{"S":"sweep-20260116-abc123"}}'
```

### Contributing

**Making changes:**
1. Modify Lambda code in `spawn/lambda/sweep-orchestrator/main.go`
2. Build: `GOOS=linux GOARCH=amd64 go build -o bootstrap main.go`
3. Deploy: `./scripts/deploy-sweep-orchestrator.sh`
4. Test with real sweep
5. Check Lambda logs
6. Iterate

**Adding features:**
- Multi-region support: See [Issue #24](https://github.com/spore-host/spore-host/issues/24)
- Cost tracking: See [Issue #25](https://github.com/spore-host/spore-host/issues/25)
- Cancellation flag: See [Issue #26](https://github.com/spore-host/spore-host/issues/26)

---

## Next Steps

- **User guide:** See [PARAMETER_SWEEPS.md](PARAMETER_SWEEPS.md)
- **Dashboard integration:** See [Issue #23](https://github.com/spore-host/spore-host/issues/23)
- **Report bugs:** [GitHub Issues](https://github.com/spore-host/spore-host/issues)

---

**Questions or feedback?** Open an issue on GitHub!
