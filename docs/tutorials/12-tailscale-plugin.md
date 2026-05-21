# Tutorial 12: Private Networking with the Tailscale Plugin

**Duration:** 15 minutes
**Level:** Intermediate
**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md), a Tailscale account with an auth key

## What You'll Learn

In this tutorial, you'll connect a spore instance to your Tailscale network for private access without a public IP:
- Install the Tailscale plugin via `spawn plugin`
- SSH into the instance using the Tailscale IP (100.x.x.x) or MagicDNS hostname
- Access services running on the instance over your Tailnet
- Remove the plugin and clean up the Tailscale node

By the end, you'll be able to launch instances with no public internet exposure and still reach them from any device on your Tailscale network.

## Why Tailscale?

The default way to reach an EC2 instance is a public IP address. That works, but it comes with friction:

❌ **Public IP approach:**
- The instance is reachable from the open internet — requires tight security group rules
- The public IP changes every time the instance stops and restarts
- You must manage inbound security group rules per service (SSH on 22, Jupyter on 8888, etc.)
- IP-based firewall rules are fragile and hard to audit

✅ **Tailscale approach:**
- The instance gets a stable private IP in the `100.x.x.x` range (called a Tailscale IP or Tailnet IP)
- The Tailscale IP never changes for the lifetime of the node — no stale firewall rules
- No inbound security group rules needed: Tailscale handles firewall traversal over UDP
- All traffic is end-to-end encrypted with WireGuard
- Works across NAT, firewalls, and mixed networks — home, office, cloud

The Tailscale plugin handles the full install-and-connect lifecycle automatically. You provide an auth key; the plugin does the rest.

## Prerequisites

Before starting this tutorial:

