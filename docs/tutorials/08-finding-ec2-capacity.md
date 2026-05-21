# Tutorial 8: Finding EC2 Capacity Before You Launch

**Duration:** 20 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

## What You'll Learn

In this tutorial, you'll learn how to investigate EC2 capacity before spending money on a launch:
- Interrogate your vCPU service quotas with `truffle quota`
- Check real-time spot pricing for a specific instance type
- Compare multiple architectures (Intel, AMD, Graviton) side by side
- Filter results to only show AZs with active capacity
- Produce machine-readable output for scripting and pipelines

By the end, you'll have a repeatable pre-launch checklist that eliminates capacity surprises and helps you find the cheapest available region for any instance family.

## The Problem: Capacity Errors

EC2 has two independent limits that can block your launch.

**Quota** is the ceiling AWS lets you run — it lives in your account and is measured in vCPUs. **Capacity** is whether AWS actually has physical hardware available in a specific Availability Zone at this moment. Hitting either one produces a launch failure, but the error messages look different:

```
# Quota exhausted — you're over your account limit
An error occurred (VcpuLimitExceeded) when calling the RunInstances operation:
You have requested more vCPU capacity than your current vCPU limit of 32 allows
for the instance bucket that the specified instance type belongs to.

# Capacity exhausted — AWS doesn't have the hardware right now
An error occurred (InsufficientInstanceCapacity) when calling the RunInstances operation:
We currently do not have sufficient c6i.8xlarge capacity in the Availability Zone
you requested (us-east-1b).
```

The distinction matters because the fixes are completely different:
- Quota errors require a service-limit increase request (takes hours or days).
- Capacity errors require switching regions, AZs, or instance types (takes seconds).

**Without capacity checking:**

❌ Launch command runs, fails after 45 seconds, you have no instance and no data.
❌ In a parameter sweep across 50 instances, partial failures corrupt your results.
❌ You find out at launch time, not planning time.

**With capacity checking:**

✅ Run `truffle spot` before you launch — pick a region and AZ that has confirmed availability.
✅ Spot price acts as a real-time signal: low price = ample capacity; high price = demand pressure.
✅ Compare architectures in one command and choose the most available option.

## Step 1: Check Your Service Quotas

Before looking at capacity, confirm you have quota headroom. The `truffle quota` command queries the Service Quotas API and shows your current limits alongside your running usage.

```bash
truffle quota
```

**Expected output:**
```
Service Quotas — EC2 vCPUs
Account: 435415984226  Region: us-east-1

Instance Family      Quota    Used    Available  Status
─────────────────────────────────────────────────────────
Standard (A, C, D,
  H, I, M, R, T, Z)   32       14       18       OK
High Memory (U)         0        0        0       NOT REQUESTED
Spot                  256       12      244       OK
GPU (G, VT)            8        4        4       OK
FPGA (F)               0        0        0       NOT REQUESTED
Inference (Inf)        0        0        0       NOT REQUESTED

Running instances:     6
Total vCPUs in use:   14

Notes:
  Standard quota covers most general-purpose workloads.
  Spot quota is separate from on-demand quota.
  Use 'truffle quota --request <limit>' to request an increase.
```

Look at the **Available** column for your target instance family. A `c6i.xlarge` uses 4 vCPUs. With 18 standard vCPUs available here, you could launch up to 4 more `c6i.xlarge` instances before hitting the quota wall.

> **Important:** Available quota does not guarantee physical capacity. A quota of 18 vCPUs means AWS *will accept* your launch request; it does not mean the hardware exists in a specific AZ. That's what Step 2 checks.

### Request a Quota Increase

If your available quota is low, request an increase before you need it:

```bash
truffle quota --request 128 --family standard
```

**Expected output:**
```
Quota increase requested
  Family:    Standard (A, C, D, H, I, M, R, T, Z)
  Current:   32
  Requested: 128
  Case ID:   7112345678

AWS typically approves standard increases within 2–4 hours.
Track status: https://console.aws.amazon.com/support/cases#7112345678
```

## Step 2: Check Spot Pricing for an Instance Type

Once you know you have quota headroom, check where the capacity actually lives. Spot pricing is the best real-time signal: AWS prices spot capacity by supply and demand, so low spot prices indicate abundant supply.

```bash
truffle spot c6i.xlarge
```

