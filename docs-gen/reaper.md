## `spawn reaper`

Run spawn's TTL reaper inside your own AWS account.

The reaper is the backstop for what spored cannot do. spored enforces TTL, idle and
cost from INSIDE each instance — so it cannot act on an instance that is stopped, it
cannot delete a filesystem that outlives its instance, and it enforces nothing at all
if it dies. "Everything dies eventually" holds in two layers, and this is the second.

spawn's own deployment of the reaper lives in the spore.host infra account and reaches
into each launching account by assuming a role there. That requires your account to
trust an external principal, which many organizations forbid outright — so for those
accounts the reaper was simply unavailable, and nothing reclaimed a stopped instance
past its TTL or an orphaned filesystem.

These commands deploy the same reaper in your account, scanning only your account. It
assumes nothing and trusts nothing but Lambda.

It deploys UNARMED (dry-run): the schedule runs and logs what it WOULD reclaim, and
touches nothing, until you run 'spawn reaper arm'. Read a cycle of those logs first —
the reaper terminates instances, and that is not reversible.

  spawn doctor                 # does anything cover this account today?
  spawn reaper deploy          # install it, unarmed
  spawn reaper status          # deployed? armed? on what schedule? covered anyway?
  spawn reaper arm             # start actually reclaiming
  spawn reaper teardown        # remove it

Versioning: the CLI and a deployed reaper are INDEPENDENT. Upgrading spawn does
not touch a reaper already in an account, and 'spawn reaper deploy' installs the
artifact for whichever version it is asked for. So a deployed reaper can be older
than the CLI talking to it — 'spawn reaper status' prints both and says so when
they differ. Nothing breaks when they do; an older reaper still reaps. Bring them
together by re-running 'spawn reaper deploy' (spawn#654).

```
spawn reaper
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--region` |  | string |  | AWS region to operate in (default: resolved region) |

### `spawn reaper arm`

Let the deployed reaper actually terminate expired instances

```
spawn reaper arm [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--yes` |  | bool |  | Skip the confirmation prompt |

### `spawn reaper deploy`

Create the reaper's execution role, upload its Lambda artifact to a bucket in
this account, create its log group with a retention (30 days by default), create
the function, and put it on a schedule.

Deploys with dry-run ON. Nothing is reclaimed until 'spawn reaper arm'.

The artifact comes from the spawn GitHub Release matching --version (default: this
binary's version). --artifact overrides that with a local path or an explicit URL, for
mirrored or air-gapped environments.

Re-running it upgrades an existing reaper in place and keeps its settings: the
armed state, the scanned regions and every other REAPER_* variable stay as they
are unless a flag here sets them. --spot-requests, --net-resources, --max-age and
--notify-url set the matching variable; pass an empty value ("") to remove one
and return the reaper to its default. The notify URL is never printed; pass "-"
to read it from stdin instead of the command line.

```
spawn reaper deploy [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--artifact` |  | string |  | Use this Lambda zip instead of downloading a release asset (local path or URL) |
| `--bucket` |  | string |  | S3 bucket in THIS account to hold the artifact (default: spawn-reaper-artifacts-&lt;account&gt;-&lt;region&gt;) |
| `--elastic-ips` |  | string |  | Orphaned launch Elastic IP sweep, REAPER_ELASTIC_IPS: off\|report\|reap (default: keep; the reaper's default is reap, or off when REAPER_NET_RESOURCES=off) |
| `--log-retention-days` |  | int32 |  | Days CloudWatch keeps the reaper's logs, a value CloudWatch accepts: 1, 3, 5, 7, 14, 30, 60, 90, ... (default: keep the existing reaper's; 30 for a new one). Below 30, 'teardown --if-idle-for' can see at most that much history |
| `--max-age` |  | string |  | Max-age ceiling, REAPER_MAX_AGE, e.g. 336h or 14d (default: keep; the reaper's default is 7d) |
| `--net-resources` |  | string |  | Network-resource sweep, REAPER_NET_RESOURCES: off\|report\|reap (default: keep; the reaper's default is report) |
| `--notify-url` |  | string |  | URL the reaper POSTs notifications to, REAPER_NOTIFY_URL; never printed. "-" reads it from stdin (default: keep) |
| `--regions` |  | string |  | Comma-separated regions for the reaper to scan (default: keep the existing reaper's; the deploy region for a new one) |
| `--schedule` |  | string |  | EventBridge schedule expression (default: keep the existing reaper's; rate(10 minutes) for a new one) |
| `--spot-requests` |  | string |  | Orphaned Spot-request sweep, REAPER_SPOT_REQUESTS: off\|report\|reap (default: keep; the reaper's default is report) |
| `--version` |  | string |  | spawn release to take the reaper artifact from (default: this binary's version) |

### `spawn reaper status`

Report whether the reaper is deployed here, and whether it is armed

```
spawn reaper status
```

### `spawn reaper teardown`

Remove the reaper from this account: the schedule, the Lambda, its role, its
CloudWatch log group, and the S3 bucket holding its artifact. --keep-logs leaves the
log group, which is the only record of what the reaper did.

The bucket used to be left behind deliberately (#653). Under "leave no trace"
that is wrong — an idle reaper that tidied everything except a bucket has left a
trace while reporting that it has not.

The original concern is answered rather than overruled. The bucket is removed
only when it is tagged spawn:managed=true AND contains nothing outside
ttl-reaper/; a bucket that fails either check is reported and kept, with
--force-artifacts to override. So the liberty is only taken over a bucket that is
provably spawn's and holds only spawn's artifacts.

The bucket is the one deploy used. Deploy records it on the function
(spawn:artifact-bucket); --bucket overrides that, and is needed for a reaper
deployed with --bucket by a spawn older than that record.

```
spawn reaper teardown [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--bucket` |  | string |  | Artifact bucket to remove (default: the one recorded on the function at deploy time, else spawn-reaper-artifacts-&lt;account&gt;-&lt;region&gt;) |
| `--force-artifacts` |  | bool |  | Remove the artifact bucket even if it is not tagged spawn:managed=true (for buckets created before the tag existed) |
| `--if-idle-for` |  | duration | `0s` | Only tear down if the reaper has reclaimed nothing for at least this long, at most 720h (the logs it reads); otherwise report and exit 0 |
| `--keep-artifacts` |  | bool |  | Leave the artifact bucket in place (it is removed by default) |
| `--keep-logs` |  | bool |  | Leave the reaper's CloudWatch log group in place (it is removed by default) |