1. **Create a Tailscale account** at [tailscale.com](https://tailscale.com) if you do not have one. The free tier supports up to 100 devices.

2. **Generate an ephemeral auth key:**
   - Go to the [Tailscale admin console](https://login.tailscale.com/admin/settings/keys)
   - Navigate to **Settings → Keys → Generate auth key**
   - Check the **Ephemeral** box — this ensures the node is automatically removed from your Tailnet when the instance terminates
   - Optionally set an expiry (1 day is sufficient for this tutorial)
   - Copy the key value — it looks like `tskey-auth-k<random>-<random>`

3. **Tailscale on your laptop** — install from [tailscale.com/download](https://tailscale.com/download) and log in. You need to be on the same Tailnet as the instance to connect to it.

> **Note:** Ephemeral nodes expire automatically when they disconnect. This is the right choice for cloud instances — you do not want stale nodes accumulating in your Tailscale admin console every time you terminate an instance.

## Step 1: Launch an Instance

Launch an instance as you normally would. Tailscale works with or without a public IP — you can add `--no-public-ip` if you want the instance to be completely private and your controller machine is already on Tailscale.

```bash
spawn launch \
  --name ts-demo \
  --instance-type t4g.medium \
  --ami al2023 \
  --ttl 2h
```

**Expected output:**
```
Launching EC2 instance...

Configuration:
  Instance Type: t4g.medium
  Region: us-east-1
  AMI: ami-0c1234abcdef5678  (Amazon Linux 2023, arm64)
  Name: ts-demo

Progress:
  ✓ Creating security group (spawn-sg-...)
  ✓ Launching instance
  ✓ Waiting for instance to start...
  ✓ Registering DNS: ts-demo.d2a4b7c9.spore.host
  ✓ Installing spored agent

Instance launched successfully!

Instance ID: i-0a1b2c3d4e5f67890
Public IP:   54.211.88.42
DNS:         ts-demo.d2a4b7c9.spore.host

Cost: $0.0464/hour
TTL: 2h (terminates at 2026-03-29 14:00:00 UTC)
```

> **Note:** If you want a fully private instance with no public IP, add `--no-public-ip` to the launch command. With that flag, the instance gets no public IP address and is reachable only via Tailscale (after the plugin is installed). Your controller machine must already be on your Tailnet — `spawn plugin install` communicates with `spored` over the existing SSH connection to set up Tailscale before the public IP disappears.

## Step 2: Install the Tailscale Plugin

Install the Tailscale plugin using `spawn plugin install`. The `--config` flag passes the auth key to the plugin:

```bash
spawn plugin install \
  github:spore-host/spore-host-plugin-tailscale/tailscale \
  --instance ts-demo \
  --config auth_key=tskey-auth-<YOUR_AUTH_KEY>
```

Replace `tskey-auth-<YOUR_AUTH_KEY>` with your actual auth key.

**Expected output:**
```
Installing plugin tailscale on ts-demo (i-0a1b2c3d4e5f67890)...

  ✓ Fetching plugin from github:spore-host/spore-host-plugin-tailscale/tailscale
  ✓ Uploading plugin to instance
  → installing  Downloading Tailscale package...
  → installing  Installing tailscale (tailscale_1.62.0_arm64)
  → installing  Enabling tailscaled service
  → starting    Running: tailscale up --authkey=tskey-auth-...
  → starting    Waiting for Tailscale to authenticate...
  ✓ running     Tailscale connected

Plugin tailscale installed successfully on ts-demo.

Tailscale IP: 100.94.12.47
MagicDNS:     ts-demo.tail12345.ts.net  (if MagicDNS is enabled)

Connect:
  ssh ec2-user@100.94.12.47
  ssh ec2-user@ts-demo.tail12345.ts.net
```

**What happens under the hood:**

The plugin lifecycle has four phases:

1. **install** — Downloads and installs the `tailscale` and `tailscaled` packages from Tailscale's official package repository. Enables and starts the `tailscaled` system service.
2. **start** — Runs `tailscale up --authkey=<key>` to authenticate the instance with your Tailscale account.
3. **health check** — Runs `tailscale status` to confirm the node is authenticated, connected, and has received a Tailscale IP.
4. **running** — The plugin reports the Tailscale IP and MagicDNS hostname (if MagicDNS is enabled on your Tailnet).

The auth key is passed securely over the existing SSH channel and is not written to any spawn-managed log files.

## Step 3: Verify the Connection

Check plugin status to confirm Tailscale is running and get the Tailscale IP:

```bash
spawn plugin status tailscale --instance ts-demo
```

**Expected output:**
```
Plugin: tailscale
Instance: ts-demo (i-0a1b2c3d4e5f67890)
Status: running

Details:
  Tailscale IP:  100.94.12.47
  MagicDNS:      ts-demo.tail12345.ts.net
  Node key:      nodekey:a1b2c3d4e5f6...
  Last seen:     2s ago
  Auth:          authenticated (ephemeral)

Health: OK
```

You can also verify the node appears in the Tailscale admin console:
- Open [admin.tailscale.com/machines](https://login.tailscale.com/admin/machines)
- The instance should appear as `ts-demo` (or the hostname of the EC2 instance) with IP `100.94.12.47`
- The node will show **ephemeral** in its details, confirming it will auto-expire on disconnect

## Step 4: Connect via Tailscale

You can now SSH directly using the Tailscale IP or MagicDNS hostname:

```bash
# SSH using the Tailscale IP directly
ssh ec2-user@100.94.12.47

# Or use the Tailscale MagicDNS hostname (if enabled in your network)
ssh ec2-user@ts-demo.tail12345.ts.net
```

Both connections bypass the public internet entirely — traffic flows through the Tailscale WireGuard tunnel between your device and the instance.

> **Note:** `spawn connect ts-demo` still works and will use the public IP if one is available. Use the Tailscale IP directly when you want to explicitly route over your Tailnet, or when the instance has no public IP.

**Expected SSH output:**
```
   __                 _       ___     ___ ____  ____  ____
  (_   _ _  _  _    |_ _ _  |_ (_   |_  (_  ) |_  ) |_  )
  __) |_)(_|| |L|   |_ (_|  |__(_)  |__ (_  ) (_  ) (_  )

  Amazon Linux 2023

[ec2-user@ip-10-0-1-47 ~]$
```

## Step 5: Access a Service Over Tailscale

One of the most practical uses of Tailscale is reaching services on the instance — a Jupyter notebook, a development server, a database — without opening any security group ports.

**Example: Jupyter notebook**

Start Jupyter on the instance:

```bash
# On the instance (connected via SSH)
pip install notebook --quiet
jupyter notebook --no-browser --ip=0.0.0.0 --port=8888
```

**Expected output on the instance:**
```
[I 2026-03-29 12:15:03.421 ServerApp] Serving notebooks from local directory: /home/ec2-user
[I 2026-03-29 12:15:03.421 ServerApp] Jupyter Server 2.13.0 is running at:
[I 2026-03-29 12:15:03.421 ServerApp] http://ip-10-0-1-47:8888/tree?token=a1b2c3d4e5f6...
```

Then on your laptop, open a browser to:

```
http://100.94.12.47:8888/?token=a1b2c3d4e5f6...
```

No security group rule for port 8888 is required. Tailscale's WireGuard tunnel allows traffic on any port between devices on your Tailnet. The notebook is accessible from your laptop but unreachable from the open internet.

This pattern works for any service: development HTTP servers, Prometheus, Grafana, VS Code remote, file shares — anything that listens on a port.

## Step 6: Remove the Plugin

When you are done, remove the Tailscale plugin:

```bash
spawn plugin remove tailscale --instance ts-demo
```

**Expected output:**
```
Removing plugin tailscale from ts-demo (i-0a1b2c3d4e5f67890)...

  → stopping    Running: tailscale down
  → stopping    Stopping tailscaled service
  ✓ removed     Plugin tailscale removed

Plugin tailscale removed from ts-demo.
```

Because the auth key was generated as **Ephemeral**, the node automatically expires from the Tailscale admin console once it disconnects. You do not need to manually remove the machine from [admin.tailscale.com/machines](https://login.tailscale.com/admin/machines) — it disappears on its own within a few minutes.

If the instance itself terminates (via TTL or `spawn terminate`), spored handles teardown and Tailscale's ephemeral node expiry takes care of the admin console entry.

---

> **Combining with spore-sync**
>
> Install both the Tailscale and spore-sync plugins for a fully private sync workflow — files sync between your laptop and the instance over your Tailnet with no public IP exposure. The spore-sync plugin can be configured to use the Tailscale IP as its target, so all file transfer traffic stays within your encrypted WireGuard tunnel and never touches the public internet.

---

## What You Learned

- The Tailscale plugin installs and configures Tailscale on the instance using a `spawn plugin install` command; you supply an ephemeral auth key via `--config`.
- After install, the instance gets a stable `100.x.x.x` Tailscale IP that is reachable from any device on your Tailnet — no public IP or security group rules required.
- You can SSH via `ssh ec2-user@100.x.x.x` or the MagicDNS hostname, bypassing the public internet entirely.
- Services running on any port on the instance are accessible over Tailscale without opening security group inbound rules.
- Ephemeral auth keys auto-expire the Tailscale node when the instance disconnects, keeping your admin console clean.

## Next Steps

📖 **[Tutorial 13: High-Speed Data Transfer with the Globus Plugin](13-globus-plugin.md)** — Install Globus Connect Personal on an instance using the push flow, then transfer large datasets between any two Globus endpoints with parallel streams and automatic retry.

🛠️ **[Plugin Authoring Guide](../how-to/plugin-authoring.md)** — Write your own spawn plugins with install, start, health-check, and remove lifecycle hooks.

📚 **[Command Reference: spawn plugin](../reference/commands/plugin.md)** — Full documentation for `spawn plugin install`, `status`, `list`, and `remove`.

---

**Previous:** [← Tutorial 11: Advanced spawn — Sweeps, Arrays, and Autoscaling](11-advanced-spawn.md)
**Next:** [Tutorial 13: High-Speed Data Transfer with the Globus Plugin](13-globus-plugin.md) →
