# The spore.host infra account

What is deployed in the shared infra account (**966362334030**, `us-east-1`),
what each piece does, and where its source lives.

**Measured, not recalled.** Every figure here came from the live account on
**2026-10-09** via `spawn footprint` (#771) and the AWS API. Re-measure rather
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

**103 control-plane resources**, none of which the TTL reaper or `spawn cleanup`
touches — those cover launched instances and their attachments, not the control
plane (#653).

| | count |
|---|---|
| Lambda functions | 18 |
| CloudWatch log groups | 18 |
| DynamoDB tables | 23 |
| EventBridge rules | 5 |
| S3 buckets | 21 |
| IAM roles | 16 |
| IAM instance profiles | 2 |

## Lambda functions

Grouped by how they are invoked, because that is the thing least obvious from the
code and it determines what breaks when a schedule is disabled.

### Event-driven (EventBridge schedule)

| function | source | schedule | notes |
|---|---|---|---|
| `spawn-ttl-reaper-production` | `lambda/ttl-reaper/` | `rate(10 minutes)` | the out-of-band backstop that terminates expired instances. Cross-account: `REAPER_SCAN_SELF=false`, assumes `spawn-ttl-reaper-ec2` in 435415984226 |
| `spawn-alert-evaluator` | `lambda/alert-evaluator/` | `rate(1 hour)` | evaluates user alert preferences |
| `spawn-cost-history-collector` | `lambda/cost-history-collector/` | `rate(1 hour)` | writes to `spawn-cost-history` |
| `spawn-autoscale-orchestrator-production` | `lambda/autoscale-orchestrator/` | `rate(1 minute)`, **ENABLED** | reconciles autoscale groups |
| `spawn-autoscale-orchestrator-staging` | same | `rate(1 minute)`, **DISABLED** | disabled 2026-10-09 (#772): it polled every minute from February against an empty table |

### API Gateway-fronted

| function | source | notes |
|---|---|---|
| `spawn-dashboard-api` | `lambda/dashboard-api/` | the dashboard's REST backend |
| `spawn-dashboard-websocket` | `lambda/dashboard-websocket/` | WebSocket connect/disconnect |
| `spore-bot` | **not this repo** | `deployment/cloudformation/spore-bot.yaml` deploys it |

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

### Code with no deployment

**`lambda/alert-handler/` has no live function in this account.** It builds and
is vetted in CI (the nested-module loop covers it), but nothing runs it. Either
it is unfinished, or it was superseded by `alert-evaluator` and not removed.
Worth a decision rather than leaving it ambiguous.

## DynamoDB tables

Mapped to the code that reads them. Four have **no reference in this repo** and
belong to adjacent projects.

| table | read by |
|---|---|
| `spawn-user-accounts` | `lambda/dashboard-api/dynamodb.go` |
| `spawn-availability-stats` | `pkg/availability/stats.go` |
| `spawn-sweep-orchestration` | `lambda/dashboard-api/models.go` |
| `spawn-pipeline-orchestration` | `lambda/pipeline-orchestrator/` |
| `spawn-websocket-connections` | `lambda/dashboard-websocket-processor/` |
| `spawn-autoscale-groups-{production,staging}` | `lambda/dashboard-api/autoscale.go`. **Both empty** — no autoscale group has ever been configured (#772) |
| `spawn-schedules`, `spawn-schedule-history` | `cmd/schedule.go`, `deployment/cloudformation/schedules-tables.yaml` |
| `spawn-alerts`, `spawn-alert-preferences`, `spawn-alert-history` | the alert path; `deployment/cloudformation/alerts-tables.yaml` |
| `spawn-cost-history` | `cost-history-collector` |
| `spawn-teams`, `spawn-team-memberships` | team sharing (#137) |
| `spore-portal-accounts` | the reaper's `ACCOUNTS_TABLE` — which accounts it scans |
| `spore-bot-{audit,registry,workspaces}` | `deployment/cloudformation/spore-bot.yaml`, `cmd/bot.go` |
| `lagotto-watches`, `lagotto-match-history` | lagotto; also read by `lambda/dashboard-api/watches.go` |
| `prism-bot-audit`, `spore-api-keys`, `spore-sms-pending` | **no reference here** — adjacent projects |

## S3 buckets

| bucket | objects | purpose |
|---|---|---|
| `spawn-binaries-<region>` × 11 | 538–803 | **load-bearing.** Instance bootstrap pulls `spored` from the bucket for its own region, and `spawn upgrade-spored` fetches `spawn/versions/<v>/`. Written by `.github/workflows/release.yaml`. Per-region by design: an instance should not pull its agent across the planet |
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
- All 18 log groups now carry **30-day retention** (#770), applied 2026-10-09.
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
- **The reaper's cross-account trust names a CFN-generated role ARN**
  (`…TTLReaperFunctionRole-ZJ84YZ2dCPei`), so recreating the stack breaks every
  onboarded account at once (#476).
- **`spawn autoscale` took its region from the ambient chain** and ignored
  `--region` until #775.

## Related

- #653 — nothing reaps the control plane
- #654 / #768 — version skew between the CLI and deployed Lambdas
- #474 — the operated stack's deploy parameters are recorded nowhere
- #476 — the CFN-generated role ARN trap
- #780 — a future self-hosted deployment would have to reproduce much of this
