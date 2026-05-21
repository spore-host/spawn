# How-To: Spot Instances

Complete guide to using EC2 spot instances for up to 90% cost savings.

## What are Spot Instances?

Spot instances are spare EC2 capacity offered at discounted prices (up to 90% off). AWS can reclaim them with 2-minute warning when capacity is needed.

**Key Characteristics:**
- **70-90% cheaper** than on-demand
- **Can be interrupted** with 2-minute warning
- **Best for:** Fault-tolerant, flexible workloads
- **Not for:** Production services, critical workloads

---

## When to Use Spot

### ✅ Good Use Cases

**Batch Processing:**
```bash
spawn launch --array 100 --spot --user-data @process.sh
```
- Process 100 files
- If interrupted, just retry failed files
- 70% cost savings

**ML Training with Checkpoints:**
```bash
spawn launch --instance-type g5.xlarge --spot --user-data @train.sh
```
- Save checkpoints every 5-10 minutes
- Resume from last checkpoint if interrupted
- Huge savings on expensive GPU instances

**Parameter Sweeps:**
```bash
spawn launch --param-file sweep.yaml --spot
```
- Independent experiments
- Failed runs can be retried
- Perfect for spot

**CI/CD Testing:**
```bash
spawn launch --spot --user-data @run-tests.sh
```
- Tests are retriable
- Save 70% on test infrastructure

### ❌ Bad Use Cases

**Production Web Services:**
- Need high availability
- Interruptions cause downtime
- Use on-demand or reserved instances

**Databases:**
- Data loss risk if not checkpointed properly
- Use on-demand with proper backups

**Long-Running Single Jobs (No Checkpointing):**
- If job runs 10 hours without checkpoints
- Interruption means starting over
- Not cost-effective

**Time-Sensitive Workloads:**
- Interruptions cause delays
- Use on-demand for deadlines

---

## Basic Spot Usage

### Launch Spot Instance

```bash
spawn launch --instance-type m7i.large --spot --ttl 4h
```

**spawn automatically:**
- Requests spot instance
- Sets interruption behavior
- Installs spored agent to handle interruptions

### Launch with Max Price

```bash
spawn launch --instance-type m7i.large --spot --spot-max-price 0.05
```

**Protects against price spikes:**
- Only launches if spot price ≤ $0.05/hour
- On-demand price for m7i.large: $0.1008/hour
- Setting max price at 50% of on-demand

**Recommendation:** Leave max price unset (defaults to on-demand price).

---

## Spot Price Comparison

### Check Current Spot Prices

```bash
aws ec2 describe-spot-price-history \
  --instance-types m7i.large \
  --start-time $(date -u +%Y-%m-%dT%H:%M:%S) \
  --product-descriptions "Linux/UNIX" \
  --query 'SpotPriceHistory[*].[AvailabilityZone,SpotPrice]' \
  --output table
```

**Example output:**
```
--------------------------------
|  us-east-1a  |  0.0302      |
|  us-east-1b  |  0.0305      |
|  us-east-1c  |  0.0298      |
|  us-east-1d  |  0.0315      |
--------------------------------
On-demand: $0.1008/hour
Average spot: $0.0305/hour (70% savings)
```

### Typical Savings

| Instance Type | On-Demand | Typical Spot | Savings |
|---------------|-----------|--------------|---------|
| t3.micro | $0.0104/h | $0.0031/h | 70% |
| t3.medium | $0.0416/h | $0.0125/h | 70% |
| m7i.large | $0.1008/h | $0.0302/h | 70% |
| c7i.xlarge | $0.1701/h | $0.0510/h | 70% |
| g5.xlarge | $1.006/h | $0.302/h | 70% |
| g5.2xlarge | $1.212/h | $0.364/h | 70% |

**Savings are typically 70-90% depending on demand.**

---

## Handling Spot Interruptions

### Interruption Flow

1. **AWS sends 2-minute warning** via EC2 metadata
2. **spored agent detects warning**
3. **Agent executes interruption handler** (save state, upload results)
4. **Instance terminates** after 2 minutes

### Default Behavior

```bash
spawn launch --spot
```

**Default interruption behavior:**
- Attempts graceful shutdown
- Terminates after 2 minutes
- Any unsaved work is lost

### Hibernation on Interruption

```bash
spawn launch --instance-type m7i.large --spot --hibernate \
  --spot-interruption-behavior hibernate
```

**What happens:**
- RAM saved to EBS
- Instance hibernates
- AWS may resume later if capacity available
- Work continues from where it left off

