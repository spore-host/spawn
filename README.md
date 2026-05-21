# spawn

Launch and manage EC2 instances with automatic lifecycle management.

Instances terminate themselves via TTL, idle detection, or completion signal — no forgotten bills.

**Requires AWS credentials.**

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

**Debian / Ubuntu**
```bash
curl -LO https://github.com/spore-host/spawn/releases/latest/download/spawn_linux_amd64.deb
sudo dpkg -i spawn_linux_amd64.deb
```

**RHEL / Fedora**
```bash
sudo rpm -i https://github.com/spore-host/spawn/releases/latest/download/spawn_linux_amd64.rpm
```

**Direct download** — pre-built binaries for Linux, macOS, and Windows (amd64/arm64) on the [releases page](https://github.com/spore-host/spawn/releases/latest).

**Build from source**
```bash
git clone https://github.com/spore-host/spawn
cd spawn && make build && sudo make install
```

## Quick Start

```bash
# Launch with auto-terminate on completion
spawn launch my-job --instance-type c6a.xlarge --ttl 4h --on-complete terminate

# Connect by name (auto-starts if stopped)
spawn connect my-job

# Run a one-shot command
spawn connect my-job -- 'nohup bash /tmp/run.sh > /tmp/run.log 2>&1 &'

# Check status
spawn status my-job

# Extend TTL without reconnecting
spawn extend my-job 2h

# List all instances
spawn list
```

## Commands

| Command | Description |
|---------|-------------|
| `launch` | Launch an EC2 instance |
| `connect` | SSH to an instance by name (auto-starts if stopped) |
| `list` | List all managed instances |
| `status` | Instance status, TTL, cost |
| `extend` | Extend TTL on a running instance |
| `stop` / `start` | Stop or start an instance |
| `hibernate` | Hibernate (saves RAM to disk) |
| `cancel` | Terminate an instance |
| `queue` | Batch job queue management |
| `schedule` | Scheduled execution |
| `sweep` | Parameter sweeps |
| `stage` | Data staging to/from S3 |
| `notify` | Chat notification (Slack/Teams) |
| `slurm` | Convert Slurm sbatch scripts |

## spored

spored is the lifecycle daemon that runs inside each instance as a systemd service. It handles TTL enforcement, idle detection, completion signals, and DNS registration. It is built and distributed alongside spawn.

## Go Library

```go
import "github.com/spore-host/spawn/pkg/aws"

client, _ := aws.NewClient(ctx)
inst, _ := client.LaunchInstance(ctx, config)
```

## Documentation

Full reference at **[spore.host/docs](https://spore.host/docs/tools/spawn)**.

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
