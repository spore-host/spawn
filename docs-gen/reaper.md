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
  spawn reaper status          # deployed? armed? on what schedule?
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
this account, create the function, and put it on a schedule.

Deploys with dry-run ON. Nothing is reclaimed until 'spawn reaper arm'.

The artifact comes from the spawn GitHub Release matching --version (default: this
binary's version). --artifact overrides that with a local path or an explicit URL, for
mirrored or air-gapped environments.

```
spawn reaper deploy [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--artifact` |  | string |  | Use this Lambda zip instead of downloading a release asset (local path or URL) |
| `--bucket` |  | string |  | S3 bucket in THIS account to hold the artifact (default: spawn-reaper-artifacts-&lt;account&gt;-&lt;region&gt;) |
| `--regions` |  | string |  | Comma-separated regions for the reaper to scan (default: the deploy region) |
| `--schedule` |  | string | `rate(10 minutes)` | EventBridge schedule expression |
| `--version` |  | string |  | spawn release to take the reaper artifact from (default: this binary's version) |

### `spawn reaper status`

Report whether the reaper is deployed here, and whether it is armed

```
spawn reaper status
```

### `spawn reaper teardown`

Remove the reaper from this account

```
spawn reaper teardown
```