**Requirements:**
- Instance type supports hibernation (m5, m6i, m7i, r5, r6i, r7i)
- Root volume encrypted
- RAM ≤ 150 GB

### Stop on Interruption

```bash
spawn launch --spot --spot-interruption-behavior stop
```

**What happens:**
- Instance stops (not terminated)
- Can manually restart later
- Loses RAM, preserves disk
- Continues paying for EBS storage

### Terminate (Default)

```bash
spawn launch --spot --spot-interruption-behavior terminate
```

**What happens:**
- Instance terminates immediately
- All data on instance lost
- No storage charges after termination

---

## Checkpointing Strategy

### Problem
Long-running job might be interrupted. Don't want to start over.

### Solution: Frequent Checkpoints

```bash
#!/bin/bash
# train-with-checkpoints.sh
set -e

CHECKPOINT_DIR="/checkpoints"
RESULTS_S3="s3://my-bucket/results/$NAME"
mkdir -p $CHECKPOINT_DIR

# Function to upload checkpoint
upload_checkpoint() {
  echo "Uploading checkpoint..."
  aws s3 sync $CHECKPOINT_DIR/ $RESULTS_S3/checkpoints/
}

# Install interruption handler
trap upload_checkpoint SIGTERM

# Train with checkpointing
python train.py \
  --checkpoint-dir $CHECKPOINT_DIR \
  --checkpoint-every 10 \
  --resume-from $CHECKPOINT_DIR/latest.pth

# Upload final results
aws s3 cp /results/ $RESULTS_S3/ --recursive
spored complete --status success
```

**Launch:**
```bash
spawn launch --instance-type g5.xlarge --spot --user-data @train-with-checkpoints.sh
```

**If interrupted:**
1. Checkpoint uploaded to S3
2. Relaunch same instance
3. Resume from last checkpoint

**Resume:**
```bash
# Download checkpoint
aws s3 sync s3://my-bucket/results/$NAME/checkpoints/ /checkpoints/

# Relaunch with same user data
spawn launch --instance-type g5.xlarge --spot --user-data @train-with-checkpoints.sh
```

---

## Spot Fleet for Capacity

### Problem
Need 100 instances, but spot capacity might be limited.

### Solution: Diversify Instance Types

```yaml
# sweep.yaml
defaults:
  ttl: 4h
  spot: true
  iam_policy: s3:FullAccess

params:
  # Spread across instance types
  - name: run-001
    instance_type: m7i.large
  - name: run-002
    instance_type: m6i.large
  - name: run-003
    instance_type: m5.large
  - name: run-004
    instance_type: m7i.xlarge
  # ... more params with variety
```

**Benefits:**
- If one type has no capacity, others might
- Increases chance all instances launch
- Minimal cost difference between generations

### Diversify Availability Zones

```yaml
params:
  - name: run-001
    az: us-east-1a
  - name: run-002
    az: us-east-1b
  - name: run-003
    az: us-east-1c
```

---

## Spot Price History Analysis

### Find Cheapest AZ

```bash
#!/bin/bash
# find-cheapest-az.sh

INSTANCE_TYPE="m7i.large"

echo "Spot prices for $INSTANCE_TYPE:"
aws ec2 describe-spot-price-history \
  --instance-types $INSTANCE_TYPE \
  --start-time $(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%S) \
  --product-descriptions "Linux/UNIX" \
  --query 'SpotPriceHistory[*].[AvailabilityZone,SpotPrice]' \
  --output text | \
  sort -k2 -n | \
  head -5

echo ""
echo "Cheapest AZ: $(aws ec2 describe-spot-price-history \
  --instance-types $INSTANCE_TYPE \
  --start-time $(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%S) \
  --product-descriptions "Linux/UNIX" \
  --query 'SpotPriceHistory[*].[AvailabilityZone,SpotPrice]' \
  --output text | sort -k2 -n | head -1 | awk '{print $1}')"
```

**Launch in cheapest AZ:**
```bash
CHEAPEST_AZ=$(./find-cheapest-az.sh | tail -1 | awk '{print $2}')
spawn launch --spot --az $CHEAPEST_AZ
```

---

## Spot Interruption Rate

### Historical Interruption Frequency

| Instance Type | Interruption Rate | Notes |
|---------------|-------------------|-------|
| t3, t4g | < 5% | Very stable |
| m5, m6i, m7i | < 10% | Generally stable |
| c5, c6i, c7i | < 10% | Generally stable |
| r5, r6i, r7i | < 15% | Memory-optimized, moderate |
| g5 | 15-25% | GPU, higher demand |
| p4, p5 | 25-50% | High-end GPU, frequent interruptions |

