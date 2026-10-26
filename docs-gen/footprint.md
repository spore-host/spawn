## `spawn footprint`

Report everything spawn has created in an account — not just instances.

'spawn orphans' covers the DATA plane: volumes, security groups, placement
groups, Elastic IPs — the things a launch creates and that a launch's end should
reclaim. This covers the CONTROL plane as well: the Lambdas, their log groups and
execution roles, EventBridge event buses and the rules on every bus (the default
event bus and spawn's own), EventBridge Scheduler schedules (what 'spawn
schedule' creates, in every schedule group), state tables, and the buckets spawn
auto-creates. The TTL reaper is the backstop for instances; nothing is the
backstop for the reaper (#653), so the honest first step is making the footprint
visible rather than deleting anything.

This command NEVER deletes. It is a report.

Resources are found two ways, and each row says which:

  tag          the spawn:managed tag — authoritative, and what 'spawn cleanup' acts on
  name         a spawn/spore/spored/lagotto/truffle name prefix — needed because
               most of the control plane predates being tagged
  target       a schedule whose target is a spore.host Lambda
  description  a schedule named like spawn's (sched-YYYYMMDD-HHMMSS) whose
               description is spawn's

Reading schedules needs scheduler:ListSchedules (and scheduler:GetSchedule for
the description check). Without them the report says so under "Could not read"
and lists everything else.

The name half has a known blind spot, stated rather than hidden: it cannot find a
resource named differently. A few known spore.host names with no prefix (such as
the 'scheduler-handler' Lambda) are matched by exact name, but any other
differently-named resource is missed. So the totals below are a floor, not a
guarantee — three separate hand-built footprint lists during #653 were each
incomplete.

```
spawn footprint [flags]
```

**Examples:**

```sh
# This region, including the control plane
  spawn footprint

  # Every region enabled in this account
  spawn footprint --all-regions

  # Machine-readable
  spawn footprint -o json
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Scan every region enabled in this account (DescribeRegions), not just the configured one |
| `--region` |  | string |  | AWS region (default: configured region) |

