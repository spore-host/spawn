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

## Deploy checklist

A release does not deploy any of the functions above. After each tag, the
release workflow's **Deploy checklist** job (#1444) posts a comment on the
release PR saying which operated components the release changed. If no single
PR matches, the comment goes to that job's summary instead. To produce the same
report locally, offline:

```sh
make deploy-checklist TO=v0.151.0            # FROM defaults to the previous tag
make deploy-checklist FROM=v0.150.0 TO=v0.151.0 JSON=1
```

**Where the list comes from.** It is not a hand-written list. The Lambdas are
the ones the deploy mechanisms name, discovered by the same rules as
`make lambda-versions`. A test fails if the two ever disagree. For each Lambda,
the tool runs `go list -deps` with the `GOOS`, `GOARCH` and `-tags` from that
mechanism's own `go build` line, at both refs. It then compares the repo files
that are compiled in, which can live in its own module or its `replace ../..`
parent. It also compares the third-party module versions the binary links,
the main module's `go` directive, and the deploy script or template itself.

**How to read each line.**

- **code-only**: only compiled inputs changed. A code update ships it: run
  the listed command from a clean checkout of the tag. The CI deploy (below)
  ships these lines, once the owner approves, and nothing else.
- **manual**: something other than code changed. That can be the Lambda's own
  deploy script, its template or Makefile, or a file they read. It can also be a
  separate deploy input, listed as its own line under *Other deploy inputs*:
  - a script that makes a control-plane change (IAM role or policy, event-source
    mapping, DynamoDB table or stream, API Gateway, DNS, KMS);
  - a CloudFormation or SAM template;
  - a policy document a script applies with `file://`;
  - a script that runs one of those.

  Review the diff and apply it by hand, the way v0.151.0's role and mapping
  changes were applied (snapshot, dry run, apply, diff; the record is on PR
  #1440).
- **added**: a new mechanism, so this is the function's first deploy.
- **removed**: nothing deploys it any more, so tear down what it created.

**The CI deploy role.** `GitHubActions-spawn-lambda-deploy` (#1445) is what
the deploy job assumes for **code-only** lines. Create or update it with
`AWS_PROFILE=spore-host-infra scripts/setup-lambda-deploy-ci-role.sh`, and
remove it with `--delete`. It trusts only
`repo:spore-host/spawn:environment:production`, so its strength is that
environment's protection: the owner as required reviewer, no admin bypass,
`v*` tags only. Keep those settings. A workflow that names a missing
environment creates it unprotected.

**What is not counted.** These never mark a component changed: `_test.go`
files, docs, a package the Lambda does not compile, a Dependabot bump to a module
it does not link, and DynamoDB item writes (backfills are data, not deploys).
EC2, EFS and FSx calls are workload resources, not operated state, so a smoke
test is not a deploy input.

**What it cannot judge.** A changed line means the shipped bytes differ. It
does not mean the behaviour does. On v0.151.0 the tool reports `dashboard-api` as
changed (code-only), because it deleted unused types. The owner chose not to
redeploy it. The orchestrators changed only behind a flag they never set, and
they were redeployed anyway so their version stamps would match. Deciding
whether a changed line needs a deploy is still the operator's call. The tool
only makes sure the question gets asked about every component.

## Deploying code-only changes from CI

After the release job, `release.yaml` calls `.github/workflows/deploy-lambdas.yaml`
(#1445). Its **plan** job refuses anything but a `vX.Y.Z` tag and puts the
checklist in its summary. Its **deploy** job runs in the `production`
environment, so it waits for the owner's approval. Nothing waits on it, so a
pending approval holds up no other job. On approval it assumes
`GitHubActions-spawn-lambda-deploy` and runs `scripts/deploylambdas`. That tool
deploys only the **code-only** lines, and lists everything else in the record
as "manual, not deployed".

**How to approve.** Open the release's workflow run and choose *Review
deployments*, then *production*, then *Approve and deploy*. Read the plan job's
summary first: it lists exactly what will ship. To skip a release, reject it,
or leave it until the approval times out. Either way the run ends red, and
nothing was changed.

**How to dry-run.** Run it by hand on a tag:

```sh
gh workflow run deploy-lambdas.yaml --repo spore-host/spawn --ref v0.152.0 -f dry_run=true
```

`dry_run` defaults to true, so the same run without `-f` is also a dry run, and
it still needs the approval. A dry run does the pre-flight, the build and the
download of the live code, and makes no mutating call. Its record says, per
function, which sha it would replace with which and the stamp it would write.
The call from `release.yaml` stays a dry run until the repository variable
`LAMBDA_DEPLOY_MODE` is `live`. Set it once a dry run's record has been
reviewed, with `gh variable set LAMBDA_DEPLOY_MODE --repo spore-host/spawn --body live`.
A tag older than the one that added this workflow (v0.151.0 and before) cannot
be run: `workflow_dispatch` reads the workflow from the ref it runs on.

**What it checks, per function, before changing any of them.**

- It refuses unless the checkout is a clean release tag that is the checklist's
  `--to`, and the function's ARN is in 966362334030, us-east-1.
- The live function must be `Active` and settled, `PackageType` `Zip`, and on
  the architecture of the checklist's `GOARCH`. Today that is x86_64 for the
  sweep orchestrator and arm64 for the pipeline orchestrator and the reaper.
  Its Runtime and Handler must be what the component's own deploy script or
  template declares. A mismatch is a configuration deploy, and stays manual.
- For a component whose deploy script runs `spawn bootstrap-body`, today the
  sweep orchestrator, it checks the published body. The body at
  `spawn-binaries-us-east-1/spawn/versions/<v>/bootstrap.sh` must be the one
  the tag renders. It is fetched over HTTPS, so it needs no S3 permission.
- It builds exactly the checklist's `go build` line (GOOS, GOARCH, `-tags`,
  `-ldflags` with `${SPAWN_VERSION}` filled in) with cgo off, from the tag.
  Any other build flag refuses. The zip holds `bootstrap` at its root, as every
  mechanism's does. A component with two deploy scripts uses the checklist's
  build line. For `dashboard-api` that is `lambda/dashboard-api/deploy.sh`'s
  `-ldflags="-s -w"`.
- It downloads the live code from `get-function`'s presigned URL, and checks
  that its sha is the live CodeSha256.

Then, one function at a time:

1. Re-read the configuration, which must be unchanged since the pre-flight.
2. Update the code (with the snapshot's revision id) and wait for it to settle.
3. Check that the remote CodeSha256 is the local zip's.
4. Diff the configuration against the snapshot. It must be empty, apart from
   the code, update-status and runtime-version fields. Lambda moves the runtime
   version on an update under automatic runtime management, and the record
   notes when it does.
5. Stamp `spawn:version` and read it back.

A function that already runs the identical build (a re-run, since the zip is
reproducible) is only stamped.

**Rollback.** Any failure at or after the update restores the saved code,
waits, and re-verifies its CodeSha256. If a stamp was written, it restores the
previous `spawn:version`. Then the job fails, and the remaining functions are
"not attempted". The record says which functions were deployed, which one
failed, whether its rollback verified, and which were never touched. The role
cannot write configuration. A rollback therefore cannot undo a configuration
change, and the diff check exists to catch exactly that; such a rollback is
recorded as `ROLLBACK INCOMPLETE`, to be fixed by hand.

**SAM-managed functions: expected drift.** The pipeline orchestrator
(`spawn-pipeline-orchestrator`) and the reaper (`spawn-ttl-reaper-production`)
belong to CloudFormation stacks. A direct code update leaves two things stale:
the stack's `Code` (the S3 object of its last `sam deploy`) and its `Version`
parameter (the function's `spawn:version` tag says the new version, while the
stack's template value says the old). This is deliberate. `sam deploy` would
need CloudFormation execute rights and `iam:PassRole`, which this role must not
have. The drift is harmless while it lasts:

- CloudFormation re-applies `Code` only when the template's code location
  changes.
- An update that touches only parameters leaves the code and the tag alone.
- The next `make -C lambda/<name> deploy` from a release tag uploads its own
  build and asserts `Version`, which reconciles both.

There is one trap. If a later stack update that DOES change the code fails and
rolls back, CloudFormation restores the stack's previous code, which is older
than the CI deploy. After a failed stack update, check the CodeSha256 and run
this workflow again. The record notes the stack name on every SAM-managed line.
Template or Makefile changes are mechanism changes, so the checklist marks them
manual and this job never ships them.

**What it could not port** from the manual scripts, all because the role does
not grant the call:

- `deploy-custom-dns.sh` checks that the Function URL's auth type is `AWS_IAM`.
  That needs `lambda:GetFunctionUrlConfig`. A code update does not change the
  URL configuration.
- `lambda/dashboard-api/deploy.sh` checks that its IAM role exists. That needs
  `iam:GetRole`, and matters only when creating the function.

The account check uses the function ARN instead of `sts get-caller-identity`.
Every configuration write the scripts make is deliberately left out: timeout,
memory, environment, role policies, log retention, Function URL and permissions.
Those stay manual.

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
