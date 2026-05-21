# Cloud Economics with spawn

## The Power of Cloud: Pay Only for What You Use

Traditional on-premises computing: You pay for hardware 24/7 whether you use it or not.

**Cloud computing done right**: You pay only when instances are running. When stopped, you pay only minimal storage/network costs (~2-3% of compute).

**spawn makes this effortless**: Automatic lifecycle management so you get cloud savings without thinking about it.

---

## Key Metrics

### Sticker Price (All-In)
What you'd pay per hour if the instance ran 24/7:
- **Compute**: $4.09/hr (r7i.16xlarge on-demand rate)
- **Storage**: $0.01/hr (typical 100GB EBS gp3)
- **Network**: $0.07/hr (IPv4 + typical egress)
- **Total**: $4.17/hr

### Effective Cost Per Hour
What you **actually** pay per hour on average:
```
Effective Cost = Total Cost ÷ Total Lifetime Hours
```

Where:
- **Total Cost**: All charges (compute + storage + network + data transfer)
- **Total Lifetime**: Time from launch to termination (running + stopped)

### Cloud Savings
```
Savings % = (Sticker Price - Effective Cost) / Sticker Price × 100
```

Positive = you're saving money vs 24/7 (good!)
Negative = you're paying more than 24/7 (bad, usually due to misconfiguration)

---

## How spawn Delivers Savings

### 1. Automatic Idle Detection
```bash
spawn launch --instance-type r7i.16xlarge --idle-timeout 30m
```

**What happens**:
- Instance launches and runs your workload
- spored agent monitors CPU/network activity
- After 30 minutes idle, instance automatically stops
- Compute charges: **$0/hour**
- Storage charges: **$0.01/hour** (99.7% savings!)

### 2. Automatic TTL Termination
```bash
spawn launch --instance-type r7i.16xlarge --ttl 8h
```

**What happens**:
- Instance runs for up to 8 hours
- At 8 hours, automatically terminates
- No orphaned resources, no forgotten instances
- Pay $0 after termination

### 3. Hibernation for Fast Resume
```bash
spawn launch --instance-type r7i.16xlarge --hibernate --on-idle hibernate
```

**What happens**:
- On idle, RAM saved to EBS and instance hibernates
- Resume in <60 seconds with full state preserved
- **Trade-off**: Higher EBS cost (root volume must be >= RAM size)
- Use when: Resume time matters more than cost

---

## Real-World Examples

### Example 1: Research Sweep (Good Cloud Usage)

**Scenario**: 100 instances, run for 10 hours each, 80% utilization

```
Configuration: spawn sweep --ttl 10h --idle-timeout 30m

Timeline:
- Launch: 100 instances × r7i.16xlarge
- Running: 800 hours total (80% of 1,000 hour-instances)
- Stopped: 200 hours (20% idle time, auto-stopped)
- Termination: All terminated at 10h TTL

Costs:
- Compute: $4.09/hr × 800h = $3,272
- Storage: $0.01/hr × 1,000h = $10
- Network: $0.07/hr × 1,000h = $70
- Total: $3,352

Sticker Price (if 24/7): $4.17/hr × 1,000h = $4,170
Effective Cost: $3,352 ÷ 1,000h = $3.35/hr
Savings: 20% ($818 saved)

✅ spawn automation delivered savings:
- Auto-stop on idle: Avoided $818 in compute costs
- Auto-terminate at TTL: No orphaned resources
- Zero manual management required
```

### Example 2: Hibernation Trade-Off (Fast Resume vs Cost)

**Scenario**: 1 instance, runs 20h over 5 days, hibernation enabled

```
Configuration: spawn launch --hibernate --ttl 5d

Timeline:
- Total lifetime: 120 hours (5 days)
- Running: 20 hours (17% utilization)
- Hibernated: 100 hours (83% stopped)

Costs WITHOUT hibernation:
- Compute: $4.09/hr × 20h = $81.80
- Storage: $0.01/hr × 120h = $1.20 (100GB EBS)
- Network: $0.07/hr × 120h = $8.40
- Total: $91.40
- Effective: $91.40 ÷ 120h = $0.76/hr
- Savings: 82% vs sticker ($4.17/hr)

Costs WITH hibernation (512GB RAM):
- Compute: $4.09/hr × 20h = $81.80
- Storage: $0.056/hr × 120h = $6.72 (512GB EBS for hibernation)
- Network: $0.07/hr × 120h = $8.40
- Total: $96.92
- Effective: $96.92 ÷ 120h = $0.81/hr
- Savings: 81% vs sticker
- Hibernation overhead: $5.52

Trade-off analysis:
✅ Good if: Resume time matters, interactive workload
❌ Bad if: Can tolerate 5min restart, batch workload
💡 spawn makes this ONE FLAG: --hibernate vs no flag
```

