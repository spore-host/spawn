# How-To: Instance Selection

Choose the right EC2 instance type for your workload.

## Understanding Instance Types

### Naming Convention

Format: `<family><generation><features>.<size>`

**Example: `m7i.xlarge`**
- `m` = Family (general purpose)
- `7` = Generation (7th generation)
- `i` = Intel processor
- `xlarge` = Size (4 vCPU, 16 GB RAM)

### Instance Families

| Family | Purpose | Use Cases |
|--------|---------|-----------|
| **t3, t4g** | Burstable | Web servers, dev/test, small databases |
| **m5, m6i, m7i** | General purpose | Balanced workloads, most applications |
| **c5, c6i, c7i** | Compute optimized | HPC, batch processing, gaming servers |
| **r5, r6i, r7i** | Memory optimized | Databases, caching, big data analytics |
| **g4dn, g5, g6** | GPU | ML training, graphics rendering |
| **p4, p5** | High-end GPU | Large-scale ML, scientific computing |
| **i3, i4i** | Storage optimized | NoSQL databases, data warehousing |

---

## Quick Selection Guide

### Development & Testing

**Light development:**
```bash
spawn launch --instance-type t3.micro
# 2 vCPU, 1 GB RAM, $0.0104/hour
```

**Active development:**
```bash
spawn launch --instance-type t3.medium
# 2 vCPU, 4 GB RAM, $0.0416/hour
```

**Compilation-heavy:**
```bash
spawn launch --instance-type c7i.large
# 2 vCPU, 4 GB RAM, optimized for CPU, $0.0851/hour
```

---

### Web Applications

**Low traffic site:**
```bash
spawn launch --instance-type t3.small
# 2 vCPU, 2 GB RAM, $0.0208/hour
```

**Production web server:**
```bash
spawn launch --instance-type m7i.large
# 2 vCPU, 8 GB RAM, $0.1008/hour
```

**High-traffic API:**
```bash
spawn launch --instance-type c7i.xlarge
# 4 vCPU, 8 GB RAM, $0.1701/hour
```

---

### Data Processing

**Batch processing (CPU-intensive):**
```bash
spawn launch --instance-type c7i.2xlarge
# 8 vCPU, 16 GB RAM, $0.3402/hour
```

**Data analysis (memory-intensive):**
```bash
spawn launch --instance-type r7i.xlarge
# 4 vCPU, 32 GB RAM, $0.2520/hour
```

**ETL pipelines:**
```bash
spawn launch --instance-type m7i.xlarge
# 4 vCPU, 16 GB RAM, $0.2016/hour
```

---

### Machine Learning

**Small model training:**
```bash
spawn launch --instance-type m7i.xlarge
# 4 vCPU, 16 GB RAM, $0.2016/hour
# CPU-only, good for small models
```

**GPU training (single GPU):**
```bash
spawn launch --instance-type g5.xlarge
# 4 vCPU, 16 GB RAM, 24 GB GPU, $1.006/hour
# NVIDIA A10G, good for most ML workloads
```

**Large model training:**
```bash
spawn launch --instance-type g5.2xlarge
# 8 vCPU, 32 GB RAM, 48 GB GPU, $1.212/hour
# NVIDIA A10G with more memory
```

**Production inference:**
```bash
spawn launch --instance-type g4dn.xlarge
# 4 vCPU, 16 GB RAM, 16 GB GPU, $0.526/hour
# NVIDIA T4, cost-effective for inference
```

**Multi-GPU training:**
```bash
spawn launch --instance-type p4d.24xlarge
# 96 vCPU, 1152 GB RAM, 8× 40GB A100 GPUs, $32.77/hour
# For large-scale training
```

---

### Databases

**Small database:**
```bash
spawn launch --instance-type t3.medium
# 2 vCPU, 4 GB RAM, $0.0416/hour
```

**Production database:**
```bash
spawn launch --instance-type r7i.large
# 2 vCPU, 16 GB RAM, $0.1260/hour
# Memory-optimized
```

**Large database with caching:**
```bash
spawn launch --instance-type r7i.4xlarge
# 16 vCPU, 128 GB RAM, $1.0080/hour
```

---

## Performance Testing

### Methodology

Test with different instance types to find optimal cost/performance ratio.

