# spawn cancel

Cancel running parameter sweeps and terminate all associated instances.

## Synopsis

```bash
spawn cancel --sweep-id <sweep-id> [flags]
```

## Description

Cancel a running parameter sweep, stop launching new instances, and optionally terminate all instances that were launched as part of the sweep.

**Behavior:**
- Marks sweep as cancelled in DynamoDB
- Stops Lambda orchestrator from launching new instances
- Optionally terminates all running instances in sweep
- Preserves launched instance data for review

## Arguments

None. The sweep ID is specified via the `--sweep-id` flag.

## Flags

### Required

#### --sweep-id
**Type:** String
**Required:** Yes
**Description:** Parameter sweep ID to cancel.

```bash
spawn cancel --sweep-id sweep-20260127-abc123
```

### Optional

#### --terminate-instances
**Type:** Boolean
**Default:** `false`
**Description:** Terminate all instances launched by this sweep.

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
```

**Warning:** This will terminate ALL instances in the sweep, including running computations. Data loss may occur if results haven't been saved.

#### --keep-running
**Type:** Boolean
**Default:** `false`
**Description:** Cancel sweep but keep currently running instances (opposite of `--terminate-instances`).

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --keep-running
# Cancels sweep, but instances continue running until their TTL expires
```

#### --force
**Type:** Boolean
**Default:** `false`
**Description:** Force cancellation even if sweep is already completed or failed.

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --force
```

#### --reason
**Type:** String
**Default:** "User cancelled"
**Description:** Reason for cancellation (stored in sweep metadata).

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --reason "Cost budget exceeded"
```

### Output

#### --json
**Type:** Boolean
**Default:** `false`
**Description:** Output in JSON format.

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --json
```

#### --quiet
**Type:** Boolean
**Default:** `false`
**Description:** Minimal output.

```bash
spawn cancel --sweep-id sweep-20260127-abc123 --quiet
```

## Output

### Standard Output

```
Cancelling sweep: sweep-20260127-abc123

Sweep Status:
  Status: running
  Launched: 28 / 50 (56%)
  Running: 5 instances
  Completed: 23 instances
  Failed: 0 instances

Cancellation:
  ✓ Sweep marked as cancelled
  ✓ Lambda orchestrator stopped
  ⚠ 5 instances still running (use --terminate-instances to stop them)

Summary:
  Sweep cancelled successfully
  Remaining instances will run until TTL expires
  To terminate now: spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
```

### With --terminate-instances

```
Cancelling sweep: sweep-20260127-abc123

Sweep Status:
  Status: running
  Launched: 28 / 50 (56%)
  Running: 5 instances

Cancellation:
  ✓ Sweep marked as cancelled
  ✓ Lambda orchestrator stopped

Terminating Instances:
  ✓ i-0123456789abc (run-028) - terminating
  ✓ i-0987654321fed (run-027) - terminating
  ✓ i-abcdef012345f (run-024) - terminating
  ✓ i-543210fedcba9 (run-022) - terminating
  ✓ i-135792468ace0 (run-020) - terminating

Summary:
  Sweep cancelled successfully
  5 instances terminated
  23 instances already completed (kept)
```

### JSON Output

```json
{
  "sweep_id": "sweep-20260127-abc123",
  "previous_status": "running",
  "new_status": "cancelled",
  "cancellation_time": "2026-01-27T15:30:00-08:00",
  "reason": "User cancelled",
  "progress": {
    "total": 50,
    "launched": 28,
    "running": 5,
    "completed": 23,
    "failed": 0
  },
  "instances_terminated": 5,
  "instances_kept": 23,
  "success": true
}
```

## Examples

### Cancel Sweep (Keep Instances Running)
```bash
# Cancel sweep, instances continue until TTL
spawn cancel --sweep-id sweep-20260127-abc123
```

### Cancel and Terminate All Instances
```bash
# Cancel and terminate everything
spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
```

### Cancel with Custom Reason
```bash
# Cancel with reason
spawn cancel --sweep-id sweep-20260127-abc123 \
  --reason "Cost exceeded budget" \
  --terminate-instances
```

### Force Cancel Completed Sweep
```bash
# Cancel even if already completed (for cleanup)
spawn cancel --sweep-id sweep-20260127-abc123 --force --terminate-instances
```

### Cancel in Automation
```bash
#!/bin/bash
SWEEP_ID="sweep-20260127-abc123"

# Monitor cost
COST=$(spawn cost --sweep-id "$SWEEP_ID" --json | jq -r '.total_cost')

# Cancel if cost exceeds threshold
if (( $(echo "$COST > 100" | bc -l) )); then
    echo "Cost exceeded $100, cancelling sweep"
    spawn cancel --sweep-id "$SWEEP_ID" --terminate-instances \
      --reason "Cost threshold exceeded: \$$COST"
