## `spawn teardown`

Carry out a teardown plan: a report from 'spawn footprint -o json'.

Every resource is re-read from the account before anything happens to it,
and the delete guard is applied again to what the account says NOW. A resource
is deleted only if it is tagged spore-host:managed-by=spawn, its name is one
spawn's teardown registers, it is not spore.host's own service
(spore-host:role=service), not tagged spore-host:permanent=true, not one of
the two permanent account-setup policies (spawn-instance-boundary and
spawn-user-lifecycle, which only 'spawn setup-account --remove' removes), and
not user data. A resource that only spawn's
name or the old spawn:managed tag matched (ownership legacy-name), and anything
another tool owns (calque, lagotto), is never deleted. Anything in use (a live
instance, an instance profile on one, a security group on a network interface,
a pool with workers, an attached policy) is kept, and so is everything it
depends on. Dependents go first: an instance before its security group and
profile, a profile before its role, a role before its policies.

--mode report (the default) re-checks everything and deletes nothing: it says
what reap would do. --mode reap deletes, and needs --yes.

This is the human-run path; the automatic idle teardown is spawn#1654. Reap
mode needs the shared-account teardown lease that keeps one user's teardown
from racing another user's setup, and that lease does not exist yet, so reap
mode refuses to run. It never runs in spore.host's infra account.

```
spawn teardown [flags]
```

**Examples:**

```sh
spawn footprint --all-regions -o json > plan.json
  # What would go, re-checked against the account
  spawn teardown --plan plan.json
  # From stdin
  spawn footprint -o json | spawn teardown --plan -
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--mode` |  | string | `report` | report (re-check, delete nothing) or reap (delete; needs --yes) |
| `--plan` |  | string |  | The 'spawn footprint -o json' report to carry out, or - for stdin (required) |
| `--yes` | `-y` | bool |  | Confirm --mode reap |

