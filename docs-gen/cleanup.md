## `spawn cleanup`

Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first. A cluster placement group that still has members is reported and
skipped, since EC2 refuses to delete one — re-run once those instances are gone.

Preview what would be removed with --dry-run; otherwise cleanup prompts for
confirmation (skip with --yes) and then deletes. By default it acts only on
resources you created; --all widens to every principal in the account.

--elastic-ips releases only the Elastic IPs spawn allocated for multi-interface
EFA launches whose instance is gone (terminated, or no longer known to EC2) and
that are attached to nothing. Each one bills and counts toward the region's
Elastic IP limit. This mode touches nothing else, so running instances do not
block it. spawn list and spawn status warn about these addresses and print the
command.

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
| `--elastic-ips` |  | bool |  | Release only orphaned spawn launch Elastic IPs (unassociated, instance gone); nothing else is touched |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