### Example 3: Bad Cloud Usage (Over-Provisioned Storage)

**Scenario**: Instances with unnecessarily large EBS volumes

```
Configuration:
- Instance: m5.xlarge (sticker $0.192/hr compute)
- EBS: 1TB gp3 (should be 100GB)

Timeline:
- Total lifetime: 100 hours
- Running: 50 hours (50% utilization)

Costs:
- Compute: $0.192/hr × 50h = $9.60
- Storage: $0.011/hr × 100h = $1.10 (1TB = 10x too large!)
- Network: $0.005/hr × 100h = $0.50
- Total: $11.20

Expected with right-sized storage:
- Compute: $9.60
- Storage: $0.0011/hr × 100h = $0.11 (100GB)
- Network: $0.50
- Total: $10.21

Sticker Price: $0.20/hr
Effective (current): $11.20 ÷ 100h = $0.112/hr
Effective (optimized): $10.21 ÷ 100h = $0.102/hr

🚨 Storage misconfiguration eating into savings!
💡 Fix: Use smaller EBS volumes or instance-store AMIs
```

---

## Cost Breakdown Output

When you run `spawn cost breakdown <sweep-id>`, you'll see:

```
Sweep: research-sweep-20260129
Status: COMPLETED
Duration: 10 hours

💰 Cloud Economics: Effective Cost Per Hour
=============================================

Sticker Price (all-in):  $4.17/hr  (if running 24/7)
  ├─ Compute:        $4.09/hr
  ├─ Storage:        $0.01/hr
  └─ Network:        $0.07/hr

Effective Cost/Hour:     $3.35/hr  (total cost ÷ total lifetime)
  ├─ Compute:        $3.27/hr  (only charged when running)
  ├─ Storage:        $0.01/hr  (charged 24/7)
  └─ Network:        $0.07/hr  (charged 24/7)

✅ You're paying 20% LESS than sticker price!
   This is the POWER of cloud economics.

🎯 spawn made this effortless:
   • You set: --idle-timeout 30m --ttl 10h
   • spawn automatically: stopped when idle, terminated at TTL
   • Result: $818 saved vs running 24/7
   • Zero manual management required

Breakdown:
  • Running: 800h (80% utilization)
  • Stopped: 200h (20% - no compute charges!)
  • Compute savings: $4.09/hr × 200h = $818 avoided
  • Storage overhead: $0.08/hr × 1,000h = $80 always-on
  • Net savings: $738 (18% of what 24/7 would cost)

💡 spawn recommendation:
   ✅ Current config is well-optimized
   • 80% utilization is excellent for stop/start strategy
   • Consider: Lower idle timeout to increase savings (if workload allows)
```

---

## Best Practices

### When to Stop vs Terminate

**Use stop (--on-idle stop)**:
- Short gaps between work (< 1 day)
- Fast resume needed (< 2 minutes)
- Storage overhead minimal (<$1/day)
- Example: Interactive dev environments

**Use terminate (--on-idle terminate)**:
- Long gaps between work (> 1 day)
- Restart time acceptable (5-10 minutes)
- Storage costs matter
- Example: Batch jobs, CI/CD agents

**Use hibernation (--hibernate)**:
- Critical state in RAM to preserve
- Fast resume essential (<60 seconds)
- Accept higher storage cost
- Example: Long-running simulations with checkpointing challenges

### Optimization Guidelines

**Target utilization for stop/start**:
- **>70% utilization**: Excellent cloud usage, 20-30% savings
- **40-70% utilization**: Good, 30-50% savings
- **<40% utilization**: Watch storage overhead, may want to terminate instead

**The math**:
```
Lower utilization = MORE savings (counterintuitive!)

100% util: Pay 100% of sticker (no savings)
80% util:  Pay 84% of sticker (16% savings)
50% util:  Pay 51% of sticker (49% savings)
20% util:  Pay 21% of sticker (79% savings)

Why? You avoid compute costs during stopped time,
and storage overhead is only 2-3% of compute cost.
```

### spawn Configuration Examples

**Aggressive cost optimization**:
```bash
spawn launch \
  --idle-timeout 15m \
  --ttl 4h \
  --on-idle terminate
```
Target: 80%+ savings for bursty workloads

**Balanced (recommended)**:
```bash
spawn launch \
  --idle-timeout 30m \
  --ttl 8h \
  --on-idle stop
```
Target: 40-60% savings, reasonable resume times

