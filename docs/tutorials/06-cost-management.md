# Tutorial 6: Cost Management

**Duration:** 20 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

## What You'll Learn

In this tutorial, you'll learn how to track and optimize AWS costs:
- Track costs for instances and sweeps
- Set budget alerts
- Analyze spending patterns
- Optimize costs with spot instances
- Right-size instance types
- Avoid unexpected bills

## Why Cost Management Matters

**Common Scenarios:**
- ❌ Forgot to terminate instance → $150 monthly bill
- ❌ Launched too many instances → $500 surprise charge
- ❌ Used expensive GPU unnecessarily → $300 wasted

**With proper cost management:**
- ✅ Set TTL on all instances → Auto-termination
- ✅ Track costs in real-time → No surprises
- ✅ Use spot instances → 70% savings
- ✅ Budget alerts → Stay under limit

## Understanding AWS Costs

### EC2 Pricing Components

| Component | Cost | When Charged |
|-----------|------|--------------|
| **EC2 Instance** | $0.0104/hour (t3.micro) | While running |
| **EBS Volume** | $0.10/GB/month | While attached |
| **Data Transfer (Out)** | $0.09/GB | When transferring data out |
| **Public IP** | $0.005/hour | While allocated |

**Example: t3.micro for 24 hours**
```
Instance: 24h × $0.0104/h = $0.25
EBS 8GB: (8 × $0.10)/30 = $0.03
Data Transfer: ~$0.01
Total: ~$0.29/day
```

### Instance Type Costs

| Instance Type | vCPU | RAM | Hourly | Daily | Monthly |
|---------------|------|-----|--------|-------|---------|
| t3.micro | 2 | 1 GB | $0.0104 | $0.25 | $7.50 |
| t3.medium | 2 | 4 GB | $0.0416 | $1.00 | $30.00 |
| m7i.large | 2 | 8 GB | $0.1008 | $2.42 | $73.00 |
| g5.xlarge | 4 | 16 GB | $1.006 | $24.14 | $725.00 |
| p5.48xlarge | 192 | 2 TB | $98.32 | $2,360 | $71,000 |

**Key Insight:** Choose the smallest instance that meets your needs.

## Tracking Costs with spawn

### Check Overall Costs

```bash
spawn cost
```

**Expected output:**
```
Spawn Costs - January 2026

Current Month (Jan 1 - Jan 27):
  Total: $456.78
  Daily Average: $16.92
  Projected Month-End: $526.42

By Resource Type:
  EC2 Instances:        $398.45  (87.2%)
  EBS Volumes:          $42.18   (9.2%)
  Data Transfer:        $12.87   (2.8%)
  Lambda:               $2.15    (0.5%)
  DynamoDB:             $1.13    (0.2%)

Top 5 Instances:
  i-0abc123 (g5.xlarge)      $142.50   23.4 days
  i-0abc234 (m7i.large)      $85.20    18.2 days
  i-0abc345 (t3.medium)      $45.30    15.5 days
  i-0abc456 (t3.micro)       $12.40    8.3 days
  i-0abc567 (t3.micro)       $8.90     6.1 days

Budget:
  Limit: $500.00
  Used: $456.78 (91.4%)
  Remaining: $43.22
  Days remaining: 4
```

### Check Specific Instance Cost

```bash
spawn cost --instance-id i-0abc123def456789
```

**Expected output:**
```
Instance Cost Report

Instance: i-0abc123def456789
Type: m7i.large
Region: us-east-1
State: running

Lifetime Costs:
  Launch Time: 2026-01-20 08:00:00 PST
  Running Duration: 7d 14h 32m
  Total Cost: $38.42

Cost Breakdown:
  EC2 Instance (7.6 days):       $18.42
  EBS Volume (20 GB × 7.6 days): $0.50
  Data Transfer (Out):           $0.15
  Total:                         $38.42

Current Burn Rate:
  Hourly: $0.1008
  Daily: $2.42
  Monthly: $73.00

If kept running:
  Next 24 hours: $2.42
  Next 7 days: $16.94
  Rest of month (4 days): $9.68
```

### Check Sweep Costs

```bash
spawn cost --sweep-id sweep-20260127-abc123
```

