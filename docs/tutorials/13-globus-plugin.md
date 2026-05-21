# Tutorial 13: High-Speed Data Transfer with the Globus Plugin

**Duration:** 20 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md), `globus-cli` installed and logged in

## What You'll Learn

In this tutorial, you'll connect a spore instance to your Globus account and use it as a high-throughput transfer endpoint:
- Install the `globus-connect-personal` plugin using the push flow
- Verify the Globus endpoint appears in your account
- Transfer a large dataset from a remote endpoint to the instance
- Transfer results back when the job is done
- Remove the plugin and automatically clean up the Globus endpoint

By the end, you'll have a repeatable workflow for moving large datasets (10 GB–10 TB) between HPC clusters, cloud storage, and spore instances using Globus's parallel transfer engine.

## Why Globus?

For small files, `scp` and `rsync` are fine. For large datasets, they hit a wall:

❌ **`scp` / `rsync` over SSH:**
- Single-stream transfer — limited by SSH throughput (~100–200 MB/s best case on a fast connection)
- No parallelism: one file at a time unless scripted with `xargs` or `parallel`
- No automatic retry on partial failure — interrupted transfers must restart from scratch
- Not natively accessible from HPC systems behind institutional firewalls

✅ **Globus:**
- Parallel multi-stream transfers — saturates available bandwidth (routinely 1–5 GB/s on fast research networks)
- Optimized specifically for large scientific datasets: 10 GB–10 TB
- Automatic checkpointing and retry — resumes from where it left off after a network interruption
- Works between any two Globus endpoints: ACCESS/XSEDE allocations, university HPC clusters, national labs, personal laptops, and cloud instances
- `--sync-level checksum` ensures data integrity without re-transferring files that already arrived

The Globus plugin connects your spore instance to this network as a Globus Connect Personal endpoint in under two minutes.

## Prerequisites

Before starting this tutorial:

1. **Install `globus-cli`:**
   ```bash
   pip install globus-cli
   ```

2. **Log in to Globus:**
   ```bash
   globus login
   ```
   This opens a browser window to complete the OAuth2 login flow.

3. **Verify your identity:**
   ```bash
   globus whoami
   ```

   **Expected output:**
   ```
   yourname@university.edu
   ```

4. **Identify a source endpoint** — the Globus endpoint you want to transfer data from. This could be:
   - Your institution's HPC cluster (ask your HPC admins for the endpoint UUID or search by name)
   - An ACCESS/XSEDE allocation endpoint
   - Another personal endpoint on your laptop
   - Any public Globus endpoint in the catalog

