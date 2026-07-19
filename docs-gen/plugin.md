## `spawn plugin`

Install, inspect, and remove service plugins on running instances.

Plugins are composable service units (Jupyter, Globus, Tailscale, etc.)
defined by YAML specs with install/start/stop/health lifecycles.

Examples:
  spawn plugin list --instance i-0abc123
  spawn plugin install globus-personal-endpoint --instance i-0abc123 \
    --config endpoint_name=my-endpoint
  spawn plugin status globus-personal-endpoint --instance i-0abc123
  spawn plugin remove globus-personal-endpoint --instance i-0abc123

```
spawn plugin
```

### `spawn plugin install`

Install a plugin on a running spore instance.

Runs the plugin's full lifecycle: local provision steps on this controller
(e.g. creating a mutagen sync or a Globus endpoint), then the remote
install/configure/start steps on the instance via spored. Values captured and
pushed by local steps are delivered before the remote configure phase runs.

Requires SSH access to the instance (the same key used by 'spawn plugin status').

Plugin ref formats:
  name                  official registry (spore-host/spore-plugins)
  name@v1.2.0           pinned to git tag
  github:user/repo/name custom GitHub repository
  ./path/to/plugin.yaml local file

```
spawn plugin install <plugin-ref> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--config` |  | stringArray |  | Config as key=value (repeatable) |
| `--instance` | `-i` | string |  | Instance ID or hostname (required) |
| `--key` |  | string |  | Path to SSH private key |
| `--user` |  | string |  | SSH username for the instance (default: ec2-user) |

### `spawn plugin list`

List plugins installed on an instance

```
spawn plugin list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--instance` | `-i` | string |  | Instance ID or hostname (required) |
| `--key` |  | string |  | Path to SSH private key |
| `--user` |  | string |  | SSH username for the instance (default: ec2-user) |

### `spawn plugin remove`

Remove a plugin from an instance

```
spawn plugin remove <name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--instance` | `-i` | string |  | Instance ID or hostname (required) |
| `--key` |  | string |  | Path to SSH private key |
| `--user` |  | string |  | SSH username for the instance (default: ec2-user) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn plugin status`

Show status of a plugin on an instance

```
spawn plugin status <name> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--instance` | `-i` | string |  | Instance ID or hostname (required) |
| `--key` |  | string |  | Path to SSH private key |
| `--user` |  | string |  | SSH username for the instance (default: ec2-user) |

### `spawn plugin validate`

Statically validate one or more plugin.yaml files without contacting any
instance. Checks schema, semver, known step/condition/config types, that the
containing directory matches the plugin name, and that every {{ config.X }}
template reference points at a declared config parameter.

Examples:
  spawn plugin validate ./plugins/tailscale/plugin.yaml
  spawn plugin validate ./plugins/*/plugin.yaml

```
spawn plugin validate <path>...
```