**Expected output:**
```
Spot Pricing — c6i.xlarge
On-demand price: $0.1700/hr

Region          AZ              Spot Price   Savings   Status
──────────────────────────────────────────────────────────────
us-east-1       us-east-1a      $0.0489/hr    71%      active
us-east-1       us-east-1b      $0.0512/hr    70%      active
us-east-1       us-east-1c      $0.0561/hr    67%      active
us-east-1       us-east-1d      $0.0489/hr    71%      active
us-east-1       us-east-1f      $0.0534/hr    69%      active
us-west-2       us-west-2a      $0.0481/hr    72%      active
us-west-2       us-west-2b      $0.0499/hr    71%      active
us-west-2       us-west-2c      $0.1698/hr     0%      constrained
us-west-2       us-west-2d      $0.0491/hr    71%      active
eu-west-1       eu-west-1a      $0.0521/hr    69%      active
eu-west-1       eu-west-1b      $0.0548/hr    68%      active
eu-west-1       eu-west-1c      $0.0521/hr    69%      active
ap-southeast-1  ap-southeast-1a $0.0501/hr    71%      active
ap-southeast-1  ap-southeast-1b $0.0501/hr    71%      active
ap-southeast-1  ap-southeast-1c $0.0501/hr    71%      active

Cheapest AZ:    us-west-2a  ($0.0481/hr, 72% savings)
Best region:    us-west-2   (avg $0.0556/hr across 4 AZs)

Data as of: 2026-03-29 14:22:07 UTC
```

**Column definitions:**

| Column | Meaning |
|--------|---------|
| **Region** | AWS region identifier |
| **AZ** | Specific Availability Zone within that region |
| **Spot Price** | Current spot price per hour — changes with market demand |
| **Savings** | Discount vs. on-demand price for the same instance type |
| **Status** | `active` = capacity available; `constrained` = spot price near on-demand, limited supply; `no-capacity` = AWS has no spot capacity here right now |

**Reading the spot price signal:**

- `us-west-2c` shows `$0.1698/hr` with 0% savings — the price has risen to nearly the on-demand rate. That means demand is extremely high in this AZ and you have a significant risk of interruption or launch failure. Avoid it.
- `us-west-2a` at `$0.0481/hr` has abundant capacity. This is where you should target your launch.

## Step 3: Compare Architectures Side by Side

Before committing to a specific instance family, compare equivalent sizes across Intel, AMD, and Graviton. Each has different performance characteristics and availability patterns.

```bash
truffle spot c6i.xlarge c6a.xlarge c7g.xlarge --sort-by-price --active-only
```

**Expected output:**
```
Spot Pricing Comparison — c6i.xlarge / c6a.xlarge / c7g.xlarge
Showing active AZs only  |  Sorted by spot price

Region          AZ              Instance      Spot Price   On-Demand   Savings   Status
───────────────────────────────────────────────────────────────────────────────────────
us-west-2       us-west-2a      c7g.xlarge    $0.0412/hr   $0.1452/hr   72%      active
us-east-1       us-east-1d      c7g.xlarge    $0.0419/hr   $0.1452/hr   71%      active
us-west-2       us-west-2d      c6a.xlarge    $0.0441/hr   $0.1530/hr   71%      active
us-east-1       us-east-1a      c6a.xlarge    $0.0448/hr   $0.1530/hr   71%      active
us-west-2       us-west-2a      c6i.xlarge    $0.0481/hr   $0.1700/hr   72%      active
us-east-1       us-east-1a      c6i.xlarge    $0.0489/hr   $0.1700/hr   71%      active
us-west-2       us-west-2b      c6a.xlarge    $0.0492/hr   $0.1530/hr   68%      active
us-east-1       us-east-1b      c6i.xlarge    $0.0512/hr   $0.1700/hr   70%      active
eu-west-1       eu-west-1a      c7g.xlarge    $0.0514/hr   $0.1452/hr   65%      active
eu-west-1       eu-west-1a      c6a.xlarge    $0.0521/hr   $0.1530/hr   66%      active
eu-west-1       eu-west-1a      c6i.xlarge    $0.0521/hr   $0.1700/hr   69%      active

(12 AZs with no-capacity omitted — use --all to include)

Cheapest overall:  c7g.xlarge in us-west-2a  ($0.0412/hr)
Best availability: c6a.xlarge (active in 9/11 surveyed AZs)
```

**What makes each family distinct:**

`c6i.xlarge` (Intel Ice Lake) is the broadest-compatible choice. If your workload uses x86 binaries or libraries without ARM builds, this is your baseline. Intel's AVX-512 support benefits numerically-intensive workloads.

`c6a.xlarge` (AMD EPYC) delivers nearly identical x86 compatibility at a slightly lower on-demand price. The spot price advantage over `c6i` is modest but consistent. Good choice if you want x86 compatibility with a small cost edge.

`c7g.xlarge` (AWS Graviton 3, ARM64) consistently shows the lowest spot prices in this comparison. Graviton has strong capacity across regions because AWS manufactures the silicon itself. If your software is compiled for `linux/arm64` — or you're using container images with multi-arch manifests — Graviton is frequently the cheapest path. Note the lower on-demand price ($0.1452 vs $0.1700 for c6i).

**The `--active-only` flag** removes AZs showing `no-capacity` or `constrained` status from the output. Without it you would see every AZ including ones with no available spot instances — useful for a complete picture, noisy for launch planning.

## Step 4: Machine-Readable Output

