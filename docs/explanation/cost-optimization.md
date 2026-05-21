# Cost Optimization Theory

Understanding the economics of ephemeral compute and strategies for minimizing AWS costs.

## Cost Model

### AWS EC2 Pricing Components

**Compute (hourly):**
```
Cost = Instance Price × Hours Running

Example: c7i.xlarge @ $0.17/hour
- 1 hour: $0.17
- 8 hours: $1.36
- 24 hours: $4.08
- 30 days: $122.40
```

**Storage (monthly):**
```
Cost = Volume Size (GB) × Storage Price × Months

Example: 50 GB gp3 volume @ $0.08/GB/month
- $4.00/month per instance
```

**Data transfer (GB):**
```
Cost = Data Out (GB) × Transfer Price

Example: 100 GB out @ $0.09/GB
- $9.00
```

**Other:**
- Elastic IPs (when not attached): $0.005/hour
- NAT Gateway: $0.045/hour + $0.045/GB processed
- Load Balancers: $0.0225/hour + $0.008/LCU-hour

### spawn Cost Formula

```
Total Cost =
  (Instance Price × Runtime Hours) +
  (EBS Volume Size × $0.08/GB/month × (Runtime Hours / 730)) +
  (Data Transfer Out × $0.09/GB) +
  (Additional Resources)
```

**Key insight:** Runtime hours dominate costs for short-lived instances.

## Cost Drivers

### 1. Instance Runtime

**Problem:** Instances running longer than needed.

**Impact:**
```
Unneeded Runtime Cost = Instance Price × Wasted Hours

Example: c7i.xlarge left running 16 extra hours
  $0.17/hour × 16 hours = $2.72 wasted
```

**Mitigation:**
- Set aggressive TTLs
- Enable idle detection
- Use `--on-complete terminate`

### 2. Instance Type Selection

**Problem:** Oversized instances.

**Impact:**
```
Oversize Cost = (Large Instance - Right Instance) × Hours

Example: Using c7i.4xlarge instead of c7i.xlarge
  ($0.68 - $0.17) × 10 hours = $5.10 wasted
```

**Mitigation:**
- Profile workload first
- Start small, scale up if needed
- Use cost/performance metrics

### 3. On-Demand vs Spot

**Problem:** Paying full price when spot available.

**Impact:**
```
Spot Savings = Instance Price × (1 - Spot Discount) × Hours

Example: c7i.xlarge for 100 hours
  On-demand: $0.17 × 100 = $17.00
  Spot: $0.05 × 100 = $5.00
  Savings: $12.00 (71%)
```

**Mitigation:**
- Default to spot for interruptible workloads
- Implement checkpointing
- Use on-demand only when necessary

### 4. Regional Price Differences

**Problem:** Using expensive regions.

**Impact:**
```
Regional Savings = (Expensive Region - Cheap Region) × Hours

Example: c7i.xlarge for 100 hours
  us-east-1: $0.17 × 100 = $17.00
  ap-south-1: $0.13 × 100 = $13.00 (23% cheaper)
```

**Mitigation:**
- Check regional pricing
- Use multiple regions
- No data gravity? Choose cheapest

## Optimization Strategies

### Strategy 1: Time-To-Live (TTL)

**Principle:** Auto-terminate prevents forgotten instances.

**Cost savings:**
```
Without TTL:
  Instance forgotten for 30 days = $122.40 (c7i.xlarge)

With TTL (2h):
  Instance terminates after 2 hours = $0.34

Savings: $122.06 (99.7%)
```

**Implementation:**
```bash
# Always set TTL
spawn launch --ttl 2h

# For unknown duration, use max estimate + 20%
spawn launch --ttl 10h  # If job takes ~8h
```

**Trade-off:**
- Too short: Job interrupted, must relaunch
- Too long: Wastes money if job finishes early
- Optimal: Job duration + 20% buffer

### Strategy 2: Idle Detection

**Principle:** Terminate when not working.

**Cost savings:**
```
Job completes in 1 hour, instance sits idle for 7 hours:
  Without idle detection: 8 × $0.17 = $1.36
  With idle detection (15m): 1.25 × $0.17 = $0.21

Savings: $1.15 (85%)
```

**Implementation:**
```bash
spawn launch --ttl 8h --idle-timeout 15m
```

**Trade-off:**
- Aggressive timeout: May terminate during legitimate pauses
- Conservative timeout: Less savings
- Optimal: 2-3× normal pause duration

