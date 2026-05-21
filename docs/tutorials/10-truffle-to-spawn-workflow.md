# Tutorial 10: From Capacity to Instance — The truffle → spawn Workflow

**Duration:** 25 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 8: Introduction to truffle](08-truffle-intro.md), [Tutorial 9: Spot Capacity with truffle](09-spot-capacity.md)

## What You'll Learn

In this tutorial, you'll learn how to combine truffle and spawn into a reliable, cost-aware launch workflow:
- Use truffle to identify the cheapest AZ with active spot capacity
- Feed that region directly into spawn to eliminate the launch retry loop
- Add `--spot` to lock in savings once truffle has validated capacity
- Walk through the full instance lifecycle with `spawn status`
- Script the workflow for CI/CD pipelines and GitHub Actions

By the end, you'll have a repeatable pattern for launching spot instances that land on their first try.

## The Workflow

When you launch a spot instance without checking capacity first, you are guessing. EC2 will accept the request and return an instance ID — but if the target AZ has insufficient spot capacity for your instance type, the instance enters `pending` and then fails with `InsufficientInstanceCapacity`. You retry in a different region. That retry takes 2–5 minutes. If you are scripting a CI job, that is a timeout or a hanging pipeline.

The truffle → spawn workflow eliminates that loop. Here is how the pieces fit together:

1. **truffle** queries spot pricing and capacity data across every AZ for a given instance type. With `--active-only`, it filters to AZs where spot capacity is currently available — not just priced. With `--sort-by-price`, the cheapest active AZ rises to the top.
2. **spawn** targets that exact AZ via `--region`. Because truffle has already verified active capacity, the spot request succeeds immediately.
3. **spored** manages the lifecycle from inside the instance — TTL countdown, idle detection, spot-interruption handling — so your laptop does not need to stay connected.

The result: one command to find the best region, one command to launch, zero retries.

## Step 1: The Manual Version

Start by running truffle interactively to understand the output before scripting it.

```bash
truffle spot c6a.xlarge --sort-by-price --active-only
```

**Expected output:**
```
Spot Availability: c6a.xlarge
Updated: 2026-03-29 14:22:11 UTC

REGION         AZ              PRICE     SAVINGS  STATUS
us-east-2      us-east-2b      $0.0381   74.8%    active
us-east-1      us-east-1d      $0.0394   73.9%    active
us-west-2      us-west-2a      $0.0401   73.4%    active
eu-west-1      eu-west-1c      $0.0418   72.3%    active
ap-southeast-1 ap-southeast-1b $0.0442   70.7%    active

On-demand price: $0.1530/hour
Showing 5 of 12 active regions (--active-only)
```

The top row is the cheapest AZ with confirmed capacity. Note the `REGION` column value — in this example, `us-east-2`.

Now launch an instance targeting that region:

```bash
spawn launch \
  --region us-east-2 \
  --instance-type c6a.xlarge \
  --name my-job \
  --ttl 2h \
  --on-complete terminate
```

**Expected output:**
```
Launching EC2 instance...

Configuration:
  Instance Type: c6a.xlarge
  Region: us-east-2
  AMI: ami-0def456abc123789 (Amazon Linux 2023)
  TTL: 2h
  On Complete: terminate

Progress:
  ✓ Creating SSH key pair (default-ssh-key)
  ✓ Creating security group (spawn-sg-a1b2c3d4)
  ✓ Launching instance
  ✓ Waiting for instance to start...
  ✓ Installing spored agent

Instance launched successfully!

Instance ID: i-0abc123def456789a
Public IP: 3.18.44.210
DNS: my-job.c0zxr0ao.spore.host

Cost: $0.0381/hour (spot)
Estimated 2h cost: ~$0.08

Connect:
  spawn connect my-job

The instance will auto-terminate in 2h or when your job completes.
```

✅ Instance launched without a retry loop — truffle's capacity check paid off.

## Step 2: The Scripted Version

Reading truffle output by eye and copying a region is fine for one-off launches. For scripts, use `-o json` to get machine-readable output and pipe through `jq`:

```bash
REGION=$(truffle spot c6a.xlarge --sort-by-price --active-only -o json \
  | jq -r '.[0].region')

spawn launch \
  --region "$REGION" \
  --instance-type c6a.xlarge \
  --name my-job \
  --ttl 2h \
  --on-complete terminate
```

**What `jq -r '.[0].region'` does:**

truffle's JSON output is an array of objects, each representing one AZ result, sorted by the flags you passed. `.[0]` selects the first (cheapest, active) element. `.region` extracts its `region` field. `-r` strips the surrounding quotes so the value is a bare string, ready to pass directly to `--region`.

```bash
# Verify what the JSON looks like before scripting
truffle spot c6a.xlarge --sort-by-price --active-only -o json | jq '.[0]'
```

**Expected output:**
```json
{
  "region": "us-east-2",
  "az": "us-east-2b",
  "instance_type": "c6a.xlarge",
  "spot_price": 0.0381,
  "on_demand_price": 0.1530,
  "savings_pct": 74.8,
  "status": "active"
}
```

