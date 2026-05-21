# spawn — Launch and Manage EC2 Instances

spawn launches AWS EC2 instances with automatic lifecycle management. Instances terminate themselves — no forgotten bills.

**Requires AWS credentials.**

---

## Installation

**macOS / Linux (Homebrew)**

```bash
brew install spore-host/tap/spawn
```

**Windows (Scoop)**

```powershell
scoop bucket add spore-host https://github.com/spore-host/scoop-bucket
scoop install spawn
```

**Debian / Ubuntu (.deb)**

```bash
curl -LO https://github.com/spore-host/spore-host/releases/latest/download/spawn_linux_amd64.deb
sudo dpkg -i spawn_linux_amd64.deb
```

**RHEL / Fedora (.rpm)**

```bash
sudo rpm -i https://github.com/spore-host/spore-host/releases/latest/download/spawn_linux_amd64.rpm
```

**Build from source**

```bash
git clone https://github.com/spore-host/spore-host
cd spore-host/spawn && make build && sudo make install
```

---

## Quick Start

```bash
# Launch with a name — gets DNS at my-job.spore.host automatically
spawn launch --name my-job --instance-type c6a.xlarge --ttl 4h --on-complete terminate

# Connect by name
spawn connect my-job

# Check status (TTL remaining, idle state)
spawn status my-job

# Extend TTL if the job is running long
spawn extend my-job 2h

# List all running instances
spawn list
```

---

## How It Works

When you launch an instance, spawn installs **spored** on it — a lightweight systemd service that runs on the instance itself, independent of your laptop. spored watches for three termination triggers. Whichever fires first wins:

| Trigger | How to set | What happens |
|---------|-----------|--------------|
| **Completion signal** | `touch /tmp/SPAWN_COMPLETE` | Terminates after 30s grace period |
| **Idle timeout** | `--idle-timeout 20m` | Terminates after N minutes below CPU threshold |
| **TTL** | `--ttl 4h` | Hard deadline — terminates no matter what |

Configuration is read from EC2 instance tags, so spored reacts to changes without requiring SSH. `spawn extend` updates the TTL tag and spored picks it up live.

**Default safety net:** if you set neither `--ttl` nor `--idle-timeout`, spawn automatically applies `--idle-timeout 1h`.

---

## Core Commands

### `spawn launch` — Launch an Instance

```bash
spawn launch [flags]
```

```bash
# Named instance with TTL and idle detection
spawn launch \
  --name my-job \
  --instance-type c6a.xlarge \
  --ttl 4h \
  --idle-timeout 20m \
  --on-complete terminate

# Spot instance (up to 70% savings)
spawn launch --name my-job --instance-type c6a.xlarge --spot --ttl 8h

# Launch with a startup script
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ttl 4h \
  --on-complete terminate \
  --script job.sh

# Specific region
spawn launch \
  --name dev \
  --instance-type m7g.large \
  --region us-west-2
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--name string` | Name your instance (sets EC2 Name tag + registers `<name>.spore.host` DNS) |
| `--instance-type string` | EC2 instance type (e.g., `c6a.xlarge`, `t4g.medium`) |
| `--region string` | AWS region (default: from `AWS_DEFAULT_REGION` or `~/.aws/config`) |
| `--ttl duration` | Hard deadline: terminate after this duration (e.g., `4h`, `30m`) |
| `--idle-timeout duration` | Terminate after idle for this long (e.g., `20m`) |
| `--on-complete string` | Action on completion signal: `terminate`, `stop`, `hibernate` |
| `--spot` | Use spot pricing |
| `--script string` | Script to run on the instance at launch |
| `--ami string` | Override AMI (auto-detected by default) |
| `--dns string` | Override DNS name if different from `--name` (advanced) |

---

### `spawn connect` — SSH by Name

```bash
spawn connect <name>
```

Connects via SSH using the instance name — no IP address or instance ID needed.

```bash
spawn connect my-job
```

---

### `spawn status` — Check Instance Status

```bash
spawn status <name>
```

Shows TTL remaining, idle state, CPU usage, and lifecycle configuration.

```bash
spawn status my-job
```

---

### `spawn extend` — Extend the TTL

```bash
spawn extend <name> <duration>
```

Adds time to a running instance's TTL. spored picks up the change live from the EC2 tag — no SSH or restart required.

```bash
spawn extend my-job 2h      # Add 2 hours
spawn extend my-job 30m     # Add 30 minutes
```

---

### `spawn list` — List Running Instances

```bash
spawn list
```

Shows all spawn-managed instances: name, instance type, region, TTL remaining, status.

---

## Examples

### Job that terminates itself on completion

```bash
# job.sh — runs on the instance
#!/bin/bash
python analyze.py --input data.csv --output results/
touch /tmp/SPAWN_COMPLETE   # signals spored to terminate
```

```bash
spawn launch \
  --name my-analysis \
  --instance-type t4g.medium \
  --ttl 4h \
  --on-complete terminate \
  --script job.sh
```

The TTL is a backstop. The completion signal fires as soon as the script finishes, so the instance terminates promptly rather than waiting for the full 4 hours.

### Find cheapest spot capacity with truffle, then launch

```bash
# Check spot prices across Intel, AMD, and Graviton
truffle spot c6i.xlarge c6a.xlarge c7g.xlarge --sort-by-price --active-only

# Launch into the cheapest region
spawn launch \
  --name my-job \
  --instance-type c6a.xlarge \
  --region us-east-2 \
  --spot \
  --ttl 4h \
  --on-complete terminate
```

spored handles spot interruption notices and terminates cleanly when AWS reclaims the instance.

### Extend a running job

```bash
spawn status my-analysis       # Check remaining TTL
spawn extend my-analysis 2h    # Add 2 hours without interrupting the job
spawn status my-analysis       # Confirm new TTL
```

---

## Parameter Sweeps

Run multiple instances as a coordinated job array:

```bash
spawn sweep --params grid.yaml --job-array-name my-sweep
spawn list --array my-sweep
spawn extend --job-array-name my-sweep 2h
```

See [PARAMETER_SWEEPS.md](PARAMETER_SWEEPS.md) for full documentation.

---

## AWS Credentials

```bash
# Standard setup
aws configure

# Environment variables
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_DEFAULT_REGION=us-east-1
```

See [IAM_PERMISSIONS.md](IAM_PERMISSIONS.md) for the minimum IAM policy required.

---

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman. See [LICENSE](../LICENSE).
