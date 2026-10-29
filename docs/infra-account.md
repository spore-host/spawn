# The spore.host infra account

What is deployed in the shared infra account (**966362334030**, `us-east-1`),
what each piece does, and where its source lives.

**Measured, not recalled.** Every figure here came from the live account on
**2026-10-11** via `spawn footprint` (#771) and the AWS API. Re-measure rather
than trusting this page:

```sh
AWS_PROFILE=spore-host-infra spawn footprint --region us-east-1
AWS_PROFILE=spore-host-infra spawn footprint --region us-east-1 -o json   # machine-readable
AWS_PROFILE=spore-host-infra make lambda-versions-deployed                # deployed versions
```

This page exists because the answer to "what is in there?" was previously
reconstructed from memory each time, and three separate attempts at it were each
incomplete — the inventory in #653, a prefix-filtered query that found 10 of 14
log groups, and a bucket list that missed an 11-bucket family.

## Scale

**90 control-plane resources**, none of which the TTL reaper or `spawn cleanup`
touches — those cover launched instances and their attachments, not the control
plane (#653).

| | count |
|---|---|
| Lambda functions | 15 |
| CloudWatch log groups | 15 |
| DynamoDB tables | 21 |
| EventBridge rules | 2 |
| S3 buckets | 21 |
| IAM roles | 14 |
| IAM instance profiles | 2 |

Down from 103 on 2026-10-09, in two steps: #794 removed `spawn-alert-evaluator`
with its rule and log group (103 → 100), and #772 removed both autoscale
orchestrator stacks — two functions, two roles, two rules, two log groups and the
two empty groups tables (100 → 90).

Every figure re-measured with `spawn footprint -o json`, never decremented by
hand. An earlier attempt at this edit did the arithmetic manually, said 101, and
was wrong because it forgot a rule — which is why the page insists on measuring.

## Lambda functions

Grouped by how they are invoked, because that is the thing least obvious from the
code and it determines what breaks when a schedule is disabled.

### Event-driven (EventBridge schedule)

| function | source | schedule | notes |
|---|---|---|---|
| `spawn-ttl-reaper-production` | `lambda/ttl-reaper/` | `rate(10 minutes)` | the out-of-band backstop that terminates expired instances. Cross-account: `REAPER_SCAN_SELF=false`, assumes `spawn-ttl-reaper-ec2` in 435415984226 |
| `spawn-cost-history-collector` | `lambda/cost-history-collector/` | `rate(1 hour)` | writes to `spawn-cost-history` |

### API Gateway-fronted

| function | source | notes |
|---|---|---|
| `spawn-dashboard-api` | `lambda/dashboard-api/` | the dashboard's REST backend |
| `spawn-dashboard-websocket` | `lambda/dashboard-websocket/` | WebSocket connect/disconnect |

`spore-bot` used to be listed here. It is not API Gateway-fronted — Slack, Teams,
Discord and spored's `/notify` reach it through a Lambda Function URL — and its
source is not in this repo; see [spore-bot](#spore-bot) below.

### Invoked directly by the CLI

No resource policy, because no AWS service calls them — the user's own
credentials do.

| function | source | caller |
|---|---|---|
| `spawn-sweep-orchestrator` | `lambda/sweep-orchestrator/` | `spawn launch --param-file` (detached sweeps, the default) |
| `spawn-pipeline-orchestrator` | `lambda/pipeline-orchestrator/` | `cmd/pipeline.go` |
| `scheduler-handler` | `lambda/scheduler-handler/` | `cmd/schedule.go`. **No `spawn-` prefix** — it defaults to `SPAWN_LAMBDA_NAME:-scheduler-handler`, which is why name-based enumeration misses it |
| `spawn-dashboard-websocket-processor` | `lambda/dashboard-websocket-processor/` | fed by the WebSocket path |

### Deployed from other repos

Present in this account, source elsewhere. Listed so they are not mistaken for
orphans:

| function | belongs to |
|---|---|
| `lagotto-capacity-poller` | lagotto |
| `portal-phone-home` | spore-host (umbrella) — account registry, see #457 |
| `spore-rest-api` | spore-host (umbrella) |
| `spawn-ts-portal` | spawn-ts. **The only non-Go runtime: `nodejs20.x`** |
| `prism-bot` | prism |
| `spawn-dns-updater` | `lambda/dns-updater/` here, deployed by `scripts/deploy-custom-dns.sh` |
| `spore-bot` | spore-host (umbrella) — see [spore-bot](#spore-bot) |

### spore-bot

**Per the spore-host repo, not measured from the account** (read 2026-10-15 at
spore-host `a80e751`, spore-host#573; spawn#1043). The next `spawn footprint` run
should confirm the function, its role and its tables, and replace this note with
the measured facts.

- **Code** is in spore-host's `lambda/spore-bot` and ships with `make deploy`
  there: it asserts the account, builds the arm64 zip, uploads it to
  `s3://spawn-binaries-us-east-1/spore-bot/function.zip`, runs
  `update-function-code`, waits, and fails unless the live `CodeSha256` matches
  the zip it built.
- **Shape** — the `spore-bot-role` execution role, runtime (`provided.al2023`),
  arch (`arm64`), memory, timeout and the public Function URL — is OpenTofu at
  spore-host's `infra/tofu/spore-bot`. Tofu ignores code and environment.
- **Environment variables** (the bot's secrets) are set out-of-band with
  `aws lambda update-function-configuration`; neither of the above touches them.
- **There is no CloudFormation stack.** The `spore-bot` stack was deleted on
  2026-06-14 when Tofu adopted the function; creating one again would fight Tofu
  for ownership.

`deployment/cloudformation/spore-bot.yaml` in this repo is **only** the
first-time setup for someone self-hosting the bot. It does not deploy, and must
not be deployed into, this account. It was audited against the bot's code in
spawn#1043; nothing gates the two together, because the code lives in another
repo (spore-host#576 proposes a check there).

Two leads from that audit are open and unmeasured: per spore-host's Tofu, the
bot's role grants no access to `spore-sms-pending`, which its SMS notifications
write (spore-host#576); and `spawn bot register` creates cross-account roles
trusting the prism-bot function role rather than `spore-bot-role` (#1046).

### Code with no deployment

**None, as of 2026-10-10.** `lambda/alert-handler/` used to be here — built and
vetted in CI, run by nothing. #783 resolved it by removal rather than
deployment, because nothing emitted the event it handled (deploying it would
have changed nothing) and `spawn-alerts` had never held a record.

Keeping this section with a negative answer on purpose: "every Lambda in the
repo is deployed somewhere" is a property worth being able to check, and
`TestEveryHardcodedLambdaNameIsCreatedByADeployMechanism` now enforces it with
an exception list of zero.

### Deployment with no deploy mechanism — the inverse, and it is NOT none

The section above only asks one of the two questions. The other direction —
**a live function nothing in this repo can update** — has one answer, and it is
a worse state than code with no deployment, because nothing about it looks
wrong from either side.

`spawn-cost-history-collector` is live, invoked **hourly** by an ENABLED
EventBridge rule (`spawn-cost-history-collector-hourly`, `rate(1 hour)`), and
was last modified **2026-02-20**. The only references to it anywhere in the
repository are its own `go.mod`, a `.gitignore` entry, a Dependabot entry and
the changelog. There is **no deploy script, no Makefile target and no
CloudFormation or SAM template** — so the code in `lambda/cost-history-collector/`
cannot be built and shipped to the function that is running, and the running
bytes are whatever someone uploaded by hand eight months ago.

**Resolved 2026-10-11:** `scripts/deploy-cost-history-collector.sh` exists now, so
the function is shippable and the existing deploy-script gates
(`TestDeployScriptsAssertTheAccount`, the census's version-stamp and
log-retention checks) apply to it. The function itself was never the problem — it
works, and `lambda/dashboard-api/cost_history.go` serves `GET /api/cost-history`
from the table it fills. It was simply unreachable from the repo.

The script deliberately does **not** create the hourly EventBridge rule. The live
one (`spawn-cost-history-collector-hourly`, `rate(1 hour)`) already exists, and a
deploy script that also owns the schedule would be able to silently change how
often a cost collector runs.

That is the general hazard recorded in the "leave no trace" notes made concrete:
version skew is the normal state for this control plane, and nothing warns. The
gate above cannot catch this one, because it reasons from hardcoded Lambda names
in Go source outward to a deploy mechanism, and nothing in Go source names this
function. Tracked as #848.

## DynamoDB tables

Mapped to the code that reads them. Two have **no reference in this repo** and
belong to adjacent projects.

| table | read by |
|---|---|
| `spawn-user-accounts` | `lambda/dashboard-api/dynamodb.go` |
| `spawn-availability-stats` | `pkg/availability/stats.go` |
| `spawn-sweep-orchestration` | `lambda/dashboard-api/models.go` |
| `spawn-pipeline-orchestration` | `lambda/pipeline-orchestrator/` |
| `spawn-websocket-connections` | `lambda/dashboard-websocket-processor/` |
| `spawn-schedules`, `spawn-schedule-history` | `cmd/schedule.go`, `deployment/cloudformation/schedules-tables.yaml` |
| `spawn-alert-preferences` | **nothing, since #794 removed `alert-evaluator`.** Still deployed and empty. Its template stays because `spawn-cost-history`, declared alongside it, is live |
| `spawn-alerts`, `spawn-alert-history` | **nothing, since #783 removed the alerts path.** Still deployed and empty; the template that creates them stays because two of its four tables are live. Their removal belongs to the control-plane reaping in #653 |
| `spawn-cost-history` | `cost-history-collector` |
| `spawn-teams`, `spawn-team-memberships` | team sharing (#137) |
| `spore-portal-accounts` | the reaper's `ACCOUNTS_TABLE` — the portal registry it reads (read-only `Scan`) for the expiry verdict. It does **not** decide which accounts are scanned; `REAPER_ROLE_ARNS` (plus `REAPER_SCAN_SELF`) does |
| `spore-bot-{audit,registry,workspaces}` | spore-bot (spore-host `lambda/spore-bot`); here, `cmd/bot.go` (registry, workspaces) and `lambda/dashboard-api/slack_oauth.go` (workspaces). `deployment/cloudformation/spore-bot.yaml` declares them for self-hosting only — the hosted tables are not in any stack (per spore-host repo) |
| `spore-sms-pending` | written by spore-bot's SMS notifications, read by spore-host's `lambda/rest-api` for replies (per spore-host repo). Declared here only by the self-hosting `spore-bot.yaml` |
| `lagotto-watches`, `lagotto-match-history` | lagotto; also read by `lambda/dashboard-api/watches.go` |
| `prism-bot-audit`, `spore-api-keys` | **no reference here** — adjacent projects |

## S3 buckets

| bucket | objects | purpose |
|---|---|---|
| `spawn-binaries-<region>` × 11 | 538–803 | **load-bearing.** Instance bootstrap pulls `spored` from the bucket for its own region, from `spawn/versions/<v>/` for the launching spawn's release (#1412), or from `spawn/current/` for a dev build (#1419; the unversioned `spawn/spored-*` and bucket-root `spored-*` are frozen at v0.149.0 for spawn releases before it), and `spawn upgrade-spored` fetches `spawn/versions/<v>/` too. Written by `.github/workflows/release.yaml`. Per-region by design: an instance should not pull its agent across the planet |
| `spawn-sweeps-us-east-1` | 7 | sweep parameter files the orchestrator reads |
| `spawn-pipelines-us-east-1` | 16 | pipeline Lambda artifacts |
| `spawn-cloudformation-artifacts-us-east-1` | 9 | SAM packaging target |
| `spawn-schedules-{us-east-1,us-west-2,eu-west-1}` | **0 each** | three regional buckets, all empty |
| `spawn-certs-us-east-1` | 2 | no reference in this repo |
| `spawn-queue-results-test` | 0 | referenced by `testdata/integration/queue/*.json`; empty but **load-bearing for tests** |
| `spore-host-docs`, `spore-host-website` | 476 / 225 | the website and docs site |

**On the `spawn-binaries-*` object spread:** us-east-1 and us-west-2 hold 130
released versions; ap-northeast-1 holds 80. The difference is historical
backfill, not a gap — **0.126.1 and `spored-windows-amd64.exe` are present in
every region checked**, so bootstrap is unaffected. Verified rather than assumed,
because an agent missing from one region would be a silent regional failure.

## What nothing reaps

The reaper and `spawn cleanup` cover instances and their attachments. Nothing
covers the above (#653). Known consequences:

- **Deleting a Lambda does not delete its log group.**
  `/aws/lambda/github-oauth-bridge` outlived the function it belonged to.
- The log groups all carried **30-day retention** when #770 applied it on
  2026-10-09; the ones added or removed since inherit or drop it with their
  function. Stated without a count on purpose — the previous wording said "all 18"
  and was wrong within a day.
  Before that, 14 had none and held 238.7 MB.
- Every operated deploy path stamps `spawn:version` (#768), so skew between the
  CLI and the control plane is readable in one API call instead of inferred from
  `LastModified`.

## Known traps

Recorded because each cost a real failure:

- **`--parameter-overrides` cannot carry a value containing a space.** It failed
  a staging deploy and rolled the stack back. `ttl-reaper` uses a `file://`
  params file for this reason.
- **A parameter omitted from `--parameter-overrides` resets to the template
  default**, it does not keep its live value. That disarmed the production reaper
  once (#650). `ttl-reaper`'s Makefile reads the live stack first; #779 gates that
  every template parameter is passed at all.
- **SAM's `Enabled: !Ref <param>` silently does not work** — at transform time a
  `!Ref` is a truthy dict, so every value yields `ENABLED`. Use `State`.
- **The reaper's cross-account trust used to name a CFN-generated role ARN**
  (`…TTLReaperFunctionRole-ZJ84YZ2dCPei`), so recreating the stack would have
  broken every onboarded account at once (#476). Fixed 2026-10-10: the trust now
  uses `:root` plus an `ArnLike` on `aws:PrincipalArn`, which survives the suffix
  being regenerated and stays narrower than bare `:root`. Note for anyone
  re-deriving this: IAM **rejects** a trust policy naming a principal that does
  not exist yet, so the exact-ARN form cannot be migrated by adding the new ARN
  first.
- **`spawn autoscale` took its region from the ambient chain** and ignored
  `--region` until #775. The command and its orchestrators were removed entirely
  in #772; the trap is recorded because the shape recurs.

## Related

- #653 — nothing reaps the control plane
- #654 / #768 — version skew between the CLI and deployed Lambdas
- #474 — the operated stack's deploy parameters are recorded nowhere
- #476 — the CFN-generated role ARN trap
- #780 — a future self-hosted deployment would have to reproduce much of this
