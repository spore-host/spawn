# spawn plugin

Manage service plugins on running spore instances.

## Synopsis

```bash
spawn plugin <subcommand> --instance <id> [flags]
```

## Description

Plugins extend running instances with additional services (e.g., Tailscale VPN, RStudio Server, file sync). They are installed over SSH by fetching a `plugin.yaml` spec that defines setup steps, configuration, and lifecycle hooks.

See the [full plugin CLI reference](../../plugins/CLI_REFERENCE.md) for complete flag documentation.

## Global Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--instance` | `-i` | (required) | Instance ID (`i-0abc123`) or hostname |
| `--key` | | `~/.ssh/id_rsa` | SSH private key path |
| `--json` | | `false` | JSON output |

## Subcommands

| Subcommand | Description |
|------------|-------------|
| `install <plugin-ref>` | Install a plugin |
| `list` | List installed plugins |
| `status <name>` | Show plugin status |
| `remove <name>` | Remove a plugin |

## Quick Reference

```bash
# Install an official plugin
spawn plugin install tailscale -i i-0abc1234 --config auth_key=tskey-auth-<key>

# Install a plugin from a custom repo
spawn plugin install github:myorg/plugins/myservice -i i-0abc1234

# Install a local plugin (development)
spawn plugin install ./my-plugin.yaml -i i-0abc1234

# List installed plugins
spawn plugin list -i i-0abc1234

# Check plugin status
spawn plugin status tailscale -i i-0abc1234

# Remove a plugin
spawn plugin remove tailscale -i i-0abc1234
```

## Plugin Reference Formats

| Format | Description |
|--------|-------------|
| `name` | Official registry (latest) |
| `name@v1.2.0` | Official registry, pinned to tag |
| `github:owner/repo/name` | Custom GitHub repository |
| `github:owner/repo/name@v1.0.0` | Custom GitHub repository, pinned |
| `./path/to/plugin.yaml` | Local file |

## See Also

- [Plugin CLI Reference](../../plugins/CLI_REFERENCE.md) — Full flag documentation
- [Plugin Authoring Guide](../../plugins/AUTHORING_GUIDE.md) — Write your own plugins
- [Tutorial 12: Tailscale](../../tutorials/12-tailscale-plugin.md)
- [Tutorial 13: Globus](../../tutorials/13-globus-plugin.md)
- [Tutorial 14: spore-sync](../../tutorials/14-spore-sync-plugin.md)
- [Tutorial 15: RStudio Server](../../tutorials/15-rstudio-server-plugin.md)
