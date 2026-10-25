# spawn TTL reaper

Out-of-band backstop that terminates spawn-managed EC2 instances past their
deadline — even when the in-instance `spored` daemon is dead.

## Why

Instance lifetime is normally enforced from *inside* the instance by `spored`'s
monitor loop (TTL / idle / cost / on-complete / pre-stop). [#65] showed that
when that loop silently dies, all of it stops — and instances run forever. The
reaper enforces the same deadline from the outside, so a spore can never outlive
its deadline regardless of `spored`'s health.

It is a **backstop, not a replacement**: `spored` remains the primary, graceful
enforcer (it runs the pre-stop hook, deregisters DNS, stops vs terminates per
policy). The reaper only catches what `spored` missed.

A reaper-kill is an external `TerminateInstances`. By default it does **not** run
the in-instance pre-stop hook; with `REAPER_GRACEFUL=true` ([#187]) it first
attempts the hook over SSM, bounded by `REAPER_GRACEFUL_MAX_WAIT`, and terminates
regardless of the outcome. That attempt runs the hook as the instance's
`spawn:local-username` and is **skipped entirely** if that tag is missing or does
not name a valid non-`root` user — SSM commands run as root, and both the hook and
the username are instance-writable tags, so running one without dropping privilege
would be arbitrary root execution on any spawn-managed instance in the account
(GHSA-8q3p-qcv8-2pr4). Skipping cannot weaken the deadline guarantee: termination
happens either way.

A running `spawn task run` instance (tagged `spawn:task-id`) is different: before
terminating it, the reaper always runs the task's terminal-flush hook over SSM
([#1186]), the same command `spawn terminate` sends, so the task's `command.log`
and `completion.json` reach its results prefix. It is bounded at 60 s, runs only
spawn's own root-owned `/etc/spawn/task-flush.sh` after the same trust check
spored applies (nothing from a tag is interpolated), and the terminate follows
whatever happens.

## What it does

Every schedule tick (`rate(10 minutes)` by default) it scans the configured
regions for instances tagged `spawn:managed=true` (running **and** stopped — an
idle-stopped instance runs no daemon, so only the reaper will ever reclaim it,
[#71]) and terminates any where **either**:

1. `now > spawn:ttl-deadline` (the authoritative, launch-anchored deadline) —
   or, when that tag is missing or unparseable, `now > spawn:launch-time +
   spawn:ttl`, the deadline spored itself enforces in that case ([#990]), or
2. `now - spawn:launch-time > REAPER_MAX_AGE` — a hard ceiling that fires even
   for `--no-timeout` / missing / unparseable deadlines.

For (1) there is one grace ([#1186]): a **running** instance whose
`spawn:last-heartbeat` is fresh (spored checked in within 10 minutes) is left up
to `REAPER_TTL_GRACE` (default 10m) past its deadline, because spored is alive to
enforce that deadline itself and flushes the task record on the way out. A stale
or missing heartbeat, or a stopped instance, is reaped at the deadline.

Note that (2) is a genuine **ceiling**, not a fallback: it applies to an instance
whose deadline is still in the future. A within-deadline instance older than
`REAPER_MAX_AGE` is reaped, because `spawn:ttl-deadline` is writable from inside
the instance and an honored far-future deadline would make a spore permanently
unreapable ([#76]). Within-deadline instances under the ceiling are spared — the
deadline is honored intent, up to the ceiling.

It also (when configured) tears down a reaped instance's Route53 records ([#247])
and — with `REAPER_DNS_SWEEP=true` — runs a **DNS reconciliation sweep** ([#438]):
per account, it lists the `{base36}.{domain}` A-records and deletes any whose IP
has no live (`running`/`pending`) `spawn:managed` instance. This catches records
orphaned when an instance exits abruptly (hard crash, out-of-band terminate, fast
spot reclaim) and has since aged out of the EC2 API — the instance-driven #247
teardown can't see those. The sweep aborts without deleting anything if a region's
live-instance scan errors (never delete against a partial live set), and honors
`REAPER_DRY_RUN`.

The sweep also reports **unmanaged subdomains** ([#457]) — records under accounts
absent from `REAPER_ROLE_ARNS`, which the per-account sweep cannot see at all — and
annotates each with the portal registry's lifecycle verdict. With
`REAPER_DNS_EXPIRE=true` it can delete the ones the registry has proven dormant or
offboarded ([#466]). See [DNS expiry](#dns-expiry-466).

Every run classifies its own failures and alarms when it could not look at all
([#469]) — see [Failure observability](#failure-observability-469). A reaper that
silently reaches nothing is indistinguishable from a fleet with nothing expired, and
that was the state before #469.

## Multi-account coverage

A spore lands in **whatever account the caller's credentials point at** — spawn
has no fixed launch account. So the reaper must cover **every** account that
launches spores, not just one. It does this by assuming one cross-account role
per account (`REAPER_ROLE_ARNS`), and optionally scanning its own account
(`REAPER_SCAN_SELF`). Add a new account by deploying the cross-account role
there and appending its ARN to the list.

## Terminated-instance events ([#1287])

In a **nondefault VPC**, EC2 refuses to release an associated Elastic IP
(`InvalidIPAddress.InUse`). On a multi-interface EFA instance the launch Elastic
IP is the instance's only egress, so spored cannot disassociate and then release
it: it is left to the reaper. Without events that means the next sweep after a
15-minute grace, holding a slot of the region's Elastic IP quota (5 by default)
while a fan-out relaunches.

With events, EC2's own `EC2 Instance State-change Notification` (state
`terminated`) invokes the reaper for that one instance, which asks EC2 to confirm
the termination and releases the instance's launch Elastic IPs through the same
`pkg/elasticip` release and tag checks as everything else, with no grace. It
retries an address EC2 still reports in use a few times, then leaves it to the
sweep. An event never runs the full scan. The release follows
`REAPER_ELASTIC_IPS` and `REAPER_DRY_RUN` exactly as the sweep does (`report`
logs `WOULD release Elastic IP ...`). An event from an account or region the
reaper is not configured for is ignored.

| Deploy path | What carries the events | Regions |
|---|---|---|
| `spawn reaper deploy` (self-hosted) | a rule on the account's default bus, `spawn-ttl-reaper-selfscan-instance-terminated`, targeting the function | the deploy region only; others wait for the sweep |
| operated reaper (`make deploy INSTANCE_EVENTS=on`) | member rule `spawn-ttl-reaper-instance-terminated` in each launch region → the reaper account's bus `spawn-ttl-reaper-<env>-instance-events` → rule → function | every region in `MEMBER_REGIONS` (default: the reaper's `Regions`) |

The operated reaper's bus admits `events:PutEvents` only from the forwarder role
of each account in `RoleArns` (that ARN with `-events` appended), so it admits
exactly the accounts the reaper already assumes into. No wildcard and no
organization principal. Order matters, because the bus policy names the
forwarder roles:

```bash
# 1. In each member account: the role stack with InstanceEvents=on, which
#    creates the forwarder role spawn-ttl-reaper-ec2-events (off by default)
aws cloudformation deploy --region us-east-1 --stack-name spawn-ttl-reaper-ec2 \
  --template-file ../../deployment/cloudformation/ttl-reaper-cross-account-role.yaml \
  --capabilities CAPABILITY_NAMED_IAM --parameter-overrides InstanceEvents=on
# 2. In each member account: the rule, in every launch region
make member-events-deploy            # MEMBER_REGIONS=... to override
# 3. In the infra account: create the bus, its policy and rule
make deploy INSTANCE_EVENTS=on
```

`InstanceEvents` defaults to `off`, and a `make deploy` that does not name it
keeps the deployed value. To remove it all: `make deploy INSTANCE_EVENTS=off`
(or delete the stack), then `make member-events-teardown` in each member account,
then redeploy its `spawn-ttl-reaper-ec2` stack with `--parameter-overrides
InstanceEvents=off`, which deletes the forwarder role (an account that never
turned it on never had one). `aws cloudformation deploy` keeps a parameter it is
not given, so a later redeploy of the role stack leaves the setting as it is. `spawn reaper
teardown` removes the self-hosted rule. `spawn footprint` lists the buses and
the rules on them.

## Configuration (env vars)

| Var | Default | Meaning |
|-----|---------|---------|
| `REAPER_ROLE_ARNS` | dev role | Comma-separated cross-account role ARNs — **one per spore-launching account** |
| `REAPER_SCAN_SELF` | `false` | Also scan the Lambda's own account directly (no assume-role) |
| `EC2_ROLE_ARN` | (unset) | Back-compat: a single role ARN, folded into the list |
| `EC2_EXTERNAL_ID` | `spawn-ttl-reaper` | ExternalId for the assume-role |
| `REAPER_REGIONS` | 11 release-bucket regions | Comma-separated regions to scan |
| `REAPER_MAX_AGE` | `168h` (7d) | Hard max-age ceiling (Go duration, or days: `7d`) |
| `REAPER_TTL_GRACE` | `10m` | How long past its TTL deadline a running instance with a fresh `spawn:last-heartbeat` is left for spored to end itself ([#1186]). `0` to `1h`; `0` reaps at the deadline. Template parameter `TtlGrace` (default `''`, i.e. `10m`) / make variable `TTL_GRACE` / `spawn reaper deploy --ttl-grace`; a deploy that does not name it keeps the deployed value. The heartbeat is instance-writable, which is why the grace is capped; `REAPER_MAX_AGE` still applies |
| `REAPER_DRY_RUN` | `true` | When true, log `WOULD reap` + notify without terminating |
| `REAPER_NOTIFY_URL` | (empty) | Slack-incoming-webhook URL; every reap is posted here |
| `REAPER_DNS_ZONE_ID` | (empty) | Route53 hosted zone ID; with `REAPER_DNS_DOMAIN`, the reaper deletes a reaped instance's DNS records (#247) |
| `REAPER_DNS_DOMAIN` | (empty) | Domain for the zone above (e.g. `spore.host`); both empty = DNS teardown disabled |
| `REAPER_DNS_EXTRA_ZONE_IDS` | (empty) | Comma-separated further hosted zone IDs to cover (#1065): custom zones spores register in with `--dns-domain` (#891). Each zone's domain is read from Route53 at startup. Teardown deletes in the zone the instance's `spawn:dns-domain` names (else the primary); the sweep, the unmanaged-subdomain report and lease expiry walk every zone. Needs the two above. Template parameter `DnsExtraZoneIds` / make variable `DNS_EXTRA_ZONE_IDS` |
| `REAPER_DNS_SWEEP` | `false` | With a zone configured, also run a **DNS reconciliation sweep** (#438): delete `{base36}.{domain}` A-records whose IP has no live instance — catches records orphaned by abrupt exits the #247 teardown can't. Honors `REAPER_DRY_RUN`. Also emits the **unmanaged-subdomain** report (#457) |
| `REAPER_DNS_EXPIRE` | `false` | With the sweep on, **delete** the A-records under an unmanaged subdomain whose account the portal registry has proven `dormant` or `offboarded` ([#466]). Requires `REAPER_DNS_SWEEP` (the walk runs inside the sweep) and honors `REAPER_DRY_RUN`. Read the report before enabling — see [DNS expiry](#dns-expiry-466) |
| `REAPER_DNS_LEASE_EXPIRE` | `false` | With the sweep on, **delete** an A-record under an account subdomain this reaper does **not** cover when its owner has stopped renewing its DNS lease for longer than `pkg/dns.LeaseMaxAge` (6h), plus the lease itself, and any lease whose A-record is already gone. The only backstop for a record left by an abruptly terminated instance in an account the reaper holds no credentials in. Records with no lease (every spored before leases) and records in covered accounts are never touched. Requires `REAPER_DNS_SWEEP`, honors `REAPER_DRY_RUN`. See [DNS leases](#dns-leases) |
| `REAPER_NET_RESOURCES` | `report` | Reclaim spawn-managed **security groups** and **cluster placement groups** with no instance referencing them, older than **7 days** ([#685]). `report` logs what it *would* reclaim and deletes nothing; `reap` deletes; `off` skips the sweep entirely. Launch Elastic IPs are **not** governed by this; see `REAPER_ELASTIC_IPS`. Three values and **not** folded into `REAPER_DRY_RUN` on purpose — production runs with the dry run off, so a boolean would arm a new destructive sweep on upgrade and the only preview would be disarming the live instance reaper. A typo resolves to `report`; `REAPER_DRY_RUN` always wins |
| `REAPER_ELASTIC_IPS` | unset: `reap`, or `off` when `REAPER_NET_RESOURCES=off` | The orphaned launch **Elastic IP** sweep ([#661], [#1232]), independent of `REAPER_NET_RESOURCES`. Template parameter `ElasticIps` (default `''`, i.e. unset) / make variable `ELASTIC_IPS` / `spawn reaper deploy --elastic-ips`. Unset keeps the behaviour from before the variable existed, so a redeploy that does not name it changes nothing. Selects addresses tagged `spawn:managed=true`, `spawn:purpose=efa-public-ip` and `spawn:instance-id` (a multi-interface EFA launch allocates one), and releases one only when **all** hold: it is unassociated; its instance is terminated or absent from `DescribeInstances`; and its `spawn:created` stamp is at least **15 minutes** old, or `DescribeInstances` lists its instance as `terminated` ([#1287]: the grace exists for an instance not yet visible, which a terminated one is not). EC2 reports no allocation time for an address; one without a usable stamp is stamped `spawn:reaper-first-seen` on first sight and aged from that. Such an address bills by the hour and counts against the per-region Elastic IP quota, and a Spot reclaim leaves one behind every time. `reap` releases them, and also releases the address of an instance the reaper has just terminated; `report` logs `WOULD release Elastic IP <alloc> (<ip>) ...` for exactly the addresses `reap` would release and changes nothing; `off` skips both. Counted in the run summary as `elastic_ips_scanned`, `elastic_ips_released`, `elastic_ips_would_release` and `elastic_ips_skipped`. A typo resolves to `report`; `REAPER_DRY_RUN` always wins |
| `REAPER_SPOT_REQUESTS` | `report` | The orphaned persistent **Spot request** sweep ([#950]). Cancels spawn-tagged persistent requests whose instance is terminated or gone, has been replaced (EC2 launched a new instance after the original was terminated outside spawn) or has expired, and terminates a replacement after cancelling its request. A request whose original instance is alive is left to the instance scan: the request's own `spawn:ttl-deadline` goes stale after `spawn extend`, and cancelling would strand a stopped instance. An untagged replacement is outside the tag-scoped terminate grant. It is reported as `SPOT REPLACEMENT NOT TERMINATED` (alarmed, and posted to `REAPER_NOTIFY_URL` once) with the account, a `describe-instances` check that the instance is the request's, and the `terminate-instances` command. No IAM condition key ties an instance to its Spot request, so this needs a person ([#984]). `report` / `reap` / `off` as for `REAPER_NET_RESOURCES`, and `REAPER_DRY_RUN` always wins |
| `ACCOUNTS_TABLE` | `spore-portal-accounts` | The portal registry supplying the expiry verdict. Read-only: the reaper only `Scan`s it, and the prober owns every write |

If neither `REAPER_ROLE_ARNS`/`EC2_ROLE_ARN` nor `REAPER_SCAN_SELF=true` is set,
the reaper falls back to scanning its own account (never a silent no-op).

## Deploy

### 1. Cross-account role in EACH spore-launching account

Deploy the role template in every account where spawn launches spores (e.g.
spore-host-dev 435415984226, plus any others):

```bash
aws cloudformation deploy \
  --template-file ../../deployment/cloudformation/ttl-reaper-cross-account-role.yaml \
  --stack-name spawn-ttl-reaper-ec2 \
  --region us-east-1 \
  --capabilities CAPABILITY_NAMED_IAM
```

No `--parameter-overrides` is needed for a spore.host deployment: `ReaperAccountId`
defaults to the infra account (966362334030) and `ReaperRoleNamePattern` to
`spawn-ttl-reaper-*`. For a self-hosted reaper, pass your own infra account:

```bash
  --parameter-overrides ReaperAccountId=<your-infra-account-id>
```

This step used to be documented as `--parameter-overrides
ReaperLambdaRoleArn=<reaper-lambda-role-arn>`, and **that parameter does not
exist** — the template declares `ReaperAccountId`, `ReaperRoleNamePattern`,
`RoleName` and `TagPrefix`, so CloudFormation rejects the documented command
outright. The instruction predated #476, which deliberately replaced the exact
role ARN with a name PATTERN: the reaper's role is generated with a random suffix,
so a trust naming it exactly breaks the moment that stack is recreated, for every
onboarded account at once, with the failure mode "expired instances are no longer
terminated".

**There is no longer a chicken-and-egg ordering.** The old text said to deploy the
Lambda first, read its `FunctionRoleArn`, then deploy the roles — which followed
from naming the ARN. With a pattern the trust does not reference a principal that
has to exist yet, so this template can be deployed first, or in either order.

### 2. Reaper Lambda in the infra account (spore-host-infra, 966362334030)

Pass all per-account role ARNs as a comma-separated `RoleArns`:

```bash
# Start in dry-run; flip to enforce after verifying.
make deploy DRY_RUN=true NOTIFY_URL=https://hooks.slack.com/services/... \
  ROLE_ARNS='arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2,arn:aws:iam::<other-acct>:role/spawn-ttl-reaper-ec2'
# After verification:
make deploy DRY_RUN=false NOTIFY_URL=https://hooks.slack.com/services/... ROLE_ARNS='...'
```

**You only need to pass what you want to change.** `make deploy` reads the live
stack's parameters first and inherits anything you do not specify, so redeploying
to pick up new code is just `make deploy` — it reports `deploy changes no
parameters (code only)` and leaves every setting as-is.

That was not always true. Until spawn#650 the Makefile asserted all of its own
`?=` defaults on every run, so a plain `make deploy` against the armed production
stack would have set `DryRun=true` — **disarming the reaper**, silently, since it
keeps running and reporting and merely stops terminating anything. It would also
have turned `DnsSweep` off, blanked the DNS zone/domain, and detached
`AlarmTopicArn` so every failure alarm fired into nothing. The examples below pass
things explicitly, which is still correct and still the way to *change* a setting;
they are no longer the only way to *keep* one.

DNS teardown (#247) and the reconciliation sweep (#438) are off unless their
parameters are passed — the sweep is opt-in on top of a configured zone, so both
DNS knobs are needed:

```bash
make deploy DRY_RUN=false ROLE_ARNS='...' \
  DNS_ZONE_ID=Z0341053304H0DQXF6U4X DNS_DOMAIN=spore.host DNS_SWEEP=true
```

To also cover custom zones (#1065), add `DNS_EXTRA_ZONE_IDS=<zone-id>[,<zone-id>…]`.

`ROLE_ARNS` must list **every** account that has records under the zone. The sweep
is per-account (it derives each `{base36}` subdomain from the account ID), so an
account absent from `ROLE_ARNS` is never reconciled and its orphaned records
persist indefinitely — exactly the leak #438 set out to close.

Because that absence used to be silent, the sweep also emits an
**unmanaged-subdomain report** ([#457]): it walks the zone once per run, decodes
each `{base36}` label back to an account ID, and logs any subdomain whose account
is not in `ROLE_ARNS`. Left alone, the hazard is that a released public IP returns
to the EC2 pool, so an abandoned A-record eventually resolves to an unrelated
instance. Counted in the summary as `DNSUnmanagedSubdomains` /
`DNSUnmanagedRecords`.

Each line carries the **portal registry's verdict** on that account ([#466]):

```
UNMANAGED subdomain 4zlw3a1t.spore.host (account 390967728545) — 1 record(s);
  registry status "active" — not eligible for expiry (#457)
```

## DNS expiry (#466)

The report above originally deleted nothing, and said why: an unmanaged subdomain
is ambiguous — an account that uninstalled and left records, or a live one someone
forgot to add to `ROLE_ARNS` — and the two are indistinguishable *precisely
because we lack the credentials*, since `DescribeInstances` is how emptiness is
proven.

The portal's [`account-prober`](https://github.com/spore-host/spore-host/tree/main/lambda/portal-account-prober)
holds credentials the registry knows about (`spore-portal-onboard`, a different
role from the reaper's `spawn-ttl-reaper-ec2`) and does prove it across every
region, writing the result to `spore-portal-accounts`. So for an account **in the
registry** the ambiguity is resolvable, and `accountlifecycle.DNSExpiryEligible`
is the verdict:

| Registry status | Eligible | Why |
|---|---|---|
| `dormant` | **yes** | reachable *and* provably empty for N days — emptiness established through a working role |
| `offboarded` | **yes** | a human stated the intent |
| `active` | no | the account is in use |
| `unreachable` | no | #457 trap 2: the role we would verify through is gone, so emptiness can no longer be proven. Needs a human, not a longer wait |
| absent / unreadable | no | the prober has no opinion, so the original ambiguity stands in full |

The verdict is **always reported**. Acting on it needs `REAPER_DNS_EXPIRE=true` on
top of the sweep's own opt-in:

```bash
make deploy DRY_RUN=false ROLE_ARNS='...' \
  DNS_ZONE_ID=Z0341053304H0DQXF6U4X DNS_DOMAIN=spore.host DNS_SWEEP=true \
  DNS_EXPIRE=true ACCOUNTS_TABLE_KEY_ARN=arn:aws:kms:us-east-1:966362334030:key/...
```

Two switches rather than one because `DNS_SWEEP` is already on in production:
folding expiry into it would make an existing flag destructive on upgrade for a
class of records it has never touched. And the cost of being wrong is asymmetric —
`spored` registers DNS **once, at boot** (`pkg/agent/agent.go`), with no periodic
re-registration, so a wrongly deleted A-record **never self-heals**. The spore
keeps running and is simply unreachable by name until it reboots.

What expiry deletes: only the `A`-records under that subdomain, never the [#121]
friendly CNAMEs (they alias the A-record and carry no IP — a dangling CNAME
resolves to nothing, whereas a stale A-record resolves to a stranger's box).

```
EXPIRING subdomain 4zlw3a1t.spore.host (account 390967728545) — 2 record(s);
  registry status "dormant" — ELIGIBLE for expiry (since …, last probed 1h0m0s ago) (#466)
DNS expiry: deleted A-record box1.4zlw3a1t.spore.host -> 54.1.2.3 (account eligible for expiry)
```

Summary fields: `DNSExpiryEligible` / `DNSExpiryIneligible` (verdicts) and
`DNSExpiredRecords` (records deleted, or would-delete under dry-run).
**Eligible-but-not-Expired is the normal, healthy reading** — it means the verdict
exists and the flag is off.

The reaper holds `dynamodb:Scan` on the registry and nothing else — no
`UpdateItem`, no `PutItem`. That the reaper *cannot write* the table is what makes
it impossible for a reaper bug to manufacture the eligibility it then acts on. If
the `Scan` fails, or the account has no row, expiry refuses and says so
(`no registry verdicts available … nothing was deleted`).


## DNS leases

The DNS sweep and DNS expiry both work by asking about the instance behind a record,
and both need credentials in the account that owns it. For an account the reaper does
not cover, neither can run, and a record left by an instance that was terminated
abruptly — console, spot reclaim, crash, anything that skips spored's cleanup — keeps
pointing at its IP after AWS hands that IP to someone else.

A **lease** lets the zone answer instead. A spored that supports it re-registers about
hourly, and the DNS updater stamps each registration's time into a TXT record,
`_spore-lease.<record>`, written atomically with the A-record and only under the
SigV4-verified caller's own namespace. A record whose lease has gone unrenewed for six
hours has been abandoned by its owner.

What the reaper does with that, in order of how much it matters:

- **Reports always.** `dns_leases_stale` is what arming would expire; `dns_leases_orphaned`
  is leases whose A-record is already gone.
- **Acts only with `REAPER_DNS_LEASE_EXPIRE=true`**, which arrives off.
- **Only for accounts it does not cover.** Where the reaper has credentials, the
  `DescribeInstances` sweep is authoritative, and a stale lease there most likely means a
  hung spored on a live instance. `dns_leases_covered` counts those, untouched.
- **Never a record without a lease.** Every spored released before leases registers
  once and never renews; the updater writes no lease for it, and clears an inherited one
  if a non-renewing client re-registers a name, so such records behave exactly as before.
- **Never an unparseable lease.** It is reported and left.
- **Re-reads the lease immediately before deleting**, so a renewal that lands mid-run wins.
- **A-records and the lease only**, never the alias CNAMEs — a dangling CNAME resolves to
  nothing, a dangling A-record resolves to a stranger.

A wrongly expired record self-heals: its spored re-registers within the hour.

## Failure observability ([#469])

The reaper's value is that it still reaps when `spored` is dead. That guarantee is
only as good as our ability to notice when **the reaper** is the thing that is dead
— and until #469 it could not be noticed at all:

- `handler` always returns `nil`, so the Lambda `Errors` metric never moved no
  matter how many scans failed.
- `Summary` went to one log line and was returned to EventBridge. Nothing read it.
- The Slack webhook fires **only on a successful reap**. No failure path notified.
- There were no metric filters and no alarms on this function.

Every field in `Summary` except `Errors` counts something that *happened*. `Errors`
alone says a scan did **not** happen — and it was pinned permanently nonzero by one
uninstalled account (11 regions × 6 runs/hour = 66/hour, forever), so it could not
distinguish *"nothing needed reaping"* from *"the reaper could not look."* That is
the same blindness #65 created the reaper to prevent, one layer up.

### What changed

Failures are now classified (`failures.go`) and aggregated **per account**, because
credentials are per account — the same role either works in every region or is
refused in every region, so eleven identical `AccessDenied`s are **one**
observation:

| Field | Counts |
|---|---|
| `Errors` | **operational** failures only — throttles, timeouts, outages. Things that mean *investigate this* and usually clear on their own |
| `AccountsDenied` | accounts whose role refused us in **every** region — one per account, not per region |
| `FSxAccountsDenied` | accounts that refused **every** FSx call — a separate grant, so this fires even when the instance scan succeeded ([#212]) |

A denial contributes **0** to `Errors`. It is not discarded — it gets its own field
and its own alarm, so it becomes *louder*, not quieter.

An account denied everywhere is **still attempted every run**. The reaper's role ARN
embeds a CloudFormation-generated physical ID, so recreating the stack changes the
suffix and breaks *every* customer's trust policy at once ([#457]) — which looks
identical, from the inside, to the whole customer base uninstalling simultaneously.
Quiescing means *not counting it as a surprise*, never *stopping the attempt*.

### Sentinel log lines and alarms

Three fixed strings exist **so they can be alarmed on**. Their spelling is a
contract with the metric filters in `template.yaml`; `TestSentinelSpellingsAreStable`
turns a rename into a build failure rather than a silently disarmed alarm.

| Sentinel | Alarm | Window | Means |
|---|---|---|---|
| `REAPER REACHED NO ACCOUNTS` | `…-reached-no-accounts` | 2×1h | **The one that matters.** Zero accounts reached: nothing was reaped, and over-deadline instances may be running with nothing left to stop them. Investigate **our** side first — execution role, the [#457] role-ARN suffix, `EC2_EXTERNAL_ID` |
| `REAPER ACCOUNT UNREACHABLE` | `…-account-unreachable` | 1×24h | One account's role refuses us everywhere. Chronic, not acute — the rest of the fleet is fine. Fix the role or remove it from `ROLE_ARNS` |
| `REAPER FSX UNREACHABLE` | `…-fsx-unreachable` | 1×24h | An account refuses every FSx call, so orphaned filesystems accrue cost unreclaimed. Usually its instance scan works fine — that combination is [#212] |
| `SPORED NOT CHECKING IN` | `…-spored-quiet` | 3×1h | A **running** instance stopped refreshing `spawn:last-heartbeat`, so `spored` started and then died — its in-instance TTL, idle and cost limits are no longer being enforced and this reaper is the only thing left that will stop it. The one sentinel here about the *agent* rather than the reaper's own reach. Never a reap trigger: a wedged agent on a healthy workload is a diagnosis, not a deadline ([#682]) |
| *(none — `AWS/Lambda` `Errors`)* | `…-invocation-errors` | 1×1h | The run died outright (panic, 900s timeout, OOM, bad deploy) and so emitted **none** of the sentinels above. Without this, the loudest failure would be the quietest signal |
| *(none — `AWS/Lambda` `Invocations`)* | `…-not-invoked` | 1×30m, `TreatMissingData: breaching` | The reaper wasn't invoked **at all** — not "ran and failed," but never started (disabled EventBridge rule, deleted schedule, concurrency set to 0, or the function deleted). The four alarms above all use `TreatMissingData: notBreaching`, correctly, but that means "not running" produces zero breaching datapoints anywhere else in this table — this is the one alarm here where absence of data IS the failure ([#475]) |

Deploy parameters: `ALARMS_ENABLED` (default **`true`** — unlike the other flags,
which change what the reaper *does*; this only changes what it *reports*) and
`ALARM_TOPIC_ARN` (optional SNS topic).

```bash
make deploy ALARM_TOPIC_ARN=arn:aws:sns:us-east-1:966362334030:spore-host-alerts ...
```

An alarm with no topic is visible in the console but pages no one, which only helps
someone who already suspected a problem and went looking — the state this change
exists to fix.

When the portal registry is configured, a denied-account line carries the registry's
status as **corroboration only**. It describes a *different role*
(`spore-portal-onboard`, which trusts phone-home) than the one that just refused us
(`spawn-ttl-reaper-ec2`, which trusts this Lambda); either can be healthy while the
other is broken. It is a second opinion about a different door, never a gate. The
registry is read at most once per run, and not at all when no account was denied.

Deliberately **not** done: no consecutive-failure counter (a stateless Lambda has
nowhere to keep "K consecutive runs"), and `handler` still returns `nil` — making it
error would mark runs failed that did real work and would change EventBridge retry
behaviour. The explicit alarms are the honest mechanism.

## Verify

```bash
# Invoke on demand (instead of waiting for the schedule):
aws lambda invoke --function-name spawn-ttl-reaper-production /dev/stdout
# Watch logs:
make logs
```

Dry-run logs `WOULD reap i-… — ttl-deadline (age …)`; enforce logs `REAPED i-…`.

The init line reports which features are actually live — check `dns-sweep=true`
there before trusting that #438 is running, and `dns-expire=true` for #466:

```
ttl-reaper initialized (accounts=[…], …, dry-run=false, …, dns-sweep=true, dns-expire=false)
```

`REAPER_DNS_EXPIRE=true` with the sweep off logs `expiry disabled (it runs inside
the sweep)` and reports `dns-expire=false` — the walk expiry acts on happens inside
the sweep, so it would otherwise look enabled while reaching no candidate.

A sweep that finds an orphan logs `DNS sweep: deleted orphaned A-record
name.{base36}.spore.host -> 1.2.3.4 (no live instance)` (`WOULD delete` in
dry-run) and counts it in the summary's `DNSScanned`/`DNSReaped`. A sweep whose
live-instance scan errored logs `aborting sweep for this account` and deletes
nothing — by design, since a partial live set could orphan a healthy record.

[#121]: https://github.com/spore-host/spawn/issues/121
[#212]: https://github.com/spore-host/spawn/issues/212
[#457]: https://github.com/spore-host/spawn/issues/457
[#466]: https://github.com/spore-host/spawn/issues/466
[#469]: https://github.com/spore-host/spawn/issues/469
[#475]: https://github.com/spore-host/spawn/issues/475
[#682]: https://github.com/spore-host/spawn/issues/682
[#685]: https://github.com/spore-host/spawn/issues/685
[#661]: https://github.com/spore-host/spawn/issues/661
[#950]: https://github.com/spore-host/spawn/issues/950
[#1232]: https://github.com/spore-host/spawn/issues/1232
[#1186]: https://github.com/spore-host/spawn/issues/1186
[#1287]: https://github.com/spore-host/spawn/issues/1287
[#984]: https://github.com/spore-host/spawn/issues/984
[#65]: https://github.com/spore-host/spawn/issues/65
[#71]: https://github.com/spore-host/spawn/issues/71
[#76]: https://github.com/spore-host/spawn/issues/76
[#990]: https://github.com/spore-host/spawn/issues/990
[#187]: https://github.com/spore-host/spawn/issues/187
[#247]: https://github.com/spore-host/spawn/issues/247
[#438]: https://github.com/spore-host/spawn/issues/438
