# Tutorial 9: Instance Lifecycle — Instances That Clean Up After Themselves

**Duration:** 20 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md)

## What You'll Learn

In this tutorial, you'll give every instance a meaningful identity and a predictable end:
- Assign human-readable names and access instances by name instead of IDs
- Understand automatic DNS registration (`<name>.<account>.spore.host`)
- Set absolute wall-clock TTLs so instances terminate by a deadline
- Use idle detection to terminate instances that have stopped working
- Send a completion signal from inside a running job to trigger immediate cleanup
- Check lifecycle status and extend TTL on a live instance
- Decide when to use `--no-timeout` for long-running servers

By the end, you'll never again pay for an instance that finished its work hours ago and was silently sitting idle.

## The Problem: Forgotten Instances

EC2 instances do not terminate themselves. If you launch an instance, close your laptop, and forget about it, it runs indefinitely at full cost. The longer your average EC2 experience, the more likely you have at least one forgotten instance in your account right now.

**The math for a single forgotten `t4g.medium`:**

| Scenario | Duration | EC2 cost | EBS (20 GB) | Total |
|----------|----------|----------|-------------|-------|
| With 4h TTL | 4 hours | 4h × $0.0464 = **$0.19** | ~$0.00 | **~$0.19** |
| Forgotten 3 days | 72 hours | 72h × $0.0464 = **$3.34** | 72h × $0.027 = **$0.12** | **~$3.46** |
| Forgotten 30 days | 720 hours | 720h × $0.0464 = **$33.41** | 30d × $0.067 = **$0.67** | **~$34.08** |

A single forgotten `t4g.medium` costs $34/month. At scale — a team of five, each forgetting one instance per month — that is $170/month in waste, before counting larger instance types.

**Without lifecycle flags:**

❌ Instance runs until manually terminated or your credit card is declined.
❌ No visibility into cost until the monthly bill arrives.
❌ No way to distinguish "still working" from "done 6 hours ago and idle."

**With lifecycle flags:**

✅ Instance terminates itself at the deadline — no action required.
✅ Cost is predictable before you even run the command.
✅ idle detection catches jobs that finished early so you are not billed for idle time.

## Step 1: Launch With a Name

The most important usability improvement over raw EC2 is the `--name` flag. It does three things at once: sets the EC2 `Name` tag (visible in the console), registers a DNS entry, and gives you a stable identifier that outlasts the instance's IP address.

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ami al2023
```

**Expected output:**
```
Launching EC2 instance...

Configuration:
  Instance Type: t4g.medium
  Region: us-east-1
  AMI: ami-0c1234abcdef5678  (Amazon Linux 2023, arm64)
  Name: my-analysis

Progress:
  ✓ Creating security group (spawn-sg-...)
  ✓ Launching instance
  ✓ Waiting for instance to start...
  ✓ Registering DNS: my-analysis.d2a4b7c9.spore.host
  ✓ Installing spored agent

Instance launched successfully!

Instance ID: i-0a1b2c3d4e5f67890
Public IP:   18.212.45.103
DNS:         my-analysis.d2a4b7c9.spore.host

Cost: $0.0464/hour

⚠  No lifecycle flags set. Instance will idle-timeout after 1h of <10% CPU.
   Use --ttl, --idle-timeout, or --on-complete to control termination.

Connect:
  spawn connect my-analysis
```

The DNS hostname `my-analysis.d2a4b7c9.spore.host` is registered automatically by the spored agent running on the instance. The subdomain segment (`d2a4b7c9`) is a base36-encoded form of your account ID, so names are scoped per account and never collide across teams.

The name resolves as long as the instance is running. When the instance terminates — for any reason — spored deregisters the DNS record automatically.

## Step 2: Connect by Name

Once you have a name, you never need to look up the instance ID or IP address again.

```bash
# Connect by name
spawn connect my-analysis
```

Compare this to the old approach that requires hunting down connection details first:

```bash
# Old approach — requires looking up the IP first
aws ec2 describe-instances \
  --filters "Name=tag:Name,Values=my-analysis" \
  --query "Reservations[0].Instances[0].PublicIpAddress" \
  --output text

