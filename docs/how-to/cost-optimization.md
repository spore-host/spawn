# How-To: Cost Optimization

Advanced techniques to minimize AWS costs.

## Cost Reduction Strategies

### 1. Always Use TTL

**Problem:** Forgot instance, ran for 30 days

**Without TTL:**
```
30 days × 24 hours × $1.006/hour = $725
```

**With TTL:**
```bash
spawn launch --instance-type g5.xlarge --ttl 8h
# Costs: 8 hours × $1.006 = $8.05
# Savings: $716.95 (99%)
```

**Best Practice:**
```bash
# Always set TTL
spawn launch --ttl 8h

# Never launch without TTL
```

---

### 2. Use Idle Timeout

**Problem:** Job finishes after 2 hours, but instance runs for 8 hours (TTL)

**Without idle timeout:**
```
8 hours × $1.006 = $8.05
```

**With idle timeout:**
```bash
spawn launch --ttl 8h --idle-timeout 30m
# Terminates 30 minutes after job completes
# Costs: 2.5 hours × $1.006 = $2.52
# Savings: $5.53 (69%)
```

**How it works:**
- Monitors CPU and network usage
- If idle for 30 minutes, terminates
- Prevents paying for unused time

---

### 3. Use on_complete: terminate

**Problem:** Batch job finishes but instance keeps running until TTL

**Without on_complete:**
```bash
spawn launch --ttl 2h --user-data @job.sh
# Job finishes in 30 minutes
# Instance runs for 2 hours
# Cost: 2h × $0.1008 = $0.2016
```

**With on_complete:**
```bash
spawn launch --ttl 2h --on-complete terminate --user-data @job.sh
# Job finishes in 30 minutes, terminates immediately
# Cost: 0.5h × $0.1008 = $0.0504
# Savings: $0.1512 (75%)
```

**In script:**
```bash
#!/bin/bash
# Process data
python process.py

# Upload results
aws s3 cp /results/ s3://bucket/results/ --recursive

# Mark complete (triggers termination)
spored complete --status success
```

---

### 4. Right-Size Instances

**Problem:** Using m7i.4xlarge for job that runs fine on t3.medium

**Test different sizes:**
```bash
# Test 1: t3.medium
spawn launch --instance-type t3.medium --ttl 30m
# Runtime: 45 minutes
# Cost: $0.031

# Test 2: m7i.large
spawn launch --instance-type m7i.large --ttl 30m
# Runtime: 20 minutes
# Cost: $0.034

# Test 3: m7i.4xlarge
spawn launch --instance-type m7i.4xlarge --ttl 30m
# Runtime: 12 minutes
# Cost: $0.081
```

**Analysis:**
- t3.medium: Slowest, cheapest
- m7i.large: Good balance ✅ **Best choice**
- m7i.4xlarge: Fastest, but costs 2.4× more for only 40% time savings

**For 100 jobs:**
- t3.medium: $3.10
- m7i.large: $3.40 ✅ **Only $0.30 more, 2.25× faster**
- m7i.4xlarge: $8.10

**Savings: $4.70 (58%) compared to largest instance**

---

### 5. Use Spot Instances

**See:** [How-To: Spot Instances](spot-instances.md)

**Quick example:**
```bash
# On-demand
spawn launch --instance-type g5.xlarge
# Cost: $1.006/hour

# Spot (70% savings)
spawn launch --instance-type g5.xlarge --spot
# Cost: $0.302/hour
# Savings: $0.704/hour (70%)
```

**For 100-hour sweep:**
- On-demand: $100.60
- Spot: $30.20
- **Savings: $70.40 (70%)**

---

### 6. Batch Jobs Efficiently

**Problem:** Running 1000 instances for 1 file each

**Inefficient:**
```bash
spawn launch --array 1000
# 1000 instances × 5 min each
# Instance startup overhead: 2 min per instance
# Total time: 7 minutes
# Cost: 1000 × 7 min × $0.1008/60 = $11.76
```

**Efficient:**
```bash
spawn launch --array 100
# 100 instances × 10 files each
# Instance startup overhead: 2 min per instance
# Total time: 12 minutes
# Cost: 100 × 12 min × $0.1008/60 = $2.02
# Savings: $9.74 (83%)
```

**In user data:**
```bash
#!/bin/bash
CHUNK_SIZE=10
START=$((TASK_ARRAY_INDEX * CHUNK_SIZE))
END=$((START + CHUNK_SIZE))

for i in $(seq $START $((END - 1))); do
  process_file $i
done
```

---

### 7. Regional Price Arbitrage

**Problem:** us-east-1 is expensive for some instance types

**Check regional pricing:**