fi
```

### Graceful Cancellation
```bash
#!/bin/bash
# Give instances time to save results before terminating

SWEEP_ID="sweep-20260127-abc123"

# Cancel sweep (stop new launches)
spawn cancel --sweep-id "$SWEEP_ID"

# Get running instances
INSTANCES=$(spawn list --tag spawn:sweep-id="$SWEEP_ID" --state running --quiet)

# Signal instances to save and exit
for instance in $INSTANCES; do
    echo "Signaling $instance to complete"
    spawn connect "$instance" -c "touch /tmp/SPAWN_COMPLETE"
done

# Wait 2 minutes for graceful completion
echo "Waiting 2 minutes for graceful completion..."
sleep 120

# Force terminate any remaining instances
echo "Terminating remaining instances"
for instance in $INSTANCES; do
    aws ec2 terminate-instances --instance-ids "$instance"
done
```

## Cancellation Behavior

### What Gets Cancelled
- ✅ Lambda orchestrator stops launching new instances
- ✅ Sweep status changed to "cancelled" in DynamoDB
- ✅ No more parameters processed
- ⚠️ Currently running instances continue (unless `--terminate-instances`)
- ✅ Completed instances remain in history

### What Doesn't Get Cancelled
- ❌ Individual instance TTLs (they still expire normally)
- ❌ Completed instances (already done)
- ❌ Results in S3 (preserved for analysis)
- ❌ Alert configurations

### Partial Cancellation Support
If cancellation fails for some instances:
- Sweep is still marked as cancelled
- Partial termination list shown
- Exit code 1 (partial failure)
- Retry possible with same command

## Exit Codes

| Code | Description |
|------|-------------|
| 0 | Cancellation successful (all operations completed) |
| 1 | Cancellation failed (AWS API error, partial failure) |
| 2 | Invalid sweep ID (malformed or missing) |
| 3 | Sweep not found (doesn't exist in DynamoDB) |
| 4 | Sweep already complete (use `--force` to override) |

## Troubleshooting

### "Sweep not found"
```bash
# List all sweeps
spawn list-sweeps

# Check sweep ID spelling
spawn cancel --sweep-id sweep-20260127-abc123  # Correct format
```

### "Sweep already completed"
```bash
# Sweep finished before cancellation
# Use --force to clean up anyway
spawn cancel --sweep-id sweep-20260127-abc123 --force --terminate-instances
```

### Instances Still Running After Cancel
```bash
# Without --terminate-instances, instances continue
# Cancel sweep only stops NEW launches

# Check running instances
spawn list --tag spawn:sweep-id=sweep-20260127-abc123 --state running

# Terminate manually
spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
```

### Partial Termination Failure
```bash
# Some instances may fail to terminate (permissions, state)
# Check error message for instance IDs

# Retry termination
for instance in i-xxx i-yyy i-zzz; do
    aws ec2 terminate-instances --instance-ids "$instance"
done
```

### Lambda Still Launching Instances
```bash
# Lambda may complete current batch (< 5s delay)
# Check after 30 seconds

sleep 30
spawn status --sweep-id sweep-20260127-abc123
# Should show "cancelled" status
```

## Best Practices

### 1. Check Sweep Status First
```bash
# Review what will be cancelled
spawn status --sweep-id sweep-20260127-abc123

# Then cancel
spawn cancel --sweep-id sweep-20260127-abc123
```

### 2. Save Results Before Terminating
```bash
# Cancel sweep (stop new launches)
spawn cancel --sweep-id sweep-20260127-abc123

# Download results from completed instances
spawn collect-results --sweep-id sweep-20260127-abc123

# Then terminate running instances if needed
spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
```

### 3. Use --reason for Auditability
```bash
# Document why sweep was cancelled
spawn cancel --sweep-id sweep-20260127-abc123 \
  --reason "Requirements changed, restarting with new parameters"
```

### 4. Monitor Cancellation
```bash
# Cancel and watch
spawn cancel --sweep-id sweep-20260127-abc123 --terminate-instances
sleep 5
spawn status --sweep-id sweep-20260127-abc123
```

## Cost Implications

- **Cancelled Instances:** Billed for partial hour (AWS minimum)
- **Terminated Instances:** Billed until termination completes
- **Lambda Orchestrator:** Minimal cost (< $0.01 per sweep)
- **Completed Instances:** Full billing (already ran)

**Cost Savings:** Cancelling early prevents launching remaining instances, saving potential costs.

## See Also
- [spawn status](status.md) - Check sweep status
- [spawn list-sweeps](list-sweeps.md) - List all sweeps
- [spawn resume](resume.md) - Resume cancelled sweeps
- [spawn collect-results](collect-results.md) - Download sweep results
- [spawn launch](launch.md) - Launch parameter sweeps