For scripting, automation, and integration with other tools, use `-o json`. The JSON output contains the full data set including fields that are truncated in the table view.

```bash
truffle spot c6i.xlarge c6a.xlarge c7g.xlarge \
  --sort-by-price \
  --active-only \
  -o json | jq '
    [.[] | select(.status == "active")]
    | sort_by(.spot_price)
    | .[0]
    | {instance: .instance_type, region: .region, az: .az,
       spot_price: .spot_price, savings_pct: .savings_pct}
  '
```

**Expected output:**
```json
{
  "instance": "c7g.xlarge",
  "region": "us-west-2",
  "az": "us-west-2a",
  "spot_price": 0.0412,
  "savings_pct": 72
}
```

Use this in a launch script to automatically select the cheapest available AZ:

```bash
#!/bin/bash
set -euo pipefail

# Find cheapest active AZ for any of three instance families
BEST=$(truffle spot c6i.xlarge c6a.xlarge c7g.xlarge \
  --active-only -o json | jq -r '
    [.[] | select(.status == "active")]
    | sort_by(.spot_price)
    | .[0]
    | "\(.instance_type) \(.region) \(.az)"
  ')

INSTANCE_TYPE=$(echo "$BEST" | awk '{print $1}')
REGION=$(echo "$BEST" | awk '{print $2}')

echo "Selected: $INSTANCE_TYPE in $REGION"
spawn launch \
  --instance-type "$INSTANCE_TYPE" \
  --region "$REGION" \
  --spot \
  --ttl 4h
```

**YAML output** (`-o yaml`) is useful for generating spawn configuration files:

```bash
truffle spot c6i.xlarge --active-only -o yaml > capacity-snapshot.yaml
```

This produces a YAML document you can commit alongside experiment configs to record the capacity state at the time of a launch decision.

## Understanding the Output

Every row in the spot pricing table represents one AZ in one region for one instance type. Here is the full field reference:

| Field | Description |
|-------|-------------|
| **region** | AWS region code, e.g. `us-east-1` |
| **az** | Full AZ identifier, e.g. `us-east-1a` — use this directly in `spawn launch --az` |
| **instance** | Instance type requested |
| **spot_price** | Current hourly spot price in USD — refreshes every few minutes as AWS adjusts |
| **on_demand** | Published on-demand price for the same type and region |
| **savings_pct** | `(on_demand - spot_price) / on_demand × 100` — higher is better |
| **status** | `active`: spot available and price is healthy; `constrained`: price has risen near on-demand, expect interruptions; `no-capacity`: AWS has no spot supply here right now |

**What spot price tells you about demand pressure:**

Spot price is a real-time auction. When many users want instances in the same AZ, the price rises. A spot price within a few percent of on-demand is a warning signal — not only is it not saving you much money, but the probability of an interruption (AWS reclaiming your instance with a 2-minute notice) is significantly higher. If `savings_pct` drops below 40%, consider a different AZ or instance family.

Conversely, 70%+ savings means AWS has abundant unallocated hardware. At those discounts, interrupt rates are typically very low — AWS has no incentive to reclaim instances it otherwise cannot sell.

---

> **Quota vs. Capacity**
>
> AWS quota is the maximum you are allowed to launch, set per account and measured in vCPUs. Capacity is whether AWS physically has the hardware in that specific Availability Zone at this moment in time.
>
> They are completely independent constraints. You can have quota headroom and still get `InsufficientInstanceCapacity` if the AZ is sold out. You can have capacity available and still fail with `VcpuLimitExceeded` if you have exhausted your vCPU limit.
>
> Check both before planning a large launch. Use `truffle quota` for quota and `truffle spot` for capacity.

---

## What You Learned

- `truffle quota` shows your per-family vCPU limits and current usage so you can plan headroom before launch.
- Spot price is a live supply-and-demand signal: high savings percentage means abundant capacity; price near on-demand means scarcity and higher interruption risk.
- `truffle spot <type> [type ...]` compares pricing and availability across every region and AZ in your enabled set, including multi-architecture comparisons in a single command.
- `--active-only` removes no-capacity and constrained AZs so your output only shows viable launch targets.
- `-o json` combined with `jq` enables scripts that automatically select the optimal region and instance type at launch time.

## Next Steps

📖 **[Tutorial 9: Instance Lifecycle](09-instance-lifecycle.md)** — Give launched instances names, DNS entries, and lifecycle controls so they terminate themselves when work is done.

🛠️ **[How-To: Launch on Spot](../how-to/launch-instances.md)** — Practical recipes for fault-tolerant spot launches with fallback strategies.

📚 **[Command Reference: truffle spot](../reference/commands/truffle-spot.md)** — Complete flag documentation including `--region`, `--az`, `--history`, and `--max-price`.

---

**Previous:** [← Tutorial 7: Monitoring & Alerts](07-monitoring-alerts.md)
**Next:** [Tutorial 9: Instance Lifecycle](09-instance-lifecycle.md) →