| Region | g5.xlarge Price | Difference |
|--------|-----------------|------------|
| us-east-1 | $1.006/hour | Baseline |
| us-west-2 | $1.006/hour | Same |
| eu-west-1 | $1.106/hour | +10% |
| ap-southeast-1 | $1.257/hour | +25% |
| us-east-2 | $0.956/hour | -5% ✅ **Cheapest** |

**Launch in cheapest region:**
```bash
spawn launch --instance-type g5.xlarge --region us-east-2
# Savings: $0.05/hour (5%)
```

**For 1000-hour sweep:**
- us-east-1: $1,006
- us-east-2: $956
- **Savings: $50 (5%)**

**Caution:** Consider data transfer costs if data in different region.

---

### 8. Delete Unused Resources

**Problem:** Old EBS volumes, AMIs, snapshots accumulating

**Find unused volumes:**
```bash
aws ec2 describe-volumes \
  --filters Name=status,Values=available \
  --query 'Volumes[*].[VolumeId,Size]' \
  --output table
```

**Output:**
```
vol-0abc123  100 GB  (available, not attached)
vol-0def456  50 GB   (available, not attached)
vol-0ghi789  200 GB  (available, not attached)
Total: 350 GB unattached

Cost: 350 GB × $0.10/GB/month = $35/month wasted
```

**Delete:**
```bash
aws ec2 delete-volume --volume-id vol-0abc123
aws ec2 delete-volume --volume-id vol-0def456
aws ec2 delete-volume --volume-id vol-0ghi789
```

**Savings: $35/month**

---

### 9. Use EBS gp3 Instead of gp2

**Problem:** Using old gp2 volumes

**gp2 pricing:**
```
$0.10/GB/month
IOPS: 3 IOPS/GB (baseline)
Max: 16,000 IOPS
```

**gp3 pricing:**
```
$0.08/GB/month (-20%)
IOPS: 3,000 IOPS (baseline, free)
Max: 16,000 IOPS
Throughput: 125 MB/s (free)
```

**Launch with gp3:**
```bash
spawn launch --volume-type gp3 --volume-size 100
```

**Savings:**
- 100 GB volume
- gp2: $10/month
- gp3: $8/month
- **Savings: $2/month per volume (20%)**

**For 10 instances:**
- gp2: $100/month
- gp3: $80/month
- **Savings: $20/month**

---

### 10. Minimize Data Transfer

**Problem:** Transferring large datasets in/out

**Data transfer costs:**
```
Data IN: Free
Data OUT:
  First 100 GB/month: Free
  Next 10 TB/month: $0.09/GB
  Next 40 TB/month: $0.085/GB
```

**Example:**
```
Transfer 1 TB out per month
Cost: 1000 GB × $0.09 = $90/month
```

**Solutions:**

**A. Process in same region as data:**
```bash
# Data in us-east-1
spawn launch --region us-east-1

# Not: Launch in us-west-2 (cross-region transfer fees)
```

**B. Use VPC endpoints for S3:**
```bash
# Create VPC endpoint (no data transfer charges for S3 via endpoint)
aws ec2 create-vpc-endpoint \
  --vpc-id vpc-xxx \
  --service-name com.amazonaws.us-east-1.s3
```

**C. Compress before transfer:**
```bash
# Compress before upload
tar -czf results.tar.gz results/
aws s3 cp results.tar.gz s3://bucket/

# Savings: 10 GB → 2 GB (80% reduction)
# Cost: $0.18 → $0.036
# Savings: $0.144 (80%)
```

---

### 11. Scheduled Scaling

**Problem:** Running instances 24/7 for 9-5 workload

**Always on:**
```
24 hours × 30 days × $0.1008 = $72.58/month
```

**Business hours only (9 AM - 6 PM, weekdays):**
```bash
# Use spawn schedule to start/stop instances

# Start Monday-Friday 9 AM
spawn schedule create start-work --cron "0 9 * * 1-5" --action start

# Stop Monday-Friday 6 PM
spawn schedule create stop-work --cron "0 18 * * 1-5" --action stop

# Runs 9 hours/day × 5 days/week
# 9 × 5 × 4 weeks = 180 hours/month
# Cost: 180 × $0.1008 = $18.14/month
# Savings: $54.44 (75%)
```

---

### 12. Budget Alerts

**Problem:** Unexpected costs due to forgotten instances

**Solution: Set budget alerts**
```bash
spawn alerts create global --cost-threshold 500.00 --email ops@example.com
```

**Alerts at:**
- 80% ($400)
- 90% ($450)
- 100% ($500)

**Catches problems early, prevents runaway costs.**

---

### 13. Use Tags for Cost Tracking

**Problem:** Don't know which team/project is spending

**Tag all instances:**
```bash
spawn launch --tags project=ml-research,team=data-science,cost-center=engineering
```

**Track costs by tag:**
```bash
spawn cost --tag project=ml-research
spawn cost --tag team=data-science
```

**AWS Cost Explorer can filter by tags for detailed analysis.**

