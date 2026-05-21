# Plugin CLI Reference

All plugin commands communicate with the `spored` daemon on the instance over
an SSH tunnel. SSH must be reachable on the public IP of the instance.

---

## Global flags

These flags are accepted by every `spawn plugin` subcommand.

| Flag | Short | Default | Description |
|---|---|---|---|
| `--instance` | `-i` | *(required)* | EC2 instance ID (`i-0abc1234`) or hostname |
| `--key` | | `~/.ssh/id_rsa` | Path to SSH private key |
| `--json` | | `false` | Emit JSON instead of human-readable output |

---

## `spawn plugin install`

Install a plugin on a running instance.

```
spawn plugin install <plugin-ref> --instance <id> [--config key=value]...
```

### Arguments

| Argument | Description |
|---|---|
| `<plugin-ref>` | Plugin reference (see formats below) |

### Flags

| Flag | Description |
|---|---|
| `--config key=value` | Supply a config parameter. Repeatable. |

### Plugin reference formats

| Format | Description |
|---|---|
| `name` | Official registry (`scttfrdmn/spore-plugins`, main branch) |
| `name@v1.2.0` | Official registry, pinned to tag |
| `github:owner/repo/name` | Custom GitHub repository |
| `github:owner/repo/name@v2.0.0` | Custom GitHub repository, pinned to tag |
| `./path/to/plugin.yaml` | Local file (development) |

For `github:owner/repo/name[@version]`, the plugin spec is fetched from:
```
https://raw.githubusercontent.com/<owner>/<repo>/<version-or-main>/<name>/plugin.yaml
```

### Examples

```bash
# Official plugin with required config
spawn plugin install tailscale --instance i-0abc1234 \
  --config auth_key=tskey-auth-xxx

# Official plugin, pinned version
spawn plugin install tailscale@v1.2.0 --instance i-0abc1234 \
  --config auth_key=tskey-auth-xxx

# Plugin from a custom repo
spawn plugin install github:myorg/my-plugins/myplugin --instance i-0abc1234

# Local development plugin
spawn plugin install ./my-plugin/plugin.yaml --instance i-0abc1234

# Globus with optional config override
spawn plugin install github:spore-host/spore-host-plugin-globus/globus-personal-endpoint \
  --instance i-0abc1234 \
  --config endpoint_name=my-endpoint \
  --config collection_path=/data
```

### Install lifecycle

1. Plugin spec is fetched and validated
2. Local `conditions` are checked — install aborts on failure
3. Remote `conditions` are checked on the instance
4. Remote `install` steps run on the instance
5. Local `provision` steps run (may push values to the instance)
6. Remote `configure` steps run once pushed values are received
7. Remote `start` steps run — plugin enters `running` state

---

## `spawn plugin list`

List all plugins installed on an instance.

```
spawn plugin list --instance <id>
```

### Output

```
NAME                      VERSION  STATUS   UPDATED
tailscale                 1.2.0    running  2026-03-01T10:00:00Z
globus-personal-endpoint  2.0.0    running  2026-03-02T14:30:00Z
```

### JSON output

```bash
spawn plugin list --instance i-0abc1234 --json
```

```json
[
  {
    "name": "tailscale",
    "version": "1.2.0",
    "status": "running",
    "updated_at": "2026-03-01T10:00:00Z"
  }
]
```

---

## `spawn plugin status`

Show the current status of a specific plugin.

```
spawn plugin status <name> --instance <id>
```

### Arguments

| Argument | Description |
|---|---|
| `<name>` | Plugin name as it was installed |

### Output

```
Plugin:  tailscale
Version: 1.2.0
Status:  running
Updated: 2026-03-01T10:00:00Z
Health:  last OK 2026-03-01T10:05:00Z
```

If the plugin has an error:
```
Plugin:  globus-personal-endpoint
Version: 2.0.0
Status:  failed
Updated: 2026-03-01T10:00:00Z
Error:   configure step exited 1: setup key rejected
```

### Status values

| Status | Description |
|---|---|
| `installing` | `remote.install` steps are running |
| `waiting_for_push` | Waiting for `local.provision` to push a value |
| `configuring` | `remote.configure` steps are running |
| `starting` | `remote.start` steps are running |
| `running` | Plugin is healthy and active |
| `degraded` | Health checks have failed 3+ times consecutively |
| `stopped` | Plugin was stopped (via `remove` or instance shutdown) |
| `failed` | Unrecoverable error during install, configure, or start |

---

## `spawn plugin remove`

Stop and remove a plugin from an instance.

```
spawn plugin remove <name> --instance <id>
```

Runs the plugin's `remote.stop` steps, then removes the plugin state from the
instance. This is irreversible — reinstall with `spawn plugin install` if needed.

### Arguments

| Argument | Description |
|---|---|
| `<name>` | Plugin name as it appears in `spawn plugin list` |

### Example

```bash
spawn plugin remove tailscale --instance i-0abc1234
```

```
Removing plugin tailscale from i-0abc1234...
Plugin tailscale removed from i-0abc1234.
```
