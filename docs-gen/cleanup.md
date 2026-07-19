## `spawn cleanup`

Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first.

Preview what would be removed with --dry-run; otherwise cleanup prompts for
confirmation (skip with --yes) and then deletes. By default it acts only on
resources you created; --all widens to every principal in the account.

A log of everything removed is written to ~/.spawn/cleanup-&lt;timestamp&gt;.log.

```
spawn cleanup [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Clean up every enabled region |
| `--all` |  | bool |  | Include resources created by other principals (default: only yours) |
| `--dry-run` |  | bool |  | Preview what would be removed without deleting anything |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

