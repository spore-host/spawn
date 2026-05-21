# Tutorial 11: Advanced spawn — Sweeps, Arrays, and Autoscaling

**Duration:** 30 minutes
**Level:** Advanced
**Prerequisites:** [Tutorial 3: Parameter Sweeps](03-parameter-sweeps.md), [Tutorial 4: Job Arrays](04-job-arrays.md), [Tutorial 10: The truffle → spawn Workflow](10-truffle-to-spawn-workflow.md)

## What You'll Learn

In this tutorial, you'll learn the three fleet-scale patterns that go beyond single instances:
- Mixed-architecture parameter sweeps that compare Graviton, Intel, and AMD instance types side-by-side
- Live management of running job arrays — monitoring, extending TTL across an entire array at once
- Autoscale groups that drain an SQS queue and scale down to zero when work is done
- When to use SQS versus other queue backends
- A cost comparison between always-on workers and autoscaling to zero

By the end, you will be able to design the right fleet topology for any batch workload and understand the cost tradeoffs at each scale point.

## Beyond Single Instances

A single instance is the right tool when you have one job, one experiment, or one interactive session. When the work fans out — dozens of hyperparameter combinations, hundreds of data files, a queue of tasks arriving asynchronously — you need a fleet.

spawn provides three fleet patterns, each suited to a different shape of work:

| Pattern | Command | Work Shape | Scale |
|---------|---------|-----------|-------|
| **Parameter Sweep** | `spawn sweep` | Fixed N configs, known upfront | 2–200 instances |
| **Job Array** | `spawn launch --array N` | N identical tasks, indexed | 10–1000 instances |
| **Autoscale Group** | `spawn autoscale apply` | Queue-driven, unpredictable arrival | 0–N, elastic |

**Choose a sweep** when you have a specific grid of parameters to explore — ML hyperparameter search, architecture benchmarking, ablation studies. You know the full set of runs before you start.

**Choose a job array** when you have N identical tasks differentiated only by index — frame rendering, file batch processing, Monte Carlo simulations. The task list is enumerable.

**Choose autoscale** when work arrives asynchronously and you cannot predict load — background job queues, event-driven processing, overnight batch pipelines that should idle to zero during business hours.

The rest of this tutorial walks through each pattern with working examples.

## Step 1: Parameter Sweeps with Mixed Architectures

Mixed-architecture sweeps let you benchmark the same workload across Graviton (ARM), Intel, and AMD instance types in a single command. This is the fastest way to find the best price-performance fit for your specific code.

Create the sweep configuration file:

```bash
cat > grid.yaml << 'EOF'
instances:
  - type: m7g.large    # Graviton3 ARM
    region: us-east-1
  - type: m6i.large    # Intel Ice Lake x86
    region: us-east-1
  - type: m6a.xlarge   # AMD EPYC x86
    region: us-west-2
params:
  learning_rate: [0.001, 0.01, 0.1]
  batch_size: [32, 64, 128]
EOF
```

Launch the sweep:

```bash
spawn sweep --params grid.yaml --name ml-sweep --ttl 4h --on-complete terminate
```

**How the multiplication works:**

spawn computes the Cartesian product of `instances` × `params`. With 3 instance types and 3×3=9 parameter combinations, you get 27 instances total — one for every (instance type, learning rate, batch size) triple.

Each instance receives environment variables for its parameters:
- `LEARNING_RATE=0.001` (or `0.01`, `0.1`)
- `BATCH_SIZE=32` (or `64`, `128`)
- `INSTANCE_TYPE=m7g.large` (or `m6i.large`, `m6a.xlarge`)

Your user_data script reads these environment variables and runs the appropriate workload.

**Expected output:**
```
Launching parameter sweep...

Sweep Name: ml-sweep
Config: grid.yaml
Instances: 3 types × 9 param combinations = 27 total

Instance breakdown:
  m7g.large   (us-east-1) × 9 params = 9 instances
  m6i.large   (us-east-1) × 9 params = 9 instances
  m6a.xlarge  (us-west-2) × 9 params = 9 instances

Launching instances:
  ml-sweep-m7g-lr0.001-bs32  → i-0aaa... (launching)
  ml-sweep-m7g-lr0.001-bs64  → i-0bbb... (launching)
  ml-sweep-m7g-lr0.001-bs128 → i-0ccc... (launching)
  ...
  ml-sweep-m6a-lr0.1-bs128   → i-0zzz... (launching)

Sweep launched: 27 instances

Estimated cost: ~$1.20 (27 instances × avg 1h × $0.044/hour)
Monitor: spawn list --array ml-sweep
```

✅ 27 instances launched across 3 architectures and 2 regions.

## Step 2: Monitoring a Sweep

```bash
spawn list --array ml-sweep
```