```bash
#!/bin/bash
# test-performance.sh

INSTANCE_TYPES="t3.medium m7i.large c7i.xlarge"

for TYPE in $INSTANCE_TYPES; do
  echo "Testing $TYPE..."

  # Launch instance
  INSTANCE_ID=$(spawn launch --instance-type $TYPE --ttl 1h --quiet)

  # Wait for ready
  spawn connect $INSTANCE_ID --wait

  # Run benchmark
  START=$(date +%s)
  spawn connect $INSTANCE_ID -c "python benchmark.py"
  END=$(date +%s)
  DURATION=$((END - START))

  # Get cost
  COST=$(spawn cost --instance-id $INSTANCE_ID --json | jq -r '.total_cost')

  echo "$TYPE: ${DURATION}s, \$${COST}"

  # Terminate
  aws ec2 terminate-instances --instance-ids $INSTANCE_ID
done
```

**Output:**
```
Testing t3.medium...
t3.medium: 180s, $0.0021

Testing m7i.large...
m7i.large: 120s, $0.0034

Testing c7i.xlarge...
c7i.xlarge: 60s, $0.0028
```

**Analysis:**
- t3.medium: Slowest, cheapest per hour, but longer runtime
- m7i.large: Middle ground
- c7i.xlarge: Fastest, best $/task ratio (60s vs 180s, only 33% more cost)

**Winner: c7i.xlarge** (fastest completion, best value)

---

## ARM vs x86

### ARM Instances (Graviton)

**t4g family** - AWS Graviton2
**m7g family** - AWS Graviton3

**Benefits:**
- 20-40% cheaper than x86
- Better price/performance
- Lower power consumption

**Drawbacks:**
- Some software not ARM-compatible
- May need recompilation

**When to use:**
```bash
spawn launch --instance-type t4g.medium  # 20% cheaper than t3.medium
```

**Check compatibility first:**
```bash
# Test if your code runs on ARM
spawn launch --instance-type t4g.micro --ttl 30m
spawn connect <instance-id>
python test.py  # Verify works
```

---

## GPU Selection

### GPU Comparison

| Instance Type | GPU | GPU Memory | vCPU | RAM | $/hour | Use Case |
|---------------|-----|------------|------|-----|--------|----------|
| g4dn.xlarge | T4 | 16 GB | 4 | 16 GB | $0.526 | Inference |
| g5.xlarge | A10G | 24 GB | 4 | 16 GB | $1.006 | Training |
| g5.2xlarge | A10G | 48 GB | 8 | 32 GB | $1.212 | Large models |
| p4d.24xlarge | 8× A100 | 8× 40 GB | 96 | 1152 GB | $32.77 | Multi-GPU |
| p5.48xlarge | 8× H100 | 8× 80 GB | 192 | 2048 GB | $98.32 | Largest models |

### Choose by Model Size

**Small models (< 1B parameters):**
```bash
spawn launch --instance-type g5.xlarge
```

**Medium models (1-7B parameters):**
```bash
spawn launch --instance-type g5.2xlarge
```

**Large models (7-70B parameters):**
```bash
spawn launch --instance-type p4d.24xlarge
```

**Massive models (> 70B parameters):**
```bash
spawn launch --instance-type p5.48xlarge
```

---

## Cost Optimization

### Right-Sizing Strategy

**Start small, scale up only if needed:**

```bash
# Step 1: Test with smallest suitable instance
spawn launch --instance-type t3.medium --ttl 30m
# Test your workload

# Step 2: If too slow, try next size up
spawn launch --instance-type m7i.large --ttl 30m

# Step 3: If still too slow, try compute-optimized
spawn launch --instance-type c7i.xlarge --ttl 30m

# Step 4: Benchmark and choose
```

### Cost vs Time Trade-off

**Example: Process 1000 files**

| Instance | Time | Cost | Notes |
|----------|------|------|-------|
| t3.medium | 10h | $0.42 | Slow but cheap per hour |
| m7i.large | 5h | $0.50 | Good balance |
| c7i.xlarge | 2h | $0.34 | Fast, best value |
| c7i.4xlarge | 1h | $0.68 | Fastest, expensive |

**Winner: c7i.xlarge** - Completes quickly at reasonable cost.

