# spawn

[![CI](https://github.com/spore-host/spawn/actions/workflows/ci.yml/badge.svg)](https://github.com/spore-host/spawn/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/spore-host/spawn)](https://goreportcard.com/report/github.com/spore-host/spawn)
[![codecov](https://codecov.io/gh/spore-host/spawn/branch/main/graph/badge.svg)](https://codecov.io/gh/spore-host/spawn)
[![Go Reference](https://pkg.go.dev/badge/github.com/spore-host/spawn.svg)](https://pkg.go.dev/github.com/spore-host/spawn)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

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

### Conventions

- **Structured output:** use the root `-o`/`--output` flag (`-o json`) on any
  command. Per-command `--json` flags are deprecated aliases and will be
  removed; prefer `-o json`.
- **Destructive commands** (`cancel`, `terminate`, `delete`, `remove`) prompt
  for confirmation and accept `-y`/`--yes` to skip it (required for
  non-interactive use). A piped/non-interactive run without `--yes` aborts
  rather than acting.

See **[docs/flag-conventions.md](docs/flag-conventions.md)** for the full
convention reference (shared across spawn, truffle, lagotto, and spored).

## spored

spored is the lifecycle daemon that runs inside each instance as a systemd service. It handles TTL enforcement, idle detection, completion signals, and DNS registration. It is built and distributed alongside spawn. As an on-instance CLI it follows the same flag conventions (subcommands `status`, `config get/set/list`, `reload`, `complete`, `version`).

## Go Library

```go
import "github.com/spore-host/spawn/pkg/aws"

client, _ := aws.NewClient(ctx)
inst, _ := client.LaunchInstance(ctx, config)
```

## Documentation

Full reference at **[spore.host/docs](https://spore.host/docs/tools/spawn)**.

- **[Windows beta guide](docs/windows-beta-guide.md)** — end-to-end: ISO → custom
  AMI → launch → connect via RDP or SSH-over-SSM, with AWS SSO sign-in.

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