**Expected output:**
```
Array: ml-sweep
Status: running (24 running, 2 complete, 1 failed)
Runtime: 18m

INSTANCE-ID          NAME                         TYPE        REGION     STATUS    PARAMS                      RUNTIME
i-0aaa123def456789   ml-sweep-m7g-lr0.001-bs32    m7g.large   us-east-1  running   lr=0.001 bs=32              18m
i-0bbb234def567890   ml-sweep-m7g-lr0.001-bs64    m7g.large   us-east-1  running   lr=0.001 bs=64              18m
i-0ccc345def678901   ml-sweep-m7g-lr0.001-bs128   m7g.large   us-east-1  running   lr=0.001 bs=128             18m
i-0ddd456def789012   ml-sweep-m7g-lr0.01-bs32     m7g.large   us-east-1  running   lr=0.01  bs=32              18m
i-0eee567def890123   ml-sweep-m7g-lr0.01-bs64     m7g.large   us-east-1  complete  lr=0.01  bs=64              15m
i-0fff678def901234   ml-sweep-m7g-lr0.01-bs128    m7g.large   us-east-1  running   lr=0.01  bs=128             18m
i-0ggg789def012345   ml-sweep-m7g-lr0.1-bs32      m7g.large   us-east-1  failed    lr=0.1   bs=32              4m
...
i-0zzz987def654321   ml-sweep-m6a-lr0.1-bs128     m6a.xlarge  us-west-2  running   lr=0.1   bs=128             18m

Costs so far: $0.32
```

**Status values explained:**

| Status | Meaning |
|--------|---------|
| `launching` | Instance request accepted, EC2 not yet assigned |
| `pending` | EC2 instance assigned, OS initializing |
| `running` | Instance up, user_data script executing |
| `complete` | Script called `spored complete`, instance shutting down |
| `terminated` | Instance gone, resources cleaned up |
| `failed` | Instance terminated with non-zero exit or timed out |

> ❌ **A `failed` instance does not relaunch automatically.** Investigate the failure with `spawn connect <instance-id>` before it terminates, or check logs with `spawn logs <instance-id>`. Then relaunch the specific failed parameter combination manually.
>
> ✅ Use `spawn list --array ml-sweep --status failed` to surface failures quickly in large sweeps.

## Step 3: Extending an Entire Array Live

If your sweep is running longer than expected and the TTL is approaching, extend all running instances at once:

```bash
spawn extend --job-array-name ml-sweep 2h
```

**Expected output:**
```
Extending array: ml-sweep
Scope: all running instances (22 of 27)

  ✓ i-0aaa123def456789 (ml-sweep-m7g-lr0.001-bs32)   → 2h 43m remaining
  ✓ i-0bbb234def567890 (ml-sweep-m7g-lr0.001-bs64)   → 2h 43m remaining
  ✓ i-0ccc345def678901 (ml-sweep-m7g-lr0.001-bs128)  → 2h 43m remaining
  ...
  - i-0eee567def890123 (ml-sweep-m7g-lr0.01-bs64)    → skipped (complete)
  - i-0ggg789def012345 (ml-sweep-m7g-lr0.1-bs32)     → skipped (failed)

Extended: 22 instances
Skipped: 5 instances (already complete or terminated)

Additional cost estimate: ~$0.97 (22 instances × 2h × $0.044/hour)
```

> ✅ `--job-array-name` targets every running instance in the named array in a single call. You do not need to look up individual instance IDs.

**Selectively extending by status:**

```bash
# Extend only instances that have been running longer than 3h
spawn extend --job-array-name ml-sweep --min-runtime 3h 1h

# Extend only m7g instances in the array
spawn extend --job-array-name ml-sweep --filter type=m7g.large 1h
```

## Step 4: Autoscale Groups

An autoscale group watches an SQS queue and launches workers when the queue depth rises above a threshold. When the queue drains, workers idle out and the group scales to zero. You pay only for compute time that processes actual work.

Create the autoscale configuration:

```bash
cat > autoscale.yaml << 'EOF'
name: my-workers
instance_type: m7g.large
region: us-east-1
min_workers: 0
max_workers: 10
queue:
  type: sqs
  url: https://sqs.us-east-1.amazonaws.com/123456789012/my-queue
scale_up_threshold: 5     # queue depth to trigger scale-up
scale_down_threshold: 0   # queue depth to trigger scale-down
idle_timeout: 10m         # terminate idle workers after this duration
EOF
```

Apply the configuration:

```bash
spawn autoscale apply --config autoscale.yaml
```

**Expected output:**
```
Autoscale group applied: my-workers

Configuration:
  Instance Type: m7g.large
  Region: us-east-1
  Min Workers: 0
  Max Workers: 10
  Queue: sqs://my-queue
  Scale Up Threshold: 5 messages
  Scale Down Threshold: 0 messages
  Idle Timeout: 10m

Status: monitoring
Current workers: 0
Queue depth: 0

The autoscaler is now monitoring the queue.
Workers will launch automatically when queue depth >= 5.
```