**Key insight:** Faster instance isn't always more expensive overall.

---

## Spot Instance Considerations

### Most Stable for Spot

**Low interruption rates (< 5%):**
- t3, t4g families
- m5, m6i, m7i families (older generations)

**Medium interruption rates (5-15%):**
- m7i, m7a (latest generation)
- c7i, c7a
- r7i, r7a

**High interruption rates (15-25%):**
- g5 (GPU)
- Latest generation compute/memory-optimized

**Very high (25-50%):**
- p4, p5 (high-end GPU)

**Strategy:**
```bash
# For critical work: Use stable families
spawn launch --spot --instance-type t3.large

# For retriable work: Any instance type fine
spawn launch --spot --instance-type g5.xlarge
```

---

## Burstable Instances (t3, t4g)

### How Burstable Works

**CPU Credits:**
- Earn credits when idle (below baseline)
- Spend credits when busy (above baseline)
- If credits exhausted, throttled to baseline

**Baseline performance:**
- t3.micro: 10% of vCPU
- t3.small: 20% of vCPU
- t3.medium: 20% of vCPU
- t3.large: 30% of vCPU

### When to Use Burstable

**Good for:**
- Web servers (bursty traffic)
- Development environments
- Batch jobs with pauses
- Cost-sensitive workloads

**Bad for:**
- Sustained high CPU
- Continuous processing
- Performance-critical applications

### Check CPU Credits

```bash
# Connect to instance
spawn connect <instance-id>

# Check credits
aws cloudwatch get-metric-statistics \
  --namespace AWS/EC2 \
  --metric-name CPUCreditBalance \
  --dimensions Name=InstanceId,Value=<instance-id> \
  --start-time $(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%S) \
  --end-time $(date -u +%Y-%m-%dT%H:%M:%S) \
  --period 300 \
  --statistics Average
```

**Low credits:** Switch to non-burstable instance (m7i, c7i).

---

## Network Performance

### Network Bandwidth

| Instance Size | Bandwidth |
|---------------|-----------|
| t3.micro | Up to 5 Gbps |
| t3.medium | Up to 5 Gbps |
| m7i.large | Up to 12.5 Gbps |
| m7i.xlarge | Up to 12.5 Gbps |
| m7i.2xlarge | Up to 12.5 Gbps |
| m7i.4xlarge | 12.5 Gbps |
| m7i.8xlarge | 12.5 Gbps |
| m7i.16xlarge | 25 Gbps |

**For data-intensive workloads:**
```bash
# Use larger instances for better network
spawn launch --instance-type m7i.4xlarge
```

---

## Memory Requirements

### Estimating Memory Needs

**General rule:** 2-4× your data size

**Examples:**

**Load 10 GB dataset:**
```bash
# Need 20-40 GB RAM
spawn launch --instance-type r7i.xlarge  # 32 GB RAM
```

**ML training:**
```bash
# Model size + batch size + activation memory
# ResNet-50: ~250 MB model
# Batch 64: ~8 GB
# Activations: ~4 GB
# Total: ~12 GB + overhead = 16 GB needed

spawn launch --instance-type m7i.large  # 8 GB (too small)
spawn launch --instance-type m7i.xlarge  # 16 GB (minimum)
spawn launch --instance-type m7i.2xlarge  # 32 GB (comfortable)
```

---

## Decision Matrix

### Quick Selection Matrix

| Workload | Instance Type | Why |
|----------|---------------|-----|
| Dev/test | t3.micro-medium | Cheap, burstable |
| Web server | m7i.large | Balanced |
| API server | c7i.xlarge | Fast CPU |
| Database | r7i.large-4xlarge | Memory |
| Batch CPU | c7i.2xlarge-4xlarge | CPU-optimized |
| Batch GPU | g5.xlarge-2xlarge | GPU |
| Large-scale ML | p4d.24xlarge | Multi-GPU |

---

## See Also

- [Tutorial 2: Your First Instance](../tutorials/02-first-instance.md) - Instance type basics
- [How-To: Cost Optimization](cost-optimization.md) - Save money
- [How-To: Spot Instances](spot-instances.md) - Use spot
- [AWS Instance Types](https://aws.amazon.com/ec2/instance-types/) - Complete list