**Expected output:**
```
Sweep Cost Report

Sweep: sweep-20260127-abc123
Instances: 50
Region: us-east-1

Total Costs: $125.40

By Instance Type:
  t3.medium (50 instances):     $125.40

Per-Instance Average: $2.51

Duration:
  Average runtime: 2h 25m
  Shortest: 2h 10m
  Longest: 2h 45m

Estimated vs Actual:
  Estimated: $150.00 (3h × 50 × $0.05/h)
  Actual: $125.40
  Savings: $24.60 (16.4%)
  Reason: Instances completed early
```

## Setting Budget Alerts

### Create Budget Alert

```bash
spawn alerts create cost --threshold 500.00 --webhook $SLACK_WEBHOOK
```

**What this does:**
- Monitors monthly spending
- Alerts at 80%, 90%, 100% of threshold
- Sends notification to Slack

**Expected output:**
```
✓ Cost alert created

Budget: $500.00/month
Current: $456.78 (91.4%)

Alerts:
  ⚠️  80% threshold: $400.00 (TRIGGERED on Jan 23)
  ⚠️  90% threshold: $450.00 (TRIGGERED on Jan 26)
  🚨 100% threshold: $500.00 (Remaining: $43.22)

Notification: Slack webhook
```

### Set Instance-Level Alert

```bash
spawn alerts create instance-cost \
  --instance-id i-0abc123 \
  --threshold 50.00 \
  --email myemail@example.com
```

**Alerts when specific instance exceeds $50.**

### Daily Cost Summary

```bash
spawn alerts create daily-summary --email myemail@example.com
```

**Sends daily email with:**
- Yesterday's costs
- Running instances
- Top 5 expensive instances
- Month-to-date total

## Cost Optimization Strategies

### 1. Always Set TTL

```bash
# Bad: No TTL, could run forever
spawn launch --instance-type t3.micro

# Good: Auto-terminates after 8 hours
spawn launch --instance-type t3.micro --ttl 8h
```

**Potential Savings:**
- Forgot instance for 30 days = $225 wasted
- With 8h TTL = $0.83

### 2. Use Spot Instances

```bash
# On-demand
spawn launch --instance-type m7i.large
# Cost: $0.1008/hour

# Spot
spawn launch --instance-type m7i.large --spot
# Cost: ~$0.03/hour (70% savings)
```

**Spot Price Comparison:**

| Instance Type | On-Demand | Spot | Savings |
|---------------|-----------|------|---------|
| t3.micro | $0.0104/h | $0.0031/h | 70% |
| m7i.large | $0.1008/h | $0.0302/h | 70% |
| g5.xlarge | $1.006/h | $0.302/h | 70% |

**When to use spot:**
- Batch processing
- ML training (with checkpoints)
- Parameter sweeps
- Development/testing

**When NOT to use spot:**
- Production services
- Critical workloads
- Jobs without checkpointing

**Example:**
```bash
# 100-instance sweep
spawn launch --param-file sweep.yaml --spot

# On-demand cost: 100 × 2h × $0.1008 = $20.16
# Spot cost: 100 × 2h × $0.03 = $6.00
# Savings: $14.16 (70%)
```

### 3. Right-Size Instances

**Example: Data processing job**

Test with different sizes:
```bash
# Test with t3.micro
spawn launch --instance-type t3.micro --name test-micro
# Runtime: 45 minutes
# Cost: $0.0078

# Test with t3.medium
spawn launch --instance-type t3.medium --name test-medium
# Runtime: 15 minutes
# Cost: $0.0104

# Test with m7i.large
spawn launch --instance-type m7i.large --name test-large
# Runtime: 8 minutes
# Cost: $0.0134
```

**Analysis:**
- t3.micro: Slowest, cheapest per hour, but longest runtime
- t3.medium: Good balance
- m7i.large: Fastest, but more expensive

**For 100 jobs:**
- t3.micro: 100 × $0.0078 = $0.78
- t3.medium: 100 × $0.0104 = $1.04 ✅ Best value
- m7i.large: 100 × $0.0134 = $1.34

**Choose t3.medium** - only $0.26 more for 3x speedup.

### 4. Use Idle Timeout for GPU Instances