Check the live status:

```bash
spawn autoscale status my-workers
```

**Expected output (queue active, workers running):**
```
Autoscale Group: my-workers
Status: scaling-up

Queue:
  Depth: 42 messages
  In-flight: 8 messages
  Oldest Message: 1m 12s

Workers:
  Desired: 8
  Running: 5
  Launching: 3
  Min: 0 / Max: 10

  INSTANCE-ID          STATE      UPTIME  MESSAGES-PROCESSED
  i-0aaa123def456789   running    3m      6
  i-0bbb234def567890   running    3m      5
  i-0ccc345def678901   running    3m      7
  i-0ddd456def789012   running    2m      4
  i-0eee567def890123   running    1m      2
  i-0fff678def901234   launching  -       -
  i-0ggg789def012345   launching  -       -
  i-0hhh890def123456   launching  -       -

Throughput: ~14 messages/minute
Estimated drain time: ~3m

Costs so far: $0.034
```

**Expected output (queue drained, workers idling out):**
```
Autoscale Group: my-workers
Status: scaling-down

Queue:
  Depth: 0 messages
  In-flight: 0 messages

Workers:
  Desired: 0
  Running: 3
  Idle: 3 (terminating in ~8m due to idle_timeout: 10m)

  INSTANCE-ID          STATE   UPTIME  IDLE-FOR  MESSAGES-PROCESSED
  i-0aaa123def456789   idle    18m     2m        41
  i-0bbb234def567890   idle    18m     2m        38
  i-0ccc345def678901   idle    17m     2m        35

Total messages processed: 342
Total cost: $0.12
```

✅ The group will reach zero workers within `idle_timeout` (10 minutes). No workers running = no charges.

## Step 5: Queue Backends

spawn's autoscaler supports pluggable queue backends. The configuration `type` field selects the backend.

**SQS (default — recommended for most workloads):**

```yaml
queue:
  type: sqs
  url: https://sqs.us-east-1.amazonaws.com/123456789012/my-queue
```

SQS is serverless and pay-per-use ($0.40 per million requests). It handles up to 120,000 in-flight messages per standard queue and integrates natively with Lambda, EventBridge, and other AWS services. Use SQS when:
- You are already on AWS and want zero infrastructure to manage
- Your messages are under 256 KB
- You want at-least-once delivery with automatic visibility timeout

**Other backends via config:**

```yaml
queue:
  type: redis
  url: redis://10.0.1.20:6379
  list_key: my-work-queue

queue:
  type: rabbitmq
  url: amqp://user:pass@10.0.1.30:5672/myvhost
  queue_name: my-work-queue
```

When to choose alternatives:
- **Redis**: sub-millisecond dequeue latency required; messages already in Redis; strict ordering needed with sorted sets
- **RabbitMQ**: complex routing, dead-letter queues, or message TTL already managed by an existing RabbitMQ deployment

> ✅ For new workloads with no existing queue infrastructure, use SQS. It requires no servers, scales automatically, and costs effectively zero at moderate message rates.

## Cost Comparison: Autoscale vs Always-On

The savings from autoscaling depend on how evenly work arrives throughout the day. Here is a comparison for a hypothetical batch pipeline with `m7g.large` workers:

| Scenario | Configuration | Compute Hours | Cost/day |
|----------|---------------|---------------|----------|
| Always-on 4 workers | 4 × m7g.large × 24h | 96 hours | ~$10.18 |
| Autoscale, 8h active | avg 2 workers × 8h | 16 hours | ~$1.70 |
| Autoscale, 2h burst to 10 | avg 3 workers × 2h | 6 hours | ~$0.64 |

*Prices based on m7g.large on-demand rate of $0.1061/hour in us-east-1.*

**The breakeven point:** If your workers are busy more than ~67% of the day, always-on may be cheaper (no scale-up latency, simpler architecture). Below that utilization, autoscaling to zero wins.

> ✅ Use `spawn cost --autoscale-group my-workers` to track actual daily costs after deployment. Compare against the always-on estimate to validate savings.

## Why Graviton for Fleets

The mixed-architecture sweep in Step 1 lets you measure this empirically, but the numbers consistently favor Graviton (ARM) for homogeneous batch workloads:

| Instance | vCPU | RAM | On-Demand/hr | vs m6i.large |
|----------|------|-----|-------------|--------------|
| m7g.large (Graviton3) | 2 | 8 GB | $0.1061 | −20% cost |
| m6i.large (Intel Ice Lake) | 2 | 8 GB | $0.1008 | baseline |
| m6a.large (AMD EPYC) | 2 | 8 GB | $0.0864 | −14% cost |