The `REGION` variable captures `us-east-2` — no parsing, no `awk`, no fragile column-cutting.

## Step 3: Adding Spot Pricing

Append `--spot` to the spawn command to request the instance as a spot instance:

```bash
REGION=$(truffle spot c6a.xlarge --sort-by-price --active-only -o json \
  | jq -r '.[0].region')

spawn launch \
  --region "$REGION" \
  --instance-type c6a.xlarge \
  --name my-job \
  --ttl 2h \
  --on-complete terminate \
  --spot
```

**Why this combination is safer than `--spot` alone:**

Without truffle, `--spot` picks whatever region your AWS CLI is configured for. If that region has low spot capacity at the moment, your instance enters the queue and may wait — or fail with `InsufficientInstanceCapacity`. With truffle's `--active-only` filter, you have already confirmed that the target AZ has available capacity right now. The spot request almost always succeeds immediately.

> **Note:** "Active capacity" from truffle reflects data at the time of the query. Spot capacity fluctuates. In practice the window between the truffle check and the spawn launch is under a second in a script, making the correlation very reliable — but not guaranteed. The multi-region fallback in a later section handles the rare miss.

**Expected output difference:**
```
Cost: $0.0381/hour (spot)   ← was $0.1530/hour on-demand
Estimated 2h cost: ~$0.08   ← was ~$0.31 on-demand
```

✅ ~75% cost reduction by combining truffle's region selection with `--spot`.

## Step 4: Full Lifecycle Walkthrough

Once the instance is running, use `spawn status` to follow it through each lifecycle stage.

### Stage 1: Pending

Immediately after launch, the instance is still initializing:

```bash
spawn status my-job
```

**Expected output:**
```
Instance: i-0abc123def456789a
Name: my-job
Region: us-east-2
State: pending

Instance Type: c6a.xlarge
Spot: yes

Lifecycle:
  Launch Time: 2026-03-29 14:23:05 UTC
  Uptime: 0m 18s
  TTL: 2h (1h 59m 42s remaining)
  Auto-terminate at: 2026-03-29 16:23:05 UTC

Note: Instance is initializing. spored agent not yet reporting.
```

### Stage 2: Running

After 30–60 seconds, the instance is up and the spored agent is reporting:

```bash
spawn status my-job
```

**Expected output:**
```
Instance: i-0abc123def456789a
Name: my-job
Region: us-east-2
State: running

Instance Type: c6a.xlarge
Public IP: 3.18.44.210
Private IP: 172.31.12.55
DNS: my-job.c0zxr0ao.spore.host
Spot: yes

Lifecycle:
  Launch Time: 2026-03-29 14:23:05 UTC
  Uptime: 2m 11s
  TTL: 2h (1h 57m 49s remaining)
  Auto-terminate at: 2026-03-29 16:23:05 UTC

Cost:
  Hourly: $0.0381 (spot)
  Current: $0.0014 (2.2 minutes)
  Projected 2h: $0.0762

Agent: spored v0.24.2 — healthy
```

✅ The instance is running. You can now connect via `spawn connect my-job` or SSH directly.

### Stage 3: Job Completes

When the job finishes and `spored complete --status success` is called (or the user_data script exits), spored begins the shutdown sequence:

```bash
spawn status my-job
```

**Expected output:**
```
Instance: i-0abc123def456789a
Name: my-job
Region: us-east-2
State: shutting-down

Instance Type: c6a.xlarge
Spot: yes

Lifecycle:
  Launch Time: 2026-03-29 14:23:05 UTC
  Uptime: 43m 18s
  Completion: job-complete (on-complete: terminate)

Cost:
  Total: $0.0274 (43.3 minutes × $0.0381/hour)

Agent: spored — shutdown in progress
```

### Stage 4: Terminated

```bash
spawn status my-job
```

**Expected output:**
```
Instance: i-0abc123def456789a
Name: my-job
Region: us-east-2
State: terminated

Instance Type: c6a.xlarge
Spot: yes

Lifecycle:
  Launch Time:      2026-03-29 14:23:05 UTC
  Termination Time: 2026-03-29 15:06:44 UTC
  Total Runtime:    43m 39s

Cost:
  Total: $0.0277 (43.6 minutes × $0.0381/hour)

Resources: cleaned up (security group, SSH key tag removed)
```

✅ Instance terminated cleanly. No orphaned resources. Total cost: $0.03.

## Output Format Note

truffle supports three output formats. Choose the right one for the context:

| Flag | Format | Best For |
|------|--------|----------|
| *(none)* | Human-readable table | Reading in the terminal |
| `-o json` | JSON array | Scripting, piping through `jq` |
| `-o yaml` | YAML | Configuration files, not pipelines |

**For scripting, always use `-o json`:**
```bash
# Correct: machine-parseable
REGION=$(truffle spot c6a.xlarge --sort-by-price --active-only -o json \
  | jq -r '.[0].region')

# Do not use -o yaml for pipeline extraction — YAML is not reliably parseable
# with simple shell tools
```