### Strategy 3: Right-Sizing

**Principle:** Use smallest sufficient instance type.

**Cost savings:**
```
Workflow needs 4 vCPU, 8 GB RAM:
  Over-sized (c7i.2xlarge, 8 vCPU): $0.34/hour
  Right-sized (c7i.xlarge, 4 vCPU): $0.17/hour

Savings: $0.17/hour (50%)

For 1000 hours/month: $170/month saved
```

**Methodology:**
1. Start with small instance
2. Monitor CPU/memory usage
3. Scale up if constrained
4. Document optimal size

**Implementation:**
```bash
# Benchmark different sizes
for TYPE in t3.medium c7i.large c7i.xlarge; do
  TIME=$(spawn launch --instance-type $TYPE --user-data @benchmark.sh)
  COST=$(calculate_cost $TYPE $TIME)
  echo "$TYPE: $TIME seconds, $COST"
done

# Choose best cost/performance ratio
```

### Strategy 4: Spot Instances

**Principle:** 70% savings for interruptible workloads.

**Cost savings:**
```
c7i.xlarge for 100 hours:
  On-demand: $0.17 × 100 = $17.00
  Spot (avg $0.05): $0.05 × 100 = $5.00

Savings: $12.00 (71%)
```

**Implementation:**
```bash
# For fault-tolerant workloads
spawn launch --spot --user-data @job-with-checkpoint.sh
```

**Requirements:**
- Checkpointing every 5-10 minutes
- Resume logic from checkpoint
- Acceptable to lose 2-10% of instances

**Trade-off:**
- High savings (60-90%)
- Interruption risk (5-30% monthly)
- Requires code changes (checkpointing)

### Strategy 5: Scheduled Termination

**Principle:** Stop instances outside business hours.

**Cost savings:**
```
Dev instance running 24/7:
  24 hours × 30 days × $0.17 = $122.40/month

Business hours only (10h/day, 22 days):
  10 hours × 22 days × $0.17 = $37.40/month

Savings: $85.00/month (69%)
```

**Implementation:**
```bash
# Stop at 6 PM, start at 8 AM
crontab:
0 18 * * * spawn stop --tag schedule=business-hours
0 8 * * * spawn start --tag schedule=business-hours
```

**Best for:**
- Development instances
- Non-production workloads
- Interactive environments

### Strategy 6: Batch Job Optimization

**Principle:** Pack work efficiently, minimize overhead.

**Cost savings:**
```
1000 tasks, each 5 minutes:

Inefficient (1 task per instance):
  1000 instances × 10 min (5m task + 5m overhead) × $0.17/hour / 60
  = $28.33

Efficient (20 tasks per instance):
  50 instances × 105 min (100m tasks + 5m overhead) × $0.17/hour / 60
  = $14.88

Savings: $13.45 (47%)
```

**Implementation:**
```bash
# Chunked processing
spawn launch --array 50 --user-data "
  for i in \$(seq \$((TASK_ARRAY_INDEX * 20)) \$((TASK_ARRAY_INDEX * 20 + 19))); do
    process_task \$i
  done
"
```

**Trade-off:**
- Larger chunks: Better efficiency, less parallelism
- Smaller chunks: More parallelism, higher overhead
- Optimal: Task overhead < 5% of task duration

### Strategy 7: Regional Arbitrage

**Principle:** Use cheapest regions for location-agnostic workloads.

**Cost savings:**
```
c7i.xlarge for 1000 hours/month:
  us-east-1: $0.17 × 1000 = $170/month
  ap-south-1: $0.13 × 1000 = $130/month

Savings: $40/month (24%)
```

**Considerations:**
- Data transfer costs (if data in different region)
- Latency (if accessing region-specific resources)
- Compliance (data residency requirements)

**Implementation:**
```bash
# Check cheapest region
spawn availability --instance-type c7i.xlarge

# Launch in cheap region
spawn launch --region ap-south-1
```

## Cost Anti-Patterns

### Anti-Pattern 1: "I'll remember to terminate"

**Problem:** Manual termination is unreliable.

**Cost:**
```
1 forgotten instance for 1 month = $122.40 (c7i.xlarge)
10 forgotten instances = $1,224/month
```

**Solution:** Always set TTL.

### Anti-Pattern 2: "Bigger is safer"

**Problem:** Overprovisioning "just in case."