```bash
spawn launch \
  --instance-type g5.xlarge \
  --ttl 12h \
  --idle-timeout 1h
```

**Scenario:**
- Launch GPU instance for ML training
- Training finishes after 3 hours
- Idle timeout terminates instance after 1 hour idle
- Total runtime: 4 hours instead of 12 hours

**Savings:**
- Without idle timeout: 12h × $1.006 = $12.07
- With idle timeout: 4h × $1.006 = $4.02
- Savings: $8.05 (67%)

### 5. Hibernate Instead of Stop

```bash
spawn launch \
  --instance-type m7i.large \
  --hibernate \
  --idle-timeout 1h \
  --hibernate-on-idle
```

**Benefits:**
- Resume faster (30s vs 2m)
- Preserve application state
- Only pay for storage when hibernated

**Cost comparison (24-hour workload with 18 hours idle):**
- Always running: 24h × $0.1008 = $2.42
- Stop/start: 6h × $0.1008 + (18h × 0.10 × 20GB/730h) = $0.60 + $0.05 = $0.65
- Hibernate: 6h × $0.1008 + (18h × 0.10 × 20GB/730h) = $0.65
- Terminate/relaunch: 6h × $0.1008 = $0.60 ✅ Cheapest

**Use hibernation when:**
- Need fast resume
- Application state is complex
- Cost of recreation > hibernation storage cost

### 6. Batch Jobs with `on_complete: terminate`

```bash
spawn launch \
  --instance-type t3.medium \
  --ttl 2h \
  --on-complete terminate \
  --user-data @process.sh
```

**Script ends with:**
```bash
# Upload results
aws s3 cp /tmp/results.json s3://my-bucket/results.json

# Mark complete (triggers termination)
spored complete --status success
```

**Savings:**
- Without auto-terminate: Runs for full TTL (2h) = $0.083
- With auto-terminate: Runs until done (30m) = $0.021
- Savings: $0.062 (75%)

## Analyzing Cost Patterns

### Daily Cost Breakdown

```bash
spawn cost --group-by day --time-range 30d
```

**Output:**
```
Daily Costs - Last 30 Days

Jan 1:   $8.45
Jan 2:   $12.30
Jan 3:   $45.60  ⚠️  Spike (parameter sweep)
Jan 4:   $9.20
Jan 5:   $15.40
...
Jan 27:  $18.90

Average: $16.92/day
Peak: $45.60 (Jan 3)
Lowest: $4.20 (Jan 15)
```

### Cost by Instance Type

```bash
spawn cost --group-by instance-type --time-range month
```

**Output:**
```
Costs by Instance Type - January 2026

t3.micro:     $84.50   (18.5%)
t3.medium:    $142.30  (31.1%)
m7i.large:    $165.40  (36.2%)
g5.xlarge:    $64.58   (14.1%)

Total: $456.78
```

**Analysis:** Most spending on m7i.large. Consider:
- Do we need this instance type?
- Can we use t3.medium for some workloads?
- Can we use spot instances?

### Cost by Region

```bash
spawn cost --group-by region --time-range month
```

**Output:**
```
Costs by Region - January 2026

us-east-1:    $385.40  (84.4%)
us-west-2:    $58.20   (12.7%)
eu-west-1:    $13.18   (2.9%)

Total: $456.78
```

### Export for Analysis

```bash
# Export to CSV
spawn cost --format csv --time-range 90d > costs.csv

# Import into spreadsheet for charting
open costs.csv
```

## Avoiding Unexpected Bills

### 1. Use Dry Run

```bash
# Check cost before launching
spawn launch --instance-type g5.xlarge --ttl 12h --dry-run
```

**Output:**
```
Dry Run - No resources created

Configuration:
  Instance Type: g5.xlarge
  TTL: 12h
  Region: us-east-1

Estimated Costs:
  Hourly: $1.006
  12 hours: $12.07
  Monthly (if left running): $725.00

Proceed with launch? Add --no-dry-run to launch.
```

### 2. Set Low TTL Initially

```bash
# Start with short TTL
spawn launch --instance-type t3.micro --ttl 30m

# Extend if needed
spawn extend <instance-id> 1h
```

Better to extend than over-provision.

### 3. Tag Instances

