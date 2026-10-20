## `spawn dns`

Manage DNS records for spawn instances.

spored registers &lt;name&gt;.&lt;account-base36&gt;.&lt;domain&gt; itself when an instance
boots: the name is --name (or --dns), unless the launch passed --no-dns, and the
domain is spore.host unless the launch targeted a custom DNS deployment
(--dns-domain; see docs/custom-dns.md).

register and delete act on the zone and updater the instance's
spawn:dns-domain / spawn:dns-endpoint tags name, unless --domain / --endpoint
say otherwise.

Examples:
  # List DNS-enabled instances
  spawn dns list

  # Register a DNS name for an instance
  spawn dns register i-1234567890abcdef0 my-server

  # Delete an instance's DNS record and its DNS tags
  spawn dns delete i-1234567890abcdef0

```
spawn dns [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--domain` |  | string |  | DNS zone (default: the instance's spawn:dns-domain, else SPAWN_DNS_DOMAIN / ~/.spawn/config.yaml / SSM, else spore.host) |
| `--endpoint` |  | string |  | Custom DNS updater Function URL (default: the instance's spawn:dns-endpoint, else the same chain, else spore.host's) |

### `spawn dns delete`

Delete DNS record for an instance

```
spawn dns delete <instance-id> [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

### `spawn dns list`

List DNS-enabled spawn instances

```
spawn dns list [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all` | `-a` | bool |  | Show all instances (including those without DNS) |

### `spawn dns register`

Register DNS name for an instance

```
spawn dns register <instance-id> <dns-name>
```