# Then connect
ssh -i ~/.ssh/my-key.pem ec2-user@18.212.45.103
```

`spawn connect my-analysis` resolves the name, looks up the current key, and opens the SSH session in one step. The name also works with `spawn status`, `spawn extend`, and `spawn terminate` — you never need the instance ID for day-to-day operations.

**Expected output:**
```
Connecting to my-analysis (i-0a1b2c3d4e5f67890)...
  ✓ Instance is running
  ✓ Resolved: my-analysis.d2a4b7c9.spore.host → 18.212.45.103
  ✓ SSH is ready

   __                 _       ___     ___ ____  ____  ____
  (_   _ _  _  _    |_ _ _  |_ (_   |_  (_  ) |_  ) |_  )
  __) |_)(_|| |L|   |_ (_|  |__(_)  |__ (_  ) (_  ) (_  )

  Amazon Linux 2023

[ec2-user@ip-10-0-1-47 ~]$
```

## Step 3: The Default Safety Net

Notice the warning printed at launch:

```
⚠  No lifecycle flags set. Instance will idle-timeout after 1h of <10% CPU.
   Use --ttl, --idle-timeout, or --on-complete to control termination.
```

When you launch without any lifecycle flags, spawn does not leave the instance running indefinitely. As a safety net, spored applies a default idle timeout of **1 hour** using a CPU threshold of **10%**. If average CPU utilization stays below 10% for 60 consecutive minutes, spored terminates the instance automatically.

This catches the most common forgotten-instance scenario: you finish work, disconnect, and walk away. CPU drops to zero, spored waits one hour to confirm the instance is not doing background work, and then terminates.

**What "idle" means:** spored samples CPU utilization every 60 seconds using the instance's local metrics (not CloudWatch). If the rolling 5-minute average stays below the configured threshold continuously for the timeout window, the instance is considered idle.

The default safety net is a last resort, not a replacement for explicit lifecycle controls. The next step covers how to set precise lifecycle rules for your specific workload.

## Step 4: Lifecycle Controls

### `--ttl` — Absolute Wall-Clock Limit

`--ttl` sets a hard deadline. The instance terminates at `launch_time + ttl` regardless of what it is doing at that moment.

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ami al2023 \
  --ttl 4h
```

Accepted formats: `30m`, `4h`, `2d`, `1h30m`. The TTL is enforced by spored running on the instance, so it fires even if your laptop is offline or your local network is down.

Use `--ttl` when you know the maximum time a job should ever need. It is your circuit breaker — even if something goes wrong with idle detection or completion signals, the instance will not run past the wall-clock limit.

### `--idle-timeout` — Terminate After Inactivity

`--idle-timeout` terminates the instance after N consecutive minutes of CPU utilization below a configurable threshold. Unlike `--ttl`, the timer resets whenever the instance becomes active again.

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ami al2023 \
  --ttl 8h \
  --idle-timeout 30m
```

Adjust the CPU threshold with `--idle-cpu`:

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ami al2023 \
  --ttl 8h \
  --idle-timeout 30m \
  --idle-cpu 5
```

`--idle-cpu 5` sets the threshold to 5% (the default is 10%). Lower values are appropriate for workloads that run background maintenance tasks between compute bursts — you do not want those tasks to reset the idle timer when the main work is done.

**Combine `--ttl` and `--idle-timeout`:** They work together. `--ttl` is the hard maximum; `--idle-timeout` terminates early if the work finishes before the deadline. This is the recommended pattern for batch jobs where runtime is uncertain.

### `--on-complete` — Terminate When the Job Finishes

`--on-complete terminate` tells spored to terminate the instance as soon as the job signals completion. This is the most cost-efficient option for single-purpose batch instances.

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ami al2023 \
  --ttl 4h \
  --on-complete terminate
```

`--on-complete stop` preserves the EBS volume instead of deleting it. The instance is stopped (not terminated), which halts compute charges but continues EBS charges. Use this when you need to resume the exact same filesystem state later.

```bash
spawn launch \
  --name checkpoint-run \
  --instance-type c6i.2xlarge \
  --ami al2023 \
  --ttl 12h \
  --on-complete stop
```

When you are ready to continue:

```bash
spawn resume checkpoint-run
```

## Step 5: Completion Signal From Inside the Job

For script-driven workloads, the cleanest lifecycle control is a completion signal emitted by the job itself. spored watches for a sentinel file at a well-known path:

```
/tmp/SPAWN_COMPLETE
```

When this file exists, spored treats the job as finished and applies the `--on-complete` action immediately. You do not need to configure a timeout long enough to cover worst-case runtimes — the instance terminates as soon as the work is done.

**Example batch script using the completion signal:**

```bash
#!/bin/bash
# process.sh — runs on the EC2 instance via --user-data

set -euo pipefail

# Trap errors so SPAWN_COMPLETE is only created on success
cleanup() {
    local exit_code=$?
    if [[ $exit_code -ne 0 ]]; then
        echo "Job failed with exit code $exit_code" >&2
        # Do NOT create SPAWN_COMPLETE on failure
        # Instance will run until TTL or idle-timeout
    fi
}
trap cleanup EXIT

echo "Starting data processing..."

# Download input data
aws s3 cp s3://my-bucket/input/dataset.tar.gz /tmp/dataset.tar.gz
tar -xzf /tmp/dataset.tar.gz -C /tmp/

# Run compute job
python3 /opt/analysis/run.py \
    --input /tmp/dataset/ \
    --output /tmp/results/

# Upload results
aws s3 sync /tmp/results/ s3://my-bucket/output/$(date +%Y%m%d)/

echo "Processing complete. Uploading complete."

# Signal completion — spored will apply --on-complete action
touch /tmp/SPAWN_COMPLETE
```

Launch this script with:

```bash
spawn launch \
  --name nightly-analysis \
  --instance-type c6a.xlarge \
  --ami al2023 \
  --ttl 6h \
  --on-complete terminate \
  --user-data @process.sh
```

The `--ttl 6h` is a safety net. If the script errors before reaching `touch /tmp/SPAWN_COMPLETE`, the instance will terminate after 6 hours at most rather than running forever. If the script succeeds and creates the file at the 45-minute mark, the instance terminates at 45 minutes — not 6 hours.

## Step 6: Checking Status and Extending Live

`spawn status` gives you a full lifecycle snapshot without needing to connect to the instance.

```bash
spawn status my-analysis
```

**Expected output:**
```
Instance: my-analysis
ID:       i-0a1b2c3d4e5f67890
Region:   us-east-1
State:    running

Instance Type: t4g.medium
Public IP:     18.212.45.103
Private IP:    10.0.1.47
DNS:           my-analysis.d2a4b7c9.spore.host

Lifecycle:
  Launch Time:     2026-03-29 09:00:00 UTC
  Uptime:          2h 14m 07s
  TTL:             4h (1h 45m 53s remaining)
  Auto-terminate:  2026-03-29 13:00:00 UTC
  Idle Timeout:    30m (timer reset 8m ago — CPU: 34%)
  On-Complete:     terminate

Cost:
  Hourly:   $0.0464
  Accrued:  $0.1040  (2.24 hours)
  Remaining at current TTL:  ~$0.082

spored Agent:
  Version:  0.24.2
  Status:   healthy
  Last heartbeat: 12s ago
```

If you realize the job needs more time than the original TTL allows, extend it without interrupting the running instance:

```bash
spawn extend my-analysis 2h
```

**Expected output:**
```
Extending TTL for my-analysis (i-0a1b2c3d4e5f67890)...
  ✓ Previous TTL:  1h 45m remaining
  ✓ Extension:     2h
  ✓ New TTL:       3h 45m remaining
  ✓ New deadline:  2026-03-29 15:45:00 UTC

spored agent updated. No restart required.
```

The extension is applied live via the spored agent's API. The instance does not stop or restart.

## Step 7: Slack Notifications for Lifecycle Events

If your workspace has spore-bot installed (see the [spore-bot guide](../../../docs/user-guide/spore-bot.md)), you can receive a Slack DM or channel message at every lifecycle transition — without needing to poll `spawn status` or stay connected to the instance.

Add `--slack-workspace` to any launch command:

```bash
spawn launch \
  --name nightly-analysis \
  --instance-type c6a.xlarge \
  --ttl 6h \
  --on-complete terminate \
  --user-data @process.sh \
  --slack-workspace T00000000
```

When the job completes and `/tmp/SPAWN_COMPLETE` is created, you'll receive:

```
✅ *nightly-analysis* has completed
  AWS Instance ID: `i-0a1b2c3d4e5f67890`  Region: us-east-1
```

spored also sends warnings before lifecycle actions fire:

```
⏱️ *nightly-analysis* terminates in 10 minutes
💤 *nightly-analysis* will stop in 10 minutes — no activity detected
⚠️ *nightly-analysis* received a Spot interruption notice — action: terminate
```

### Who receives notifications

Three delivery patterns work independently or together:

| Pattern | Setup | Who receives it |
|---------|-------|----------------|
| **A — Channel** | Connect workspace via "Add to Slack", pick a channel | Whole workspace → one channel |
| **B — Registered user** | `spawn bot register` for each person | Each registered user → DM |
| **C — Self-service** | User types `/spore notify nightly-analysis` | That user → DM |

Pattern C requires no admin involvement — any workspace member can subscribe to notifications for any registered instance:

```
/spore notify nightly-analysis
→ 🔔 You'll receive DMs when *nightly-analysis* changes state.
```

To unsubscribe: `/spore unnotify nightly-analysis`

## Step 8: Long-Running Servers

Not every instance is a batch job. If you are running a development environment, a database, or a web server, a wall-clock TTL would terminate the instance in the middle of active work. Use `--no-timeout` for these cases.

```bash
spawn launch \
  --name dev-server \
  --instance-type m7g.large \
  --ami al2023 \
  --no-timeout
```

**Expected output:**
```
Launching EC2 instance...
  ...
  ✓ Lifecycle: no-timeout (manual termination only)

Instance launched successfully!

Instance ID: i-0f9e8d7c6b5a43210
DNS:         dev-server.d2a4b7c9.spore.host

⚠  No-timeout mode: this instance will run until manually terminated.
   Monitor costs with: spawn cost --instance-id i-0f9e8d7c6b5a43210
   Terminate when done: spawn terminate dev-server
```

`--no-timeout` is appropriate for:
- Active development environments you connect to throughout the day
- Long-running services (database, build cache, CI runner) that need persistent state
- Instances where you cannot predict the end time

`--no-timeout` is NOT appropriate for:
- Batch jobs with a defined completion condition
- Parameter sweep workers
- Any workload you might forget about

When using `--no-timeout`, build a habit of checking costs weekly:

```bash
spawn cost --instance-id dev-server
spawn list --show-costs
```

---

> **`terminate` vs `stop`: Which to use with `--on-complete`**
>
> `--on-complete terminate` — The instance is terminated, the root EBS volume is deleted (unless you attached a separate data volume with `DeleteOnTermination=false`), and no further charges accrue. This is the right default for batch jobs and parameter sweeps. Once the job is done, there is nothing worth preserving on the instance.
>
> `--on-complete stop` — The instance is stopped rather than terminated. Compute charges stop immediately. The EBS volume is preserved and you continue to pay EBS storage rates (~$0.10/GB/month). The instance can be restarted with `spawn resume` and will have its previous filesystem state intact.
>
> Use `stop` only when you have a specific reason to restart the same instance later — for example, a long ML training run where you want to examine intermediate checkpoints, or an instance with a large dataset already downloaded to EBS that would be expensive to re-download. In all other cases, `terminate` is cheaper and simpler.

---

## What You Learned

- Naming instances with `--name` gives you a stable identifier, sets the EC2 Name tag, and automatically registers a DNS record at `<name>.<account-base36>.spore.host`.
- `spawn connect <name>` resolves the DNS and opens SSH in one step — no IP lookup required.
- When no lifecycle flags are set, spored applies a default 1-hour idle timeout as a safety net.
- `--ttl` sets an absolute wall-clock deadline; `--idle-timeout` terminates after N minutes of low CPU; the two combine naturally as hard limit + early exit.
- Creating `/tmp/SPAWN_COMPLETE` inside a job script signals spored to apply the `--on-complete` action immediately, making actual job duration — not estimated duration — control the cost.
- `spawn status <name>` shows TTL remaining, idle timer state, accrued cost, and agent health; `spawn extend <name> <duration>` pushes the deadline out without interrupting the instance.
- `--slack-workspace` enables proactive Slack notifications at every lifecycle transition; users subscribe via `/spore notify <name>` without admin involvement.
- `--no-timeout` is for persistent servers and development environments — not batch jobs.

## Next Steps

📖 **[Tutorial 10: Teams and Resource Sharing](10-teams.md)** — Share instances, SSH keys, and results across a team without per-person AWS credential management.

🛠️ **[How-To: Batch Workflows](../how-to/batch-workflows.md)** — Patterns for script-driven jobs including retries, checkpointing, and S3 result collection.

📚 **[Command Reference: spawn launch](../reference/commands/launch.md)** — Full documentation for `--ttl`, `--idle-timeout`, `--idle-cpu`, `--on-complete`, and `--no-timeout`.

📚 **[Command Reference: spored](../reference/spored.md)** — The on-instance agent: lifecycle enforcement, DNS registration, completion signal handling.

---

**Previous:** [← Tutorial 8: Finding EC2 Capacity](08-finding-ec2-capacity.md)
**Next:** [Tutorial 10: Teams and Resource Sharing](10-teams.md) →
