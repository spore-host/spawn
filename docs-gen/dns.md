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
| `--domain` |  | string |  | DNS domain for record registration (default: spore.host) |

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