---

### 14. Clean Up Failed Sweeps

**Problem:** 50 failed instances from sweep, each with 100 GB volume

**Cost:**
```
50 volumes × 100 GB × $0.10/GB/month = $500/month wasted
```

**Find and delete:**
```bash
# List failed instances
spawn list --state terminated --exit-code 1

# Delete associated volumes (AWS does this automatically for most instances)
# But check for orphaned volumes:
aws ec2 describe-volumes --filters Name=status,Values=available

# Delete orphaned volumes
for vol in $(aws ec2 describe-volumes --filters Name=status,Values=available --query 'Volumes[*].VolumeId' --output text); do
  aws ec2 delete-volume --volume-id $vol
done
```

---

### 15. Aggregate Small Jobs

**Problem:** Launching 1000 separate instances for 1-minute jobs

**Inefficient:**
```
1000 instances × 3 minutes (2 min startup + 1 min job)
Cost: 1000 × 3 min × $0.0104/60 = $0.52
```

**Efficient:**
```bash
# Launch 1 instance, process all jobs
spawn launch --ttl 2h --user-data @process-all.sh

# process-all.sh
for i in {1..1000}; do
  process_job $i
done

# Runtime: 1000 minutes = 16.7 hours (with startup overhead)
# But charged for 17 hours
# Cost: 17 × $0.0104 = $0.177
# Savings: $0.343 (66%)
```

**Even better: Use batch queue**
```bash
# Process jobs in queue instead of launching separate instances
# Only 1 instance, processes all 1000 jobs sequentially
```

---

## Cost Monitoring Dashboard

### Weekly Cost Review

```bash
#!/bin/bash
# weekly-cost-review.sh

echo "=== Weekly Cost Report ===="
echo ""

echo "This week:"
spawn cost --time-range 7d

echo ""
echo "By instance type:"
spawn cost --time-range 7d --group-by instance-type

echo ""
echo "By region:"
spawn cost --time-range 7d --group-by region

echo ""
echo "Top 10 expensive instances:"
spawn list --time-range 7d --sort-by cost --limit 10

echo ""
echo "Budget status:"
spawn cost --show-forecast
```

**Run every Monday:**
```bash
crontab -e
0 9 * * 1 /path/to/weekly-cost-review.sh | mail -s "Weekly spawn Cost Report" ops@example.com
```

---

## Real-World Cost Optimization Example

### Before Optimization

**ML Training Pipeline:**
```
- 100 experiments
- g5.xlarge on-demand
- No TTL (ran 24h each)
- No idle timeout
- Monthly cost: 100 × 24h × $1.006 × 4 weeks = $9,657.60/month
```

### After Optimization

**Changes:**
1. Switch to spot (-70%)
2. Add idle timeout (jobs finish in 4h, not 24h)
3. Add on_complete terminate
4. Right-size (some jobs work on g4dn.xlarge at $0.526/h)
5. Batch small experiments

**New Setup:**
```bash
# Large experiments (50)
spawn launch --param-file large-experiments.yaml \
  --instance-type g5.xlarge \
  --spot \
  --ttl 8h \
  --idle-timeout 1h \
  --on-complete terminate

# Small experiments (50)
spawn launch --param-file small-experiments.yaml \
  --instance-type g4dn.xlarge \
  --spot \
  --ttl 4h \
  --idle-timeout 30m \
  --on-complete terminate
```

**New monthly cost:**
```
Large: 50 × 4h × $0.302 (spot) × 4 weeks = $2,409.60
Small: 50 × 2h × $0.158 (spot) × 4 weeks = $632.00
Total: $3,041.60/month

Savings: $6,616.00 (68%)
```

**Optimization breakdown:**
- Spot instances: -70% ($6,760)
- Idle timeout: -67% (24h → 4h average)
- Right-sizing small jobs: -48% (g5 → g4dn)
- Combined effect: -68%

---

## Cost Optimization Checklist

Before launching:
- [ ] Set TTL
- [ ] Enable spot if applicable
- [ ] Set idle-timeout for long jobs
- [ ] Use on_complete: terminate for batch jobs
- [ ] Right-size instance type
- [ ] Check regional pricing
- [ ] Use gp3 volumes
- [ ] Tag for cost tracking

After launching:
- [ ] Monitor costs weekly
- [ ] Review budget alerts
- [ ] Clean up failed instances
- [ ] Delete unused volumes
- [ ] Analyze cost patterns
- [ ] Optimize based on actual usage

---

## See Also

- [Tutorial 6: Cost Management](../tutorials/06-cost-management.md) - Cost basics
- [How-To: Spot Instances](spot-instances.md) - Spot guide
- [How-To: Instance Selection](instance-selection.md) - Right-sizing
- [spawn cost](../reference/commands/cost.md) - Cost command reference
