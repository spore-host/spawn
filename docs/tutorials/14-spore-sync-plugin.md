# Tutorial 14: Live Directory Sync with the spore-sync Plugin

**Duration:** 20 minutes | **Level:** Intermediate | **Issue:** [#211](https://github.com/spore-host/spore-host/issues/211)

---

## What You'll Learn

- Install [mutagen](https://mutagen.io) on your local machine
- Sync a local project directory to a spore instance in real time
- Watch edits propagate in both directions automatically
- Switch sync modes (one-way, two-way)
- Remove the sync and terminate the session cleanly

**Prerequisites:** [Tutorial 2: Your First Instance](02-first-instance.md), mutagen installed locally

---

## Background

The spore-sync plugin uses [mutagen](https://mutagen.io) to keep a directory on your local machine in sync with a directory on a running spore instance. Think of it like Dropbox for your development workflow: edit a file locally, and it appears on the instance within seconds. Run output appears locally without explicit `scp`.

Unlike `rsync` (one-shot copy), mutagen maintains a persistent daemon that watches both sides for changes and reconciles them continuously.

**How it works:**

1. Plugin installs `rsync` on the instance (mutagen uses it as a transport)
2. `spawn plugin install` runs `mutagen sync create` on your local machine
3. mutagen daemon connects over SSH and watches both directories
4. Changes on either side are mirrored to the other

The mutagen session is named `spore-<instance-id>` so it is deterministic and idempotent — re-running install terminates the old session and creates a fresh one.

---

## Step 1: Install mutagen

### macOS (Homebrew)

```bash
brew install mutagen-io/mutagen/mutagen
```

### Linux

```bash
# Download the latest release
curl -Lo /tmp/mutagen.tar.gz \
  https://github.com/mutagen-io/mutagen/releases/latest/download/mutagen_linux_amd64_v0.18.0.tar.gz
tar -xzf /tmp/mutagen.tar.gz -C /tmp
sudo mv /tmp/mutagen /usr/local/bin/
sudo mv /tmp/mutagen-agents.tar.gz /usr/local/bin/
```

Verify:

```bash
mutagen version
# mutagen version 0.18.x
```

---

## Step 2: Launch a Spore Instance

```bash
spawn launch \
  --name sync-demo \
  --instance-type t3.medium \
  --ttl 2h
```

Note the instance ID:

```
Launched: i-0abc123def456789
DNS:      sync-demo.spore.host
```

---

## Step 3: Create a Project Directory to Sync

```bash
mkdir -p ~/my-project
echo "# My Project" > ~/my-project/README.md
echo "print('hello from local')" > ~/my-project/main.py
```

---

## Step 4: Install the spore-sync Plugin

```bash
spawn plugin install github:spore-host/spore-host-plugin-spore-sync/spore-sync \
  --instance i-0abc123def456789 \
  --config local_path=~/my-project \
  --config remote_path=/home/ec2-user/my-project
```

Expected output:

```
[spore-sync] Checking conditions...
[spore-sync] ✓ mutagen found at /usr/local/bin/mutagen
[spore-sync] Installing on instance...
[spore-sync] ✓ rsync installed
[spore-sync] ✓ Remote directory /home/ec2-user/my-project created
[spore-sync] Running local provision...
[spore-sync] ✓ Mutagen session spore-i-0abc123def456789 created
[spore-sync] Plugin installed successfully
```

---

## Step 5: Verify the Sync Session

```bash
mutagen sync list spore-i-0abc123def456789
```

```
Session: spore-i-0abc123def456789
State:   Watching for changes
Alpha:   /Users/you/my-project
Beta:    ec2-user@sync-demo.spore.host:/home/ec2-user/my-project
Mode:    Two Way Resolved
```

Check the remote side:

```bash
ssh ec2-user@sync-demo.spore.host "ls /home/ec2-user/my-project"
# README.md  main.py
```

---

## Step 6: Test Live Sync — Local → Remote

Add a file locally:

```bash
echo "x = 42" > ~/my-project/config.py
```

Within 1–2 seconds, it appears on the instance:

```bash
ssh ec2-user@sync-demo.spore.host "ls /home/ec2-user/my-project"
# README.md  config.py  main.py
```

---

## Step 7: Test Live Sync — Remote → Local

Create a file on the instance:

```bash
ssh ec2-user@sync-demo.spore.host \
  "echo 'result = 99' > /home/ec2-user/my-project/output.py"
```

Within seconds, it appears locally:

```bash
ls ~/my-project
# README.md  config.py  main.py  output.py
```

---

## Step 8: Monitor Sync Status

```bash
# One-time status
mutagen sync list spore-i-0abc123def456789

# Watch continuously (refresh every 2s)
mutagen sync monitor spore-i-0abc123def456789
```

If there are conflicts (both sides changed the same file simultaneously):

```bash
mutagen sync flush spore-i-0abc123def456789
```

---

## Step 9: Change Sync Mode

The default mode is `two-way-resolved` (both sides can write, conflicts resolved automatically). For read-only workflows where you only want local → remote:

```bash
# Remove the current session
mutagen sync terminate spore-i-0abc123def456789

# Recreate with one-way mode
spawn plugin install github:spore-host/spore-host-plugin-spore-sync/spore-sync \
  --instance i-0abc123def456789 \
  --config local_path=~/my-project \
  --config remote_path=/home/ec2-user/my-project \
  --config mode=one-way-safe
```

Available modes:

| Mode | Description |
|---|---|
| `two-way-resolved` | Both sides sync; conflicts auto-resolved (default) |
| `two-way-safe` | Both sides sync; conflicts pause for manual resolution |
| `one-way-safe` | Local → Remote only; remote changes are ignored |
| `one-way-replica` | Local → Remote mirror; remote changes are deleted |

---

## Step 10: Remove the Plugin

When you are done, uninstall the plugin to terminate the mutagen session:

```bash
spawn plugin uninstall spore-sync --instance i-0abc123def456789
```

```
[spore-sync] Running local deprovision...
[spore-sync] ✓ Mutagen session spore-i-0abc123def456789 terminated
[spore-sync] Plugin removed
```

Verify:

```bash
mutagen sync list
# (no sessions)
```

---

## Pro Tips

### Combine with Tailscale for Private-Network Sync

Install Tailscale first so mutagen connects over the Tailnet instead of a public IP:

```bash
spawn plugin install github:spore-host/spore-host-plugin-tailscale/tailscale \
  --instance i-0abc123def456789 \
  --config auth_key=tskey-auth-...

spawn plugin install github:spore-host/spore-host-plugin-spore-sync/spore-sync \
  --instance i-0abc123def456789 \
  --config local_path=~/my-project
```

mutagen will use the 100.x.x.x Tailscale address automatically if that is the SSH target.

### Ignore Large Build Artifacts

The `--ignore-vcs` flag (set by default in the plugin) skips `.git/`. To also ignore build outputs, create a `.mutagen.yml` in your local project:

```yaml
sync:
  defaults:
    ignore:
      paths:
        - "__pycache__"
        - "*.pyc"
        - "node_modules"
        - "dist"
        - ".venv"
```

mutagen picks this up automatically.

### Multiple Projects on One Instance

Use different `remote_path` values for each project:

```bash
spawn plugin install github:spore-host/spore-host-plugin-spore-sync/spore-sync \
  --instance i-0abc123def456789 \
  --config local_path=~/project-a \
  --config remote_path=/home/ec2-user/project-a

spawn plugin install github:spore-host/spore-host-plugin-spore-sync/spore-sync \
  --instance i-0abc123def456789 \
  --config local_path=~/project-b \
  --config remote_path=/home/ec2-user/project-b
```

Each creates a separate mutagen session named `spore-<instance-id>-<remote-path-hash>`.

---

## What You Learned

- mutagen provides continuous bi-directional sync over SSH
- spore-sync plugin creates a named session tied to the instance ID
- Changes on either side propagate in 1–2 seconds
- Sync mode controls conflict resolution behavior
- Deprovision cleans up the mutagen session automatically

---

## Next Steps

- [Tutorial 15: RStudio Server Plugin](15-rstudio-server-plugin.md) — replicate your R environment on an instance
- [Tutorial 12: Tailscale Plugin](12-tailscale-plugin.md) — combine with private networking for secure sync
- [Tutorial 9: Instance Lifecycle](09-instance-lifecycle.md) — TTL and auto-termination to avoid forgotten instances

---

*[Back to Tutorial Index](README.md)*
