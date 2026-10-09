## `spawn footprint`

Report everything spawn has created in an account — not just instances.

'spawn orphans' covers the DATA plane: volumes, security groups, placement
groups, Elastic IPs — the things a launch creates and that a launch's end should
reclaim. This covers the CONTROL plane as well: the Lambdas, their log groups and
execution roles, EventBridge schedules, state tables, and the buckets spawn
auto-creates. The TTL reaper is the backstop for instances; nothing is the
backstop for the reaper (#653), so the honest first step is making the footprint
visible rather than deleting anything.

This command NEVER deletes. It is a report.

Resources are found two ways, and each row says which:

  tag    the spawn:managed tag — authoritative, and what 'spawn cleanup' acts on
  name   a spawn/spore/spored/lagotto/truffle name prefix — needed because most
         of the control plane predates being tagged

The name half has a known blind spot, stated rather than hidden: it cannot find a
resource named differently. One live Lambda is called 'scheduler-handler', with no
prefix at all. So the totals below are a floor, not a guarantee — three separate
hand-built footprint lists during #653 were each incomplete.

```
spawn footprint [flags]
```

**Examples:**

```sh
# This region, including the control plane
  spawn footprint

  # Every region spawn knows about
  spawn footprint --all-regions

  # Machine-readable
  spawn footprint -o json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Scan every region spawn supports |
| `--region` |  | string |  | AWS region (default: configured region) |

