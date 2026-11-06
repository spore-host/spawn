## `spawn egress-proxy`

A --no-public-ip launch with --bootstrap-egress proxy, in a VPC with no NAT,
fetches spawn's own bootstrap downloads through a spawn egress proxy: one
t4g.micro per VPC running 'spored egress-proxy', with two security groups,
spawn-egress-proxy-&lt;vpc&gt; and spawn-egress-clients-&lt;vpc&gt;. The launch finds or
creates it. A proxy lives at most 6 days and ends sooner when unused for its
idle period (72h by default); the TTL reaper is the backstop for both, and its
managed-network sweep deletes the groups once nothing uses them.

These commands show, delete and upgrade them. The region is the global
--region, else your configured region.

```
spawn egress-proxy
```

### `spawn egress-proxy delete`

Terminate the egress proxy instances of a VPC (--vpc) or of every VPC in
the region (--all), wait until they are terminated, then delete the two
security groups: the proxy's group first, then the clients group.

Only instances spawn can tell it launched as the VPC's proxy are terminated:
tagged spawn:egress-proxy=&lt;vpc&gt;, carrying exactly the proxy group, running with
spored-instance-profile, and with a deadline no later than launch plus the
proxy's life. A tagged instance that fails these checks is refused, left
alone, and named with the reason; the command then exits non-zero.

A clients group that member instances still carry is kept, and its users are
named; the TTL reaper's managed-network sweep deletes it once nothing uses it.
Members that have finished their bootstrap do not use the proxy, so deleting it
does not affect them.

```
spawn egress-proxy delete [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all` |  | bool |  | Delete the proxies and groups of every VPC in the region |
| `--dry-run` |  | bool |  | Print what would be terminated and deleted, and change nothing |
| `--vpc` |  | string |  | The VPC whose proxy and groups to delete (a vpc- ID) |
| `--yes` | `-y` | bool |  | Do not ask for confirmation |

### `spawn egress-proxy status`

Show every instance tagged spawn:egress-proxy in the region (or in --vpc):
its state, address, the spawn release that launched it, its deadline, the
idle activity it published, and
whether the next --bootstrap-egress proxy launch would reuse it (and if not,
why). For each VPC, also the two security groups and how many network
interfaces use the clients group.

The activity is what the proxy last published, and spored rewrites it at most
every 10 minutes in steady use, so it can trail the proxy's real traffic: the
last activity is "at or after" the time shown, and an idle expiry is "no
earlier than" the one shown. The proxy itself ends on its own current clock.

-o json prints the same as JSON, with stable snake_case keys.

Read-only: ec2:DescribeInstances and ec2:DescribeSecurityGroups.

```
spawn egress-proxy status [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Show every region enabled in this account |
| `--vpc` |  | string |  | Show only this VPC (a vpc- ID) |

### `spawn egress-proxy upgrade`

Launch a fresh egress proxy at this CLI's version in the VPC named by
--vpc, through the same find-or-create a launch uses, wait until it is ready,
then terminate the old one (unless --keep-old, which leaves it to expire on
its own). The new proxy
keeps the idle period the old one published, else 72h.

If the VPC's proxy is already at this CLI's version with more than 12 hours left, it
is reported as current and nothing changes. A VPC with no proxy is left
without one: the next --bootstrap-egress proxy launch creates it.

A member that is mid-bootstrap through the old proxy when it is terminated can
fail its download; members that have finished their bootstrap do not use it.

```
spawn egress-proxy upgrade [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--keep-old` |  | bool |  | Leave the old proxy running until it expires on its own |
| `--vpc` |  | string |  | The VPC whose proxy to upgrade (a vpc- ID; required) |
| `--yes` | `-y` | bool |  | Do not ask for confirmation |