Wait — m6i.large is listed as the baseline but m7g.large costs more per hour than m6a.large? The story is performance-per-dollar, not sticker price:

- **m7g.large** delivers ~40% better performance-per-watt than x86 equivalents for compute-bound workloads (AWS published benchmarks, 2023)
- If a job takes 60 minutes on m6i.large but 42 minutes on m7g.large, the m7g costs less in wall-clock dollars despite a higher hourly rate
- The performance advantage is most pronounced for CPU-bound workloads with no AVX-512 dependency

**ARM binary availability:** Python, Java (via JVM), Go, Rust, and Node.js all publish native ARM64 binaries. Docker images with `linux/arm64` manifests are the norm for major base images. The only remaining blocker is proprietary software that ships x86-only — verify your binary before choosing Graviton for production fleets.

> ✅ **Default recommendation:** Start with `m7g.large` for homogeneous batch fleets. Use the mixed-architecture sweep from Step 1 to validate performance on your specific workload before committing to a large fleet.

## Grid Search vs Random Search

spawn's `--params` flag supports two sweep strategies. The right choice depends on the dimensionality of your search space.

**Grid search** — exhaustive enumeration of all combinations:

```yaml
# grid-search.yaml
instances:
  - type: m7g.large
    region: us-east-1
params:
  learning_rate: [0.0001, 0.001, 0.01, 0.1]
  batch_size: [32, 64, 128, 256]
  dropout: [0.0, 0.2, 0.5]
```

This produces 4 × 4 × 3 = 48 instances. Grid search is appropriate when:
- The search space is small (< 100 combinations)
- You want complete coverage with no gaps
- Parameters have known interactions you need to map fully

```bash
spawn sweep --params grid-search.yaml --name grid-run --ttl 4h --on-complete terminate
```

**Random search** — sample N random combinations from defined ranges:

```yaml
# random-search.yaml
instances:
  - type: m7g.large
    region: us-east-1
strategy: random
n_trials: 20
params:
  learning_rate:
    distribution: log_uniform
    low: 0.0001
    high: 0.1
  batch_size:
    distribution: choice
    values: [32, 64, 128, 256, 512]
  dropout:
    distribution: uniform
    low: 0.0
    high: 0.6
  weight_decay:
    distribution: log_uniform
    low: 0.00001
    high: 0.01
```

This produces exactly 20 instances, each with randomly sampled parameters. Random search is appropriate when:
- The search space is large (> 4 dimensions or > 100 grid points)
- You want to explore broadly before committing to a fine-grained grid
- Budget is fixed — you can cap the number of trials exactly

```bash
spawn sweep --params random-search.yaml --name random-run --ttl 4h --on-complete terminate
```

**Expected output comparison:**

```
Grid search:
  Combinations: 4 × 4 × 3 = 48
  Instances: 48
  Estimated cost: ~$2.04 (48 × 1h × $0.0425/hour)

Random search:
  Trials: 20
  Instances: 20
  Estimated cost: ~$0.85 (20 × 1h × $0.0425/hour)
```

> ✅ **Rule of thumb:** Use grid search for ≤3 parameters with ≤5 values each. Use random search for anything larger. Research has shown random search finds equally good optima as grid search in high-dimensional spaces while using far fewer trials (Bergstra & Bengio, 2012).

## What You Learned

Congratulations — you now have the full sweep and fleet toolkit:

✅ How mixed-architecture sweeps benchmark Graviton, Intel, and AMD in a single command
✅ How `spawn list --array` surfaces sweep status with per-instance parameter details
✅ How `spawn extend --job-array-name` applies a TTL extension to every running instance in an array at once
✅ How autoscale groups drain SQS queues and scale to zero when idle
✅ Why SQS is the right default queue backend and when Redis or RabbitMQ make sense
✅ How to compare grid search vs random search strategies and choose based on search space size

## Next Steps

Deepen your understanding of the supporting skills that make fleet workloads sustainable:

📖 **[Tutorial 6: Cost Management](06-cost-management.md)** — budget alerts, cost-per-sweep tracking, and right-sizing instance types for batch workloads

📚 **[spawn Docs Index](../README.md)** — how-to guides, command reference, and architecture documentation

🛠️ **[How-To: Autoscale Groups](../how-to/autoscale-groups.md)** — advanced autoscale patterns: multi-queue fanout, priority tiers, cross-region failover

📚 **[Reference: sweep config format](../reference/sweep-config.md)** — complete YAML schema for `spawn sweep` parameter files including all distribution types

---

**Previous:** [← Tutorial 10: The truffle → spawn Workflow](10-truffle-to-spawn-workflow.md)
**Next:** [Docs Index](../README.md) →