**Source:** AWS Spot Instance Advisor

**Check current rates:**
https://aws.amazon.com/ec2/spot/instance-advisor/

---

## Cost Analysis: Spot vs On-Demand

### Example: 100-Hour ML Training

**On-Demand (g5.xlarge):**
```
100 hours × $1.006/hour = $100.60
```

**Spot (g5.xlarge, 70% savings):**
```
100 hours × $0.302/hour = $30.20
Savings: $70.40 (70%)
```

**Spot with 2 Interruptions (Resume from Checkpoint):**
```
- Run 1: 40 hours × $0.302 = $12.08 (interrupted)
- Run 2: 35 hours × $0.302 = $10.57 (interrupted)
- Run 3: 30 hours × $0.302 = $9.06 (complete)
Total: $31.71
Savings: $68.89 (68%)
```

**Even with interruptions, spot is much cheaper.**

---

## Monitoring Spot Instances

### Check Interruption Status

```bash
# On instance
curl -s http://169.254.169.254/latest/meta-data/spot/instance-action

# If interruption warning:
# {"action": "terminate", "time": "2026-01-27T15:32:00Z"}

# If no warning:
# (returns 404)
```

### Spored Handles This

spored agent automatically monitors and handles interruptions. You don't need manual checks.

### Get Notification on Interruption

```bash
spawn alerts create global --slack $WEBHOOK --on-spot-interruption
```

Slack notification when instance interrupted:
```
⚠️ Spot Interruption Warning

Instance: i-0abc123def456789
Type: g5.xlarge
Time: 2026-01-27 15:30:00 PST (2 minutes)

Actions:
  • Saving checkpoint
  • Uploading results to S3
  • Graceful shutdown

Relaunch:
  spawn launch --instance-type g5.xlarge --spot --resume-from-checkpoint
```

---

## Best Practices

### 1. Always Use Spot for Batch Work

```bash
# Default to spot for sweeps
spawn launch --param-file sweep.yaml --spot
```

**Why:** 70% savings with minimal downside.

### 2. Checkpoint Frequently

```bash
# Save checkpoint every 5-10 minutes
python train.py --checkpoint-every 10
```

**Why:** Minimize lost work on interruption.

### 3. Diversify Instance Types

```yaml
# Don't hardcode single type
params:
  - name: run-001
    instance_type: m7i.large
  - name: run-002
    instance_type: m6i.large
```

**Why:** Improves capacity availability.

### 4. Don't Set Max Price Too Low

```bash
# Good: Use default (on-demand price)
spawn launch --spot

# Bad: Too restrictive
spawn launch --spot --spot-max-price 0.02
```

**Why:** Low max price causes launch failures.

### 5. Upload Results Early and Often

```bash
# Don't wait until end
aws s3 cp /results/ s3://bucket/ --recursive --exclude "*" --include "*.json"
```

**Why:** Preserves results if interrupted.

### 6. Use Interruption-Tolerant Designs

```bash
# Design jobs to be retriable
for file in *.csv; do
  if ! aws s3 ls s3://results/$file.done; then
    process_file $file
    touch /tmp/$file.done
    aws s3 cp /tmp/$file.done s3://results/
  fi
done
```

**Why:** Idempotent operations survive interruptions.

---

## Troubleshooting

### Spot Request Failed

**Error:**
```
InsufficientInstanceCapacity: We currently do not have sufficient m7i.large capacity
```

**Solutions:**
1. Try different AZ
2. Try different instance type
3. Try different region
4. Wait and retry

### Frequent Interruptions

**Problem:** Getting interrupted every 30 minutes.

**Solutions:**
1. Switch to less popular instance type
2. Try different AZ
3. Consider on-demand for this workload
4. Check spot instance advisor for stable types

### Lost Work Due to Interruption

**Problem:** Didn't save checkpoint before interruption.

**Solution:** Implement checkpoint handler:
```bash
trap save_checkpoint SIGTERM
```

---

## See Also

- [Tutorial 6: Cost Management](../tutorials/06-cost-management.md) - Cost tracking
- [How-To: Cost Optimization](cost-optimization.md) - More savings techniques
- [spawn launch](../reference/commands/launch.md) - Spot flags reference
- [AWS Spot Instance Advisor](https://aws.amazon.com/ec2/spot/instance-advisor/) - Interruption rates