**Performance over cost**:
```bash
spawn launch \
  --hibernate \
  --idle-timeout 1h \
  --ttl 24h
```
Target: Fast resume, still 20-40% savings

---

## Understanding the Savings

### Why Lower Utilization = More Savings

This seems backwards but it's the core insight of cloud economics:

**Traditional thinking** (on-prem):
- You paid for hardware whether you use it or not
- Goal: Maximize utilization (spread fixed cost)

**Cloud thinking**:
- Compute charges STOP when instance stops
- Storage/network continue but are ~2-3% of compute cost
- Goal: Minimize waste (run only when needed)

**The formula**:
```
Avoided Compute Cost = compute_rate × stopped_hours
Overhead Cost = (storage + network) × total_hours

Savings = Avoided Compute Cost - Overhead Cost

Since compute >> storage+network:
More stopped time → More avoided cost → More savings
```

### Real Example

r7i.16xlarge: Compute $4.09/hr, Storage $0.01/hr, Network $0.07/hr

**Scenario A: 90% utilization** (traditional "efficient"):
- Run 90h, stopped 10h (100h total)
- Compute: $4.09 × 90 = $368
- Overhead: $0.08 × 100 = $8
- Total: $376
- Sticker (100h @ $4.17/hr): $417
- **Savings: 10% ($41)**

**Scenario B: 20% utilization** (seems "wasteful"):
- Run 20h, stopped 80h (100h total)
- Compute: $4.09 × 20 = $82
- Overhead: $0.08 × 100 = $8
- Total: $90
- Sticker (100h @ $4.17/hr): $417
- **Savings: 78% ($327)**

Lower utilization = MORE savings! 🎉

### spawn Enables This

Without automation, you'd:
1. Forget to stop instances → Pay 24/7 rates
2. Forget to terminate → Accumulate orphaned resources
3. Manually start/stop → Too much effort, give up

With spawn:
1. Set `--idle-timeout 30m` → Automatic stop
2. Set `--ttl 8h` → Automatic terminate
3. spored agent → Monitors and enforces
4. Cost tracking → Proves it's working

**Result**: You get cloud savings effortlessly.

---

## Validating Your Savings

Run `spawn cost breakdown` after each sweep to see:

1. **Effective cost vs sticker price**
   - Positive savings = good cloud usage
   - Negative = investigate misconfiguration

2. **Utilization percentage**
   - Shows what % of time instances ran
   - Lower = more savings (counterintuitive but correct!)

3. **Breakdown by cost component**
   - Compute: Should be majority of cost
   - Storage: Should be <5% (watch for hibernation overhead)
   - Network: Should be <5%

4. **Recommendations**
   - spawn suggests optimizations
   - One-line config changes for better savings

---

## FAQ

### Q: Shouldn't I maximize utilization like on-prem?

**A**: No! In cloud, maximize **value**, not utilization.

- Run instances when you need them
- Stop/terminate when you don't
- Pay only for actual usage
- Lower utilization often means better cloud economics

### Q: Why does effective cost decrease with more stopped time?

**A**: Because compute charges stop when instance stops, but storage/network overhead is tiny.

```
Running 100%: Pay $4.17/hr (no savings)
Running 50%:  Pay ~$2.10/hr (50% savings on compute)
Running 10%:  Pay ~$0.50/hr (90% savings on compute)
```

### Q: When should I NOT use stop/start?

**A**: When storage overhead exceeds compute savings:
- Very short workloads (<5 min) with large EBS
- Instances with expensive attached resources (EFS, FSx)
- In these cases, terminate and relaunch is cheaper

### Q: How does spawn compare to AWS Instance Scheduler?

| Feature | spawn | AWS Instance Scheduler |
|---------|-------|------------------------|
| Idle detection | ✅ Yes (CPU/network) | ❌ No (time-based only) |
| TTL auto-terminate | ✅ Yes | ❌ No |
| Cost tracking | ✅ Yes (comprehensive) | ❌ No |
| Hibernation | ✅ Yes | ❌ No |
| Setup complexity | ✅ One command | ❌ CloudFormation + tags |

spawn is purpose-built for research workloads with automatic cost optimization.

---

## Summary

**Cloud economics**: Pay only for what you use, minimize waste.

**spawn's role**: Makes optimization automatic and effortless.

**Key insight**: Lower utilization often means MORE savings (not less).

**Validation**: `spawn cost breakdown` proves you're saving money.

**Result**: Get 40-80% savings vs 24/7 with zero manual management.

This is the power of cloud computing when done right. spawn makes it easy.
