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
policy). The reaper only catches what `spored` missed. A reaper-kill is an
external `TerminateInstances` — it does **not** run the in-instance pre-stop hook.

## What it does

Every schedule tick (`rate(10 minutes)` by default) it scans the configured
regions for instances tagged `spawn:managed=true` (running **and** stopped — an
idle-stopped instance runs no daemon, so only the reaper will ever reclaim it,
[#71]) and terminates any where **either**:

1. `now > spawn:ttl-deadline` (the authoritative, launch-anchored deadline), or
2. `now - spawn:launch-time > REAPER_MAX_AGE` — a hard ceiling that fires even
   for `--no-timeout` / missing / unparseable deadlines.

Within-deadline instances are always spared (the deadline is honored intent).

## Multi-account coverage

A spore lands in **whatever account the caller's credentials point at** — spawn
has no fixed launch account. So the reaper must cover **every** account that
launches spores, not just one. It does this by assuming one cross-account role
per account (`REAPER_ROLE_ARNS`), and optionally scanning its own account
(`REAPER_SCAN_SELF`). Add a new account by deploying the cross-account role
there and appending its ARN to the list.

## Configuration (env vars)

| Var | Default | Meaning |
|-----|---------|---------|
| `REAPER_ROLE_ARNS` | dev role | Comma-separated cross-account role ARNs — **one per spore-launching account** |
| `REAPER_SCAN_SELF` | `false` | Also scan the Lambda's own account directly (no assume-role) |
| `EC2_ROLE_ARN` | (unset) | Back-compat: a single role ARN, folded into the list |
| `EC2_EXTERNAL_ID` | `spawn-ttl-reaper` | ExternalId for the assume-role |
| `REAPER_REGIONS` | 11 release-bucket regions | Comma-separated regions to scan |
| `REAPER_MAX_AGE` | `168h` (7d) | Hard max-age ceiling (Go duration) |
| `REAPER_DRY_RUN` | `true` | When true, log `WOULD reap` + notify without terminating |
| `REAPER_NOTIFY_URL` | (empty) | Slack-incoming-webhook URL; every reap is posted here |
| `REAPER_DNS_ZONE_ID` | (empty) | Route53 hosted zone ID; with `REAPER_DNS_DOMAIN`, the reaper deletes a reaped instance's DNS records (#247) |
| `REAPER_DNS_DOMAIN` | (empty) | Domain for the zone above (e.g. `spore.host`); both empty = DNS teardown disabled |

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
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides ReaperLambdaRoleArn=<reaper-lambda-role-arn>
```

The reaper Lambda role ARN is the `FunctionRoleArn` output of the Lambda stack
(step 2). First-deploy is chicken-and-egg: deploy the Lambda once (it can't
assume until the roles exist), read its role ARN, deploy the roles, then the
next scheduled run works.

### 2. Reaper Lambda in the infra account (spore-host-infra, 966362334030)

Pass all per-account role ARNs as a comma-separated `RoleArns`:

```bash
# Start in dry-run; flip to enforce after verifying.
make deploy DRY_RUN=true NOTIFY_URL=https://hooks.slack.com/services/... \
  ROLE_ARNS='arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2,arn:aws:iam::<other-acct>:role/spawn-ttl-reaper-ec2'
# After verification:
make deploy DRY_RUN=false NOTIFY_URL=https://hooks.slack.com/services/... ROLE_ARNS='...'
```

## Verify

```bash
# Invoke on demand (instead of waiting for the schedule):
aws lambda invoke --function-name spawn-ttl-reaper-production /dev/stdout
# Watch logs:
make logs
```

Dry-run logs `WOULD reap i-… — ttl-deadline (age …)`; enforce logs `REAPED i-…`.

[#65]: https://github.com/spore-host/spawn/issues/65
[#71]: https://github.com/spore-host/spawn/issues/71
