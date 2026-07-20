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

### `spawn plugin inspect`

Resolve a plugin reference and render its plan — resolved source and
version, local (controller) vs remote (instance) steps, requested controller
environment, root vs login-user execution, downloads, health checks, cleanup,
and its declared permissions block — WITHOUT executing anything or contacting an
instance.

Installing a plugin runs its author's code on your machine and, on the instance,
as root. Inspect it first, especially for third-party (github:) plugins.

Plugin ref formats are the same as 'spawn plugin install':
  name                  official registry (spore-host/spore-plugins)
  name@v1.2.0           pinned to git tag
  github:user/repo/name custom GitHub repository
  ./path/to/plugin.yaml local file

```
spawn plugin inspect <plugin-ref>
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
| `--dry-run` |  | bool |  | Preview the plan without installing (contacts no instance) |
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

### `spawn plugin manifest`

Generate the checksum manifest (manifest.json) for a plugin directory. The
manifest records the sha256 of the plugin's plugin.yaml so that spawn can verify
a fetched official plugin matches the released bytes. This is the generator side
of the registry supply-chain story: the registry's release workflow runs it and
publishes the output as a GitHub Release asset; spawn verifies against it at
install time. Contacts nothing.

Examples:
  spawn plugin manifest ./plugins/tailscale
  spawn plugin manifest ./plugins/tailscale -o manifest.json

```
spawn plugin manifest <plugin-dir> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--output` | `-o` | string |  | Write manifest to this file instead of stdout |

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
containing directory matches the plugin name, and that every &#123;&#123; config.X &#125;&#125;
template reference points at a declared config parameter.

Examples:
  spawn plugin validate ./plugins/tailscale/plugin.yaml
  spawn plugin validate ./plugins/*/plugin.yaml

```
spawn plugin validate <path>...
```