**Cost:**
```
Using c7i.4xlarge ($0.68/hr) instead of c7i.xlarge ($0.17/hr)
  "Just to be safe"

Wasted: $0.51/hour × 100 hours = $51/month per instance
```

**Solution:** Profile first, then optimize.

### Anti-Pattern 3: "Spot is too risky"

**Problem:** Avoiding spot due to perceived complexity.

**Cost:**
```
1000 hours on-demand instead of spot:
  Overpaid: ($0.17 - $0.05) × 1000 = $120/month
```

**Solution:** Implement checkpointing, use spot.

### Anti-Pattern 4: "One-size-fits-all"

**Problem:** Same instance type for all workloads.

**Cost:**
```
Using c7i.xlarge for everything:
  Small jobs (could use t3.medium @ $0.04/hr): Overpaid $0.13/hr
  Large jobs (need c7i.4xlarge @ $0.68/hr): Underpowered, takes 2× longer
```

**Solution:** Match instance type to workload.

### Anti-Pattern 5: "Dev == Prod"

**Problem:** Using production-sized resources for development.

**Cost:**
```
Dev instance (could use t3.medium @ $0.04/hr):
  Using prod size (c7i.2xlarge @ $0.34/hr)

Wasted: $0.30/hour × 160 hours/month = $48/month
```

**Solution:** Right-size per environment.

## Cost Monitoring

### Key Metrics

**1. Cost per instance-hour:**
```
Total Cost / Total Instance Hours
```

**2. Waste ratio:**
```
(Idle Time / Total Time) × 100%
```

**3. Utilization:**
```
(Actual Work Time / Paid Time) × 100%
```

**4. Spot savings:**
```
(On-Demand Cost - Spot Cost) / On-Demand Cost × 100%
```

### Alerting Thresholds

**Daily cost:**
```bash
# Alert if daily cost > $100
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-daily-cost \
  --comparison-operator GreaterThanThreshold \
  --threshold 100
```

**Instance count:**
```bash
# Alert if > 50 running instances
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-instance-count \
  --comparison-operator GreaterThanThreshold \
  --threshold 50
```

## Cost Optimization Workflow

### Phase 1: Baseline (Week 1)

1. Enable cost tracking (tags)
2. Run workloads normally
3. Measure:
   - Total cost
   - Cost per instance type
   - Runtime distribution

### Phase 2: Quick Wins (Week 2)

1. Add TTLs to all launches
2. Enable idle detection
3. Switch to spot (where applicable)
4. Expected savings: 30-50%

### Phase 3: Right-Sizing (Week 3)

1. Profile workloads
2. Test smaller instance types
3. Document optimal sizes
4. Update launch scripts
5. Expected savings: 10-30%

### Phase 4: Advanced (Week 4+)

1. Regional arbitrage
2. Scheduled stop/start
3. Batch job optimization
4. Custom AMIs (faster boot = less cost)
5. Expected savings: 10-20%

## Total Cost of Ownership (TCO)

### spawn (Cloud)

**Costs:**
```
Compute: Variable (pay-per-use)
Storage: $4/month per instance (50 GB)
Network: $0.09/GB out
Management: $0 (no cluster to maintain)
```

**Example: 100 hours/month compute:**
```
Compute: $17.00 (c7i.xlarge × 100 hours)
Storage: $0.55 (50 GB × 100 hours / 730 hours/month)
Network: $1.00 (assume 10 GB out)
Total: $18.55/month
```

### On-Premises

**Costs:**
```
Hardware: $50,000 / 36 months = $1,389/month (10-node cluster)
Power: $100/month per node × 10 = $1,000/month
Cooling: $50/month per node × 10 = $500/month
Staff: 0.25 FTE × $120k/year = $2,500/month
Total: $5,389/month
```

**Break-even analysis:**
```
spawn cost for 24/7 usage: ~$1,224/month (c7i.xlarge)
On-prem per node: $539/month

Break-even: ~2-3 nodes worth of utilization

Conclusion:
- < 20% utilization: spawn cheaper
- > 50% utilization: on-prem cheaper
- Variable workloads: spawn much cheaper
```

## Related Documentation

- [How-To: Cost Optimization](../how-to/cost-optimization.md) - Practical cost recipes
- [How-To: Spot Instances](../how-to/spot-instances.md) - Spot patterns
- [Tutorial 6: Cost Management](../tutorials/06-cost-management.md) - Cost basics
- [spawn cost command](../reference/commands/cost.md) - Cost analysis tool