> ❌ **There is no `truffle best-region` command.** The correct incantation is:
> ```bash
> truffle spot <type> --sort-by-price --active-only -o json | jq -r '.[0].region'
> ```
> There is no shorthand alias. Write it out every time or put it in a shell function.

## Going Further: Multi-Region Fallback

Even with truffle's capacity check, a spot request can occasionally fail — capacity data has a short staleness window, and another customer may have launched into the same AZ in the intervening millisecond. For production scripts, iterate over truffle's sorted results and break on the first successful launch:

```bash
REGIONS=$(truffle spot c6a.xlarge --sort-by-price --active-only -o json \
  | jq -r '.[].region')

for REGION in $REGIONS; do
  echo "Attempting launch in $REGION..."
  if spawn launch \
       --region "$REGION" \
       --instance-type c6a.xlarge \
       --name my-job \
       --ttl 2h \
       --on-complete terminate \
       --spot; then
    echo "Launch succeeded in $REGION"
    break
  fi
  echo "Launch failed in $REGION, trying next..."
done
```

**What this does:**

- `jq -r '.[].region'` extracts all regions from the sorted array, one per line. The shell `for` loop iterates over them in cheapest-first order.
- `spawn launch` exits with code 0 on success, non-zero on failure.
- The `if` tests the exit code: success breaks the loop, failure continues to the next region.
- Because truffle has already sorted by price and filtered to active-only, the first attempt succeeds the vast majority of the time. The loop is a safety net, not the primary path.

**Expected output (first region succeeds):**
```
Attempting launch in us-east-2...
Instance launched successfully!
Launch succeeded in us-east-2
```

**Expected output (first region fails, second succeeds):**
```
Attempting launch in us-east-2...
Error: InsufficientInstanceCapacity in us-east-2b
Launch failed in us-east-2, trying next...
Attempting launch in us-east-1...
Instance launched successfully!
Launch succeeded in us-east-1
```

## Going Further: CI/CD Integration

The Step 2 script drops directly into any CI/CD environment that has `truffle`, `spawn`, and `jq` installed. Here is the same logic as a GitHub Actions step:

```yaml
jobs:
  launch-and-run:
    runs-on: ubuntu-latest
    steps:
      - name: Install tools
        run: |
          # Install truffle
          curl -LO https://github.com/spore-host/spore-host/releases/latest/download/truffle-linux-amd64
          chmod +x truffle-linux-amd64
          sudo mv truffle-linux-amd64 /usr/local/bin/truffle

          # Install spawn
          curl -LO https://github.com/spore-host/spore-host/releases/latest/download/spawn-linux-amd64
          chmod +x spawn-linux-amd64
          sudo mv spawn-linux-amd64 /usr/local/bin/spawn

      - name: Configure AWS credentials
        uses: aws-actions/configure-aws-credentials@v4
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Launch instance in cheapest spot region
        run: |
          REGION=$(truffle spot c6a.xlarge --sort-by-price --active-only -o json \
            | jq -r '.[0].region')

          spawn launch \
            --region "$REGION" \
            --instance-type c6a.xlarge \
            --name ci-job-${{ github.run_id }} \
            --ttl 2h \
            --on-complete terminate \
            --spot

      - name: Wait for job completion
        run: |
          spawn wait ci-job-${{ github.run_id }} --timeout 2h
```

**Notes on CI usage:**

- The `--name` flag uses `github.run_id` to make each CI run's instance uniquely addressable.
- `--on-complete terminate` ensures the instance shuts itself down when the job finishes — no cleanup step needed.
- `spawn wait` blocks the CI step until the instance terminates, propagating exit codes.
- This pattern works identically in GitLab CI, CircleCI, Buildkite, Jenkins, and any other shell-capable runner.

## What You Learned

✅ How truffle's `--active-only` filter removes AZs with no current capacity, not just no current price
✅ How to extract the cheapest active region from truffle's JSON output with `jq -r '.[0].region'`
✅ Why combining truffle's region selection with `--spot` reduces failed spot requests to near zero
✅ How to follow a full instance lifecycle from `pending` → `running` → `shutting-down` → `terminated`
✅ How to build a multi-region fallback loop for production resilience
✅ How to integrate the truffle → spawn pattern into GitHub Actions and other CI systems

## Next Steps

Continue to the advanced tutorial to learn about running entire fleets:

📖 **[Tutorial 11: Advanced spawn — Sweeps, Arrays, and Autoscaling](11-advanced-spawn.md)** — mixed-architecture parameter sweeps, live array management, and elastic autoscale groups backed by SQS queues

🛠️ **[How-To: Spot Instances](../how-to/spot-instances.md)** — recipes for spot with checkpointing, fallback strategies, and interruption handling

📚 **[Command Reference: truffle spot](../reference/commands/truffle-spot.md)** — complete flag documentation for truffle's spot subcommand

---

**Previous:** [← Tutorial 9: Spot Capacity with truffle](09-spot-capacity.md)
**Next:** [Tutorial 11: Advanced spawn — Sweeps, Arrays, and Autoscaling](11-advanced-spawn.md) →