> **Note:** If you do not have a second Globus endpoint available, you can create a personal endpoint on your laptop using [Globus Connect Personal](https://www.globus.org/globus-connect-personal) and transfer files between your laptop and the instance.

## How the Push Flow Works

The Globus plugin uses a **push flow** to set up the endpoint — a pattern designed for cases where the instance cannot initiate outbound browser-based OAuth2 authentication.

Here is the full lifecycle:

1. **install** — The plugin downloads and installs Globus Connect Personal on the instance.
2. **local.provision** — The spawn controller (your laptop) runs `globus gcp create mapped` to create a new Globus endpoint in your account and captures the setup key.
3. **push** — The spawn controller pushes the setup key to the instance over SSH. The instance does not need to contact Globus auth endpoints directly.
4. **configure** — The instance runs `globusconnectpersonal -setup <key>` to bind the endpoint to your account.
5. **start** — The instance starts the `globusconnectpersonalhelper` daemon. The endpoint goes online.
6. **running** — Health checks confirm the endpoint is reachable and `globus endpoint show` reports it as `connected`.

The push flow is why you need `globus login` on your laptop before running `spawn plugin install` — the local provision step uses your Globus credentials to create the endpoint on your behalf.

## Step 1: Launch an Instance

Allocate enough disk for your dataset. The `--disk` flag sets the EBS root volume size in GB.

```bash
spawn launch \
  --name globus-demo \
  --instance-type m7g.large \
  --ami al2023 \
  --disk 200 \
  --ttl 4h \
  --on-complete terminate
```

**Expected output:**
```
Launching EC2 instance...

Configuration:
  Instance Type: m7g.large
  Region: us-east-1
  AMI: ami-0c1234abcdef5678  (Amazon Linux 2023, arm64)
  Name: globus-demo
  Disk: 200 GB

Progress:
  ✓ Creating security group (spawn-sg-...)
  ✓ Launching instance
  ✓ Waiting for instance to start...
  ✓ Registering DNS: globus-demo.d2a4b7c9.spore.host
  ✓ Installing spored agent

Instance launched successfully!

Instance ID: i-0b2c3d4e5f6a78901
Public IP:   54.211.99.15
DNS:         globus-demo.d2a4b7c9.spore.host

Cost: $0.1120/hour
TTL: 4h (terminates at 2026-03-29 16:00:00 UTC)
On complete: terminate
```

> **Note:** Use `--disk` to allocate space appropriate for your dataset before launching. Resizing a running EBS volume is possible but requires additional steps. It is easier to size correctly upfront. See the callout at the end of this tutorial for cost guidance.

## Step 2: Install the Globus Plugin

Install the plugin with `spawn plugin install`. The `--config` flags pass the endpoint name and the local collection path (the directory the endpoint will serve):

```bash
spawn plugin install \
  github:spore-host/spore-host-plugin-globus/globus-connect-personal \
  --instance globus-demo \
  --config endpoint_name=my-spore-endpoint \
  --config collection_path=/data
```

This command takes approximately 2 minutes — the full install, provision, configure, and start cycle takes longer than the Tailscale plugin because it involves creating a Globus endpoint on your behalf.

**Expected output:**
```
Installing plugin globus-connect-personal on globus-demo (i-0b2c3d4e5f6a78901)...

  ✓ Fetching plugin from github:spore-host/spore-host-plugin-globus/globus-connect-personal
  ✓ Uploading plugin to instance
  → installing        Downloading Globus Connect Personal...
  → installing        Installing globusconnectpersonal
  → waiting_for_push  Running local.provision step...
  → waiting_for_push  Creating Globus endpoint: my-spore-endpoint
  → waiting_for_push  Endpoint created: a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx
  → waiting_for_push  Pushing setup key to instance...
  → configuring       Running: globusconnectpersonal -setup <key>
  → configuring       Endpoint bound to account: yourname@university.edu
  → starting          Starting globusconnectpersonalhelper daemon
  → starting          Waiting for endpoint to come online...
  ✓ running           Endpoint connected

Plugin globus-connect-personal installed successfully on globus-demo.

Endpoint name: my-spore-endpoint
Endpoint ID:   a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx
Collection:    /data
```

The endpoint ID (`a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx`) is the UUID you will use in subsequent `globus transfer` commands.

## Step 3: Verify the Endpoint

Check plugin status to confirm the endpoint is online:

```bash
spawn plugin status globus-connect-personal --instance globus-demo
```

**Expected output:**
```
Plugin: globus-connect-personal
Instance: globus-demo (i-0b2c3d4e5f6a78901)
Status: running

Details:
  Endpoint name: my-spore-endpoint
  Endpoint ID:   a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx
  Collection:    /data
  Account:       yourname@university.edu
  Connected:     yes
  Last seen:     5s ago

Health: OK
```

Confirm the endpoint is visible to Globus with `globus-cli`:

```bash
globus endpoint search my-spore-endpoint
```

**Expected output:**
```
ID                                   | Owner                       | Display Name
------------------------------------ | --------------------------- | --------------------
a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx | yourname@university.edu     | my-spore-endpoint
```

The endpoint is now live and ready to receive transfers from any other Globus endpoint you have access to.

## Step 4: Transfer Data to the Instance

First, collect the endpoint UUIDs for both ends of the transfer. You need the ID of your source endpoint (the system with your data) and the instance endpoint created in Step 2.

```bash
# Get the instance endpoint ID (created in Step 2)
globus endpoint search my-spore-endpoint \
  --jmespath 'DATA[0].id' --format unix
# → a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx

# Get your source endpoint ID (replace with your actual endpoint name)
globus endpoint search "my-source-endpoint" \
  --jmespath 'DATA[0].id' --format unix
# → e5f6g7h8-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

Now initiate the transfer:

```bash
globus transfer \
  e5f6g7h8-xxxx-xxxx-xxxx-xxxxxxxxxxxx:/datasets/my-data \
  a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx:/data/my-data \
  --label "upload to spore" \
  --sync-level checksum
```

**Expected output:**
```
Message: The transfer has been accepted and a task has been created and queued for execution
Task ID: 9f8e7d6c-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

Monitor the transfer until it completes:

```bash
globus task wait 9f8e7d6c-xxxx-xxxx-xxxx-xxxxxxxxxxxx --polling-interval 10
```

**Expected output (while running):**
```
9f8e7d6c-xxxx-xxxx-xxxx-xxxxxxxxxxxx [ACTIVE] 42 / 120 files transferred (18.7 GB / 54.3 GB)
```

**Expected output (on completion):**
```
9f8e7d6c-xxxx-xxxx-xxxx-xxxxxxxxxxxx [SUCCEEDED]
```

`--sync-level checksum` tells Globus to verify file integrity with checksums rather than just comparing file sizes and timestamps. This is the safest option for scientific data and has negligible performance overhead for large files.

> **Note:** Globus transfers run asynchronously in the cloud — you do not need to keep a terminal open. The transfer continues even if you close your laptop. Check status anytime with `globus task show <task-id>`.

## Step 5: Transfer Results Back

After processing, transfer the results from the instance back to your source endpoint or a different destination:

```bash
globus transfer \
  a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx:/data/results \
  e5f6g7h8-xxxx-xxxx-xxxx-xxxxxxxxxxxx:/results/run-$(date +%Y%m%d) \
  --label "results from spore" \
  --sync-level checksum
```

**Expected output:**
```
Message: The transfer has been accepted and a task has been created and queued for execution
Task ID: 2c3d4e5f-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

The `$(date +%Y%m%d)` in the destination path creates a date-stamped directory (e.g., `/results/run-20260329`), keeping each run's output separate.

Wait for completion before removing the plugin:

```bash
globus task wait 2c3d4e5f-xxxx-xxxx-xxxx-xxxxxxxxxxxx --polling-interval 10
```

## Step 6: Remove the Plugin

Once all transfers are complete, remove the plugin:

```bash
spawn plugin remove globus-connect-personal --instance globus-demo
```

**Expected output:**
```
Removing plugin globus-connect-personal from globus-demo (i-0b2c3d4e5f6a78901)...

  → stopping         Stopping globusconnectpersonalhelper daemon
  → deprovisioning   Running local.deprovision step...
  → deprovisioning   Running: globus endpoint delete a1b2c3d4-xxxx-xxxx-xxxx-xxxxxxxxxxxx
  → deprovisioning   Endpoint deleted from Globus account
  ✓ removed          Plugin globus-connect-personal removed

Plugin globus-connect-personal removed from globus-demo.
```

The `local.deprovision` lifecycle hook automatically runs `globus endpoint delete` using your local Globus credentials. The endpoint is permanently removed from your Globus account — it will not appear in `globus endpoint search` or the [Globus web app](https://app.globus.org) after removal.

If the instance terminates before you run `spawn plugin remove` (for example, the TTL expires mid-transfer), spored calls the plugin's teardown hooks on the instance side. The `local.deprovision` step still runs on your laptop when you next run `spawn plugin remove`, cleaning up the Globus endpoint registration.

---

> **Large Dataset Tip**
>
> For datasets larger than 100 GB, use `--disk 500` or larger when launching. EBS `gp3` storage costs approximately $0.08/GB/month on `gp3` (default), so 500 GB costs roughly $1.33/day — inexpensive insurance against running out of space mid-transfer and having to restart. A failed transfer that exhausts disk space can leave partial files that are difficult to distinguish from complete ones even with `--sync-level checksum`.
>
> Size guidance by dataset:
> - 10–50 GB dataset → `--disk 100` (default 20 GB is not enough)
> - 50–200 GB dataset → `--disk 300`
> - 200 GB–1 TB dataset → `--disk 1000` or a separate attached EBS volume
>
> For very large datasets, consider attaching a separate data volume at launch with `--data-volume 2000` so that the data volume can be detached and reattached to a new instance independently of the root volume.

---

## What You Learned

- The Globus plugin uses a push flow: the spawn controller creates the Globus endpoint on your behalf and pushes the setup key to the instance, avoiding the need for browser-based auth on the instance.
- `spawn plugin install` drives the full lifecycle from download through `globusconnectpersonal -setup` to daemon start — the endpoint is online and usable in ~2 minutes.
- `globus transfer` with `--sync-level checksum` initiates asynchronous, parallel, integrity-checked transfers between any two Globus endpoints; the transfer continues independently of your terminal session.
- `spawn plugin remove` triggers `local.deprovision`, which automatically deletes the Globus endpoint from your account via `globus endpoint delete` — no manual cleanup in the Globus web app.
- Allocating sufficient disk with `--disk` before launch avoids failed transfers due to space exhaustion.

## Next Steps

📖 **[Tutorial 12: Private Networking with the Tailscale Plugin](12-tailscale-plugin.md)** — Combine Tailscale and Globus for a fully private transfer workflow: Globus moves the data at high speed, Tailscale handles secure management access with no public IP.

🛠️ **[Plugin Authoring Guide](../how-to/plugin-authoring.md)** — Learn the full plugin lifecycle API including `local.provision`, `push`, and `local.deprovision` hooks for push-flow plugins.

📚 **[Command Reference: spawn plugin](../reference/commands/plugin.md)** — Full documentation for `spawn plugin install`, `status`, `list`, and `remove`.

📚 **[Globus CLI Reference](https://docs.globus.org/cli/)** — Complete `globus-cli` documentation including `transfer`, `task`, and `endpoint` subcommands.

---

**Previous:** [← Tutorial 12: Private Networking with the Tailscale Plugin](12-tailscale-plugin.md)
**Next:** [Plugin Authoring Guide](../how-to/plugin-authoring.md) →