```bash
spawn launch \
  --instance-type t3.micro \
  --tags project=test,temporary=true,expires=2026-02-01
```

Review temporary instances weekly:
```bash
spawn list --tag temporary=true
```

### 4. Monitor Budget Daily

```bash
# Check costs every morning
spawn cost

# or
spawn cost --show-forecast
```

### 5. Use Cost Explorer

For detailed analysis beyond spawn:

```bash
# Open AWS Cost Explorer
open https://console.aws.amazon.com/cost-management/home#/cost-explorer
```

Filter by tag: `spawn:managed=true`

## Real-World Cost Optimization Example

**Scenario:** Running 50 ML training jobs per week.

**Initial Setup (Wasteful):**
```bash
spawn launch --instance-type g5.xlarge --ttl 24h --array 50
```

**Costs:**
- 50 instances × 24h × $1.006/h = $1,207.20/week
- $5,231/month

**Optimized:**
```bash
spawn launch \
  --instance-type g5.xlarge \
  --spot \
  --ttl 8h \
  --idle-timeout 1h \
  --on-complete terminate \
  --array 50
```

**Improved Costs:**
- Spot pricing: $0.302/h (70% off)
- Actual runtime: 4h (with idle timeout)
- 50 × 4h × $0.302/h = $60.40/week
- $262/month

**Savings: $4,969/month (95%!)**

**Optimizations applied:**
1. Spot instances: 70% savings
2. Accurate TTL (8h vs 24h): 67% savings
3. Idle timeout + on_complete: 50% savings
4. Combined effect: 95% savings

## Best Practices

### 1. Always Set TTL

```bash
spawn launch --instance-type t3.micro --ttl 8h
```

Never launch without TTL.

### 2. Use Spot for Non-Critical Work

```bash
spawn launch --spot --array 100
```

### 3. Enable Budget Alerts

```bash
spawn alerts create cost --threshold 500.00
```

### 4. Review Costs Weekly

```bash
# Every Monday morning
spawn cost --time-range 7d
```

### 5. Tag Everything

```bash
--tags project=myapp,team=ml,cost-center=engineering
```

Makes cost attribution easy.

### 6. Right-Size Instances

Test different instance types. Choose cheapest that meets performance needs.

### 7. Clean Up Regularly

```bash
# List all instances
spawn list

# Terminate unused instances
aws ec2 terminate-instances --instance-ids i-xxx
```

## What You Learned

Congratulations! You now understand:

✅ AWS cost components (EC2, EBS, data transfer)
✅ How to track costs with spawn
✅ Setting budget alerts
✅ Cost optimization strategies (spot, TTL, right-sizing)
✅ Analyzing spending patterns
✅ Avoiding unexpected bills

## Practice Exercises

### Exercise 1: Cost Tracking

1. Launch a t3.micro instance for 1 hour
2. Track its cost with `spawn cost --instance-id`
3. Compare estimated vs actual cost

### Exercise 2: Spot Savings

1. Calculate cost for 24h on m7i.large (on-demand)
2. Calculate cost for 24h on m7i.large (spot)
3. Compute savings percentage

### Exercise 3: Budget Alert

Set up a $100/month budget alert and verify it works.

## Next Steps

Continue your learning journey:

📖 **[Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md)** - Set up monitoring and notifications

🛠️ **[How-To: Cost Optimization](../how-to/cost-optimization.md)** - Advanced cost-saving techniques

📚 **[Command Reference: cost](../reference/commands/cost.md)** - Complete cost command documentation

## Quick Reference

```bash
# Check overall costs
spawn cost

# Check instance cost
spawn cost --instance-id <instance-id>

# Check sweep cost
spawn cost --sweep-id <sweep-id>

# Cost breakdown
spawn cost --breakdown
spawn cost --group-by instance-type
spawn cost --group-by region
spawn cost --group-by day

# Set budget alert
spawn alerts create cost --threshold 500.00

# Export costs
spawn cost --format csv > costs.csv

# Launch with cost optimization
spawn launch \
  --instance-type t3.micro \
  --spot \
  --ttl 8h \
  --idle-timeout 1h \
  --on-complete terminate
```

---

**Previous:** [← Tutorial 5: Batch Queues](05-batch-queues.md)
**Next:** [Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md) →
