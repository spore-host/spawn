## `spawn dns`

Manage DNS records for spawn instances.

DNS names are automatically registered when launching with --dns flag.
Format: &lt;name&gt;.&lt;account-base36&gt;.spore.host

Examples:
  # List DNS-enabled instances
  spawn dns list

  # Register DNS name
  spawn dns register i-1234567890abcdef0 my-server

  # Delete DNS record
  spawn dns delete my-server

```
spawn dns
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--domain` |  | string |  | DNS zone (default: the instance's spawn:dns-domain, else SPAWN_DNS_DOMAIN / ~/.spawn/config.yaml / SSM, else spore.host) |
| `--endpoint` |  | string |  | Custom DNS updater Function URL (default: the instance's spawn:dns-endpoint, else the same chain, else spore.host's) |

### `spawn dns delete`

```
spawn dns delete <instance-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn dns list`

```
spawn dns list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all` | `-a` | bool |  | Show all instances (including those without DNS) |

### `spawn dns register`

```
spawn dns register <instance-id> <dns-name>
```

