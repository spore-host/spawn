# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **An image spawn creates records where it came from** (#1364). `spawn ami
  create` / `create-ami` and the warm AMI `spawn image import` builds (unless
  `--no-warm`) tag the image and its snapshots with
  `spawn:image-spawn-version` (the creating spawn's version) and, when the
  source instance has them, copies of its `spawn:version`,
  `spawn:spored-version`, `spawn:mpi-enabled` and `spawn:job-array-index` as
  `spawn:source-version`, `spawn:source-spored-version`,
  `spawn:source-mpi-enabled` and `spawn:source-job-array-index`; an imported
  Windows AMI gets `spawn:image-spawn-version`. A tag the source lacks is left
  off. `--tag` cannot set these keys (it is refused). `spawn ami copy` keeps
  them as they are and does not stamp its own version. `spawn ami list` has a
  `SRC SPAWN` column, and its JSON has `source_version` and a `provenance`
  object. `spawn ami create` and `spawn ami copy` now refuse, before calling
  EC2, an image that would carry more than 50 tags. API:
  `aws.ImageProvenance`, `aws.ImageProvenanceFromTags`, `aws.ImageCreateTags`,
  `aws.ImageProvenanceTagKeys`, `aws.IsImageProvenanceTagKey`, the tag-key
  constants, `AMIInfo.Provenance` and `CreateAMIInput.SourceTags`. No new IAM:
  the tags ride CreateImage's TagSpecifications.

### Changed

- **A parameter sweep refuses, before any AWS call, the launch flags it
  cannot honour** (#1366). Each used to be accepted and dropped, so the
  members launched without it: `--strata-formation`/`--strata-profile`/
  `--strata-registry` (no environment), `--dns-domain`/`--dns-api-endpoint`/
  `--dns-function-arn` (a row's `dns:` name registered with spore.host's
  updater), `--nist-800-171`/`--nist-800-53`/`--compliance-strict` (no
  encrypted-EBS or IMDSv2 enforcement), `--allow-cidr` and `--efa-group` (no
  security group is created for a sweep), `--interactive`, `--count`,
  `--job-array-name`, `--instance-names`, `--min-viable`, the `--mpi` flags,
  `--auto-placement-group` and `--template-var`. (`--cartesian`, deprecated
  and inert since #674, is still accepted with cobra's warning.) The
  error names every such flag with what to do instead. Every launch flag now
  has a recorded sweep rule (`cmd/launch_sweep_flags.go`), and a test checks
  each claim on a real foreground member, so a new flag cannot be dropped
  silently.

### Fixed

- **`setup-dashboard-lambda-role.sh` now finishes removing the legacy
  `SpawnDashboardLambdaPolicy` managed policy, and the three inline policies
  added by hand before it owned the role** (#1376). `delete-policy` failed with
  `DeleteConflict` on a policy with old versions, so the script now deletes
  every non-default version first, and only when the policy is attached to
  nothing. `DynamoDBStreamsAccess`, `SlackSecretsAccess` and
  `SporeSlackWorkspaceAccess` on `SpawnDashboardLambdaRole` are removed by
  exact name, each only if every action it allows is one the script's own
  policies grant for the same purpose (`SlackSecretsAccess` only when the
  script wrote the Slack secret grant), with its document printed first. Any
  other inline policy is left alone, and a policy that fails the check is kept
  with a warning.
- **`spawn pool status` prints `unknown` for a queue count SQS did not
  report, instead of 0** (#1362). A missing or unparseable
  `ApproximateNumberOfMessages` / `ApproximateNumberOfMessagesNotVisible` was
  shown as `0 waiting, 0 in-flight`, as if the queue were known to be empty.
  API: `taskpool.Queue.Depth` and `taskpool.Pool.Depth` now return
  `(visible, inFlight *int, err error)`; a nil count is unknown, not zero. The
  counts are parsed by the new `poolqueue.Count`, the same rule
  `spawn cleanup --pool-queues` and the reaper use.
- **A nested-spawn role named directly no longer skips `--nested-spawn`'s
  `--ttl` requirement** (#1375). The `spawn-instance-nested-*` roles may call
  RunInstances and pass roles, so `--nested-spawn` gives one only to an instance
  with a TTL; naming the role yourself attached the same capability with no
  TTL. Now `spawn launch --instance-profile spawn-instance-nested-<hash>` needs
  `--ttl` (refused before any AWS call, same reason as `--nested-spawn`, and not
  with `--no-timeout`); a foreground (`--no-detach`) sweep or `spawn resume`
  refuses any row whose profile is one (the row's `iam_role:`, else
  `--instance-profile`, else `defaults: iam_role:`) and whose TTL (the row's
  `ttl:`, else `defaults: ttl:` / `--ttl`) is empty; `spawn burst
  --instance-profile` applies the same rule. `--iam-role` may no longer name one
  on any launch path, and neither may `--instance-profile` with `--batch-queue`:
  spawn would create or rewrite that role's grants, which are fixed so they can
  be audited (`CreateOrGetInstanceProfile` refuses such a `RoleName` too).
  `aws.Client.Launch` and `launcher.Provision` refuse a nested-spawn
  `IamInstanceProfile` with no `TTL`, so SDK callers (lagotto, the MCP server)
  are held to it as well. New in `pkg/aws`: `CheckNestedSpawnTTL`,
  `LaunchConfig.ValidateNestedSpawnTTL`, `NestedSpawnTTLError`,
  `NestedSpawnRoleReservedError`, `ErrNestedSpawnNeedsTTL`,
  `ErrNestedSpawnRoleReserved`.
- **The per-task IAM role sweep keeps a role an enabled schedule will launch
  with** (#1378). `spawn schedule create` accepts a `spawn-instance-*` profile,
  stores its name in the schedule's params, and every firing launches with it
  without re-creating it, so a schedule that fires weekly left its role idle
  between firings and an armed reaper (`REAPER_INSTANCE_ROLES=reap`) or
  `spawn cleanup --instance-roles` would have deleted it, failing the next
  firing at RunInstances. Both now read the schedules (the `spawn-schedules`
  table in us-east-1 and each params object, through one shared source in
  `pkg/instancerole`) and keep every profile named by an active schedule's
  `defaults.iam_role` or a row's `iam_role`; the reaper logs `role <name>
  kept: referenced by schedule <id>` and counts `instance_roles_referenced`,
  and the CLI's preview shows the same reason. A paused, cancelled, deleted
  or run-out schedule (max executions reached, end-after passed: the
  scheduler refuses those firings) protects nothing. If the schedules cannot
  be read completely (AccessDenied, a missing table, a failed page, an active
  schedule's missing params object), no role is deleted: the reaper counts
  the account in `instance_roles_references_unreadable`, and the CLI refuses
  with the reason. New grants on the reaper's own role, read-only:
  `dynamodb:Scan` on `spawn-schedules` (us-east-1) and `s3:GetObject` on
  `spawn-schedules-*/schedules/*` (SAM template and `spawn reaper deploy`);
  the member-account role is unchanged. `spawn cleanup --instance-roles` now
  also calls `dynamodb:Scan` and `s3:GetObject` with the infra profile
  `spawn schedule` uses.
- **A detached sweep from an older spawn now stops at the first member the
  account has no `spored-instance-profile` for, and says what to run**
  (#1369). The sweep orchestrator launches a member from a v0.146.0-or-older
  CLI's params, or from a schedule one created, with `spored-instance-profile`
  when it names no `iam_role`. That CLI never created the profile on the
  detached path, so an account that only ran detached sweeps or schedules may
  not have it, and EC2 refuses every such member (`InvalidParameterValue`:
  "Invalid IAM Instance Profile name"). The orchestrator now recognises that
  refusal for those members: the member is not retried with its other instance
  types, the members not yet attempted are not attempted, and the sweep is
  `FAILED`. Its error, which `spawn status --sweep-id` shows, and the member's
  row both name the remedy: re-run the sweep with spawn v0.147.0 or later
  (`spawn launch --param-file <params-file>`), or for a schedule, `spawn
  schedule cancel <id>` and `spawn schedule create <params-file>` again. Both
  create the profile with your credentials before anything is uploaded. No
  IAM is granted to the orchestrator or to any member-account role. A member
  whose params a current CLI rendered keeps the old behaviour: its row fails
  with EC2's error and the sweep's other members are still attempted. Takes
  effect when the sweep orchestrator is redeployed from a release tag.

- **Foreground parameter sweeps no longer drop `--fsx-create`, `--fsx-recall`
  and `--vpc` without `--subnet`** (#1366). A `--no-detach` sweep accepted all
  three and launched every member without them: no filesystem was created or
  recalled, and the members landed in the region's default VPC. Both sweep
  paths now refuse them before any AWS call, with one message: create or
  recall the filesystem first and pass its `--fsx-id`, or pass `--subnet` with
  a subnet in that VPC. The `--fsx-*` options that only mean something with
  `--fsx-create` are refused with it. The detached path's refusal no longer
  points at `--no-detach`.
- **Sweep members now mount `--efs-id`, `--fsx-id` and `--attach-volume`
  storage** (#1366). Both sweep paths rendered their members' user data with
  no storage script, so the filesystems were tagged and never mounted and an
  attached volume was never formatted or mounted. Each member now gets the
  mount script a single launch of the same config runs (including
  `--efs-mount-options`, `--efs-profile` and the EFS mount-target IP
  fallback), its `--command` waits for the mounts, and an `--fsx-id`
  filesystem is looked up once per region before anything is created, so one
  that a member's region does not have fails the sweep.
- **`--spot-max-price` reaches a sweep's members** (#1366). Only the
  batch-queue path read it; a row's own `spot_max_price:` still wins.

## [0.147.0] - 2026-10-28

### Added

- **`spawn cleanup --pool-queues` and the reaper's `REAPER_POOL_QUEUES`:
  abandoned pools' SQS queues and staged specs now have a deleter** (#1351).
  `pkg/poolqueue` holds the one rule both apply. A `spawn-pool-<run id>` queue
  is deleted only when it is tagged `spawn:managed=true` with a
  `spawn:pool-run-id` tag matching its name, no instance in any enabled region
  (pending through stopped) holds the pool's worker profile or tag, it holds no
  messages, and it is at least 24 hours old; a region that cannot be read
  means nothing is deleted. The specs `spawn pool submit` staged under
  `staging/pool-specs/<run id>/` in the results bucket go once the queue is
  gone, no instance is the pool's, and the newest is 24 hours old; nothing
  else in the bucket is touched. Queues made before this release are untagged
  and kept (`spawn pool drain` deletes one). Reaper: template parameter
  `PoolQueues` / `make deploy POOL_QUEUES=` / `spawn reaper deploy
  --pool-queues`, `off|report|reap`, default `report` (logs `WOULD delete pool
  queue ...` and changes nothing), and a deploy that does not name it keeps the
  deployed value. New grants, scoped by name: `sqs:ListQueues`;
  `sqs:GetQueueAttributes`, `sqs:ListQueueTags`, `sqs:DeleteQueue` on
  `spawn-pool-*`; `s3:DeleteObject` on `spawn-results-*/staging/pool-specs/*`.
  The member-account role stack (`ttl-reaper-cross-account-role.yaml`) must be
  redeployed; until then the account is counted in
  `pool_queues_accounts_denied`.

- **`spawn launch --efa --efa-group NAME`: a shared EFA security group for
  launches that are not one MPI job array** (#1168). EFA needs a group that
  admits all traffic from itself, and spawn made one only for `--mpi`, so a
  fleet of different instance types (a GPU client plus EFA proxies) needed a
  hand-built group that someone had to remember to delete. `--efa-group NAME`
  creates or joins `spawn-efa-NAME` with the same self-referencing all-traffic
  rules as the MPI group (an existing group is brought up to date), adds it to
  the launch's other groups, and opens nothing to the outside. It is tagged
  `spawn:managed=true` with a creation stamp, and the TTL reaper's managed
  network sweep deletes it once no instance has used it for 7 days. Launch the
  fleet in one AZ; EFA does not cross AZs. Requires `--efa`. No new permission.

- `spawn ami copy <ami-id|name> --to-region <region> [--name] [--kms-key] [--wait]` copies a spawn-managed AMI to another region inside spawn's inventory: the source's tags go onto the new AMI and every snapshot backing it (atomically, via CopyImage TagSpecifications), plus `spawn:copied-from=<ami>@<region>`, so `spawn ami list --region`, `spawn footprint` and `spawn ami delete --region` cover the copy. An encrypted source is copied encrypted with the target region's default EBS key unless `--kms-key` names one there; `Ec2ImageBuilderArn` is not copied, so deleting a copy cannot delete the source's Image Builder image. AMIs without `spawn:managed=true` are refused. `--wait` polls a bounded number of times (`--wait-timeout`, default 1h) with a progress line. Deleting the source does not delete its copies. Needs `ec2:DescribeImages`, `ec2:CopyImage` and `ec2:CreateTags`; a denial names the missing action (#1169)
- **`spawn launch --nested-spawn`: let a spore launch its own children**
  (#39), e.g. a workflow head node dispatching tasks. Opt-in: the instance gets
  a dedicated role, `spawn-instance-nested-<hash>`, and the default
  `spored-instance-role` is unchanged (a test snapshots it before and after).
  The role grants what `spawn launch`, `list`, `status`, `extend`, `connect`
  and `terminate` call, as discovered from their source by a test: reads on
  `*` (AWS has no resource-level permissions for them); `RunInstances` only
  for an instance and volumes tagged `spawn:managed=true` at creation, plus
  the resource types it references (AMI, subnet, security group, key pair,
  network interface, placement group, Spot request, capacity reservation);
  `ImportKeyPair`, `CreateSecurityGroup` and `CreatePlacementGroup` only when
  tagged `spawn:managed=true`; start, stop, terminate, SSM commands and
  sessions only on `spawn:managed=true` resources; and `iam:PassRole` only for
  `spored-instance-role` and `spawn-instance-*` roles, only to EC2. It grants
  no IAM write, so a CLI running as the role never writes IAM: children use
  `spored-instance-profile` (the parent launch makes sure it exists), an
  existing `--instance-profile`, or with `--nested-spawn` the parent's own
  role, and `--iam-*`/`--s3-*` flags, a custom DNS updater and `--no-timeout`
  are refused on a nested spore with a message saying why. FSx creation,
  staging buckets, sweeps and multi-card EFA public IPs are not granted.
  `--nested-spawn` requires `--ttl`, and is refused with `--no-timeout`,
  `--instance-profile`, IAM flags, sweeps and batch queues. Children get their
  own TTL (or the 1h idle default) and spored as any launch does. The role is
  in the `spawn-instance-*` family, so the instance-role sweep (#1300: the
  TTL reaper's `REAPER_INSTANCE_ROLES` and `spawn cleanup --instance-roles`)
  deletes it once nothing uses it.

- **spawn now removes its own empty buckets** (#1301). `spawn cleanup
  --buckets` and, optionally, the TTL reaper delete
  `spawn-staging-<account>-<region>` and `spawn-results-<account>-<region>`
  once they have been empty for 7 days. Before this nothing ever removed
  either. **No object is ever deleted**: a bucket holding anything (an object,
  an old version, a delete marker, an unfinished multipart upload) is kept,
  and deleting results data stays opt-in (#1076). A bucket is deleted only if
  it has spawn's exact name for the account and region, is tagged
  `spawn:managed=true` (or is an untagged results bucket on that exact name),
  belongs to your account (every call sends it as the expected bucket owner),
  carries no FSx stack configuration, and, for a results bucket, has no
  lifecycle rules, since a re-created bucket would not get back the tiering
  `spawn results lifecycle --apply` installed. S3 records no "empty since",
  so the first sweep that finds a bucket empty tags it `spawn:empty-since`; a
  sweep that finds it in use removes the tag, and so does a launch that uses
  it. The staging bucket is never listed, since no role spawn makes may list
  it (#1274): S3's refusal to delete a bucket that is not empty is the check
  there. spawn re-creates either bucket the next time it needs it.
  `spawn cleanup --buckets --dry-run` writes nothing, not even the tag.
  The reaper's sweep is `REAPER_EMPTY_BUCKETS` (template parameter
  `EmptyBuckets`, make variable `EMPTY_BUCKETS`, `spawn reaper deploy
  --empty-buckets`): `off`, `report` (the default, which logs what it would
  do and writes nothing) or `reap`; a deploy that does not name it keeps the
  deployed value. It needs new S3 grants, scoped to `arn:aws:s3:::spawn-staging-*`
  (bucket tagging and `DeleteBucket` only) and `arn:aws:s3:::spawn-results-*`, which the `spawn-ttl-reaper-ec2` member
  stack, the ScanSelf policy and `spawn reaper deploy` now carry: redeploy the
  member stacks before arming it, or the sweep logs each bucket it could not
  read with what to redeploy (`empty_buckets_failed`). The run summary counts
  `empty_buckets_scanned`, `empty_buckets_stamped`, `empty_buckets_deleted`
  and `empty_buckets_would_delete`. `spawn footprint` shows a bucket's
  `spawn:empty-since` stamp and the date it becomes deletable.

- **`spawn launch` can set spored's metrics, tracing and idle CPU threshold**
  (#1308). `--metrics` starts spored's Prometheus metrics server and tags the
  instance `spawn:metrics-enabled=true`, which the EC2 discovery in
  `deployment/prometheus` matches from the first scrape; `--metrics-port`,
  `--metrics-bind` and `--metrics-path` set the rest. `--tracing` turns on
  tracing with spans in spored's journal, and `--tracing-sampling` sets the
  fraction kept. `--idle-cpu-threshold` sets the CPU percentage below which
  spored counts the instance as idle (default 5). They write the tags spored
  already read at boot and nothing could set, so the only route was
  `aws ec2 create-tags` and a spored restart; that route stays documented for
  running instances in `docs/self-hosted-monitoring.md`. A setting spored would
  ignore is refused: a `--metrics-*` without `--metrics`, a port outside
  1024-65535, a bind that is not an IP address or `localhost`, a sampling rate
  outside 0-1, a threshold of 0. `--tracing-exporter xray` is refused too,
  because the instance role spawn creates does not grant
  `xray:PutTraceSegments` (#1335). SDK callers set
  `LaunchConfig.Observability` and `LaunchConfig.IdleCPUPercent`, checked by
  `LaunchConfig.ValidateSporedSettings`.
- **A CI gate holds every spore.host role to the AWS calls its code makes**
  (#254, spore-host#610). `internal/iamcoverage` type-checks each principal's
  program (spored, its pool-worker, pipeline-stage, provenance and batch-queue
  subcommands, and every Lambda), builds its call graph, finds every AWS SDK
  operation reachable from its entry points (direct calls, calls through
  interfaces, method values, paginators, waiters, the S3 transfer manager,
  STS assume-role providers and hand-signed Function URL requests), maps each
  to its IAM action, and fails unless the policy spore.host provisions for
  that role grants it by name. A wildcard does not count, an operation it
  cannot map fails rather than being skipped, and every main package in the
  repository must be registered as a principal. Gaps that need a decision are
  recorded in a ledger with the issue that tracks them (#1344), and the gate
  fails on a ledger entry that is no longer a gap. The TTL reaper's own drift
  gates (`pkg/reaperiam`) now use the same discovery instead of a regex, which
  could not see calls made in other packages and had skipped Route53 entirely.
- **`docs/reference/cli-iam-actions.md` lists the IAM actions each spawn
  command can call** (spore-host#610), generated from the code by
  `make cli-iam-actions`. The CLI runs with the user's own credentials, so this
  is the permission set a user's identity needs, per command. A test fails
  when the committed copy is stale.

### Changed

- **New AWS permissions for the credentials you run spawn with.** The full list of
  AWS actions each command needs is now generated and enforced in
  [`docs/reference/cli-iam-actions.md`](docs/reference/cli-iam-actions.md) (#254).
  New in this release:
  - `ec2:CopyImage`: `spawn ami copy` (#1169).
  - `sqs:ListQueues`, `sqs:ListQueueTags`, `s3:ListBucketVersions` and
    `s3:ListBucketMultipartUploads`: `spawn cleanup` (#1301, #1351).
  - `sqs:TagQueue`: `spawn pool create` tags its queue (#1351). Without it the pool
    still works, warns, and only `spawn pool drain` deletes that queue.
  - `pricing:GetProducts` and `account:GetAccountInformation`: `spawn schedule create`
    now prices the sweep it schedules (#1330).
  - The instance-role calls `spawn launch` already made (`iam:CreateRole`,
    `iam:PutRolePolicy`, `iam:CreateInstanceProfile` and the rest) are now also made by
    `spawn app launch`, `image import`, `pool create`, `service`, `task run` and
    `launch --batch-queue` (#1300, #1319, #1344).

  Old CLIs keep working with the redeployed sweep orchestrator: a v0.146.0-or-older
  detached sweep or existing schedule gets its spored bootstrap and tags built by the
  orchestrator (#1368).

- **Sweeps and schedules from a spawn CLI at v0.146.0 or older keep launching
  on the redeployed sweep orchestrator, now with spored** (#1368). Such a CLI
  uploads only `ami`, `iam_role`, `instance_type`, `key_name`, `spot` and
  `ttl` per member, and an existing schedule replays those params on every
  firing, so neither carries `spawn_user_data` or `spawn_tags`. The
  orchestrator now renders both for those members itself, with the builders a
  CLI launch uses (`launcher.BuildUserData`, `aws.BuildLaunchTags`): a stub
  that fetches and verifies the bootstrap body published for the
  orchestrator's own release, and the full tag set (`spawn:managed`,
  `spawn:root`, `spawn:iam-user` and `spawn:account-id` from the sweep record,
  `spawn:ttl` and `spawn:ttl-deadline` anchored at the launch, `spawn:version`,
  the sweep tags). The member runs spored, with in-instance TTL, idle and cost
  enforcement, and passes the no-heartbeat check (#1313); the reaper, `spawn
  list`, `spawn cost` and the dashboard treat it like any member. A member
  with no `ttl` gets the 1h idle timeout a CLI sweep member gets (terminate on
  Spot, which a one-time request cannot stop); one with no `iam_role` gets
  `spored-instance-profile`, not `spawnd-role`, which cannot write spored's
  heartbeat tag. The bootstrap is Linux's, other row keys are ignored as
  before, and no `spawn:price-per-hour` is written (legacy params carry no
  cost limit). Params carrying both keys are unchanged; params carrying only
  one are refused, as damaged. `scripts/deploy-sweep-orchestrator.sh` now
  deploys only a clean release tag whose published body matches the one
  compiled in, injects that release (`-X main.Version`), and builds the
  package: it built `main.go` alone, which no longer compiles.
- **Task stage-out through `spored provenance upload` uploads files
  concurrently, with larger parts, sized to the instance** (#1324). On a
  release build every task output goes through spored, not `aws s3 cp`, so
  #1256's CLI tuning did not reach stage-out: a directory output was uploaded
  one file at a time with 5 concurrent 5 MiB parts. It now uploads up to 4-32
  files at once sharing 32-512 part requests of 8-16 MiB, chosen from the same
  NIC-class table the wrapper uses for the CRT client (new
  `taskproto.NICTargetGbps`), capped per vCPU, and with the worst-case part
  buffer held to an eighth of MemTotal; a small box falls back to the old
  setting, never below it. Each object is still tagged in its own upload and
  annotated bound to its ETag, and the result keeps the walk's order. One
  change in failure handling: a failed file no longer stops the rest of the
  output (as `aws s3 cp --recursive`); every file is attempted and the failures
  are reported together, so the wrapper's untagged `aws s3 cp` fallback still
  runs. Not yet measured on real hardware: an A/B on a #1256 workload with a
  large output is needed before release.
- **A detached sweep refuses an instance profile its orchestrator cannot
  pass** (#1331). `--instance-profile NAME`, `--iam-role NAME` and a
  `defaults:` or row `iam_role:` naming a profile other than the ones spawn
  creates (`spored-instance-profile`, `spawnd-role`, `spawn-instance-*`) are
  refused before anything is created, with a message pointing at
  `--iam-policy` (without `--iam-role`) or `--no-detach`. Before, the sweep
  was uploaded and every member failed at RunInstances. A grant broad enough
  to pass an arbitrary role would let the orchestrator hand any role in the
  account, admin included, to an instance, so the refusal is deliberate.
- **A detached sweep member with no `ttl` and no `idle_timeout` now gets the
  1h idle timeout** a foreground member gets (#1330). Before, such a member
  had no bound at all except the reaper's max-age ceiling. `--no-timeout`
  still opts out, as in the foreground.
- **A detached Spot member whose lifecycle stops the instance is refused**
  (#1330). A foreground launch makes such a member a persistent Spot request
  (#922); the orchestrator makes one-time requests, which EC2 cannot stop, and
  it has no way to cancel a persistent one before terminating its instance.
  This includes a Spot row with no `ttl`, which gets the 1h idle default:
  give it a `ttl`, or use `--no-detach`.

- **Task stage-in and stage-out use the AWS CLI's CRT transfer client, sized to
  the instance's network** (#1256). Every `aws s3 cp` in the task wrapper ran at
  the CLI's defaults (classic client, 10 concurrent requests, 8 MiB parts),
  which measured 179-293 MB/s on Graviton4 boxes with 15-30 Gb/s NICs; staging
  was up to 96% of a task's bill. The wrapper now writes a per-run AWS CLI
  config (mktemp, deleted at the end of the run) selecting
  `preferred_transfer_client = crt`, a `target_bandwidth` from the instance
  type's NIC class (read from IMDS, so pooled workers get it too) and 16 MiB
  parts. The same copies done that way ran at 1.53-2.68 GB/s, 5-15x faster.
  Only the wrapper's own `aws` calls see the file: the user's command does not
  inherit `AWS_CONFIG_FILE`. Instances with under ~8 GiB of RAM, and AWS CLIs
  older than 2.14.0 (where `crt` was experimental), keep the classic client
  with 32 concurrent requests and 16 MiB parts. If the task supplies its own
  CLI config (`AWS_CONFIG_FILE`, `AWS_PROFILE` or `AWS_DEFAULT_PROFILE` in the
  spec's `env`, or an existing `~/.aws/config`), nothing is changed and
  `command.log` says so. The terminal-flush hook writes its own config pinned
  to the classic client, because the CRT client does not honour the
  connect/read timeouts that keep the hook inside its 45 s deadline; this also
  stops AWS CLI 2.38+, whose `auto` picks CRT on every host, from dropping
  them. Output uploads that go through spored's provenance uploader are not
  affected.
- **The TTL reaper and `spawn cleanup --instance-roles` delete per-task
  `spawn-instance-*` IAM roles once nothing uses them** (#1300). A task's role
  and instance profile are named after a hash of its policy, so every distinct
  set of buckets minted a new pair and nothing deleted them, walking an account
  towards the IAM role quota. A role and its profile are now deleted only when
  both are tagged `spawn:managed=true`, no instance in any region enabled in
  the account (pending through stopped; `ec2:DescribeRegions`, not only the
  regions the reaper scans) has the profile attached, the role and its profile
  are at least 24 hours old, and the role is unused for 24 hours; if the region
  list or any region cannot be read nothing is deleted. `spored-instance-role` is never touched, and IAM itself denies
  the reaper any other name: the new grants in `pkg/reaperiam`, the SAM
  template and `ttl-reaper-cross-account-role.yaml` allow the deletes only on
  `spawn-instance-*` roles and profiles tagged `spawn:managed=true`, and add
  `ec2:DescribeRegions` (read-only) for the region list. The
  reaper's sweep has its own switch, `REAPER_INSTANCE_ROLES` (template
  `InstanceRoles`, make `INSTANCE_ROLES`, `spawn reaper deploy
  --instance-roles`): `off`, `report` or `reap`, default **`report`**, which
  logs `WOULD delete role ...` and deletes nothing; a deploy that does not
  name it keeps the deployed value. The run summary gains
  `instance_roles_scanned`, `_deleted`, `_would_delete`, `_skipped` and
  `_accounts_denied`. Member accounts need the role stack redeployed for the
  grant; until then they are counted in `instance_roles_accounts_denied`.
  `spawn cleanup --instance-roles` applies the same rule (`pkg/instancerole`)
  from a workstation, checking every enabled region, with `--dry-run`, and
  `spawn footprint` names these two as the roles' deleters. `DELETED` is now an
  action word for `spawn reaper teardown --if-idle-for`.
- `taskproto.FlushCommandWithOutputs` and `taskproto.FlushOutputsSlackSeconds`
  (#1312): the SSM flush command with an output grant, and the slack every
  caller adds to its bound.

- **A Spot pre-stop now fits inside the 2-minute notice (#1311).** The task
  flush and the user's `--pre-stop` hook used to have separate bounds, up to 90
  s each, so together they could outlast the notice. The hook ran last, so EC2's
  reclaim cut it off and nothing reported it. Both now share one deadline: the
  interruption time minus 25 s, which leaves room for the Elastic IP release and
  the notifications. The task flush is sized first: its record gets 45 s, then
  the declared outputs get what is left (at most 45 s, previously 40 s). The
  user's hook then gets at most the time remaining, even when
  `--pre-stop-timeout` is set. If no time remains, the hook is not started and a
  `pre_stop_failed` notification says so. A flush whose deadline has already
  passed is not started either; it is recorded as
  `spawn:flush-status=skipped:no-time-left`.
- **`spawn terminate` and the TTL reaper now stage a task's declared outputs
  (#1312).** Their flushes over SSM used to grant no output seconds, so they
  recorded `outputs_staged: "skipped"`. The hook now gets the budget it declares
  in its `# spawn-flush-outputs-seconds:` header as `$2`, the same grant spored
  passes. `spawn terminate` grants what the task declares in
  `lifecycle.flush_outputs_seconds`, up to that field's 60 s default rather than
  its 300 s maximum, because the instance bills for the whole wait. It waits for
  the hook's on-box bound (45 s plus the grant plus 5 s) and 30 s of SSM slack,
  so at most about 2.3 minutes, and it prints how long it may wait, then the
  remaining wait every 30 s. The reaper ends instances one after another inside
  its 900 s invocation, so it grants at most 30 s, and none once fewer than 5
  minutes of the invocation remain. A hook with no header, which includes every
  hook written before v0.146.0, runs as before. The reaper change takes effect
  when the reaper is redeployed.
- **`spawn slurm convert` and `spawn slurm submit` refuse multi-node MPI jobs**
  (#1242). Every param-file row is one independent instance, so a
  `--nodes=N` job used to convert to a single row and launch one instance with
  no MPI setup, after a cost estimate that priced all N nodes. They now stop
  with an error pointing at `spawn launch --mpi --count N`. `submit` converts
  before asking for confirmation, so this comes before the cost prompt.
- **`spawn slurm submit` launches in the region it priced, in the foreground**
  (#1217). It pins the converted file to the estimate's region (`--region`, else
  `#SPAWN --region`, else your AWS config region), so the launch can no longer
  land in a region nobody quoted, and it passes `--no-detach`, because a
  detached sweep does not run row commands yet (#1323). An `--array=...%N`
  limit becomes `--max-concurrent N`.
- **`{{ instance.id }}` in plugin local steps is the EC2 instance ID** (#1225).
  It was the `--instance` value as typed, which for `--instance my-box` is a
  name. It is now the ID when spawn can resolve the instance, as it is in
  remote steps, and the typed value only when it cannot.

- **Generated S3 write grants now include `s3:AbortMultipartUpload`** (#254).
  The task, `--s3-write`, pipeline-stage and pool-worker policies granted
  `s3:PutObject` alone. The SDK's transfer manager (`spored provenance upload`,
  the pipeline stage hand-off) and `aws s3 cp` split large files into parts
  and abort the upload when a part fails, and that abort is a separate action.
  Without it, a failed upload's parts stayed in the bucket, billed, until a
  lifecycle rule removed them. The action is added on the same object ARNs as
  `s3:PutObject`. Because `spawn-instance-*` role names are a hash of their
  policy, the first launch after upgrading creates new roles. The old ones
  are removed by `spawn cleanup --instance-roles`, or by the reaper's role
  sweep with `InstanceRoles=reap` (its default is `report`), once they are
  unused (#1300).

### Fixed

- **Detached sweeps and pipeline stages can no longer launch with a
  nested-spawn role** (#1372). The orchestrators' `iam:PassRole` on
  `role/spawn-instance-*` also matched the `--nested-spawn` roles
  (`spawn-instance-nested-*`), which may themselves call RunInstances and
  PassRole, so a param row naming one would have been given that capability
  without the `--ttl` `--nested-spawn` requires. The CLI now refuses such a
  profile in `--instance-profile`, `--iam-role`, `defaults` or a row before a
  detached or scheduled sweep uploads anything ("nested spawn isn't available
  for detached sweeps; use --nested-spawn on a single launch, which requires
  --ttl"); the sweep orchestrator refuses the member before RunInstances, for
  CLI-rendered and legacy params alike; the pipeline orchestrator refuses a
  stage whose record names one; and both orchestrator roles
  (`SpawnSweepCrossAccountRole` from `scripts/setup-sweep-cross-account-role.sh`,
  and the pipeline orchestrator's role in its template) carry an explicit,
  unconditional `Deny` of `iam:PassRole` on `role/spawn-instance-nested-*`.
  Deploy: re-run `scripts/setup-sweep-cross-account-role.sh` in each member
  account, redeploy the sweep orchestrator Lambda
  (`scripts/deploy-sweep-orchestrator.sh`) and the pipeline orchestrator stack.

- **`spawn launch --tag` and `--team` now reach every member of a parameter
  sweep and the batch-queue instance** (#1356). They were merged only on the
  single-instance path, after the sweep and batch-queue launches had been
  handed off, so on `--param-file`/`--params` (foreground, detached and
  multi-region) and `--batch-queue`/`--queue-template` they were accepted and
  silently dropped: no member carried the custom tags, `spawn:team-id` or
  `spawn:team-name`. A malformed or `spawn:`-prefixed `--tag` is now refused
  on every path before any AWS call, as on a single launch, and the 50-tag
  pre-flight (#1336) now counts the `--tag` values on a sweep and runs on a
  batch queue too, before its upload, key pair and IAM role. `spawn schedule
  create` has neither flag, so a scheduled sweep cannot drop them.

- **A detached sweep member now launches with its RunInstances settings**
  (#1353). The sweep orchestrator sent RunInstances only the instance type,
  AMI, key, profile, user data and tags, so a detached member dropped
  `volume_size`/`--volume-size` (and the AMI-snapshot minimum),
  `--security-group`, `--subnet`, `az`, `spot_max_price`, hibernation
  (`hibernate`, `--on-idle hibernate`, `on_complete: hibernate`, with its
  RAM-sized encrypted root), `--efa`, `--placement-group`,
  `--reservation-id`/`--capacity-block`, `--nested-virtualization` and
  `--attach-volume`, and got no public-IP network interface. The CLI now
  resolves each member's settings as a foreground launch does and ships them
  as `spawn_launch_settings`, a versioned schema (`aws.LaunchSettings`);
  the orchestrator applies them with the code `Client.Launch` uses. Params
  that carry settings are held to them: the orchestrator refuses a member
  whose settings are of another schema version, hold a field it does not
  know, or cannot be applied, rather than launching without them. Params with
  no `spawn_launch_settings`, from an older CLI, launch as before (the bare
  `spot` flag and nothing else), with one log line per sweep. So do the
  members of a schedule an older CLI created: scheduler-handler replays the
  params uploaded at `spawn schedule create` on every firing, so such a
  schedule stays on the old behaviour until it is re-created. (Such params
  also carry no `spawn_user_data` or `spawn_tags`; the orchestrator renders
  those itself, #1368.) What the orchestrator cannot honour is refused at `spawn launch
  --detach`, with the remedy: `--fsx-create`, `--fsx-recall`,
  `--efa-interfaces` above 1, a customer-managed EBS KMS key, `--vpc` without
  `--subnet`, and hibernation with an instance-type pattern (a foreground
  sweep also drops the first two and `--vpc`, #1366). Deploy the orchestrator
  before or with the CLI release: an older orchestrator ignores the settings.
- **Cancelling a sweep from the dashboard now terminates its instances**
  (#1349). The dashboard's cancel assumed `SpawnSweepCrossAccountRole`, whose
  trust names only the sweep orchestrator, so the assume was always denied: the
  sweep was marked CANCELLED and its members ran on until their TTL. The
  dashboard no longer assumes any role. It marks the sweep CANCELLED, as
  before, and invokes `spawn-sweep-orchestrator` asynchronously; the response
  says the orchestrator is terminating the instances (`instances_terminated`
  is now always 0; `instances_to_terminate` is new). The orchestrator now
  terminates a cancelled sweep's live members (pending, running, stopping,
  stopped) in every region of the sweep, whichever way the cancel reached it:
  its polling loop finding the flag, the dashboard's invoke, or a
  self-reinvocation that starts after the cancel. Members are found by their
  `spawn:sweep-id` and `spawn:managed=true` tags, which are checked on every
  instance, so a member launched after the last record save is included and
  another sweep's instances or untagged ones are not touched. A Spot member's
  request is cancelled before its instance, as on spawn's other terminate
  paths. A re-run finds nothing live and writes nothing. The outcome is
  recorded with an UpdateItem (status CANCELLED, `cancel_terminated` count)
  rather than a put of the orchestrator's copy, and the polling loops' own
  saves are now conditional, so a loop that read the record before the cancel
  can no longer overwrite it back to RUNNING. Previously the orchestrator
  stopped launching on a cancel but left the members running. Needs
  `lambda:InvokeFunction` on the orchestrator for `SpawnDashboardLambdaRole`
  and `ec2:CancelSpotInstanceRequests` for `SpawnSweepCrossAccountRole`, both
  added to their setup scripts. If the dashboard role has not been updated,
  the cancel still succeeds, a running orchestrator still acts on it within
  one poll, and the message says to run `spawn cancel <id>` if the instances
  are still running. Instance terminate from the dashboard
  in a member account stays as it is: the 403 names `spawn terminate <id>`
  (owner decision).

- **A launch over AWS's 50-tag limit is refused before anything is created**
  (#1336). spawn only budgeted the droppable `spawn:param:*` tags, so a launch
  with many `--tag` values, or a maximal launch with every spored setting
  (`--idle-cpu-threshold`, `--metrics` and its port, bind and path,
  `--tracing` and its sampling: 54 tags), reached RunInstances, which rejected
  it after the key pair, IAM role, security groups, Strata lockfile or durable
  FSx had been created. Now `spawn launch`, a parameter sweep (foreground and
  detached), `--estimate-only` and `--dry-run`, and `launcher.Provision` (task
  runs, services, SDK callers) refuse it before their first AWS call, with an
  error giving the count and the limit and naming what can be dropped: the
  number of `--tag` values and each spored setting that added a tag. The
  count includes the tags spawn writes after RunInstances: a Spot instance's
  `spawn:spot-price-per-hour` (on a persistent request, the request's
  `spawn:original-instance-id`), and an ephemeral FSx's four mount tags. A
  Spot sweep member therefore keeps one fewer `spawn:param:*` tag when
  parameters are dropped to fit. The documented monitoring setup
  (`--metrics --metrics-bind 0.0.0.0 --idle-cpu-threshold`) still fits on a
  maximal launch. Snapshot tags copied onto an attached volume are now
  limited to what fits beside the launch's tags (the rest are named in a
  warning), and the sweep orchestrator refuses a member carrying more than 50
  tags. New in `pkg/aws`: `LaunchConfig.ValidateTagCount` and `TagLimitError`.
- **A terminated instance's cost no longer keeps growing** (#1341). The
  single-instance `spawn cost` report, the EC2-side `spawn status` report and
  `spawn task diagnose` priced EBS and Elastic IPs from first launch to now,
  whatever the state, so a terminated instance was shown accruing storage and
  IPv4 spend that nothing bills. For a terminated or shutting-down instance the
  window now ends at the termination time EC2 gives in StateTransitionReason,
  shown as `Terminated:` (and `terminated_at` in `spawn status --output json`).
  When that time is no longer available (EC2 clears it about an hour after
  termination), storage and Elastic IPs are reported as not counted, with the
  reason, and the total as "at least" (`termination_time_unknown` in JSON).
  Running and stopped instances are unchanged; `--cost-limit` enforcement is
  unaffected.
- **`spawn pool create` tags its SQS queue** (`spawn:managed=true`,
  `spawn:pool-run-id`), so an undrained pool's queue can be recognised and
  deleted (#1351). Tagging on create needs `sqs:TagQueue`; without it the pool
  is still created, untagged, with a warning that only `spawn pool drain` will
  delete its queue.

- **An instance launched from an image of an MPI cohort member no longer
  authorizes that cohort's cluster key for root** (#1343). The bootstrap's
  `spored discard-inherited-state`, and spored when it starts, now remove
  every line of `/root/.ssh/authorized_keys` whose last field is exactly
  `spawn-mpi-cohort`. They do this once per instance, recorded in
  `/var/lib/spawn/root-ak-owner`, so a source whose bootstrap runs again
  (`cloud-init clean` and the CreateImage reboot) keeps its live cohort's
  key. Every other line is kept byte for byte, and the log gives only the
  count. For `--mpi` the strip runs before the MPI script installs this
  cohort's key. The SSM key install records the instance too, so a strip that
  comes after the install leaves the new key alone. `spawn ami create` does
  not touch the source's keys. **Rebuild images made before this release
  from a non-zero MPI rank**: releases before #1338 installed rank 0's key
  untagged (`root@ip-...`), and nothing on the instance tells that line from
  a user's key. Nor can spawn find such images: an image records only
  `spawn:managed`, `spawn:created`, `spawn:created-from`,
  `spawn:source-region`, `spawn:arch`, `spawn:gpu`, `spawn:base-ami` and your
  `--tag`s, not the source's MPI tags or spawn version (#1364 tracks recording
  them). To rebuild, launch from the image, delete the `root@ip-...` line in
  `/root/.ssh/authorized_keys` that is not yours, then run `spawn ami create`
  again.

- **A pool nobody drains no longer keeps its IAM role forever** (#1319).
  `spawn pool drain` was the only thing that deleted a pool's worker role and
  instance profile, so a pool whose caller crashed, or whose run-end hook
  never fired, left them in the account after its workers hit their TTL.
  `spawn pool create` now names them `spawn-instance-pool-<hash of the run
  id>` (was `spawn-pool-worker-<hash>`), inside the per-task family the TTL
  reaper (`REAPER_INSTANCE_ROLES`) and `spawn cleanup --instance-roles`
  already remove (#1300), under the same rule: tagged `spawn:managed=true`,
  no instance in any enabled region holding the profile, the role and profile
  at least 24 hours old and the role unused for 24 hours. A pool role must
  also carry a `spawn:pool-run-id` tag that hashes to its name. No new IAM
  grant: the reaper's existing `spawn-instance-*` grants cover the new name.
  Roles of pools created before this release keep the old name, which the
  reaper's grants do not reach: `spawn cleanup --instance-roles` removes those
  under the same rule (and only the exact `spawn-pool-worker-<16 hex>` name
  its run-id tag hashes to), and `spawn pool drain` removes both names.
- **`spawn launch --batch-queue` can upload its results** (#1344). With no
  `--iam-role` or `--instance-profile`, a batch queue ran on the shared
  `spored-instance-role`, which may write to no user bucket, so every results
  and final-state upload from `spored run-queue` failed with AccessDenied
  unless you brought your own role. A queue now gets a `spawn-instance-*` role,
  as `spawn task run` does: the spored baseline, plus `s3:PutObject` and
  `s3:AbortMultipartUpload` on `arn:aws:s3:::<result_s3_bucket>/<result_s3_prefix>/*`
  and `s3:GetObject` on the queue files in the schedules bucket
  (`schedules/*/params.yaml`). Queues with the same results location share one
  role, and the reaper deletes it once no instance uses it (#1300). A
  `result_s3_bucket` that is not a plain bucket name, or a `result_s3_prefix`
  containing `*`, `?` or `$`, is now refused before anything is launched.
  `--iam-role` and `--instance-profile` are still attached as given.
- **The dashboard Lambdas' role script now grants what the Lambdas call**
  (#1344). `lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh`
  granted reads on two tables and one `UpdateItem`, far behind dashboard-api,
  dashboard-websocket, dashboard-websocket-processor and cost-history-collector,
  so running it would not have reproduced the hand-managed live role. It now
  writes one inline policy with `put-role-policy`, so a re-run converges on it.
  Every statement is scoped to the tables, indexes and keys the code uses:
  the user-accounts, sweep, team, membership, WebSocket ticket and connection,
  cost-history, lagotto watch/history and Slack workspace tables (names follow
  the Lambdas' env overrides); the processor's two DynamoDB streams; `GetObject`
  on the strata registry and `PutObject` on its `locks/` prefix
  (`STRATA_REGISTRY_BUCKET`, default `strata-registry`). Two grants are written
  only when their resource exists: `secretsmanager:GetSecretValue` on the
  `SLACK_SECRETS_ARN` secret (from the env or the deployed function's
  configuration), and `execute-api:ManageConnections` on the
  `spawn-dashboard-websocket` API's `@connections`. It detaches, and if nothing
  else uses it deletes, the customer-managed policy earlier versions created.
- **The dashboard's member-account read role is read-only and narrow**
  (#1344). `setup-cross-account-role.sh` attached the AWS-managed
  `AmazonEC2ReadOnlyAccess` and `AmazonDynamoDBReadOnlyAccess`. Every
  signed-in dashboard user can assume this role directly, so any of them could
  read every EC2 resource and every DynamoDB table in the member account. The
  role now carries one inline `ec2:DescribeInstances` grant, the only EC2 call
  the dashboard makes on it, and the script detaches the two managed policies.
  Its trust names `Cognito_SpawnDashboard_Auth_Role`, as the production role's
  does, instead of the whole infra account.
- **Terminating from the dashboard in a member account explains why it
  cannot** (#1344). The read role does not grant terminate, and should not:
  every dashboard user can assume it directly, so a terminate grant would skip
  the dashboard's ownership check. The dashboard now answers 403 with the
  `spawn terminate <id>` command instead of a raw 500. Choosing a terminate
  path is spawn#1349.
- **A detached sweep member now gets its configuration** (#1330). The sweep
  orchestrator wrote five tags of its own (`spawn:sweep-id`,
  `spawn:sweep-index`, `Name`, `spawn:managed`, and `spawn:ttl-deadline` for
  a row with a `ttl`), while spored and the bootstrap read everything else
  from tags. So a member booted spored and then ran nothing: a row's
  `command:` (the `spawn:command` tag) and its `PARAM_*` values never reached
  it, and `idle_timeout`, `cost_limit`, `on_complete`, `completion_file`,
  `completion_delay`, `session_timeout`, `dns`, `--pre-stop`, `--tag` and the
  rest were not enforced. A `cost_limit: 8` capped nothing. The CLI now
  renders each member's tags with the code a foreground launch uses
  (`aws.LaunchTagger`, after the foreground's per-member defaults) and ships
  them in the params as `spawn_tags`. The orchestrator tags the instance and
  its volumes with them, anchors `spawn:launch-time` and `spawn:ttl-deadline`
  at the real launch (so a TTL counts from when the member started, not from
  when the CLI ran), and sets the sweep ID itself, since a schedule's run gets
  its ID only when it fires. Params from an older CLI carry no rendered tags;
  the orchestrator renders those members' tags itself (#1368). Each
  instance type is priced once per sweep, not once per member.
- **`--iam-policy`, `--iam-policy-file`, `--iam-managed-policies`,
  `--s3-read` and `--s3-write` now work on a detached sweep** (#1331). They
  give members a `spawn-instance-<hash>` profile, and the orchestrator's
  cross-account role could not pass its role, so EC2 refused every member.
  `scripts/setup-sweep-cross-account-role.sh` now grants `iam:PassRole` on
  `role/spawn-instance-*` too, still only to EC2
  (`iam:PassedToService=ec2.amazonaws.com`). **Re-run it in each member
  account.**
- **A sweep row's `user_data:` now runs** (#1327). It was parsed and then
  overwritten by the foreground path and ignored by the detached one. It runs
  after spored's bootstrap and after `--user-data`, on both paths, and on
  `spawn resume`. A Windows member with one is refused, since its bootstrap
  would drop it.

- **`spawn slurm submit` works** (#1217). It handed off to `spawn launch
  --param-file` without a spore name, which launch refuses, so every submit
  failed after the user had approved the cost. It now passes a name made from
  the script's `--job-name` (else the script's file name) and reduced to a valid
  DNS label, since launch registers the name in DNS and uses it as the hostname.
- **`spawn slurm convert` output can be launched** (#1242). It wrote each job
  script under a `script:` key, which `spawn launch` rejects (and nothing on the
  instance would have run). The script now goes under `command:`, array members
  get distinct valid names, and the `%N` limit is no longer written as the
  `max_concurrent:` key launch rejects. The printed launch line now names the
  spore and uses `--param-file` (it said `--params`, which is not implemented).
- **A parameter-sweep row's `command:` reaches the instance when it is long or
  multi-line** (#1242). It rode only the `spawn:command` tag, which EC2 caps at
  256 characters, so such a row was refused by RunInstances. A command the tag
  cannot carry is now embedded in the row's user data, as `--command` is on a
  single launch; short one-line commands keep the tag. A row's own `command:`
  also now wins over a CLI `--command`, as the documented row > flag precedence
  says; before, the CLI value took the tag.

- **The sweep orchestrator never launches a detached sweep member without a
  spored bootstrap** (#1323). The CLI renders it into the params; for params
  from an older spawn, which carry none, the orchestrator renders it (#1368).
  `spawn schedule create` now builds every member's launch config, so a
  schedule row key that `spawn launch --param-file` would reject (a misspelled
  spawn key, for example) is now rejected here too. A file may not set
  `spawn_user_data`, which is spawn's own key.
- **The orchestrator's fallback instance profile is now `spored-instance-profile`**,
  the one spawn creates, instead of `spawnd-role`, which only a hand-run script
  made. The CLI now ensures `spored-instance-profile` for every detached or
  scheduled member that names no `iam_role`, as the foreground path does.

- **P0: detached sweep members had no in-instance TTL** (#1323, #1313). A
  parameter sweep runs detached by default, launched by the sweep-orchestrator
  Lambda, and it launched every member with no user data, so spored never ran
  on one: no TTL, idle stop, cost limit or completion handling inside the
  instance. Only the TTL reaper ended them, on its next cycle and only in the
  regions it covers. Members now boot the same spored bootstrap a foreground
  sweep member gets: the CLI builds it with the same code and ships it in the
  sweep's params (a release build fetches that release's published body; a dev
  build carries it inline), and the Lambda launches each member with it and with
  an instance profile. The orchestrator also checks each member's spored as the
  CLI does for its fleets (#1305): a member still running with no
  `spawn:last-heartbeat` 5 minutes after launch (10 on Windows) is terminated and
  counted as a failed row, so it frees its `--max-concurrent` slot, and a sweep
  in which no member came up records FAILED, in multi-region sweeps too. Needs
  the sweep-orchestrator redeployed (`scripts/deploy-sweep-orchestrator.sh`) and,
  in each member account, `scripts/setup-sweep-cross-account-role.sh` re-run: its
  role may now pass `spored-instance-role` (and `spawnd-role`, as before) to EC2,
  and nothing else.
- **`spawn connect <i> -- <cmd>` over SSH exits with the remote command's
  status** (#1306) instead of 1 for any failure, the same as over SSM; ssh's
  own failures exit 255, as with ssh.

- **`spawn connect <linux> -- <cmd>` runs the command when connect uses SSM**
  (#1306). With no public IP, `--session-manager`, a failed key injection, or
  security groups closed to SSH, it opened an interactive Session Manager
  session, which dropped the command and waited on stdin; v0.145.0 made the
  closed-port case an immediate error instead. It now runs the command with SSM
  RunCommand (`AWS-RunShellScript`), as the same user SSH would log in as, in
  their home directory with their login shell, and exits with the command's
  status, with stdout and stderr kept apart. It does not need the AWS CLI or the
  session-manager-plugin. Output arrives when the command exits, SSM keeps only
  the first 24,000 characters of stdout and 8,000 of stderr (connect warns when
  output may have been cut), stdin is not forwarded, `--tty` has no effect, and
  the command is limited to an hour.
- **An instance launched from an image no longer inherits its source
  instance's plugins, job-array identity, peers, queue or flush hook** (#1241).
  `spawn ami create` (or any CreateImage) snapshotted the per-instance state
  the bootstrap and spored write on the root volume, so a child loaded the
  parent's `/etc/spawn/plugins.json` and plugin state, sourced the parent's
  `job-array.sh`, found the parent's `job-array-peers.json` (so MPI built its
  hostfile from the parent cohort's addresses without waiting for its own),
  and carried the parent's queue, command, push token and task flush hook,
  which writes to the parent's results prefix. spored now records the
  instance that owns this state in `/var/lib/spawn/instance-owner`; on an
  instance where it names another instance, the state written before the
  current boot is discarded and the new owner recorded, while anything written
  during this boot (the launch's own plugins and command, a peers file pushed
  over SSM) is kept. The bootstrap runs this as `spored
  discard-inherited-state` right after installing spored, before spored or
  any workload starts, and the daemon does it at startup, which covers images
  launched by something other than spawn. `spawn ami create` records the
  source as the owner before the snapshot, so images of instances launched by
  older releases are covered too; the source keeps its state. The paths are
  discovered from the source by a gate, not listed by hand. Not covered: an
  image of an older instance, made outside `spawn ami create`, booted without
  spawn's bootstrap and with that older spored, and Windows instances.
- **A flaky gate, `TestNoGeneratedPolicyReachesTheStagingBucket`, failed `make
  check` on an unmodified tree** (#1334). `TestReleaseBuildLeavesTheTreeClean`
  created and removed `dist/` and the before-hook outputs inside the checkout
  while tests in other packages walked it, so a walk could list an entry and
  fail to stat it (11 failures in 50 concurrent runs). The release gate now
  probes a temporary work tree against the checkout's own index and ignore
  rules, and every test that walks the source tree (33 of them, found by a
  gate that parses every test's walk callback) skips an entry that vanishes
  mid-walk.
- **`--cost-limit` now counts public IPv4, every attached EBS volume's
  provisioned performance, and volumes or addresses added after boot**
  (#51). Every under-count made the cap fire late:
  - Public IPv4 was not counted at all. AWS charges $0.005/hr per public IPv4
    address, in use or idle (VPC pricing page, fetched 2026-10-26). spored now
    counts the instance's actual addresses on every interface
    (`spawn:public-ipv4-count`, `spawn:elastic-ipv4-count`): an auto-assigned
    address over running time, since it is released on stop, and an Elastic
    IP, including the launch EIP of a multi-interface EFA launch, over the wall
    clock, since it bills while stopped too.
  - EBS was priced on size alone. Provisioned IOPS (gp3 above 3,000, io1, io2)
    and gp3 throughput above 125 MB/s are now counted too. An io2 volume's IOPS
    are often most of its cost.
  - The footprint was measured once, at first boot, and kept for the life of
    the instance. A volume attached later, or an Elastic IP associated after
    spored's first look, was never counted. spored now re-measures on every
    start and every 15 minutes (two read-only EC2 calls it already had
    permission for). A recorded rate only goes up: lowering it would forgive
    cost already incurred.
  - With no `spawn:launch-time` tag, EBS was dropped from the cap entirely.
    EC2's pending time is now the fallback anchor.
  - `spored status` priced EBS over stopped time only, so on a running
    instance it showed none of the EBS the cap was counting. It now prices
    EBS over the time since launch.

  `spored`'s enforcement, `spored status`, `spawn status`, `spawn cost` and
  `spawn extend`'s cost-limit check now share one function,
  `pkg/costmodel.Accrue`, so the figure shown is the figure enforced. The
  static rates are labelled with their source and date (`costmodel.RatesAsOf`).
  An instance whose spored predates this keeps its old enforcement until
  `spawn upgrade-spored`. `spawn status` and `spawn cost` say when IPv4 is not
  counted for that reason.
- **Security: an MPI cohort launched from an image of a rank-0 instance no
  longer reuses the parent cohort's root SSH key** (#1338). Rank 0 minted the
  cluster key only when `/root/.ssh/id_rsa` was missing, and every
  `authorized_keys` write appended, so a child cohort reused the parent's
  private key, every member authorized it, and root's `authorized_keys` kept
  the parent's public key: whoever held the parent's key could ssh in as root.
  Rank 0 now mints a fresh key unless `/root/.ssh/spawn-mpi-key-owner` names
  its own instance (so a reboot of the same instance keeps it), every cluster
  key carries the comment `spawn-mpi-cohort`, and each install replaces the
  lines carrying it instead of appending; rank 0 also drops the line matching
  the key it replaces, which covers images made before this release. The
  control plane reads rank 0's public key only once the owner file names that
  instance, so the inherited key cannot be handed out before `ssh-keygen` runs.
  The `Host *` block in root's ssh config is written once rather than on
  every run. Behaviour change: a `/root/.ssh/id_rsa` already on a custom
  image is replaced on MPI rank 0 (it used to be adopted as the cluster key
  and authorized on every member). A peer image made by an earlier release
  still carries the old untagged line; see the #1343 entry.

- **`spawn cleanup` no longer offers spawn's S3 buckets for removal and then
  fails on them** (#1301). With `--all` it listed a `spawn:managed` bucket as
  removable and the delete failed with "don't know how to remove"; buckets are
  now reported as not removed, with a pointer to `--buckets`. Discovery also
  names a bucket by its name instead of reporting the name as its type.
- **The results bucket is no longer tagged as an FSx backing bucket** (#1301).
  `CreateS3BucketIfNotExists`, which creates `spawn-results-<account>-<region>`
  and the ISO import bucket, tagged every bucket `spawn:fsx-backing-bucket=true`.
  It now tags them `spawn:managed=true`, plus `spawn:purpose=task-results` on the
  results bucket. FSx buckets keep their tags.
- **A launch-time plugin that cannot be installed now shows as failed, with
  the reason** (#1226). When a `spawn launch` plugin's spec or config did not
  resolve (a missing required key, an undeclared one such as tailscale's old
  `auth_key`, or a spec that could not be fetched), spored only logged it, so
  `spawn plugin status` said "not installed". It now records a `failed` state
  carrying the error, which `spawn plugin status` and `spawn plugin list`
  show. A plugin already running is left alone if its spec cannot be fetched
  on a later boot.
- **Plugin local steps get `{{ instance.login_user }}`** (#1225). Provision,
  reconcile and deprovision steps run on the controller got only
  `instance.id`, `instance.name` and `instance.ip`, so a plugin that SSHes to
  the instance had to hardcode `ec2-user`, which is wrong on Ubuntu. They now
  get every `instance.*` variable remote steps get: `login_user` is the
  instance's `spawn:local-username` tag, else `ec2-user`, and is kept in the
  local record for later reconcile and deprovision.
- **spored's idle terminate (`spawn:terminate-on-idle`) now sends a
  notification** (#1205). `spawn service` instances and pool workers that
  terminated at their idle timeout sent no Slack, Discord or SMS message. They
  now send one once EC2 has accepted the terminate. It is spore-bot's
  "has terminated" message (`ttl_expired`), which names the scheduled end time
  as the reason, until spore-bot lists an idle-terminate event.

- **A reused instance profile with no role in it is repaired before launch**
  (#1318). `CreateOrGetInstanceProfile` returned an existing
  `spawn-instance-*` profile as it found it, so a profile left empty (a
  cleanup or reaper delete that had removed the role but not yet the profile,
  or a launch that died between creating the profile and attaching the role)
  booted an instance with no credentials, and spored could not read its tags
  or end itself. It now re-attaches the role, and refuses a profile that holds
  a different role.

- **The pipeline orchestrator can send its notifications** (#254). Its SAM
  template granted no SNS actions, so creating a pipeline's
  `spawn-pipeline-<id>` topic, subscribing the notification email and
  publishing to it were all denied. It now grants `sns:CreateTopic`,
  `sns:Subscribe`, `sns:ListSubscriptionsByTopic` and `sns:Publish` on
  `spawn-pipeline-*` topics in its own account and region, plus
  `ec2:DescribeInstanceTypes`, which the AMI architecture lookup calls.
  Redeploy the stack to pick it up.
- **Multi-region sweeps can validate their regions, and clean up their
  placement groups, under `SpawnSweepCrossAccountRole`** (#254). The
  orchestrator checks each region with `ec2:DescribeAvailabilityZones` on the
  assumed role and marks every parameter in a region that refuses it as
  failed ("Region access denied"); the role's setup script did not grant it.
  The script now grants it, and `ec2:DeletePlacementGroup` scoped to
  `spawn:managed=true`, which the orchestrator calls on `spawn-mpi-*` groups
  when a sweep ends. Re-run `scripts/setup-sweep-cross-account-role.sh` in each
  member account.

### Security

- **`github.com/go-jose/go-jose/v4` 4.1.4 → 4.1.6** (CVE-2026-116587, a DoS when
  parsing JWKs that contain RSA keys). An indirect dependency of the root module.

## [0.146.0] - 2026-10-25

### Added

- **`spawn task status` shows the outcome of spored's terminal flush** (#1295).
  spored has recorded it in the instance's `spawn:flush-status` tag since
  v0.144.0 (#1186), but nothing in the CLI read it. A task that terminated with
  no completion record now gets a `Flush:` line naming the tag (`ok`,
  `failed:<reason>` or `skipped:<reason>`): for a failure, the
  `aws ec2 get-console-output` command that shows the hook's output; for `ok`,
  a pointer to `--results-prefix`, since the record was written somewhere
  status did not look; with no tag, that the instance ended without spored's
  flush. A task with a record gets a compact `Flush:` line under it. Once EC2
  stops listing the instance, about an hour after it ends, status says the
  flush status is no longer available. The tag is read from the instance list
  status already fetched, so a task with no record costs no new EC2 call; a
  task with a record now costs one `DescribeInstances` (best-effort: a failed
  lookup leaves the line out). `tasklaunch.ReadFlushStatus` is the exported
  reader, for the MCP server too, with `taskproto.FlushStatusTag`.

### Changed

- **A task killed by a lifecycle limit now stages out its declared outputs,
  best-effort and time-boxed** (#1297). A TTL, `lifecycle.cost_limit`, idle or
  Spot kill used to upload `command.log` and `completion.json` but none of the
  spec's `outputs`, so a capped run's partial evidence (a tool's own log, its
  counters) was lost with the disk. The terminal-flush hook now copies each
  declared output as the wrapper does (`aws s3 cp`, `--recursive` for a
  directory, untagged), after the log and before the record, within a budget
  set by the new `lifecycle.flush_outputs_seconds` (default 60, `0` disables
  it, at most 300). spored grants that budget and extends the hook's 45 s
  timeout by it plus 5 s, so the log and record still land; on a Spot
  interruption it grants at most 40 s, so the whole flush stays within 90 s of
  the 2-minute notice. The record states the outcome in the new
  `outputs_staged` (`complete`, `partial`, `skipped` or `disabled`) and names
  each output that did not land in `outputs_not_staged`, with its reason
  (`missing`, `failed` or `timeout`). Both fields are additive and omitted when
  the spec declares no outputs; a reader must treat their absence (any older
  record) as not reported. `terminal_reason` is unchanged. **The seconds are
  billed: on a `cost_limit` kill they are a bounded overshoot of the limit**,
  at most `flush_outputs_seconds` plus 5 s of the instance's rate; set it to
  `0` to keep the old behaviour. The flush that `spawn terminate` and the TTL
  reaper run over SSM still stages no outputs (their waits leave no room), and
  records `skipped`. A hook from before this release is run exactly as before.

- **Release binaries and CI are now built with Go 1.27.3** (was 1.26.10), per
  spore-host/spore-host#609. `go.mod` still declares `go 1.26.0`, so programs
  that import spawn's packages are not forced onto Go 1.27. The pinned linters
  (staticcheck 2026.2.2, golangci-lint 2.15.0, govulncheck v1.3.0) were
  verified under 1.27.3 and are unchanged.

### Fixed

- **`spawn list` shows instances that are shutting down** (#1167). The default
  listing left out `shutting-down`, so a GPU instance that sits in that state
  for up to half an hour after terminate, still billing and holding Spot vCPU
  quota, was missing from `spawn list --output json`, and a teardown check built
  on it passed while the instance was still there. The default (also `--state
  live`) is now pending, running, stopping, stopped and shutting-down; the table
  colours `shutting-down` and ends with a note that those instances are still
  billed. `--state active` gives the previous listing. Only `spawn list`
  changes: the commands that act on an instance (terminate, extend, connect,
  resume, the cohort adapters) keep skipping shutting-down instances.
- **`spawn footprint` lists EventBridge Scheduler schedules** (#1219), the
  schedules `spawn schedule` creates and that keep launching instances while
  they exist. It lists every schedule group and identifies spawn's by their
  target (the `scheduler-handler` Lambda), a spore.host name or group, or, for
  a schedule named like spawn's (`sched-YYYYMMDD-HHMMSS`), spawn's description.
  This needs `scheduler:ListSchedules` (and `scheduler:GetSchedule` for the
  description check); without them the report says so under "Could not read"
  and lists everything else.
- **The TTL reaper counts a failed security group or placement group delete as
  an error** (#1211). It recorded the describe call's nil error instead, so a
  failed delete, including a denied `ec2:DeleteSecurityGroup` or
  `ec2:DeletePlacementGroup`, was counted as a successfully reached region and
  never reached `Summary.Errors`. It now counts as an operational error, and the
  `nilnesserr` linter that found it is enabled. Takes effect when the reaper is
  redeployed.
- **The self-hosted monitoring doc no longer tells you to run `spawn launch
  --tag spawn:metrics-enabled=true`** (#1244), which spawn refuses because the
  `spawn:` prefix is reserved. It now shows the route that works: tag the
  instance after launch with `aws ec2 create-tags` and restart spored. A launch
  flag is tracked in #1308.

- **`WrapperOptions.Validate` rejects a `ResultsPrefix` that is not an
  `s3://bucket[/prefix]` URI** (#1258). Only an empty prefix was rejected, so a
  bare bucket name passed, and the wrapper copied the completion record to a
  relative path on the instance's disk, where it was deleted at termination: the
  task succeeded and no record, exit code or log reached S3. `GenerateWrapper`,
  `GeneratePooledJobScript` and `GenerateFlushScript` now return the new
  `ErrInvalidResultsPrefix`; pass `EffectiveResultsPrefix`'s result.
- **Job arrays, MPI clusters, parameter sweeps, `spawn resume` and `spawn burst`
  now verify every member's spored, and terminate what a failed launch leaves
  behind** (#1305), as a single `spawn launch` has since v0.145.0 (#1298).
  Each member gets the single-launch check (the `spawn:last-heartbeat` tag, with
  SSM as a second signal), run in parallel against one shared deadline, so a
  fleet waits about as long as one instance. A member whose spored never comes
  up is terminated and reported; its verified siblings are kept, since each is
  enforcing its own TTL. In a sweep that member counts as a failed parameter
  set, and `spawn resume` no longer reads it as completed. A failure that ends
  the launch after instances exist, such as a `ListInstances` error after a job
  array came up (which left every member running), a failed cohort member the
  job-array-id drain missed, or a burst that stopped part way, terminates every
  member launched so far. Each launch prints how many members were verified and
  which were terminated. `--keep-on-error` keeps them instead (`spawn launch`,
  and new on `spawn burst` and `spawn resume`); an explicit `--wait-for-ssh=false`
  skips verification, as on a single launch.

- **A rolling-queue sweep whose parameter sets span regions now tracks each
  member in its own region** (#1251). The queue polled only the first set's
  region, so a member elsewhere was missing from the result and was taken as
  finished at once: the queue launched past `--max-concurrent`. It now makes one
  `ListInstances` call per region with active members on each tick, and a region
  whose call fails decides nothing about its members that tick.
- **A pool worker whose job script never started now writes a failed
  completion record before it acks the task** (#1237). If the script could not
  be rendered, written to the workspace (a full disk, a bad workspace) or
  started (bash missing), the worker acked the task with no record, so
  `spawn task status` reported it unknown forever, as #1149 did for an
  unparseable spec. The record has `state` failed, `exit_code` -1 and
  `retry_class` `spec_error` (not retryable) for a script that cannot be
  rendered, or `instance_health` (retryable: a fresh worker usually succeeds)
  for one that cannot be written or started. A failure after the script
  started still writes nothing, because the script may already have written
  its own record.
- **Release binaries are no longer stamped `+dirty`** (#1266). GoReleaser
  writes `dist/` inside the checkout and it was not gitignored, so Go's VCS
  stamping saw an untracked file and recorded `vcs.modified=true`: the
  embedded module version of v0.139.1 read `v0.139.1+dirty`, misreporting the
  build to SBOM and provenance tooling. `--version` and the published
  checksums were always correct. A new test probes every path a release
  writes into the tree (discovered from `.goreleaser.yaml`) and fails if git
  reports any of them.

## [0.145.0] - 2026-10-24

### Added

- **The TTL reaper can release a launch Elastic IP the moment EC2 reports its
  instance terminated, in any VPC** (#1287). In a nondefault VPC EC2 refuses to
  release an associated address (`InvalidIPAddress.InUse`), and on a
  multi-interface EFA instance that address is the only egress, so spored can
  never release it; it waited for the next sweep and its 15-minute grace while
  holding a slot of the region's Elastic IP quota. Now an `EC2 Instance
  State-change Notification` with state `terminated` invokes the reaper for that
  one instance, which confirms the state with EC2 and releases only addresses
  tagged `spawn:managed=true`, `spawn:purpose=efa-public-ip` and
  `spawn:instance-id` for that instance, through the account's existing reaper
  role, with no grace. It follows `REAPER_ELASTIC_IPS` and `REAPER_DRY_RUN`; an
  event from an account or region the reaper is not configured for is ignored,
  and an event never runs the full scan. The operated reaper gets this with the
  new template parameter `InstanceEvents` (`off` by default; `make deploy
  INSTANCE_EVENTS=on`; a deploy that does not name it keeps the deployed value),
  which creates an event bus whose policy admits `events:PutEvents` only from
  the forwarder role of each account in `RoleArns`. Member accounts get the
  forwarder role `spawn-ttl-reaper-ec2-events` from
  `ttl-reaper-cross-account-role.yaml` only when its new `InstanceEvents`
  parameter is `on` (default `off`; turning it off deletes the role), and a forwarding rule per launch region
  from the new `ttl-reaper-instance-events.yaml` (`make member-events-deploy` /
  `member-events-teardown`). `spawn reaper deploy` adds a same-account rule in
  the deploy region, `spawn reaper teardown` removes it, and `spawn reaper
  status` reports it. `spawn footprint` now lists custom event buses and the
  rules on every bus.

### Changed

- **The reaper's Elastic IP sweep no longer waits out its 15-minute grace for an
  address whose instance EC2 lists as `terminated`** (#1287). The grace covers an
  instance `DescribeInstances` does not list yet; it still applies to an absent
  instance, and to an address whose instance check fails.
- **`spawn reaper deploy` now also creates the rule
  `spawn-ttl-reaper-selfscan-instance-terminated`** on the default event bus of
  the deploy region (#1287), so a redeploy subscribes an existing self-hosted
  reaper to terminated-instance events. It releases launch Elastic IPs under the
  reaper's existing `REAPER_ELASTIC_IPS` mode; no IAM change.

- **`spawn launch` on Linux no longer waits for SSH by default; spored is still
  verified, and a closed port 22 is reported at once** (#1296). spawn creates
  no security group for a plain Linux launch, so the instance gets the VPC's
  default group, which in an unmodified default VPC admits nothing on tcp/22;
  every such launch spent the two-minute SSH wait and then printed an error
  about a healthy instance. `--wait-for-ssh` now defaults to off, and spored is
  verified either way (heartbeat tag, SSM as a second signal), since it never
  needed SSH. Passing `--wait-for-ssh` waits as before, except that when no
  attached security group has an ingress rule on tcp/22 (or all traffic) from
  any source other than the group itself, the launch says so immediately,
  naming the groups, and points at `spawn connect` (SSM) or
  `--security-group-ids`, instead of waiting. An explicit
  `--wait-for-ssh=false` keeps its old meaning, skipping both the wait and the
  spored check. A default launch whose groups admit no SSH says so too and no
  longer prints an `ssh user@<dns-name>` line. Windows launches are unchanged.
  `spawn connect` on a Linux instance whose groups admit no SSH goes straight
  to SSM Session Manager instead of trying SSH; `--ssh` (previously
  Windows-only) forces SSH, and a one-shot `spawn connect <i> -- <cmd>` to such
  an instance fails at once with the reason. The check needs
  `ec2:DescribeSecurityGroups`; if it is denied, launch and connect behave as
  before.
- **`spawn launch` terminates the instance when the launch fails after
  RunInstances succeeded, unless `--keep-on-error`** (#1298). That covers
  spored never coming up (or not being verifiable), WaitForRunning failing,
  the public-IP lookup failing, the ephemeral FSx failing to start, and any
  other error the launch returns once the instance exists. An ephemeral FSx
  the launch created for the instance is deleted with it. Previously the
  instance was kept unless `--terminate-on-error`, so a caller saw a failed
  launch while the instance kept billing with nothing enforcing its TTL or
  cost limit: one ran 74 hours past a 4 h TTL. `--keep-on-error` keeps it and
  warns that it has no TTL, idle or cost-limit enforcement until spored is
  confirmed. `--terminate-on-error` is still accepted but deprecated, since it
  is now the default; `--terminate-on-error=false` keeps the instance, as it
  always did. Also, every launch now warns when the reaper probe positively
  finds that no out-of-band reaper scans the launch region, not only
  `--on-complete stop`/`hibernate` and ephemeral-FSx launches. The probe now
  checks the `--region` being launched into rather than the client's default
  region, and an inconclusive probe stays silent for those launches.

## [0.144.0] - 2026-10-24

### Changed

- **The TTL reaper leaves a running instance whose spored is still checking in
  up to 10 minutes past its TTL deadline, and runs a task's flush hook before
  it terminates one** (#1186). spored enforces the deadline itself and uploads
  the task's `command.log` and `completion.json` on the way out; a reaper cycle
  landing in that window terminated the instance first. A running instance
  with a `spawn:last-heartbeat` under 10 minutes old now gets `REAPER_TTL_GRACE`
  (default `10m`, `0` to `1h`, `0` restores reaping at the deadline) past its
  deadline. A stale or missing heartbeat, or a stopped instance, is still
  reaped at the deadline, and `REAPER_MAX_AGE` still applies. Set it with
  `spawn reaper deploy --ttl-grace`, the `TtlGrace` template parameter or
  `make deploy TTL_GRACE=…`; a deploy that does not name it keeps the deployed
  value. Separately, before terminating a running instance tagged
  `spawn:task-id`, the reaper now runs the task's flush hook over SSM (bounded
  at 60 s, the same command `spawn terminate` sends, whatever
  `REAPER_GRACEFUL` says), using the SSM grants the reaper roles already hold.
- **A release build's `spawn task run` stages the task wrapper and flush hook
  in S3 instead of putting them in user data, so a task's user data no longer
  grows with its command and manifests** (#1274). Before RunInstances the
  launch uploads both, as one object, `<task_id>/bootstrap-<sha256>.sh`, to a
  dedicated bucket, `spawn-staging-<account>-<region>`, which it creates on
  first use: Block Public Access on, SSE-S3, BucketOwnerEnforced, tagged
  `spawn:managed=true`, and a lifecycle rule that expires every object and
  aborts incomplete uploads after one day. An existing staging bucket without
  that rule gets the whole configuration re-applied; one owned by another
  account is never written to (every call sets the expected bucket owner). The
  user data carries only the object's URI and sha256. At boot, before the
  bootstrap body and spored, the instance fetches it with its own role
  (`aws s3 cp --region <task region>`, 6 tries), checks the sha256, and writes
  `/etc/spawn/command` (0600) and the flush hook (root, 0700) exactly as the
  inline user data did. A missing object, a wrong sha256 or no aws CLI retires
  the instance. A trivial task's user data drops from 3,965 to 2,070 bytes
  gzipped; a task with a 20 KB inline command, which could not launch before
  (16,533 bytes), now sends 2,076. `spawn task run --dry-run` reports the user
  data actually sent and says when the files are staged. The task's instance
  policy gains `s3:GetObject` on `<task_id>/bootstrap-<64 characters>.sh` in the
  staging bucket, and nothing else there: no policy spawn generates lists the
  staging bucket, and a TaskSpec (inputs, outputs, `resources.s3_read_write`,
  `results_prefix`) or a `--s3-read`/`--s3-write` naming a `spawn-staging-*`
  bucket is refused. Roles are shared by tasks with the same buckets, so the
  grant covers every task's key, but each key is the sha256 of bytes holding
  the attempt's random run id and appears only in that attempt's user data, so
  a task's role can read only its own object in practice. The policy change
  means the next task run per set of buckets and region creates a new
  `spawn-instance-*` role. A re-run of a task_id deletes earlier attempts'
  staged objects. The launching principal needs, on the staging bucket,
  `s3:ListBucket`, `s3:CreateBucket`, `s3:PutBucketPublicAccessBlock`,
  `s3:PutEncryptionConfiguration`, `s3:PutBucketOwnershipControls`,
  `s3:PutLifecycleConfiguration`, `s3:GetLifecycleConfiguration` and
  `s3:PutBucketTagging`, and `s3:PutObject` and `s3:DeleteObject` on its
  objects. When the bucket cannot be made ready or the upload fails, a task
  that fits inline launches inline with a warning. Dev builds, and an embedder
  whose `tasklaunch.AWS` lacks the staging methods, keep everything inline.
  Adds `launcher.StagedFiles`, `launcher.StagedFilesObject`,
  `launcher.NewStagedFiles`, `BootstrapConfig.StagedFiles` and
  `Options.StagedFiles`; `aws.Client.EnsureStagingBucket`, `PutS3Object` and
  `ListS3Keys`, `aws.StagingLifecycleRuleID` and `StagingObjectExpiryDays`;
  `taskproto.StagingBucket`, `IsStagingBucket`,
  `StagingBucketUses` and `StagingBucketPrefix`; `taskiam.TaskRunPolicyFor`,
  `StagedFilesKey`, `StagedFilesKeyPrefix` and `StagedFilesResources`; and
  `tasklaunch.TaskPlan.StagedBytes`. Pool workers are unchanged: their tasks
  arrive through the queue, not user data.

### Fixed

- **A task's terminal flush no longer fails invisibly** (#1186). A TTL-killed
  task could leave its results prefix empty, with the reason only in spored's
  journal, which dies with the instance. spored now logs the flush hook's full
  output and exit status, records the outcome in a `spawn:flush-status` tag
  (`ok`, `failed:<reason>` or `skipped:<reason>`; readable for about an hour
  after termination), and on a failure or skip (no hook on a task instance, a
  refused hook, a timeout, failed uploads, any other non-zero exit) writes the
  reason and the hook's output, followed by the command log's tail, to the
  serial console (`aws ec2 get-console-output`) and sends a `pre_stop_failed`
  notification naming the flush. The hook now retries each upload up to three
  times with backoff inside its 45 s budget, prints the aws CLI's error on the last
  failure, still uploads `completion.json` and `.exitcode` when `command.log`
  fails, and exits 3 when an upload did not land. A completion record the
  wrapper wrote locally but never got to S3 is now uploaded by the hook
  instead of being taken as proof the record existed (an incomplete local copy
  from a full `/tmp` is not). spored also bounds the hook's output pipes, so
  an `aws` child left running cannot hold termination past the timeout.

## [0.143.0] - 2026-10-24

### Added

- **A container task can run a workflow engine that starts its own containers:
  `container_docker_socket: true`** (#1231). Cromwell's local backend, miniwdl
  and Nextflow with `docker.enabled` start each workflow task as a sibling
  container through the host's Docker socket, which a container task could not
  reach. The new TaskSpec field adds `-v /var/run/docker.sock:/var/run/docker.sock`
  and `--group-add` with the socket's group to `docker run`; `--user` is
  unchanged. Off by default, because access to the socket is root-equivalent on
  the instance. It needs `container`, and is refused for pooled tasks, where
  one task could control the others on a shared worker: by `spawn pool
  submit`, by `taskpool.Pool.Enqueue`, and by the worker for a spec staged some
  other way (recorded as `spec_error`). The provenance recipe records
  `docker_socket`, and the retention report never proves such a run
  regenerable, since the images the engine started are not recorded. Adds
  `TaskSpec.ContainerDockerSocket` and `TaskSpec.ValidatePooled` to
  `pkg/taskproto`.

### Changed

- **A release build of spawn no longer puts the static spored bootstrap in EC2
  user data; it puts a small stub that fetches it, verifies it and runs it**
  (#1274). Each release now publishes the bootstrap body, signed with the
  spored signing key, as `spawn/versions/<version>/bootstrap.sh` (with `.sig`
  and `.sha256`) in every `spawn-binaries-<region>` bucket, beside spored. The
  stub downloads the body for its own release from the instance's regional
  bucket (us-east-1 fallback, with retries) and runs it only if its sha256 is
  the one that spawn build expects and its signature verifies against the key
  compiled into spawn. On any failure it runs nothing from the body (no spored,
  no workload), logs the reason to the journal, the console and
  `cloud-init-output.log`, and terminates the instance through the role's
  existing self-only terminate grant; where that cannot work (no aws CLI, no
  permission) it shuts down, which stops the instance. The per-launch parts (key, command, task flush
  hook, readiness gates, plugins, storage, container, custom user data) stay
  inline, in the same order around the body as before. User data shrinks from
  7,253 to 1,771 bytes gzipped for a bare launch, and from 9,324 to 3,968 for a
  trivial `spawn task run`, which leaves a task about 12.4 KB of EC2's 16 KiB.
  Every launch path gets the stub: `spawn launch` (single, array, sweep, batch
  queue, resume), `spawn task run`, `burst`, `pool`, and `launcher.Provision`
  (lagotto, the MCP server), which uses the spawn release it was built against.
  A dev or pseudo-version build, a build with no signing key, and a caller that
  sets the new `launcher.BootstrapConfig.InlineBody` keep the full inline
  bootstrap. The release publishes the body to every bucket before GoReleaser
  publishes the CLI and the taps, so no installable spawn exists without its
  body. It fails if the body cannot be generated, signed or uploaded, or if the
  released binary's `spawn bootstrap-body` differs from what it published. The bootstrap's completion-file path is now a shell variable,
  `SPAWN_CMD_COMPLETION_FILE`, assigned at the top of the script.
- **The verified spored installer is stripped of comments wherever it is
  spliced into user data** (#1274): `spawn app launch`, the pipeline
  orchestrator's stage instances and `spawn upgrade-spored`, about 1.6 KB
  gzipped each.

### Fixed

- **A low-memory instance (e.g. t4g.nano) could run its workload with no spored,
  and so no TTL and no on-complete, when an optional package install was
  OOM-killed** (#1274). The bootstrap installed acpid with dnf before starting
  spored; on a 0.5 GiB instance dnf was killed, `systemctl enable --now acpid`
  then failed, `set -e` ended the bootstrap, and `systemctl start spored` never
  ran, while the `--command` started earlier kept running and the instance kept
  billing. spored now starts right after its verified install, before every
  optional step (acpid, which is no longer fatal, the login banner, session
  timeouts, the user, hostname and array/sweep setup, and the `--command`).
  And any exit of the bootstrap before spored is running, for any reason, now
  retires the instance, naming the command that failed.
- **`spored reload` now reaches the running daemon** (#1278). It used to
  reload a throwaway agent in its own short-lived process and print that
  agent's config, so the daemon kept its old TTL, cost limit and other
  settings until its periodic tag refresh, up to about 5 minutes later. It now
  signals the daemon (SIGHUP to the systemd unit's main process on Linux, the
  ParamChange service control on Windows), which re-reads its tags on its
  monitor loop and publishes an acknowledgement to `/run/spored/reloaded.json`
  (`%PROGRAMDATA%\spored\reloaded.json` on Windows). `reload` waits up to
  about 15 seconds for it and prints the config the daemon now enforces. It
  exits non-zero when spored is not running, does not acknowledge, or could
  not refresh, and it does not signal a running spored that predates this
  change (SIGHUP would stop it). `spored config set` notifies the daemon the
  same way, and `spawn extend`'s "reloaded" line now means the daemon has the
  new config. `Agent.Reload` also reads and swaps the config under its lock.

- **A bootstrap that cannot install spored terminates its instance instead of
  leaving it running with no TTL enforcement** (#1274). When the spored install
  failed, or the architecture had no spored, the bootstrap exited and the
  instance kept running and billing with nothing to stop it, unless a TTL
  reaper was deployed in the account. It now reports why and terminates itself;
  if it cannot (no aws CLI, no permission, no egress), it shuts down, which
  stops it, because spawn sets no instance-initiated shutdown behaviour.
- **`spawn resume` sent its relaunched sweep members' user data without gzip,
  over EC2's limit** (#1274). It base64-encoded the plain bootstrap, about
  25 KB, so RunInstances refused every resumed Linux member ("User data is
  limited to 16384 bytes"). It now encodes user data the way every other launch
  path does.

## [0.142.0] - 2026-10-23

### Added
- **`spawn list` and `spawn status` warn about orphaned spawn Elastic IPs, and
  print the command that releases them** (#1232). A multi-interface EFA launch
  allocates an Elastic IP; when its instance ends without `spawn terminate`
  (a Spot reclaim, for one), the address stays allocated, billing and counting
  toward the region's Elastic IP limit. The warning gives the count, the
  region, the allocation IDs (first five) and the command. `list` checks every
  region it searched, `status` the instance's region: one
  `ec2:DescribeAddresses` each, plus one `ec2:DescribeInstances` only when a
  spawn address is unassociated. It goes to stderr, and a failed lookup prints
  nothing and never fails the command.
- **`spawn cleanup --elastic-ips` releases only orphaned spawn Elastic IPs**
  (#1232): tagged `spawn:managed=true` and `spawn:instance-id`, attached to
  nothing, and whose instance is terminated or no longer exists. Nothing else
  is touched, so running instances do not block it (a plain `spawn cleanup`
  refuses while any run). `--dry-run`, `--yes` and `--all` work as for
  `cleanup`; `--all` is needed for addresses another principal launched.
- **The TTL reaper's launch Elastic IP sweep has its own switch,
  `REAPER_ELASTIC_IPS`** (#1232): `off`, `report` or `reap`, set as the SAM
  parameter `ElasticIps`, `make deploy ELASTIC_IPS=...`, or
  `spawn reaper deploy --elastic-ips`. What a reaper does by default is
  unchanged: unset (the template default is `''`) means `reap`, or `off` when
  `REAPER_NET_RESOURCES=off`, exactly as before. The switch is independent of
  `REAPER_NET_RESOURCES`, so the release can be held in `report` or turned `off`
  without touching the security-group and placement-group sweep, and kept on
  while that sweep is off. It also governs the release of an instance the
  reaper has just terminated. `report` logs a `WOULD release Elastic IP` line
  for each address `reap` would release. The run summary gains
  `elastic_ips_scanned`, `elastic_ips_released`, `elastic_ips_would_release`
  and `elastic_ips_skipped`. A deploy that does not name the setting keeps the
  deployed value, on all three deploy paths. No IAM change.

### Changed
- **`spawn extend` no longer raises an instance's cost limit unless asked**
  (#1262). Since #639 it raised `spawn:cost-limit` to whatever the new
  deadline needed, so `--cost-limit` was not a ceiling for any instance that
  was extended. In #1262 eight c8gn.48xlarge launched with `--cost-limit 25`
  were extended twice, their caps were raised to about $46, and each ran to
  about $29 with no cost-limit termination. Now the cap is kept. When it will
  terminate the instance before the new deadline, `extend` prints a ⚠️ warning
  with about when, what running to the deadline costs, and the flag to allow
  it. `--raise-cost-limit` restores the old raise; `--cost-limit N` sets a
  value. `--keep-cost-limit` is now the default, and is deprecated but still
  accepted. `--cost-limit` together with `--raise-cost-limit`, or either with
  `--keep-cost-limit`, is refused.
- **A container task's /tmp is now its own RAM-backed tmpfs, half of instance
  memory, instead of the root disk** (#1270). Unless a manifest path sat
  directly in /tmp, the container's /tmp was the image overlay on the root EBS
  volume (20 GiB by default), so a task staging only directories such as
  /tmp/in/ wrote its scratch to disk: PLINK 2 died with ENOSPC on a 128 GiB box.
  `docker run` now gets `--tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777,size=50%`,
  the size AL2023 gives the host's /tmp, with staged directories mounted on
  top. It is a separate tmpfs, not the host's /tmp, which holds spored's files.
  This is a behaviour change: scratch now uses RAM, so a task that writes more
  than about half of RAM to /tmp now fails with ENOSPC on the tmpfs where it
  used to fill the root disk, and memory written there is not available to the
  workload; size memory_gib for it, or write large scratch elsewhere. A spec
  with a manifest path directly in /tmp (such as /tmp/run.sh) keeps today's
  bind mount of the host's /tmp, since Docker refuses both on one path.
  command.log says which /tmp the container got.
- **A directory input whose prefix is empty or missing now fails stage-in**
  (#1207). `aws s3 cp --recursive` of a prefix with no objects exits 0, so a
  task whose upstream never wrote its input started on an empty directory and
  failed much later inside its own command, or succeeded on nothing. When an
  input with a source ending in `/` stages no regular file (a prefix holding
  only a zero-byte folder marker counts as empty), the task now fails with
  `input s3://… copied no objects: the prefix is empty or missing`, classified
  `staging_error`, and the command does not run. A spec that relied on an
  empty prefix must set the new per-input field `allow_empty: true`. Validate
  rejects `allow_empty` on a single-object input and on outputs.
- **`spawn cleanup` no longer releases a spawn Elastic IP whose instance still
  exists** (#1232). It released every unassociated spawn-tagged address,
  including one a launch had allocated and not yet associated. It now requires
  the instance to be terminated or gone, as the TTL reaper's sweep does, and
  lists the others as not removed with the instance's state. An Elastic IP
  attached to a network interface but not an instance is no longer reported
  as unassociated.
- **The reaper's Elastic IP sweep checks an instance's state itself and ages
  unstamped addresses instead of skipping them** (#1232). An address whose
  instance is still listed as `terminated` was already treated as orphaned
  through the `DescribeInstances` filter; the reaper now also checks the state
  itself rather than relying only on the filter. An address with no usable
  `spawn:created` stamp used to be skipped on every cycle, so it billed until
  someone released it. Every release since 0.138.0 writes the stamp in the same
  call that allocates the address, so this affects only addresses from
  pre-release builds or hand-edited tags. Such an address is now stamped
  `spawn:reaper-first-seen` on first sight and released 15 minutes later, if
  every other condition still holds.
- **Elastic IPs are no longer counted in the reaper summary's `net_reaped` /
  `net_skipped`**; they have their own `elastic_ips_*` fields (#1232).

## [0.141.0] - 2026-10-23

### Changed

- **The spored bootstrap no longer sends its comments to EC2, which roughly
  quadruples the room a task has for its own command** (#1246). EC2 allows
  16 KiB of user data, and the bootstrap's static parts carried 16.5 KB of
  inline documentation. A trivial `spawn task run` used 14,711 of the 16,384
  bytes after gzip, leaving about 1.7 KB, and specs that launched on 0.131.1
  were refused on 0.138+. Comment and blank lines are now stripped from
  spawn's own script templates when the user data is built (heredoc bodies and
  every other line are kept byte for byte); the same task now uses 9,149
  bytes, leaving about 7.2 KB. The bare bootstrap is 7,253 bytes gzipped, down
  from 12,829. Caller-supplied scripts (the command, storage, container and
  custom user data) are not touched. A test parses each template with
  mvdan.cc/sh before and after stripping and requires the same syntax tree,
  and another holds the bare bootstrap to an 8 KiB gzipped budget.
- **`spawn version` and `spawn --version` describe spawn as "ephemeral compute
  for AWS"**, in line with the project's own description, instead of "EC2
  Instance Lifecycle Manager".
- **spawn's help text and package descriptions say "ephemeral compute for
  AWS"**. `spawn --help` (from libs v0.51.3, in all six languages), the
  Homebrew, Scoop and Linux package descriptions, and the README tagline
  described an EC2 instance launcher or "lifecycle management". libs v0.51.3
  also brings `spawn launch --help`'s truffle example up to date:
  `truffle find`, not the deprecated `truffle search`.

### Fixed

- **FSx for Lustre cost estimates for the 250, 500 and 1000 MB/s/TiB tiers were
  1.4 to 1.9 times the list price** (#1165). The table had 0.290, 0.580 and
  1.160 $/GiB-month; the AWS Price List (us-east-1, Persistent SSD) gives
  0.210, 0.340 and 0.600. 0.290 is the PERSISTENT_1 200 MB/s/TiB rate. A
  4.8 TiB 1000-tier file system was reported at about $5,700/month instead of
  $2,950, which also skewed `--cost-limit` sizing for FSx launches. Every FSx
  price spawn prints now says it is a static us-east-1 list price, from when,
  and that spawn has no live FSx price lookup yet; a test pins the table to the
  Price List values.
- **`spawn task run --dry-run` showed instance types under 1 GiB as "0 GiB"**
  (#1276). Memory is now printed as it is: a t4g.nano shows 0.5 GiB.

- **A task that fills the instance's `/tmp` still leaves a readable completion
  record** (#1179). The task wrapper and the terminal-flush hook wrote
  `completion.json` and `.exitcode` to `/tmp` and uploaded those files; when the
  task had filled `/tmp` (a container task's staging lives there too) both came
  out empty and were uploaded empty, so `spawn task status` failed with
  "unexpected end of JSON input" although the task's failure was in
  `command.log`. Each record is now built in memory and checked after it is
  written; if the local copy did not take, the record is streamed to S3 from
  memory instead. A record left empty by an older instance is now reported by
  `spawn task status` and `spawn task run --wait` as an empty record, probably
  from a full disk, with the location of the task's `command.log`.

## [0.140.0] - 2026-10-20

### Added
- **`placement.subnet_id` in a TaskSpec launches a task into that subnet and
  its VPC** (#1267). Tasks could not launch in a region with no default VPC,
  which Control Tower and Landing Zone Accelerator accounts usually lack:
  `spawn launch --subnet-id` covered plain launches, but a TaskSpec had no
  subnet field, so `spawn task run` and spore-host-mcp's `spawn_task_run`
  always launched into the default VPC. The value must be a `subnet-` id; the
  subnet fixes the AZ, so `placement.availability_zone`, if set, must match it.
- **A task with no `placement.subnet_id` in a region with no default VPC now
  fails before anything is created** (#1267), including in `--dry-run`, and the
  error lists up to five subnets to use, those that map public IPs first. It
  used to fail at RunInstances. If EC2 cannot be asked, the launch goes ahead
  as before. The dry run gains a `Network:` line saying where the task will go.
  New in `pkg/aws`: `Client.DefaultVPCOrSubnets` and `SubnetHint`; in
  `pkg/tasklaunch`: `TaskPlan.Network`. `spawn doctor`'s no-default-VPC
  warning now mentions `placement.subnet_id` alongside `--subnet-id`.

## [0.139.1] - 2026-10-20

### Security
- **Built with Go 1.26.10** for GO-2026-6809 (disclosed 2026-10-21), which
  makes `crypto/x509` distrust compromised intermediate certificates. The CI
  Go pin moves 1.26.9 → 1.26.10.

## [0.139.0] - 2026-10-20

### Added

- **`pkg/tasklaunch`: `ReadCompletion`, the one reader of task completion
  records (#1260).** `tasklaunch.ReadCompletion(ctx, store, taskID,
  resultsPrefix, region)` resolves the record's location exactly as the launch
  does (`EffectiveResultsPrefix`, honouring a spec's `results_prefix` as
  `spawn task status --results-prefix` does), reads it and parses it, and
  returns the record with its `s3://` URI. A missing record wraps the new
  sentinel `tasklaunch.ErrNoCompletionRecord` (test with `errors.Is`), so a
  caller can tell "no record yet" from a failed read or a malformed record.
  `ReadCompletionAt(ctx, s3, region, bucket, taskPrefix)` is the same read for a
  caller that already holds the resolved location. `spawn task status` and
  `spawn task run --wait` now read through them, and spore-host-mcp can drop
  its copy of the chain, so front ends cannot drift from the writer again (the
  #715 class). A test fails if `cmd/` reads a completion record any other way.
- **Completion records carry `instance_type` and `purchase` (#1260).** The task
  wrapper asks IMDS for the instance's type and life cycle when the task starts,
  and writes `instance_type` (for example `m7i.large`) and `purchase` (`spot` or
  `on_demand`) into `completion.json`. The terminal-flush hook's record carries
  the same two fields. A reader can now price a record that has no
  `cost_estimate` after EC2 has stopped listing the terminated instance. Both
  fields are additive and omitted when IMDS does not answer, and records written
  before this release still parse. `spawn task status` prints them. The lookup
  adds about 200 bytes to the task's 16 KiB user-data budget.
  `taskproto.SetIMDSBaseForTest` lets exec-based tests outside the package keep
  the generated wrapper off the real IMDS address.

## [0.138.0] - 2026-10-20

### Added

- **`pkg/tasklaunch`: `spawn task run`'s plan-and-launch path as a public Go
  package (refs spore-host-mcp#60, spore-host-mcp#61).** spore-host-mcp
  re-implemented the task launch and drifted from the CLI. It called
  `launcher.Provision` with empty Options, so its launches had no storage
  mounts and no terminal-flush hook (the #1144 class). It never retried
  On-Demand on a Spot capacity error, and it sized with `spec.Resources`, which
  skipped the container memory floor (#1180). The package exports
  `Plan(ctx, deps, spec, region)` (sizing via `SizingRequest()`, the pinned-type
  lookup, the pinned-type checks and the 16 KiB user-data preflight),
  `Render(w, plan)` (the dry run as the CLI prints it) and
  `Launch(ctx, deps, spec, region)` (results bucket, stale-record clear, scoped
  instance profile, storage script, wrapper and flush hook, and the On-Demand
  fallback with identical Options). Dependencies (`Deps`: finder, AWS client,
  provisioner) are interfaces, so callers and tests can inject fakes, and
  `NewDeps(client, region, warn)` builds the real ones. `spawn task run` now
  runs through it and behaves as before. `pkg/tasklaunch` joins the
  public-surface snapshot.
- **`resources.shm_gib` sizes a container task's `/dev/shm` (#1111).** Docker
  gives every container 64 MB, too small for Open MPI's shared-memory segments
  at many ranks: a 64-rank LAMMPS run on one box died with SIGBUS in its first
  collective. The field maps to `docker run --shm-size=<n>g`; unset keeps the
  64 MB default. It must be a positive number of GiB, needs `container`, and
  may not exceed the instance's memory (the effective `memory_gib`, or the
  container floor, for a sized spec; the pinned type's memory at launch). The
  dry run prints the `/dev/shm` size. Go API: `ResourceRequest.ShmGiB` and
  `TaskSpec.CheckShmAgainstMemory`.
- `taskproto.PhaseTimestampFormat`, the `date` format of the task wrapper's
  phase markers (Go API, #1072).

### Changed

- **Task logs are quieter and their phase markers carry milliseconds (#1072).**
  Every `aws s3 cp` the task wrapper runs (stage-in, stage-out and the record
  uploads) passes `--no-progress`, so `command.log` keeps one line per staged
  file and every error but no longer fills with about 20 progress lines per
  file. The `spawn: [...]` phase markers are stamped
  `2026-10-20T16:28:51.123Z` instead of to the second; `started_at`/`ended_at`
  in `completion.json` are unchanged.

- **A container task installs Docker while its inputs stage (#1072).** The
  install (~66 s in E1, every launch) used to run after stage-in; it now starts
  in the background before it, and the wrapper waits for it before starting the
  daemon, pulling or running anything. A failed install is still
  `instance_health`, and a failed stage-in still waits for the install to
  finish. The install's output is printed as one block when it is waited for.
- `aws.Client.WaitForStopped` (Go API): the SDK's instance-stopped waiter.

- **`spawn connect` wakes a stopped, hibernated or stopping instance on every
  connect path (#57).** The shell/one-shot path and the DCV app path each had
  their own auto-start; the app path started only a `stopped` instance and
  ignored `--no-start`. Both now share one helper. A `stopping` instance is
  waited out and then started (EC2 refuses to start one mid-stop, so this
  used to fail on the shell path and fall through to a five-minute ready-URL
  wait on the app path). A hibernated instance is reported as "hibernated —
  resuming it", and the wait for it to run is now up to five minutes rather
  than two, since a resume restores RAM from disk.

- **`spawn resume` no longer guesses us-east-1 (#495).** It reconciles each
  launched instance in the region recorded for it in the sweep state (new
  `region` field, written at launch), else the region its parameter set or the
  file's defaults name, else `--region`/`SPORE_REGION`/`AWS_REGION`/spore
  config. With none of those it stops with an error asking for `--region`,
  instead of reconciling against, and launching into, a region the sweep never
  used. Sweeps whose instances span regions are reconciled in each.

- **`spawn ami create` removes the instance's `--command` run state before
  imaging (#1057).** On a distro whose `/tmp` is on the root volume (Rocky 9
  Base), the completion file and `/tmp/SPAWN_EXITCODE` were baked into the AMI,
  so an instance launched from it started with a stale `SPAWN_COMPLETE` and
  `--on-complete` fired at boot. The embedded command (`/etc/spawn/command`)
  was baked in too, and a later launch from the image re-ran it even without
  `--command`. Now, before CreateImage, spawn removes over SSM: the
  completion file (the launch's `--completion-file` and the default), every
  `/tmp/SPAWN_*` file, `/tmp/spawn-command.sh`, `/etc/spawn/command`, the
  sweep environment and the ready-gate list. If that cannot be done (the
  instance has no SSM, is stopped, or its `--command` is still running),
  **the image is not created** unless `--force` is given. Imaging a stopped
  Linux instance therefore now needs `--force`. Windows instances run no
  `--command`, so nothing is removed from them.

- **The Linux bootstrap runs `--command` once per instance (#1057).** It
  writes `/var/lib/spawn/command-ran-<instance-id>` before starting the
  command and skips the command if that file already exists. A command that
  ends with `cloud-init clean` used to run a second time on the source
  instance during the CreateImage reboot of `spawn ami create --reboot`,
  overwriting what it had uploaded. Because the marker is keyed on the
  instance ID, an instance launched from the image still runs its own
  `--command`. Delete the marker to re-run a command on purpose.

- **`spawn plugin install`, `list`, `status` and `remove` work on keyless,
  SSM-only instances (#855).** spored's plugin push API listens on the
  instance's loopback, so these commands reached it only through an SSH
  port-forward. An instance with no public IP or no usable SSH key could not
  take a plugin. Now an instance with no public IP is reached over SSM: spawn
  runs the request on the instance with `curl` to the loopback API, and the
  token is read there and never leaves the box. An instance with a public IP
  is tried over SSH first. If SSH cannot reach the API, the call and every
  later one in the command go over SSM. A request that SSH delivered is never
  repeated over SSM, so an install cannot run twice. If both SSH and SSM fail,
  the command exits non-zero with an error that names both failures. A
  request larger than one SendCommand can carry is staged in chunks, and a
  response that SSM's 24,000-character output limit cuts short is reported as
  truncated rather than parsed.

- **`spawn launch --efa` now attaches one EFA interface per network card**
  (#661). It attached exactly one interface, so a multi-card instance ran on a
  fraction of its fabric: c8gn.48xlarge on 1 of 2 EFAs, p5en.48xlarge on 1 of
  16, p5.48xlarge on 1 of 32. The primary interface (card 0, device 0) is
  `efa`; each further card gets an `efa-only` interface at device 0, per AWS's
  multi-card EFA guide. EC2 assigns no public IP to an instance with more than
  one interface, so such a launch waits for the instance to run, allocates an
  Elastic IP tagged `spawn:managed=true` / `spawn:instance-id=<id>` and
  associates it to the primary interface; if that fails the instance is
  terminated and the launch fails. `spawn terminate` releases the address,
  `spawn cleanup` releases one left unassociated, and the TTL reaper releases
  orphans. Single-card types (c5n.18xlarge, hpc7a) are unchanged. The launching
  principal needs `ec2:AllocateAddress`, `ec2:AssociateAddress`,
  `ec2:DescribeAddresses`, `ec2:DisassociateAddress`, `ec2:ReleaseAddress`, and
  `ec2:CreateTags` on `elastic-ip` with `ec2:CreateAction=AllocateAddress`.
  `--efa-interfaces 1` restores the old single interface.

- **`spawn launch --efa` now attaches one EFA interface per network card**
  (#661). It attached exactly one interface, so a multi-card instance ran on a
  fraction of its fabric: c8gn.48xlarge on 1 of 2 EFAs, p5en.48xlarge on 1 of
  16, p5.48xlarge on 1 of 32. The primary interface (card 0, device 0) is
  `efa`; each further card gets an `efa-only` interface at device 0, per AWS's
  multi-card EFA guide. EC2 assigns no public IP to an instance with more than
  one interface, so such a launch waits for the instance to run, allocates an
  Elastic IP tagged `spawn:managed=true` / `spawn:instance-id=<id>` and
  associates it to the primary interface; if that fails the instance is
  terminated and the launch fails. Every ending releases the address:
  `spawn terminate` does; spored does after its own terminate (TTL, idle,
  cost limit, on-complete) and 20 seconds before a Spot terminate interruption;
  the TTL reaper does for instances it terminates and for any left orphaned;
  and `spawn cleanup` releases one left unassociated. From the instance this
  works in a default VPC only: in a nondefault VPC EC2 refuses to release an
  associated address and the instance cannot disassociate it without losing
  the egress the release needs, so the reaper releases it once EC2 detaches it.
  Single-card types (c5n.18xlarge, hpc7a) are unchanged. The launching
  principal needs `ec2:AllocateAddress`, `ec2:AssociateAddress`,
  `ec2:DescribeAddresses`, `ec2:DisassociateAddress`, `ec2:ReleaseAddress`, and
  `ec2:CreateTags` on `elastic-ip` with `ec2:CreateAction=AllocateAddress`.
  `--efa-interfaces 1` restores the old single interface.

- **CI now shellchecks every script spawn generates (#689).** The spored
  bootstrap, MPI/storage/container/queue user-data, the task wrapper, pooled
  job script and flush hook, the app templates, the pool worker loop, the
  pipeline stage runner, `upgrade-spored`, the verified spored installer and
  the Slurm conversions are each rendered and run through
  `shellcheck -S warning`, including any script they write through a quoted
  heredoc. The generators are discovered from source: a function holding a
  shell shebang that no test shellchecks fails the build. The Test job installs
  shellcheck v0.11.0, pinned by version and sha256, and the gate fails under
  `CI=true` if shellcheck is missing. Intentional patterns carry a scoped
  `# shellcheck disable=` with a reason.

- **A tag contract gate (#689).** Every `spawn:*` tag `buildTags` writes at
  launch must be read somewhere in the repo, and every tag read must be written
  somewhere. Both sides are discovered from source, and the few keys written
  outside spawn's code are allow-listed with their writer named. A Substrate
  round trip checks that each launch tag reaches `ListInstances` in exactly one
  place (a named field or `Tags`), and that every tag-sourced `InstanceInfo`
  field is filled by some tag.

### Fixed

- **A pinned `resources.instance_type` shows its real sizes and cost in the
  task dry run (#1253).** The plan printed `(0 vCPU, 0 GiB` for a pinned type,
  described it as "chosen as cheapest of 1 matching type(s)", and dropped the
  `Max cost` line, because sizing never looked the pin up. Sizing now looks the
  pinned type up once (one DescribeInstanceTypes and one Price List request for
  that type) and the plan prints e.g. `r8g.2xlarge  (8 vCPU, 64 GiB,
  $0.4713/hr on-demand)`, "pinned by spec (resources.instance_type)" and
  `Max cost`. If the lookup fails the pin still launches as given and the plan
  says "sizes unavailable" instead of zeros. The container-floor warning and the
  `shm_gib` check for a pin read the type's memory from that same lookup. Go
  API: `taskproto.SizeResult` gains `Pinned`, `PinLookupErr` and `SizesKnown()`.
- **`spawn resume` no longer relaunches parameters that already ran (#1248).**
  A resume launched only the pending parameter sets but saved a fresh sweep
  state: each new instance was recorded by its position in the pending list,
  not its parameter index, and the sweep's size shrank to the pending count.
  The next resume then read the wrong parameters as done and launched
  completed work again. Relaunched instances are now recorded under their own
  parameter index and merged into the existing state, which keeps its
  completed records, its size and its sweep metadata.

- `spawn resume` panicked (`interface conversion: interface {} is nil, not
  string`) on a sweep whose parameter file has no `region` in its defaults —
  every sweep launched with `--region` alone — once any instance had launched
  (#495).
- `spawn ami create --force` (and `spawn create-ami --force`): create the image
  even when spawn could not remove its run-state files from the instance first
  (#1057).

- `launcher.ImageStateCleanupScript`, `launcher.CommandRanMarkerDir`,
  `launcher.ImageCleanOK` and `launcher.ImageCleanCommandRunning` (Go API).

- `spawn task run` (and its `--dry-run`) no longer spends minutes sizing an instance type before launching (#1181). Sizing priced every matching type with its own AWS Price List request: a `cpu: 2` spec matched 1325 types in us-east-1, and experiment E1 measured 149-256 s before `RunInstances`. It now fetches the region's On-Demand table once, in about 14 paged requests whatever the spec matches, which also puts much less load on the throttled Price List API (truffle#182). When the live table is unavailable, candidates are priced from the static table by exact match, as before, and the output now says so and gives the table's collection date. `-v` prints how long the instance search and the pricing took and how many Price List requests were made.
- `pkg/aws`: `MaxUserDataBytes`, `MaxEncodedUserDataBytes`, `CheckUserDataSize`, `UserDataSize`, `UserDataTooLargeError` and `LaunchConfig.ValidateUserDataSize`, the EC2 user-data limits and one shared check against them (#1063). `pkg/launcher`: `BuildUserData` returns the exact encoded user data `Provision` sends, so a caller can measure it before launching.
- `spawn task run --dry-run` prints the task's user-data size against EC2's 16,384-byte limit (#1063).

- A TaskSpec whose user data is over EC2's 16 KiB limit no longer passes `spawn task run --dry-run` and then fails at `RunInstances` with HTTP 400 (#1063). Both the dry run and the real run now measure the generated user data the same way RunInstances does (after gzip, then base64), and refuse an oversized spec before any AWS call. The message says how big it is and suggests moving long inline scripts from `command` into a staged `inputs:` file. A trivial task already uses about 14,100 of the 16,384 bytes, so roughly 2 KB of compressed command and inputs remain.
- Every launch path now refuses oversized user data before making any AWS call, instead of failing at RunInstances (#1063). This covers `spawn launch`, sweeps, job arrays, burst, pools, `launcher.Provision`, and the pipeline orchestrator Lambda (the Lambda only after it is redeployed). The check is in `Client.Launch`. `launcher.Provision` now builds the user data before its AMI lookup and IAM setup, so an oversized config creates nothing.
- **`spawn launch --efa-interfaces N`** chooses how many EFA interfaces an
  `--efa` launch attaches, one per network card: `0` (the default) is every EFA
  the instance type supports, `1` is the single interface `--efa` used to
  attach, and a count above the type's maximum is refused before anything is
  created (#661). `spawn launch --dry-run` shows the interface layout and
  whether an Elastic IP will be associated. With more than one interface,
  RunInstances needs a subnet on each, so spawn takes the `--az`'s subnet when no
  `--subnet-id` is given, and asks for one of them otherwise.

- `pkg/aws`: `Client.EFACapacityInRegion`, `EFACapacity`, `EFANetworkCard`,
  `EFANetworkInterfaces`, `EFALaunchInterfaces`, `NeedsLaunchElasticIP`,
  `DescribeEFAInterface`, `IsSpawnAllocatedAddress`, `ElasticIPInstanceTagKey`,
  `ElasticIPPurpose`, `ElasticIPQuota`, `Client.ElasticIPQuotaInRegion`,
  `CheckElasticIPHeadroom`, `ElasticIPQuotaCode`, and
  `LaunchConfig.EFAInterfaces` (#661). Additions only;
  `ValidateInstanceTypeForEFAInRegion` and `EFANetworkInterface` are unchanged,
  and an SDK caller that does not set `EFAInterfaces` launches as before.

- **A multi-interface EFA launch checks the Elastic IP quota first** (#661).
  Each such instance needs an Elastic IP, and the region quota ("EC2-VPC Elastic
  IPs", `L-0263D0A3`, 5 by default) used to surface only at AllocateAddress,
  after the instance had booted and had to be terminated. The launch, and its
  `--dry-run`, now stops before creating anything with "N of M Elastic IPs in
  use in <region>", counting one per `--count` instance, and says that
  `--efa-interfaces 1` needs no address. If the quota cannot be read the launch
  warns and goes on; the terminate-on-AllocateAddress-failure still backs it
  up. The caller needs `servicequotas:GetServiceQuota` and
  `servicequotas:GetAWSDefaultServiceQuota` for the check (without them it only
  warns).

- **The TTL reaper releases launch Elastic IPs** (#661). Its net-resources sweep
  releases an address spawn allocated for a multi-interface EFA launch once it
  is unassociated, its instance is gone and it is 15 minutes old, and the reaper
  releases the address of an instance it has just terminated. Unlike the
  security-group and placement-group sweep, this one releases in `report`, the
  default, as well as in `reap`: the address is selected by
  `spawn:managed=true`, `spawn:purpose=efa-public-ip` and `spawn:instance-id`,
  which only spawn writes and the instance role is denied from writing, nothing
  references it, and it bills by the hour (the first field use leaked two in
  seven minutes). `off` and `REAPER_DRY_RUN` still hold it back. The reaper
  role gains `ec2:DescribeAddresses` and `ec2:ReleaseAddress` (the latter on the
  `spawn:managed=true` tag rail) in `pkg/reaperiam`, the SAM template and the
  cross-account role template; a deployed reaper needs a redeploy for both. A
  `RELEASED` line counts as reaper activity for `spawn reaper teardown
  --if-idle-for`, like `REAPED`.

- **A pooled task whose spec does not parse now gets a completion record
  (#1149).** The pool worker acked such a task without writing one, so
  `spawn task status` reported it unknown forever. The worker now writes a
  failed record (`exit_code` -1, `retry_class` `spec_error`) to the key
  `task status` reads, then acks. A spec that is valid JSON but fails
  validation has its `results_prefix` honoured. The pooled-execution design doc
  no longer claims a reconcile loop tops the pool up or replaces reclaimed Spot
  workers: `pool create` provisions once, and a lost worker's task is rerun by a
  surviving worker after the visibility timeout.

- **`spawn slurm convert` quotes the job name and working directory (#689).**
  `#SBATCH --job-name` with a space exported only its first word, and
  `--chdir` with a space made `cd` fail. Because the script has no `set -e`,
  the job body then ran in the wrong directory. Both values are now
  shell-quoted, and a failed `cd` stops the job, as Slurm's `--chdir` does.

### Security

- **The spored instance role may release its own launch Elastic IP, and only
  that** (#661). Both spored policies (the per-launch baseline and the shared
  `spored-instance-role` policy) gain `ec2:DescribeAddresses` on `*`
  (read-only; the action has no resource-level permissions) and
  `ec2:ReleaseAddress` conditioned on `ec2:ResourceTag/spawn:managed=true` AND
  `ec2:ResourceTag/spawn:instance-arn` equal to `${ec2:SourceInstanceARN}`, the
  ARN of the instance whose credentials signed the request. Launch writes
  `spawn:instance-arn` on the address at allocation. This is a widening, and it
  is justified by the cost leak it closes: without it an instance ending any way
  but `spawn terminate` left its address billing until someone noticed. An
  instance cannot release another instance's address or a user's, and a
  request not signed with instance credentials matches nothing. Because the
  instance role may tag any `spawn:managed=true` resource, which includes these
  addresses, `spawn:instance-arn` and `spawn:instance-id` join the keys the
  role is denied from writing or deleting (with `spawn:pre-stop` and the rest),
  so an instance cannot forge its ARN onto another's address. Instances launched
  before this keep their old role policy until a launch refreshes it, and
  addresses allocated before this carry no `spawn:instance-arn`; the reaper
  releases both kinds.
- `retry_class` has a new value, `spec_error` (not retryable): the task's spec
  does not parse, so nothing ran (#1149). Go API: `taskproto.RetrySpecError`,
  and `taskpool.Worker` gains `Results`, `AccountID` and `Region` fields plus the
  `taskpool.ResultsAPI` interface.

## [0.137.0] - 2026-10-20

### Added

- `dns.NewClientWithConfig`, `dns.Client.SetHTTPClient` and
  `aws.Client.DeleteInstanceTags` (Go API).

- `spawn list -o json` has a `first_launch_time` field: the instant the Age
  column is measured from (#1029). `launch_time` is still EC2's LaunchTime,
  the most recent start.

### Changed

- **Go API:** `cost.NewClient` takes an `aws.Config` instead of a DynamoDB
  client, and builds both the sweep-table client and the on-demand pricer from
  it (#967).

- **CI now runs golangci-lint, with a config that can only widen (#1028).**
  golangci-lint ran nowhere, and its defaults report ~500 findings on this repo
  (mostly `errcheck` on `fmt.Fprint*`). A new `.golangci.yml` enables the 23
  linters and the formatter that pass today; a new Lint job runs `make lint`
  (golangci-lint v2.15.0, pinned by version and sha256) over the root and every
  nested module, and `make check` runs it when golangci-lint is installed.
  `docs/lint-ratchet.md` records every linter still off with its finding count,
  and `lint_ratchet_test.go` fails if the config enables fewer linters than that
  record, adds path exclusions, or stops being run by CI. Enabling the floor
  meant small, behaviour-neutral cleanups: redundant loop-variable copies,
  conversions and initial assignments, and `reflect.Ptr` → `reflect.Pointer`.

- **`spawn list` Age is now the time since the instance was first launched**,
  across stops and starts — the age the TTL reaper's max-age ceiling acts on.
  It used to be the time since EC2's LaunchTime, which resets on every start, so
  an instance launched three days ago and restarted an hour ago showed `1h`. The
  list is sorted by the same instant. `spawn task diagnose`'s Age is the same
  figure (#1029).
- **`spawn cost <instance>` reports cost so far across every boot.** Compute is
  the on-demand rate × the instance's running time, from spored's
  `spawn:compute-seconds` (the total `--cost-limit` is enforced against), since
  compute bills only while running; with no such tag it is the wall clock since
  first launch, labelled as an upper bound. When `spawn:ebs-hourly-cost` is set
  it adds EBS over the time since first launch, which bills while stopped too.
  It used to be rate × time since EC2's LaunchTime: the latest boot only, and
  for a stopped instance the stopped time counted as compute. `spawn task
  diagnose`'s estimate uses the same compute time (#1029).
- Every age, TTL and cost figure in the CLI is measured from the earlier of the
  `spawn:launch-time` tag and EC2's LaunchTime — the rule the TTL reaper
  enforces with (GHSA-c93q-4jh7-6pvj) — through one shared helper
  (`ttl.LaunchAnchor`), so what is shown is what is enforced (#1029).

- `spawn connect <id> --session-manager` on a DCV app instance opens a shell
  instead of the streaming session. Plain `spawn connect` there still opens the
  session. A DCV instance's security group has no port 22, so this is the way to
  a shell, and the DCV failure messages now say so (#1153).
- `spawn app launch --health-path` applies to any web app, a catalog one
  included, and is refused for a DCV or desktop app. It used to be read only
  with `--web-port` and silently ignored otherwise (#1153).

- **Plugin declarations are now strict (#1116).** Each of these used to be
  accepted silently, so a plugin was not installed or was misconfigured with no
  word; each is now an error:
  - `spawn launch --config FILE` where FILE is missing or does not parse. The
    error was dropped and the instance launched with no plugins. It is now
    reported before any AWS work, `--dry-run` included.
  - A key the launch config does not have, at the top level
    (`instance_type:`, `ttl:`) or in a `plugins:` entry (`name:` instead of
    `ref:`), and a `plugins:` entry with no `ref`. The file is decoded with
    unknown fields refused, and the error names the line.
  - A plugin config key the plugin's spec does not declare
    (`spawn plugin install --config`, or a launch config's `config:` map). It
    used to be passed through and ignored. The error lists the keys the
    plugin accepts.

- **`spawn plugin validate` (and spored, which validates every spec it is
  sent) rejects a step that its executor would refuse or partly ignore
  (#1130).** Each section now accepts only the step types its executor runs,
  and for each type only the fields that executor reads:
  - `local.provision`: `run` (`run`, `env`, `capture`) and `push` (`key`,
    `value`);
  - `local.deprovision` and `local.reconcile`: `run` (`run`, `env`). A `push`
    there used to pass validation and then fail at teardown; `capture:` was
    parsed and discarded;
  - every `remote.*` section: `run` (`run`, `env`, `background`, `as_user`),
    `fetch` (`url`, `dest`, `sha256`) and `extract` (`src`, `dest`).
    `capture:` on a remote step used to be silently ignored.

  So `background:`/`as_user:` on a local step, `url:`/`dest:` on a `run`
  step, and the like, are now errors that name the field. `spawn plugin
  install` also validates the spec before running any local step, so a spec
  spored would refuse no longer leaves a controller-side footprint first.
  Every plugin in spore-host/spore-plugins passes `--strict`.

- **`spawn onboard` re-runs keep the existing role's ExternalId and refresh its
  policies.** A re-run without `--external-id` used to generate a new
  ExternalId, leave the existing role trusting the old one, and register the new
  one with the portal, which then could not assume the role. Now the ExternalId
  is read from the role's trust policy. If it can't be determined (no ID, or
  more than one), onboard refuses and asks for `--external-id`. Passing
  `--external-id` updates the trust policy to that ID. Re-runs also rewrite the
  trust policy and both inline policies (`PortalEC2Launch`,
  `PortalPassSporedProfile`) to the current documents, so a role made by an
  older spawn doesn't keep the grants it had before #817's scoping. Onboard
  won't rewrite a `spore-portal-onboard` role that isn't tagged
  `Purpose=spore-portal-onboard` (#1138).

- **The global `--region` now narrows instance lookups by name or ID.**
  `spawn logs`, `connect` and the other commands that resolve an instance used
  to search every region even when `--region` was given. With `--region` set,
  they now search only that region (#1160).
- **`spawn doctor` no longer fails an account just because it has no default
  VPC.** If the region has subnets, the "VPC & subnet" check now warns and tells
  you to pass `--subnet-id` on every launch, or to create a default VPC. Doctor
  can't know whether every launch will pass `--subnet-id`, and launches that do
  work without a default VPC. A region with no subnet at all still fails
  (#1161).

- **Reaper coverage follows the regions the reaper scans** (`spawn doctor` and
  the launch-time warning). It used to count any `spawn-ttl-reaper*` function
  in the launch region as coverage. Now it reads the function's
  `REAPER_REGIONS`, the same way the reaper does (unset means the reaper's 11
  default regions). A function that doesn't scan the launch region doesn't
  count. A function in another default region that does scan it counts. That
  second check is up to ten extra `lambda:ListFunctions` calls, made only when
  nothing in the launch region covers it. The member-role verdict now says that
  the central reaper's regions can't be read from the member account. The
  default region list moved to `pkg/reaperconfig` so the reaper and the CLI
  share it (#1157).
- **`spawn reaper status` reports coverage instead of NOT DEPLOYED** when this
  region has no `spawn reaper deploy` reaper but something else covers the
  account (the template reaper, a reaper in another region, or a member role).
  It uses the same probe as `spawn doctor`. If coverage can't be determined,
  status says that instead of claiming nothing is deployed (#1157).

- **A launch is refused when the account's `/spawn/dns/*` SSM setting can't be
  read.** A missing parameter still means "not set". Any other error
  (AccessDenied, throttling, the network) now refuses `spawn launch`,
  `spawn dns register` and `spawn dns delete`, naming the parameter and
  `ssm:GetParameter`. Before this, the error was treated as "not set", so a
  launcher without the read permission in an account that publishes a custom
  DNS deployment registered under spore.host's updater without any warning. If
  you can't be granted the read, set the value by flag, environment or config
  file instead; that skips the lookup (#1121).
- **A custom `--dns-api-endpoint` now needs the updater's function ARN.**
  The instance role's invoke grant was pinned to spore.host's function, so a
  self-hosted updater answered every spore with 403 and spored set
  `spawn:dns-status=failed`. The ARN can't be derived from a Function URL,
  so a custom endpoint without one is refused before anything is launched,
  unless `--instance-profile` names a role spawn doesn't manage (#1158).
- **Go API:** `aws.Client.ResolveDNSTarget` and `config.ResolveDNSTarget` take
  a function-ARN argument and return an error (#1121, #1158).

- **A re-deploy keeps the reaper's schedule and log retention (#1214).**
  `spawn reaper deploy` reapplied `--schedule` (default `rate(10 minutes)`)
  and `--log-retention-days` (default 30) every time, so re-running it without
  the flags reset a custom schedule or retention. Like the `REAPER_*` settings
  since #1154, they now change only when the flag is passed. Otherwise the
  live rule's schedule and the log group's retention are kept, and the defaults
  apply only to a new reaper (or a log group with no retention). Deploying now
  also reads the group with `logs:DescribeLogGroups`.

### Fixed

- `spawn dns register` and `spawn dns delete` signed the DNS updater request
  with the ambient AWS credentials, ignoring `--profile` and `SPORE_PROFILE`.
  With `--profile` naming another account, the record was computed for one
  account and the request signed by another. They now sign with the config
  that resolved the instance (#967).
- `spawn cost <sweep>` priced compute with the ambient credentials, so with
  only `--profile`'s credentials usable every instance came back unpriced. The
  pricer now uses the command's config (#967).
- `spawn dns --help` listed `list`, `register` and `delete` with blank
  descriptions, and `spawn --help` did the same for `status`. The long help said
  names are registered "when launching with --dns flag" (they're registered
  from `--name` by default) and gave `spawn dns delete` a DNS name rather than
  an instance (#1159).
- `spawn dns delete` blanked `spawn:dns-name` and left `spawn:dns-domain`,
  `spawn:dns-endpoint` and spored's `spawn:dns-status` behind. It now removes
  every `spawn:dns-*` tag (#1159).
- `docs/custom-dns.md` said `spawn status` shows `spawn:dns-status:
  registered`. It prints nothing on success, and the page now says how to read
  the tag (#1159).

- `spawn status`, `spawn list`'s TTL column and `spawn start`'s notice put the
  deadline of an instance with `spawn:ttl` but no `spawn:ttl-deadline` at EC2
  LaunchTime + TTL. After a stop/start that is later than the deadline spored
  and the reaper enforce (`spawn:launch-time` + TTL); they now show that one
  (#1029).
- `spawn status`'s "Max compute cost" is now compute so far across every boot
  plus the time to the deadline, as `spawn extend` sizes `--cost-limit`. It was
  rate × (deadline − EC2 LaunchTime), which after a restart left out every
  earlier boot (#1029).
- `spawn app launch <name> --image <ref> --web-port <n> --instance-type <type>`
  launches your own web-app image under a name that is not in the catalog, as
  `docs/bring-your-own-app.md` and the README already showed. It used to fail
  with "not found in catalog". `--instance-type` is required, because there is
  no catalog hardware spec to default from (#1153).
- `spawn app list -o json` prints the list as JSON. It used to print the table
  (#1153).

- `spawn app launch --image <ref>` with a bare ref (no `:tag`) launches
  `:latest` (or `--app-version`), as its own comment said. On a recipe app it
  failed with "has no default image tag" (#1153).
- The "Reconnect:" hint after an app launch said `spawn app launch <app>
  --name <session>`, which launches a second instance. It now says `spawn
  connect <id>` for a DCV session. For a web app it says to open the URL,
  because `spawn connect` opens a shell there (#1153).
- DCV failure messages said "inspect: spawn connect <id>, then /var/log/…",
  but on a DCV instance that opens the streaming session, not a shell. They now
  say `spawn connect <id> --session-manager` (#1153).
- The session page's "Restart Session" button navigated to a `spawn://` URL
  that nothing registers. It now shows the command that restarts the session
  and waits for it (#1153).
- `spawn service --host <box> --dry-run` no longer says a 1h idle timeout is
  applied, because `--host` launches nothing (#1153).
- Help examples that could not run: `spawn app --help` launched paraview and
  igv, which are recipe-only. `spawn service --help` showed a uvicorn example,
  but uvicorn rejects the `--addr` spawn appends. `spawn slurm` showed a
  `--spot` flag that doesn't exist and `spawn launch --params` (inline JSON)
  with no name. The README and `docs/bring-your-own-app.md` had the same
  problems (#1153).
- `docs/catalog-schema.md` and `docs/catalog-overlay.example.yaml` called a
  legacy `launch_command` launchable. They had no `args` row, and they said a
  cross-account grant is enough for a private image, which `spawn app launch`
  refuses (#1153).
- `spawn task run`'s error for a malformed results prefix named
  `resources.results_prefix`. The TaskSpec field is the top-level
  `results_prefix` (#1145).
- `spawn task --help` lists `run` and `status`, not only `diagnose` (#1145).

- `plugin.CheckSporeOS` (the Windows-spore refusal) now reads the OS through
  `NormalizeOS`, the normaliser a `type: platform` condition uses, so the two
  cannot disagree about whether a spore runs Windows (#1055).

- `spawn onboard` signs the portal phone-home for the region in the Function
  URL's hostname instead of `--region`, so onboarding a region other than the
  endpoint's no longer fails the signature check. `--region` is still sent in
  the request body as the onboarded region. `-o json` now writes only the JSON
  result to stdout, with progress lines on stderr (#1138).
- **Plugin outputs are now surfaced (#1117).** `spawn plugin inspect` promised
  "Outputs (surfaced after install)", but nothing ever showed them. A plugin's
  declared `outputs:` are now printed at the end of `spawn plugin install`, and
  `spawn plugin status` (text, and the `outputs` map in `-o json`) shows them
  afterwards. An output's value is the `local.provision` capture of the same
  name. An output nothing captures is listed as not recorded. Captures the
  spec does not declare as outputs are never shown, because they are often
  credentials captured only to push to the instance. Outputs are recorded
  on the machine that ran the install, so `status` shows them only there.
  `outputs:` entries may carry a `description:`, which the registry already
  uses, and `inspect` and `status` now print it.

- `spawn logs` falls back to its console cache, by name or instance ID, after
  EC2 stops listing the instance. Before, it failed with "no instance found"
  before checking the cache, though keeping the log after AWS drops the instance
  is why the cache exists. Cached dumps now record the instance name beside them
  (`<id>.name`). When reading the console output is denied, the error now names
  `ec2:GetConsoleOutput`, which the minimal IAM baseline leaves out (#1160).
- `spawn doctor` hints: the VPC hint names `--subnet-id` (it said `--subnet`).
  The Route 53 hint no longer says `--dns` breaks without Route 53 access,
  because records are registered through the spore.host DNS updater. "Nitro
  fleet" now appears among the skipped checks when credentials fail (#1161).
- `spawn footprint` reads every page of EventBridge rules instead of only the
  first 100. Its help and footer now say these are default-bus rules (Scheduler
  schedules aren't scanned), and they no longer call `scheduler-handler` a blind
  spot, since it is matched by exact name. `--all-regions` is described as the
  regions enabled in the account (#1161).
- `spawn reaper teardown --bucket` names the artifact bucket to remove. Deploy
  now records its bucket on the function (`spawn:artifact-bucket` tag), and
  teardown removes that one by default (#1157).

- `spawn reaper teardown` removes the artifact bucket chosen with `--bucket` at
  deploy time. It used to target only the default bucket name, so a custom
  bucket was left behind silently. For a reaper deployed before this release
  with `--bucket`, pass the same `--bucket` to teardown (#1157).
- `docs/infra-account.md` said `ACCOUNTS_TABLE` decides which accounts the
  reaper scans. That is set by `REAPER_ROLE_ARNS` (and `REAPER_SCAN_SELF`); the
  table only supplies the expiry verdict (#1157).
- `spawn launch --dns-function-arn` (also `SPAWN_DNS_FUNCTION_ARN`,
  `dns.function_arn` in `~/.spawn/config.yaml`, SSM `/spawn/dns/function_arn`)
  names the Lambda function behind a custom `--dns-api-endpoint`. The launch
  grants the instance role `lambda:InvokeFunctionUrl` on it, in its own inline
  policy `spored-dns-invoke-<hash>`, so spored can register through a
  self-hosted updater in another account. `--dry-run` shows the grant, and
  `-o json` carries `dns_function_arn` (#1158).
- `aws.Client.EnsureDNSInvokeGrant`, `aws.DNSInvokePolicyName`,
  `aws.LaunchConfig.DNSFunctionARN`, `config.DefaultDNSFunctionARN`,
  `config.SSMDNSFunctionARNPath`, `config.DNSSettingUnreadableError` and
  `dns.ValidateFunctionARN` (Go API).

- **`spawn reaper teardown --if-idle-for` refuses a value it can never
  satisfy (#1215).** Idleness is read from the reaper's logs, which cover at
  most 30 days, so a larger value (e.g. `1000h`) silently never fired. It is
  now rejected with an error before any AWS call.

## [0.136.0] - 2026-10-20

### Added

- `spawn reaper deploy --spot-requests`, `--net-resources`, `--max-age` and
  `--notify-url` set the in-account reaper's `REAPER_SPOT_REQUESTS`,
  `REAPER_NET_RESOURCES`, `REAPER_MAX_AGE` and `REAPER_NOTIFY_URL`. Before
  this, a CLI-deployed reaper's Spot-request and network sweeps could only stay
  in `report`. Values are checked against what the reaper itself accepts
  (`off|report|reap`, a positive duration, an http(s) URL) before anything is
  touched; the reaper now parses these with the same code (`pkg/reaperconfig`),
  so the two cannot disagree. An empty value removes the variable. The notify
  URL is never printed, and `--notify-url -` reads it from stdin so it need not
  appear on the command line (#1154).
- `spawn reaper deploy --log-retention-days` (default 30) and
  `spawn reaper teardown --keep-logs` (#1156).
- `taskproto.ExitUserTerminated` (`user_terminated`), the terminal-flush
  reason `spawn terminate` passes to a task's flush hook (#802).

- `aws.Client.DeleteInstanceProfileAndRole` and
  `aws.Client.LiveInstancesWithTag` (Go API). Unlike `ListInstances`,
  `LiveInstancesWithTag` returns the describe error rather than skipping the
  region, because callers delete things when the result is empty.

- **`spawn pool create --cost-limit`** sets a spend ceiling in USD per worker,
  covering compute and storage, passed to spored as `spawn launch --cost-limit`
  is. spored terminates a worker that reaches it. Before this a pool had no
  spend bound except `--ttl` (#1183).

- **`spawn pool create --container IMAGE`** (repeatable) declares the images
  the pool's tasks run. If any of them is a private-ECR image, the workers' role
  gets the ECR pull grant `spawn task run` gives (`ecr:ReadOnly`). Before this a
  private-ECR image could not run in a pool (#1148).

### Changed

- **Idle `spawn burst` members now terminate instead of stopping** (#1204).
  A burst member has nothing to come back to, and a stopped one kept billing its
  EBS volume until its TTL. `--idle-timeout` now ends the member, like pool
  workers and `spawn service` (#1200). An instance needs a spored that includes
  #1200; an older spored still stops it.

- **`spawn extend` now exits non-zero when spored could not be reloaded.**
  The new TTL/cost limit is still written to the tags, but a reload that
  reached no path used to print a warning and exit 0, so a script raising a
  limit mid-run could not tell that spored was still enforcing the old one.
  The job-array form also exits non-zero when any member was not updated or
  not reloaded (#1113).

- **`spawn task status` no longer reports a task with no completion record as
  running unless its instance is live (#802).** It now asks EC2: a live
  instance is `running` (exit 2); a task whose instances are all terminated or shutting down is
  `terminated (no completion record)` with EC2's reason (`--check-complete`
  exit 1); no instance at all, or a failed lookup, is `unknown` (exit 3). A
  task killed without a terminal flush used to read `running` forever.

- **`spawn launch` verifies spored by its heartbeat tag, not only over SSM
  (#1053).** spored stamps `spawn:last-heartbeat` about once a minute on Linux
  and Windows. The launch now waits for that tag, read with
  `ec2:DescribeInstances` (already in the IAM baseline; no new permission).
  SSM is still asked as a second signal and can answer first, but a missing
  SSM agent is no longer the verdict. Before, every launch on an AMI without
  `amazon-ssm-agent` (official Rocky 8/9, RHEL) waited 4.5 minutes and then
  exited 1 with "spored never came up", even when spored was healthy. A launch
  with no heartbeat and no SSM answer inside the budget (5 min Linux, 10 min
  Windows) still fails as before and exits 1. The message now says which
  evidence was missing, and what SSM saw.
- `spawn launch --wait-for-ssh=false` now prints that spored was not verified,
  and the flag's help says it skips spored verification as well as the SSH
  wait (#878).

- `spawn task run`: a container task is now sized to at least 2 GiB of memory (`taskproto.ContainerMemoryFloorGiB`), including when `resources.memory_gib` is set lower. A container spec with only `resources.cpu` used to size to t4g.nano (0.5 GiB), where the boot-time `dnf install docker` was OOM-killed in 3 of 3 runs (#1180). The dry run says when the floor raised the request. A pinned `resources.instance_type` is still honoured as given, with a warning when it is below the floor. New `TaskSpec.SizingRequest()` returns the request to size against; Go callers of `taskproto.Size` should pass it instead of `spec.Resources`. `taskproto.Size` now also drops candidates whose known memory is below the requested memory, rather than trusting the finder to have filtered them.
- `taskproto.RetryInstanceHealth` (`instance_health`) is now produced and is retryable (#1180).

- **Re-running `spawn reaper deploy` keeps the existing reaper's settings
  (#1154).** It used to carry over only `REAPER_DRY_RUN` and replace the rest of
  the Lambda environment, so an upgrade silently dropped every other `REAPER_*`
  variable, including a hand-set `REAPER_SPOT_REQUESTS=reap`, a raised
  `REAPER_MAX_AGE` or a notify URL. It also reset the scanned regions to the
  deploy region when `--regions` was omitted. Now every existing `REAPER_*`
  variable is kept unless a flag sets it, and `--regions` defaults to the
  existing reaper's regions (the deploy region only for a new reaper).

- `aws.LaunchConfig.TerminateOnIdle` (Go API), written to the instance as
  `spawn:terminate-on-idle=true`. spored then terminates the instance when its
  idle timeout runs out, instead of stopping it; it takes precedence over
  `HibernateOnIdle`. A Spot launch whose only stop-shaped setting is an idle
  timeout with `TerminateOnIdle` stays a one-time request (#1151).

- **`spawn app launch` now sets an 8h TTL when `--ttl` is not given (#1150).**
  An app's idle action is still a stop, so a session can be restarted, but
  before this an abandoned session stayed stopped, billing its EBS volume, with
  no deadline at all. The TTL is printed at launch and in the ready summary,
  `--ttl` still overrides it, and an explicitly empty `--ttl ""` is refused.
  There is no `--no-timeout` for apps: pass a longer `--ttl` or use
  `spawn extend`.
- **`spawn service` instances are now terminated when idle, not stopped
  (#1151).** `--idle-timeout`'s help already said "Terminate"; spored stopped
  the instance instead, leaving a billable EBS volume for a service nothing
  would restart. The dry run's `Lifetime:` line now names the idle action.
- **`spawn pool` workers are now terminated when idle, not stopped** (refs
  #1146). "Workers self-terminate on idle-timeout" was the documented
  behaviour; a drained worker was stopped and billed its EBS volume until its
  TTL.
- `spawn status` and `spored status` say "terminates" for an instance with
  `spawn:terminate-on-idle`.
- **`spawn reaper deploy` now creates the reaper's log group and sets a 30-day
  retention, and `spawn reaper teardown` deletes the log group (#1156).** Before
  this, `/aws/lambda/spawn-ttl-reaper-selfscan` was created by the first
  invocation with no retention, kept its logs forever, and outlived a teardown.
  `--keep-logs` leaves the group. The deploying credentials now also need
  `logs:CreateLogGroup`, `logs:TagResource`, `logs:PutRetentionPolicy` and, for
  teardown, `logs:DeleteLogGroup`.
- **Every shell Lambda deploy now creates the log group before setting its
  retention, and a failed retention call fails the deploy (#827).**
  `put-retention-policy` creates nothing, so on a function never invoked it was
  a tolerated no-op and the group Lambda created later had no retention.
  `scripts/lambda-deploy-census.sh` now requires the `aws logs create-log-group`
  invocation before the retention call, rejects a tolerated retention call, and
  covers Go deploy paths too: any package that calls `CreateFunction` must call
  `CreateLogGroup` and `PutRetentionPolicy`.
- **Pool workers that run out of work now finish and terminate (#1146).** A
  cleanly drained worker wrote no completion record, so
  `on_complete=terminate` never fired, and nothing retired a worker whose
  queue was gone. Now:
  - a worker that drains writes the completion record, and spored terminates
    the instance;
  - a worker whose queue is deleted (`spawn pool drain`) drains at its next
    poll. It used to retry the missing queue once a second until its TTL;
  - a worker whose every claim fails for `--idle-timeout` drains too, instead
    of retrying until TTL;
  - a worker that exhausts its 20 restarts writes a failed completion record
    and terminates. It used to wait for the TTL.
- **`spawn pool drain` is idempotent (#1183).** Draining a pool whose queue is
  already gone reports "already drained" and exits 0, after one lookup. It
  used to retry for about 30 s and exit 1.

- **Each pool now gets its own worker IAM role (#1147).** All pools shared the
  role `spawn-pool-worker`, whose one inline policy is scoped to a single run's
  queue. A second `spawn pool create` overwrote it and cut the first pool's
  workers off from their queue. Each pool now gets its own role and instance
  profile, `spawn-pool-worker-<hash of run id>`, tagged `spawn:pool-run-id`.
  `spawn pool drain` deletes them once no worker of the pool is pending,
  running, stopping or stopped. A worker whose role is deleted loses its
  credentials and spored can no longer terminate it. Workers terminate soon
  after their queue is deleted, so drain re-checks for up to `--wait` (new,
  default 2m) before removing the role. If workers remain after that, drain
  keeps the role and says so; re-run `spawn pool drain` to remove it. The old shared `spawn-pool-worker` role is no longer
  used: `spawn orphans` lists it, and you can delete it.

### Fixed

- **Pooled task outputs are tagged again (#1148).** The pooled job script
  uploads outputs with `spored provenance upload`, as `spawn task run` does,
  but the pool worker policy granted only `s3:PutObject`. Without
  `s3:PutObjectTagging` and `s3:PutObjectAnnotation`, pooled outputs fell back
  to untagged uploads, out of reach of the results-retention lifecycle (#1076).
  The worker policy now grants both on the results bucket and on every
  `--s3-write` bucket, and not on read-only buckets.
- **`spawn extend` reloads spored over SSM on keyless instances (#1113).**
  The reload ran only over SSH, so on a `spawn task run` instance (SSM-only, no
  launch key) the new limit was tagged but never applied and the run was killed
  at its original cap. extend now runs the same `spored reload` via SSM
  `AWS-RunShellScript` when there is no key or public IP, or when SSH fails.
  The manual fallback it prints is now `spawn connect <id> -- 'sudo spored reload'`.

- **`spawn terminate` flushes a task's log and completion record before
  terminating (#802).** On a running `spawn task run` instance it first runs
  the task's terminal-flush hook over SSM with reason `user_terminated`, the
  same hook TTL and cost-limit kills run (#643), so `command.log` and a
  `completion.json` (`state: failed`, `terminal_reason: user_terminated`)
  reach S3 instead of being lost with the instance. The flush is bounded (45s
  on the box, 75s SSM wait) and best-effort: if it fails, the instance is
  terminated anyway. Job-array terminates flush all task members concurrently
  before the first TerminateInstances call.

- **spored acts on a short task's completion even when it finishes before
  the `spawn:on-complete` tag is visible (#270).** spored reads on-complete from
  EC2 tags, which are eventually consistent and were re-read only on tick 1
  and every 5th tick. A task that wrote its completion file before the tag was
  readable was ignored until the next scheduled refresh, and if that refresh
  had already happened, the instance ran to its TTL. Now, when the
  completion file is present and no on-complete action is known yet, spored
  re-reads the tags on that tick. When the completion-file tag is not visible
  either, it checks the default `/tmp/SPAWN_COMPLETE`. These extra refreshes
  are capped at 5 per instance, so a workload that writes the file with no
  on-complete configured does not cost a DescribeTags call every minute.
- `spawn launch --output json` reports `spored_verified`: `true` when the
  launch confirmed spored came up, `false` when it did not check
  (`--wait-for-ssh=false`).
- Go API (`pkg/aws`): `HeartbeatTag`, `DescribeSporedLiveness`, `SSMOnline`,
  `AccessDeniedError`, `IsAccessDenied` and `IAMBaselineURL`.

- **A missing caller permission is no longer reported as "spored never came
  up" (#1126).** An AccessDenied from `ssm:DescribeInstanceInformation` was
  polled as "not online yet" until the timeout, and the launch then told the
  user the instance had no safety net. AccessDenied from SSM
  (`DescribeInstanceInformation`, `SendCommand`, `GetCommandInvocation`) now
  returns at once as an error naming the action. During launch verification
  it only disables the SSM signal, because the heartbeat tag still answers.
  If `ec2:DescribeInstances` itself is refused, the launch stops at once with
  "spored could not be verified" and the action's name. It does not say the
  instance is broken. This also applies to `spawn connect`, the warm-AMI build
  and MPI cohort assembly, which share `WaitForSSMOnline`.

- `spawn task run`: the Spot→on-demand fallback (`resources.fallback: on_demand`) now launches with the same storage mounts and terminal-flush hook as the Spot attempt. The retry passed empty launcher options, so a fallback instance came up without its EFS/FSx/volume mounts and without the hook that writes a completion record on a TTL kill (#1144).

- `spawn task run`: when a container task's Docker install fails or its daemon never starts, the wrapper now skips the image pull and run and records `retry_class: instance_health` (retryable). It used to ignore the failure, run `sudo docker run` anyway, and record `app_error` (not retryable) for a command that never ran (#1180).
- `spawn reaper deploy --spot-requests`, `--net-resources`, `--max-age` and
  `--notify-url` set the in-account reaper's `REAPER_SPOT_REQUESTS`,
  `REAPER_NET_RESOURCES`, `REAPER_MAX_AGE` and `REAPER_NOTIFY_URL`. Before
  this, a CLI-deployed reaper's Spot-request and network sweeps could only stay
  in `report`. Values are checked against what the reaper itself accepts
  (`off|report|reap`, a positive duration, an http(s) URL) before anything is
  touched; the reaper now parses these with the same code (`pkg/reaperconfig`),
  so the two cannot disagree. An empty value removes the variable. The notify
  URL is never printed, and `--notify-url -` reads it from stdin so it need not
  appear on the command line (#1154).
- `aws.LaunchConfig.TerminateOnIdle` (Go API), written to the instance as
  `spawn:terminate-on-idle=true`. spored then terminates the instance when its
  idle timeout runs out, instead of stopping it; it takes precedence over
  `HibernateOnIdle`. A Spot launch whose only stop-shaped setting is an idle
  timeout with `TerminateOnIdle` stays a one-time request (#1151).

- **Help and notices no longer say idle "terminates" where it stops (#1151,
  #1132).** The auto-applied 1h idle-timeout notice said "Instance will
  terminate after 1 hour of inactivity" on every launch path; it now names the
  real action (stop, hibernate, or terminate for `spawn service`), and so does
  the sweep's. Corrected: `spawn launch --idle-timeout`, `--active-ports` and
  `--active-processes`, `spawn burst --idle-timeout`, `spawn app`, and
  `spawn defaults` (`hibernate-on-idle`: "Hibernate instead of stopping on
  idle"). A new test walks the whole command tree and fails when a command's
  help says idle terminates but its launch config does not set
  `TerminateOnIdle`.
- `spawn notify workspace add --help` showed the deprecated `workspace-add`
  spelling in its example, and two error messages named it too (#1132).

- **`spawn reaper teardown --if-idle-for` and `spawn reaper status` measure
  idleness correctly (#1155).** Any reaper that had run without reaping counted
  as idle for the whole 30-day window. So a reaper deployed an hour ago could be
  removed by `--if-idle-for 720h`, and a value above 30 days could never
  trigger. Idleness is now measured from the first run still in the logs or the
  deployment's creation (its role's creation date), whichever is later. Status
  prints that duration instead of "at least 30 days". Only `REAPED` used to
  count as work, so Spot requests `CANCELLED`, replacements `TERMINATED` and
  DNS `EXPIRING` all counted as idle; every action the reaper logs now counts.

- **`spawn connect` to an app session no longer opens another account's host
  (#1152).** When there was neither a session file nor a ready-url, the DCV
  fallback appended one hardcoded account's subdomain to the instance's DNS
  name, so every other account opened a host that wasn't theirs or didn't
  exist. The host is now the instance's own FQDN, built from its
  `spawn:account-id` (or `spawn:account-base36`) and `spawn:dns-domain` tags,
  so a custom DNS domain (#891) is honoured. Without them it falls back to the
  public IP. A new test fails on any account subdomain of spore.host written
  as a string literal in non-test Go code.

## [0.135.2] - 2026-10-19

### Added

- `launcher.BootstrapConfig.CompletionFile` (Go API, for headless callers such
  as lagotto). It sets the path the `--command` wrapper writes its completion
  record to. Pass the same value as `LaunchConfig.CompletionFile`, the file
  spored watches. Empty means `/tmp/SPAWN_COMPLETE`, as before (#1131).

### Fixed

- **`spawn launch --efa` and `--placement-group` without `--mpi` now take effect.**
  `EFAEnabled` was set only on the `--mpi` path, so a single launch with
  `--efa` passed the EFA pre-flight, printed nothing, and came up with a plain
  ENA interface: no `/sys/class/infiniband` device and `fi_info -p efa` empty.
  Any EFA pair of different instance types (a GPU client and a CPU proxy, say,
  which cannot be one MPI job array) was affected. The flag now reaches the
  launch config the way `--subnet-id` and `--security-group-ids` do since #667.
  `--placement-group` had the same gap: applied only under `--mpi`, so an
  explicit cluster placement group was silently ignored on a single launch.
- **spored now installs on Rocky Linux 8 and RHEL 8 (#1056).** Every download
  in the spored installer used `curl --retry-all-errors`, an option added in
  curl 7.71. EL8 ships curl 7.61.1, which rejects it, so every download failed,
  the bootstrap aborted, and the instance ran with no spored. That meant no
  TTL, no cost limit and no `--on-complete`. Downloads are now retried by a
  portable loop with the same behaviour (up to 5 retries, 2 s apart, on any
  network or HTTP error). curl's error message is no longer hidden on the
  final attempt, so a usage error no longer looks like a network failure. A
  new test runs the installer against a curl that accepts only curl 7.61.1's
  options, and another checks every curl command spawn generates against that
  option set.
- **spored now starts on SELinux-enforcing AMIs such as Rocky Linux 9
  (#1052).** The installer downloads spored to `/tmp` and moves it into
  `/usr/local/bin`. A move keeps the file's SELinux label, here
  `cloud_init_tmp_t`, and systemd is not allowed to run a file with that
  label. spored failed with `status=203/EXEC` on every restart, and the
  instance again had no TTL or cost limit. The installer now runs `restorecon`
  on the binary after installing it, and on the spored systemd unit, and
  `spawn upgrade-spored` does the same when it rolls back. Nothing changes on
  hosts without SELinux (Ubuntu) or with it disabled.

- **`--command` now honours a custom `--completion-file`** (#1131). The
  wrapper always wrote its completion record to `/tmp/SPAWN_COMPLETE`, while
  spored watched the file `--completion-file` named. With any other path,
  `--on-complete` never fired and the instance ran until its TTL. The wrapper
  now writes the configured path, creating its directory if needed. The
  default is unchanged.
- **`--mpi-command` now honours a custom `--completion-file`** (#1170). The
  MPI script wrote every completion record, on rank 0 and over ssh on each
  peer, to `/tmp/SPAWN_COMPLETE`, while spored watched the file
  `--completion-file` named. With any other path, no member of the cohort
  fired `--on-complete` and every instance ran until its TTL. Each write now
  targets the configured path, quoted for both rank 0's and the peer's shell,
  creating its directory if needed. The default is unchanged.
- **A `--batch-queue` file may omit `queue_id`** (#1131). spawn was meant to
  generate an ID when the file has none, but validation rejected the file
  first, with `queue_id is required`. A missing `queue_id` now gets a
  generated `queue-<timestamp>` ID.

- **A container task now gets its `env`** (#1059). For a task with
  `container`, the spec's `env` map was set in the host shell but never
  passed to `docker run`, so the command ran without it. Each key is now
  passed with `-e KEY=VALUE`. Host-mode tasks are unchanged. The values
  appear on the instance's `docker run` command line, so they are visible
  in its process list while the task runs.
- **`spawn task run --wait` now exits non-zero when the task failed**
  (#1143). If the command exited 0 but the task was still marked failed,
  `--wait` exited 0. The common case is a declared output that failed to
  upload (`retry_class: output_delivery_error`). `--wait` now exits 1 for
  any state other than `completed`, matching
  `task status --check-complete`. A non-zero command exit code is still
  returned as before.

- **Windows spored shuts down promptly** (#1104). Every Windows idle and
  session reader (CPU, network, disk, processes, `quser`, port connections) and
  the `msg.exe` user warning started a fresh PowerShell or tool process that
  nothing could stop, so a monitor tick in flight held up spored's shutdown,
  hibernate preparation and resume handling until its readers finished: about
  14 seconds on a CI runner. Each of these processes, and the Linux `who`,
  `pgrep` and `wall` calls, now stops when spored's context is cancelled and is
  bounded by a 30-second timeout otherwise. The accelerator and DCV probes on
  the idle path now take the same context instead of a fresh one. A new test
  fails if any file in `pkg/agent`, for any GOOS, starts a process with
  `exec.Command`.

- **`--account` / `SPORE_ACCOUNT` now stops spawn acting in the wrong account**
  (#1118). The flag was documented as an "optional guard" and the docs said
  spawn "refuses to act if your credentials resolve to a different account",
  but only `spawn doctor` read it. `spawn --account 111111111111 launch …` with
  credentials for another account launched anyway. Now, when an account is
  pinned by `--account`, `SPORE_ACCOUNT` or `account =` in
  `~/.config/spore/config.toml`, spawn checks the credentials with STS
  `GetCallerIdentity` before it uses them. It refuses, naming both accounts, on
  a mismatch, and also when the account can't be determined. The messages match
  truffle and lagotto.
  - The check runs once per set of credentials, when spawn first needs them.
    Commands that never touch AWS (`version`, `completion`, `help`) make no STS
    call. With no account pinned nothing changes and there is no extra call.
  - Credentials for spawn's control-plane account through a dedicated infra
    profile (`SPAWN_INFRA_PROFILE`, default `spore-host-infra`) are exempt:
    that account is separate by design. When infra resolves through your own
    profile, it is checked like everything else. The compute profile, where
    instances launch, is always checked.
  - `spawn doctor` reports the mismatch as a failed `account` check and stops
    there instead of probing an account you said you didn't mean.

- **A queue file using `"backoff": "exponential-jitter"` now loads** (#1119).
  The retry loop has always implemented it, but validation accepted only
  `fixed` and `exponential`, so the documented example was refused at launch.
  Validation now asks the retry implementation which strategies exist instead
  of keeping its own list, rejects a `jitter` outside 0–1, and names the
  supported strategies when it refuses one.

- **The `spawn launch` summary closes every section it opens** (#1125). The
  `IAM instance profile` line came after the last rule, so the output ended
  like a box with no bottom, and two rules sat back to back. The profile line
  is now in the instance details, the DNS record and its connect hint are in
  the connect section, and each section ends with one rule. The progress list
  no longer leaves `Installing spawn agent` pending (⏳) after the agent is
  verified. spored installs from user-data, nothing ever started that step,
  and `Verifying spored agent` already reports it.
- **`spawn list` prints `Searching all AWS regions...`, not six dots** (#1129).
  It added `...` to a message that already ends in one, in all six languages.
  A new check renders every localized formatted message in every supported
  language and fails on a doubled ellipsis.
- **`spawn doctor` shows truffle's version on one line** (#1140). It printed
  all of `truffle --version` (version, `Git Commit:`, `Build Date:`), and the
  extra two lines looked like checks without a status symbol. It now keeps the
  first line, does the same for the AWS CLI, and folds any multi-line detail
  or fix onto one line. Every line of the text report before the summary
  starts with a status symbol or `→`.

- **A Windows CI flake in the on-complete retry test.** The test timed five completion ticks against a 300 ms grace period, and on the Windows runner the user-warning probes around the grace sleep made that wall-clock bound fail. It now counts the grace waits through a test seam instead of timing them.

### Security

- **Pipeline stage instances now install spored through the verified
  installer (#1173).** The pipeline orchestrator Lambda generated each stage
  instance's user data with its own inline install: `curl` straight into
  `/usr/local/bin/spored`, then a sha256 served from the same bucket as the
  binary. It never checked the signature, so whoever could write to the
  binaries bucket could run code as root on every pipeline stage instance.
  This is the gap GHSA-3x4q-99gh-fxhx (#819) closed for the other install
  paths. The stage user data now embeds the spore.host signing key and calls
  the shared installer from `pkg/sporedinstall`, which requires the checksum
  AND the signature, installs nothing on any failure, and fails the stage
  (FAILED marker, instance terminated) rather than run an unverified agent.
  The gate that checks every spored download path for the installer now scans
  every `.go` file in the repository, nested Lambda modules included; it
  previously looked only in `cmd/` and `pkg/`, which is how this path was
  missed. Takes effect only once the pipeline-orchestrator Lambda is
  redeployed.

## [0.135.1] - 2026-10-18

### Security
- **An instance can no longer make itself unreapable** (GHSA-c93q-4jh7-6pvj).
  The TTL reaper's 7-day max-age ceiling, which caps a deadline an instance
  rewrites, measured age from the `spawn:launch-time` tag. It also found
  instances only through `spawn:managed=true`. The instance role could write
  both tags, so an instance could stay "young" forever, or drop out of every
  reaper check.
  - The instance role now denies writing or deleting `spawn:launch-time` and
    `spawn:managed`, alongside the three keys GHSA-8q3p-qcv8-2pr4 protected.
    Nothing on the instance writes either key. A role is updated the next time
    `spawn launch` runs in its account, and the change applies to instances
    already running.
  - The reaper measures age from the **earlier** of the tag and EC2's launch
    time. A forged later value is ignored, and a stopped-and-restarted
    instance still counts from its first launch. This takes effect when the
    reaper is redeployed (`spawn reaper deploy`, or the template deployment).
  - A new gate discovers the tag keys the reaper bounds lifetime with from its
    source, and fails if the instance role can write any of them.

## [0.135.0] - 2026-10-18

### Changed

- **`spawn launch` asks before spending money on an interactive terminal**
  (#1115). It shows what it's about to launch (instance, or count and type for
  an array; purchase; lifetime; rate) and the **maximum cost**, which is the
  on-demand rate for the whole TTL, for every instance, plus any `--fsx-create`
  filesystem. It then asks `[y/N]`; `--yes` skips the question. Previously
  `--yes` was documented as "Auto-approve cost estimate", but a single launch
  showed no estimate and asked nothing: `echo n | spawn launch …` launched.
  Non-interactive runs (scripts, CI, pipes) launch exactly as before, without
  asking, but every launch now prints its maximum cost to stderr. The `--yes`
  help now lists what it actually skips.

### Fixed

- **Launch progress no longer shows `?` for every running and skipped step**
  (#1124). The progress display asked the symbol catalog for `running` and
  `skipped`, keys it doesn't have (they're `progress` and `skip`), so each got
  the unknown-symbol fallback: `?`, or `[?]` with `--no-emoji` or
  `--accessibility`.
- **Launch progress no longer lists steps for features you didn't ask for**
  (#1124), such as `FSx Lustre filesystem` on a launch with no FSx, or
  `Creating security group` when none is needed. Skipped steps aren't shown.

- **`spawn status` polish from the 0.134.0 demo rehearsal** (#1114):
  - the idle line reads "idle for 1m 51s — stops in 3m 8s", not "stops for
    1m 51s — stops in 3m 8s";
  - for the first minute after a boot, before spored's first sample, it shows
    one line, "Idle check: first reading pending …", instead of three
    "unknown (single sample; assuming active)" readings (the JSON reports
    `idle_reading.source: "pending"`);
  - the compute-cost duration says it's billed time (compute plus boot), so it
    no longer looks like it contradicts the elapsed line.
- **`--mpi-command` output no longer starts with ssh "Permanently added … to the
  list of known hosts" warnings** (#1114). The MPI cohort's ssh config now sets
  `LogLevel ERROR`.
- **`spawn upgrade-spored` help names `--allow-downgrade`**, the flag it
  actually takes, instead of the deprecated `--force`.

- **`--active-ports` help no longer claims open connections prevent idle
  termination.** They never did, by design: an abandoned browser tab holds a
  connection open forever, so spored counts the ports' connections (the
  `spawn:logged-in-count` tag) without letting them block idle. The help now
  says so and points to `--active-processes` for keeping a busy app alive.
- **`spawn results retain` help names the permission a versioned bucket also
  needs:** `s3:PutObjectVersionTagging`, because it writes the tags of the exact
  version it read.

- **Windows: paths on a mapped network drive are no longer refused** (#1107).
  `--user-data-file` and spored's result uploads refused every existing path
  on a drive mapped to a network share (`Z:\data\out.txt`), because Windows
  names such a file `\\server\share\data\out.txt` and the check from #1000
  refused UNC paths outright. A path is now admitted when it stays on the share
  its drive is mapped to. A link on the drive that leads anywhere else is still
  refused, and so is a drive mapped to a share on this machine's own system
  volume (`\\localhost\C$`, or a share of `C:\Windows`), which would
  otherwise reach the system directories under a name no check recognises.
  That is decided by volume serial number, so a share on another machine whose
  system volume has the same serial (both cloned from one image) is refused
  too.

### Documentation

- README: the commands table lists the 23 commands it was missing (`task`,
  `pool`, `array`, `results`, `reaper`, `logs`, `doctor`, `footprint` and
  others), drops the removed `stage`, and uses `sweep cancel` and `cleanup` in
  place of their deprecated spellings. Found in the documentation audit
  (spore-host/spore-host#585).

## [0.134.0] - 2026-10-18

### Added

- **`spawn results lifecycle`: tier task outputs by what they cost to make
  again** (#1076, phase 3; dry run unless `--apply`). It installs two S3
  lifecycle rules on a bucket. `spawn:regen-class=cheap` outputs go to
  Standard-IA at 30 days and Glacier Instant Retrieval at 90, and `moderate`
  outputs to Standard-IA at 60. Every rule requires `spawn:retain=false` and at
  least 128 KiB, so expensive, unclassified, retained and older outputs are never
  moved. The rules only tier; nothing is deleted. It works on this account's
  results bucket by default, or on any bucket you own with `--bucket`. The
  bucket's other lifecycle rules and its transition minimum-size setting are
  kept exactly as they are. Every call names your account as the expected
  bucket owner, and the configuration is read back after writing. The dry run
  shows the costs: request charges per object moved, retrieval per GB, and AWS's
  minimum storage durations. Prices are a labelled, dated static table.
  `--remove` takes spawn's rules out again. Nothing installs the rules
  automatically.
- **`spawn results retain <s3-uri>...`** marks outputs to keep
  (`spawn:retain=true`), or with `--off` lets the rules apply. A URI ending in
  `/` covers a prefix. It keeps every other tag, since S3 replaces the whole tag
  set on a write, and only changes spawn outputs. `--off` is also how outputs
  written before v0.133.0 are opted in to tiering.
- **`spawn results retention` shows what tiering itself would cost**: the
  one-time transition request charge for the objects the rules would move.

## [0.133.0] - 2026-10-17

### Added

- **`spawn results retention`: what task results cost to keep, and what
  retention by regeneration cost would do** (#1076, phase 2; read only). For a
  bucket (the account's results bucket by default, or `--bucket`/`--prefix`), it
  reports run by run: what is stored and its monthly storage cost, what it would
  cost once the decided tiering rules had moved it, and whether the deletion
  sweeper could prove the run regenerable today, with every reason it can't. An
  input you can't read counts as unproven, never as unchanged. Storage prices
  are a static us-east-1 table from the AWS Price List API, labelled as such.
  Nothing is tiered, tagged or deleted.

### Changed

- **Task outputs are tagged `spawn:retain=false` at upload** (#1076). Every
  future tiering or deletion rule requires that tag, because an S3 lifecycle
  filter can't negate: setting it to `true` will take an output out of every
  rule. Outputs written before this match no rule and are kept.

## [0.132.2] - 2026-10-17

### Fixed

- **An idle-check test no longer fails intermittently on Windows CI.** A
  monitor-loop test cancelled its loop without waiting for it to exit, so on
  the slower Windows runner a final tick landed in the next test and counted as
  a sample it never took. The test now waits for the loop to stop. Test-only.
- **A plugin-runtime test no longer fails CI intermittently.** A health check
  already in flight could still write a plugin's state while the test's temp
  directory was being removed ("directory not empty"). Health loops are now
  tracked, and the tests wait for them to exit before cleaning up. Test-only;
  spored's behaviour is unchanged.
- **`--idle-timeout` works again on Linux: an unused instance now stops**
  (#1093). It never could on any supported AMI, because both idle thresholds
  were 100 KB a minute and an instance nobody is using does more than that on
  its own. Measured untouched for 20 minutes on AL2023 (arm64 and x86_64) and
  Ubuntu 24.04, disk I/O was 0.14–7.8 MB a minute, almost all
  systemd-journald's own logging, and network was up to ~190 KB a minute from
  the SSM agent and AL2023's telemetry agent. So every check read "Not idle:
  Disk I/O …" and the instance ran to its TTL, including under the default
  `--idle-timeout 1h`. Before v0.129.0 this was hidden: NVMe disks weren't
  counted (#937), so disk read as zero. The thresholds are now 16 MiB of disk
  I/O and 1 MiB of network a minute, set from those measurements. A job
  writing or transferring data still clears them easily; CPU, open sessions
  and accelerators are judged as before.
- **`spored status` and `spawn status` show real CPU, network and disk
  readings, and why an instance is or isn't idle** (#1090). Every status
  screen used to print all three as `unknown (single sample; assuming active)`
  however long the instance had run. The status command took its own one-off
  sample, with nothing earlier to measure against, while the daemon measured
  every minute. spored now publishes each minute's idle evaluation (in
  `/run/spored/idle-state.json`, so the write isn't itself disk I/O), and
  status shows it. A new `Idle check:` line gives spored's own verdict, e.g.
  `Not idle: Disk I/O 1204224 bytes (spored's last check, 20s ago)`, which
  until now appeared only in `/var/log/spored.log`. JSON output gains
  `idle_reading`. When the daemon has published nothing recent (not running,
  or on Windows), status samples once as before and labels it
  `daemon reading unavailable; one-shot sample`.

## [0.132.1] - 2026-10-17

### Fixed

- **Task outputs upload again on v0.132.0 spores** (#1094). On every instance,
  v0.132.0's output upload (`spored provenance upload`) failed with
  `Invalid region`, so every `spawn task run` and pooled task with outputs
  lost them and was reported as `output_delivery_error` even when the command
  succeeded. spored now takes the region from instance metadata, as the AWS CLI
  does. If the provenance upload fails for any other reason, the output is
  uploaded untagged with `aws s3 cp` instead of being lost; untagged outputs are
  kept, never tiered or deleted. Input provenance, which failed silently the
  same way, is recorded again too.

- **A plugin's health loop is no longer lost after a failed stop or hibernate**
  (#1091). A health check that finished just as spored stopped its plugins
  could record the plugin as running again, overwriting "stopped". If the stop
  then failed, spored resumed only the plugins recorded as stopped, so that one
  never got its health loop back, and a later failure went unnoticed.

## [0.132.0] - 2026-10-17

### Added

- **Task outputs record what they would cost to regenerate** (#1076, phase 1
  of [retention by regeneration cost](docs/recompute-tag-design.md); record
  only, nothing is tiered or deleted). `spawn task run` and pooled workers now
  upload each output tagged `spawn:regen-class` (`cheap` < $1 and < 1 h,
  `expensive` ≥ $25 or ≥ 12 h, `moderate` between), `spawn:regen-usd`,
  `spawn:regen-hours` and `spawn:run-id`, set in the same request as the upload.
  Each output also gets an S3 object annotation, `spawn.provenance`, holding the
  recipe (command, env names but never values, image and the digest that
  actually ran, AMI, instance type, versions), each input's ETag and version as
  it was read, and the cost with its rate source. `completion.json`'s
  `cost_estimate`, defined but never written until now, is filled in. An
  unknown rate means no class, so the output is kept rather than treated as
  cheap. A static truffle rate is labelled `on-demand:static`, a Spot run uses
  the price paid, and Windows is marked as a lower bound. Uploads go through
  `spored provenance upload`, which keys objects exactly as `aws s3 cp` does
  (pinned against the real CLI); with an older spored the wrapper falls back to
  plain `aws s3 cp`, untagged.
- **`regenerable` in a task spec** opts its outputs in to future deletion by
  spawn's retention sweeper. Deletion is opt-in; tiering will be automatic.
  It's recorded only when every input could be read, so the sweeper can later
  prove the inputs unchanged.
- **`spawn:price-source`** (`live` or `static`) is tagged beside
  `spawn:price-per-hour`, so a cost computed later from it can say which it was.

### Fixed

- **`spawn reaper deploy` reports an upgraded reaper accurately** (#1086).
  Re-running it on an armed reaper printed "DRY RUN … then arm it" although the
  reaper stayed armed. It never updated the function's `spawn:version` tag, so
  `spawn reaper status` and the version-skew report kept showing the release it
  was first deployed with. And it said it had created an artifact bucket it
  had only reused. It now says whether the reaper is armed, tags the function
  with the version it deployed, and says "converged" for an existing bucket.
  An existing bucket is still not tagged `spawn:managed`: that tag is how
  teardown knows spawn created the bucket.
- **Re-running `spawn reaper deploy` to upgrade a deployed reaper no longer
  fails half-way** (#1084). It updated the function's configuration
  immediately after its code, and Lambda refuses that while the code update is
  still being applied (`ResourceConflictException: An update is in progress`).
  The new code went live but the configuration, schedule and `spawn:version`
  tag were never written, so `spawn reaper status` kept reporting the old
  version. The deploy now waits for the code update to finish, and stops with
  Lambda's reason if it failed. An armed reaper stayed armed throughout.

## [0.131.1] - 2026-10-17

### Fixed

- **A retired Spot replacement now terminates itself instead of being left
  stopped** (#1080). When spored found it was running on a replacement that EC2
  launched from a leaked persistent Spot request (#1008), it cancelled the
  request and shut the OS down. With EC2's default shutdown behaviour that
  STOPS the instance, leaving it untagged with its request cancelled. Nothing
  would ever start it again, nothing would end it (the reaper's terminate is
  tag-scoped, #984), it was invisible to `spawn list`, and its EBS volume
  billed indefinitely. Observed live on v0.131.0. The spored role now carries a
  `TerminateOnlySelf` grant (`ec2:TerminateInstances` where
  `aws:ARN = ${ec2:SourceInstanceARN}`), and the replacement terminates itself
  through the API after cancelling the request. The grant was proven on real
  AWS before it was written: IAM stores it; from an untagged instance it
  refuses terminating another instance, allows terminating itself, and refuses
  both together. If the terminate is refused (a role from before this change),
  spored falls back to the shutdown and now says the instance is stopped and
  still needs terminating, with the command. Any `spawn launch` refreshes the
  spored role.

## [0.131.0] - 2026-10-17

### Added

- **The TTL reaper's DNS cleanup covers custom zones** (#1065). Its DNS teardown,
  reconciliation sweep, unmanaged-subdomain report and lease expiry all worked
  on exactly one zone, so a spore registered in a custom zone (#891) had no
  backstop. A record its spored never deleted, after a crash or an out-of-band
  terminate, stayed and pointed at a released IP. The new `DnsExtraZoneIds`
  parameter (`DNS_EXTRA_ZONE_IDS`, env `REAPER_DNS_EXTRA_ZONE_IDS`) takes
  further hosted zone IDs. The reaper reads each zone's domain from Route53 at
  startup, so the IAM grant and the reaper are built from the same list. It
  walks every zone, and tears an instance's records down in the zone its
  `spawn:dns-domain` names. A zone it cannot read is logged as not covered, and
  a spore in an uncovered zone is reported rather than guessed at. Deployments
  that don't set the parameter behave exactly as before.
- **The TTL reaper now cleans up orphaned persistent Spot requests** (#950).
  When a `--spot` instance with a stop-based lifecycle is terminated outside
  spawn (the EC2 console, `aws ec2 terminate-instances`), its persistent
  request reopens and EC2 launches a replacement. That replacement carries no
  spawn tags, so `spawn list` and the reaper never see it. A new sweep finds
  spawn-tagged persistent requests and cancels any whose instance is gone,
  replaced or past its deadline. It then terminates the replacement, always
  cancelling first. The sweep never cancels the request of a live original
  instance on the request's own deadline tag: `spawn extend` leaves that tag
  stale, and cancelling would leave the instance unable to restart after its
  next stop. The reaper's grant is tag-scoped, so an untagged replacement is
  outside it. The sweep still cancels its request, then raises the alarmed
  `SPOT REPLACEMENT NOT TERMINATED` with the command to end the instance, every
  run until someone does. The sweep is staged like the network-resource sweep:
  `SpotRequests` / `REAPER_SPOT_REQUESTS` is `report` by default, `reap` to act,
  or `off`. It needs `ec2:DescribeSpotInstanceRequests`, newly granted to the
  scan-self policy, the SAM template and the cross-account
  `spawn-ttl-reaper-ec2` role. An account whose role lacks it raises
  `REAPER SPOT REQUESTS UNREACHABLE`.
- **Persistent Spot requests now expire at the instance's TTL deadline**
  (#950). The deadline is sent as `ValidUntil`; with no TTL it is launch plus
  the reaper's 7-day ceiling. Before, a request left open had no end date and
  could keep launching replacements indefinitely. The instance and the request
  carry `spawn:spot-valid-until`. After launch the request is also tagged with
  `spawn:original-instance-id`, so the reaper can recognise a replacement even
  if it inherited the tags. EC2 cannot move a request's end date. So
  `spawn extend` warns when the new deadline is past it: from then on, a stop
  may not be restartable. And `spawn start` names the expired request as the
  likely cause when EC2 refuses to start the instance.
- **spored retires a Spot replacement it finds itself running on** (#995).
  EC2 launches a replacement from the request's launch specification, which
  carries spawn's user data and instance profile, so spawn's bootstrap and
  spored run there. But the replacement has no spawn tags, and with them no TTL.
  At startup spored now checks whether it is on one. It is if this instance is
  not tagged `spawn:managed`, and its Spot request is a spawn persistent request
  whose `spawn:original-instance-id` names a different instance. If so, spored
  logs `SPOT REPLACEMENT DETECTED`, sends a `spot_replacement_retired`
  notification through the original's notification settings, cancels the
  request and shuts the OS down. The original instance can never match: it is
  tagged `spawn:managed`, and its request names it or is not yet tagged. If
  anything can't be read, or the cancel fails, spored does nothing and the
  reaper's alarm still applies. This needs no new IAM: `DescribeInstances`,
  `DescribeTags` and the tag-scoped `CancelSpotInstanceRequests` were already
  granted. Not yet verified against a real Spot replacement. Since the launch
  specification can't carry a shutdown behaviour, an OS shutdown may stop the
  replacement rather than terminate it. A stopped replacement bills only for
  EBS, can't restart with its request cancelled, and the reaper still reports it.
- **A Spot replacement no longer re-runs your workload** (#1007). The
  replacement boots the original's user data, so it used to start `--command`
  again from the beginning, side effects included, while spored was still
  deciding to retire it. The Linux bootstrap now asks the new
  `spored is-spot-replacement` (exit 0 replacement, 1 not, 2 unknown) before it
  starts `--command` or `spawn task` work, and again after spored starts. On a
  replacement it skips the command and stops there, so storage mounts, the
  container, custom user data and the MPI, batch-queue and pool-worker scripts
  don't run either; spored then retires the instance. Declared `--plugin`s are
  unaffected: they are written before spored starts (#1023), ahead of the stop.
  Anything short of a definite "replacement" runs the workload as before: an
  unreadable answer is retried for up to a minute (not at all on an IAM
  refusal), then the workload starts. Holding instead would leave every
  original stuck whenever EC2 or IMDS is slow or a custom role can't read tags. A replacement that gets past the
  check is still retired by spored and reported by the reaper. The check usually
  answers on its first call, but it does add an EC2 round trip before the
  post-spored steps of every launch. The Windows bootstrap starts no workload
  (it doesn't run `--command` at all), so it has nothing to gate.
- **The `spot_replacement_retired` notification now names the original
  instance** (#1007). It carries `original_instance_id`. spore-bot uses that
  field to reach the users registered for the original. Nobody registers the
  replacement's own id, so without it the bot could post only to channel
  webhooks.
- `pkg/aws.EFANetworkInterface`, the RunInstances network interface that
  attaches an EFA, shared by `spawn launch --efa` and pipeline EFA stages
  (#956). This is an addition to the public `pkg/aws` surface.
- **`spawn pipeline status` shows the run id**, the one `spawn pipeline
  collect --run` takes, and lists the pipeline's other runs found in its
  `s3_bucket`, newest first, with the `collect --run` command to fetch one
  (#1014). The id used to be only in the stored record. `status` also honours
  `--output json` now, which it ignored: it prints the record with `run_id`
  (always present, `""` on a pipeline launched before run ids) and `runs`, every
  run id found, oldest first (`null`, with `runs_error`, when they could not be
  listed). Listing the runs needs `s3:ListBucket` on the pipeline's
  `s3_bucket`; without it `status` warns and shows the rest.
- **`pkg/aws` `Client.GetSelfOwnedSpawnAMI`** (#924) returns the newest
  self-owned `spawn-*` AMI that fits an instance type (architecture, Linux, and
  GPU when the image is tagged `spawn:gpu`), or `""` when none does.
- **Hybrid job-array members can read their peers** (#933). On a `spawn burst`
  instance or a local node, spored now writes the job array's live members (EC2
  instances and local nodes, each listed once) to
  `/etc/spawn/job-array-peers.json`, the same file and format `spawn launch
  --mpi` uses. It rewrites the file every minute, so members that join after
  boot appear and members whose registry heartbeat lapses drop out. If a
  refresh fails, the previous list is kept. Before this, spored discovered peers
  once at boot and only logged how many it found. `spored status` (table and
  `-o json`) now shows a job-array member's peers, when the list was last
  refreshed, and whether it is stale. spored never writes the file on
  `--mpi` instances, where its arrival is the signal the MPI setup waits for.
- `aws.InstanceInfo.StateReason`: EC2's state-reason message for a
  stopped or terminated instance (#928). This is an addition to the public
  surface.

### Changed

- **spawn now builds against libs v0.51.1 and truffle v0.59.0**, in the CLI
  and in the Lambda modules that depend on them.

### Deprecated

- **`result_s3_bucket` and `result_s3_prefix` in a pipeline definition**
  (#989). Nothing ever wrote to them: stage outputs stay in `s3_bucket`, and
  the stage instances are granted only `s3_bucket/<s3_prefix>/*`, so they
  could not write anywhere else. `spawn pipeline collect` no longer reads them,
  and `validate`, `launch` and `collect` warn when they are set. Stage commands
  are no longer given them either (#1013, below). To keep a pipeline's results
  in a particular bucket, make it the `s3_bucket`.
  The three examples that set them no longer do.

### Fixed

- **`--on-idle hibernate` and `--on-complete hibernate` now hibernate** (#949).
  EC2 hibernates only an instance launched configured for hibernation, and only
  `--hibernate` configured it, so these two (and the deprecated
  `--hibernate-on-idle`) launched an instance whose idle or completion hibernate
  quietly became a plain stop, losing RAM. Each one now configures hibernation
  as `--hibernate` does: an encrypted root volume and `HibernationOptions`. A
  launch is refused up front when the type can't hibernate, and the error names
  the flag that asked for it. With `--spot`, a persistent request is interrupted
  by hibernating rather than stopping. The same applies to SDK callers that set
  `LaunchConfig.HibernateOnIdle` or `OnComplete: "hibernate"`. When no
  `--volume-size` is given, the root volume for any hibernation launch is now
  sized from the type's real memory plus 10 GiB. Before, a per-family guess gave
  an m7i.4xlarge (64 GiB) a 26 GiB root. `spawn status` now says an idle
  timeout will stop, not hibernate, an instance that was launched before this
  fix and is not configured for hibernation. New public API, additions only:
  `LaunchConfig.HibernationRequested()` and `InstanceInfo.HibernationConfigured`.
- **A Spot replacement the TTL reaper cannot terminate is now reported with
  everything needed to end it** (#984). An untagged instance launched by a
  leaked persistent Spot request is outside the reaper's tag-scoped grant, and
  there is no safe way to widen it. IAM has no condition key that ties an
  instance to its Spot request, and a terminate grant without the
  `spawn:managed` condition would reach instances spawn does not own. So a
  person still ends the instance. The `SPOT REPLACEMENT NOT TERMINATED` line
  now names the 12-digit account (scan-self mode used to print `self`), the
  instance's type, state and launch time, and the original instance. It gives a
  `describe-instances` check that the instance belongs to the request,
  followed by the `terminate-instances` command. A throttled terminate is no
  longer blamed on the grant. The line is also posted to `REAPER_NOTIFY_URL`
  once, on the run that finds the replacement. Before, the stranded path
  returned before notifying, so the webhook heard nothing of either the cancel
  or the instance. The alarm's description now says where to look and what to
  run.
- **Pipeline stages now receive their `data_input` and keep their `data_output`**
  (#910, #911). Both were validated and then ignored: the stage script had only
  TODOs where the S3 download and upload belonged, so a stage started with an
  empty input directory, its outputs were lost when its instance terminated, and
  the pipeline could still report `COMPLETED`. Stage instances now run
  `spored run-pipeline-stage`, which already implemented the handoff and peer
  discovery but was never invoked. It downloads the upstream stages' outputs,
  runs the command, uploads the declared outputs, and writes `COMPLETE` only
  after that, so a downstream stage cannot start before its inputs exist. Any
  failure writes `FAILED`. Both markers are written at the keys the orchestrator
  reads. The termination, TTL and `FAILED` guarantees from #912 are unchanged.
  - `spawn pipeline launch` now creates (or reuses) an instance profile for the
    stages, scoped to the pipeline's `s3_bucket`/`s3_prefix`. The shared default
    spored profile has no access to pipeline data, so the stages could not
    read or write it, markers included. Launching a pipeline therefore needs IAM
    permissions, as `spawn task run` does.
  - The stage command now runs in `/opt/spawn/pipeline/<pipeline>/<stage>`, as
    before, rather than `/root`. Its `env` is applied by the runner.
  - The stage runner's S3 clients use the bucket's own region, so a stage in
    another region can still reach the pipeline's bucket.

  The `spawn-pipeline-orchestrator` Lambda has to be redeployed (its template
  now lets it pass the stage profile), and pipelines have to be launched with
  this CLI.
- **A stage with `instance_count > 1` completes only when every instance has**
  (#930). Every instance wrote the same stage-level `COMPLETE` key. The first
  one to finish marked the whole stage complete, its dependents launched while
  the other instances were still running, and a sibling's later `FAILED` was
  never read. Markers are now per instance
  (`<prefix>/stages/<stage>/<instance-index>/COMPLETE|FAILED`). Any one instance
  failing, or exiting without `COMPLETE`, fails the stage. Stage-level markers
  from instances launched before the upgrade are still read.
- **`max_cost_usd` is enforced against real prices, including running stages**
  (#930). Every instance-hour was charged a flat $0.10 whatever the instance
  type, and only once a stage finished. On a `p4d.24xlarge` (about $32/hour) a
  $100 budget let roughly $30,000 through. Each stage is now priced at its
  instance type's on-demand rate, through the same truffle pricing that
  `--cost-limit` uses. Spot stages are also priced at on-demand, so the
  estimate is an upper bound. The budget is checked on every poll against
  finished stages plus the elapsed time of running ones. With a budget set, a
  stage whose price cannot be determined fails before launching anything. The
  figure is reported as an estimate everywhere: notifications, and
  `spawn pipeline status`/`list` ("Estimated cost", "EST COST"). It excludes EBS
  and data transfer.
- **Streaming and EFA pipelines with stages outside the Lambda's region now
  launch** (#929). The security group and placement group were created once,
  in the orchestrator Lambda's region (us-east-1), and passed to `RunInstances`
  in each stage's region, where they do not exist. One of each is now created
  in every region that needs it, recorded per region, and deleted there.
- **Launching a pipeline again no longer hijacks the run in flight, and runs
  never read each other's results** (#943). `pipeline_id` comes from the
  definition file, so launching the same file twice reused it, and two things
  went wrong:
  - The second launch replaced the running pipeline's record with an empty
    one. The first run's instances were no longer listed anywhere, and two
    orchestrators ran against one record. `spawn pipeline launch` now refuses
    when the pipeline has a run in flight, and says how to proceed. The new
    `--replace` flag cancels the run in flight, waits up to 5 minutes for its
    instances to be terminated, and then starts the new run. The check is a
    conditional write, so two concurrent launches cannot both pass it. A
    finished pipeline (completed, failed or cancelled) can still be launched
    again without `--replace`.
  - Stage markers and outputs were keyed by `s3_prefix` alone, so a second run
    under the same prefix found the first run's `COMPLETE` markers. Its stages
    were marked complete before they had done anything, and its dependents
    downloaded the first run's outputs. A stale `FAILED` failed a fresh run.
    Every launch now gets a run id (printed at launch, and in the record as
    `run_id`). A run's markers, outputs and stage config live under
    `<s3_prefix>/runs/<run-id>/`, and `spawn pipeline collect` reads only that
    run. The stage command sees the run's prefix as `SPAWN_S3_PREFIX`, and the
    run id as `SPAWN_RUN_ID`.
  - The orchestrator writes only over its own run's record. A run that has been
    replaced terminates its own instances and stops. The security and placement
    group names carry the run id.

  Pipelines launched before this keep the old layout until they finish. The
  refusal needs only this CLI; run isolation also needs the
  `spawn-pipeline-orchestrator` Lambda redeployed.
- **Pipeline stage outputs arrive where the examples say, and a multi-instance
  stage's instances no longer overwrite each other's outputs** (#944).
  - An absolute `data_output` path kept its whole path, so the
    `/data/processed/a.csv` written by `preprocess` in
    `examples/pipelines/ml-training.json` arrived in `train` as
    `/data/input/data/processed/a.csv`, not `/data/input/a.csv`. Every output
    file is now keyed by its path relative to the directory part of the pattern
    that matched it. Absolute and relative patterns now behave alike, and a
    relative pattern no longer flattens subdirectories to bare file names.
  - With `instance_count > 1`, every instance uploaded to the same place, so
    same-named outputs overwrote each other and the last upload won. Each
    instance's outputs are now kept under its index, and arrive downstream as
    `<dest_path>/<instance-index>/<file>`.
  - Two outputs of one stage that map to the same name, or two `source_stages`
    that deliver the same file name to one `dest_path`, now fail the stage
    instead of one silently replacing the other. A downloaded key that would
    land outside `dest_path` is refused.
  - `data_input.pattern` is matched against that same relative path. For a
    multi-instance source, it is matched within each instance's directory.
    `spawn pipeline validate` now rejects an absolute pattern, which could never
    match: the stage silently received nothing. An invalid pattern used to be
    logged and then ignored, so every file was downloaded; it now fails.

  The layout is documented in `examples/pipelines/README.md`.
  `fanout-fanin.json` used absolute patterns and gave its three fan-in sources
  colliding output names, and both are fixed. `ml-training.json`'s `evaluate`
  now reads `/models/0/best.h5`. Stage instances download `spored` at boot, so
  this takes effect once `spored` is released. Stages that are already running
  keep the old layout.
- **`efa_enabled` pipeline stages get an EFA interface** (#956). The
  orchestrator added only the cluster placement group, so the stage's instances
  had an ordinary ENA interface and `fi_info -p efa` found nothing. They are now
  launched with an EFA network interface, built by the same code as
  `spawn launch --efa`. The interface carries the pipeline's security group,
  which in a region with an EFA stage now admits all traffic from itself, as EFA
  requires. The group is created for EFA pipelines even when nothing streams.
  `spawn pipeline launch` checks, before creating anything, that each EFA
  stage's instance type supports EFA in its region. `ml-training.json` used
  `p3.8xlarge`, which has no EFA, and now uses `p3dn.24xlarge`. The
  `spawn-pipeline-orchestrator` Lambda has to be redeployed.
- **`spawn pipeline validate` rejects streaming between stages in different
  regions** (#956). Such a pipeline launched and then could not connect. Each
  region's stages run in their own default VPC and security group, and nothing
  connects them. A `stream`-mode `data_input` whose source stage is in another
  region is now an error that says to use `s3` mode or one region. Cross-region
  networking is not implemented.
- **`spawn pipeline collect` downloads the run's stage outputs** (#989). It
  read `result_s3_bucket` whenever a definition set it, and nothing writes
  there, so on `ml-training.json`, `fanout-fanin.json` and
  `video-streaming.json` it printed "No results found". It now reads every
  stage's `data_output` files from `s3_bucket`, under
  `<s3_prefix>/runs/<run-id>/stages/<stage>/output/`, into
  `<output-dir>/<stage>/<file>`, or `<output-dir>/<stage>/<instance-index>/<file>`
  for a stage with `instance_count > 1`. Also:
  - It collects every object, not just the first 1000. It made one listing
    call and reported the first page as everything.
  - The `COMPLETE` and `FAILED` markers are no longer downloaded as results.
  - It collects the pipeline's latest run. The new `--run <run-id>` flag
    collects an earlier run of the same `pipeline_id`. A run with no outputs
    lists the runs that have data.
  - A file that fails to download makes `collect` exit non-zero. It used to
    print a warning and then report success.

  Downloads are laid out by stage, not by full S3 key as before, so scripts
  that read `./results/<s3_prefix>/stages/...` need the new paths.
- **Pipeline stage commands are no longer given `SPAWN_RESULT_S3_BUCKET` and
  `SPAWN_RESULT_S3_PREFIX`** (#1013). They carried the deprecated
  `result_s3_bucket`/`result_s3_prefix`, a location the stage's instance
  profile does not allow it to write and that `spawn pipeline collect` does not
  read, so a command that trusted them failed with AccessDenied. Declare stage
  results as `data_output`, which `collect` gathers. The `spawn-pipeline-orchestrator` Lambda has to be redeployed
  for this to take effect.
- **`spawn burst --ttl` accepts day units** (`1d`, `2d12h`), like every other
  TTL since #990; it used to refuse them because the tag builder could not turn
  them into a deadline, which it now can.
- **A Spot launch with a day-unit `--idle-timeout` (`1d`) now gets a persistent,
  stoppable request** (#990, #922). The request type was chosen with Go's
  duration parser, which has no day unit, while spored arms the idle stop with
  the day-aware one, so the stop would have been refused by EC2.
- **`spawn extend` after a stop/start raises the cost limit far enough**
  (#992). EC2 resets an instance's launch time on every start, and extend
  measured from it, for both the single-instance and the job-array path. The
  raised `--cost-limit` therefore left out the compute from every earlier boot
  and could still stop the instance before its new deadline, the #639 trap.
  The cap now matches what spored counts: compute so far across every boot
  (`spawn:compute-seconds`) plus running until the new deadline, and EBS from
  the original launch (`spawn:launch-time`). Time spent stopped is no longer
  priced as compute. `spawn:ttl` is again the total from the original launch,
  not from the latest start. On an old instance with no deadline tag, extend
  now adds to launch + TTL, the deadline that is actually enforced, rather
  than to now + TTL.
- **`spawn burst` instances can see the local nodes they work with** (#918).
  Peer discovery on EC2 only looked at instance tags, so a burst instance found
  its EC2 siblings but never the local nodes of its job array, which are only
  listed in the hybrid registry. Discovery worked one way: local nodes saw burst
  instances, burst instances did not see local nodes. Burst instances
  (`spawn:burst=true`) now also read the hybrid registry and merge its entries
  with their tag-discovered peers, listing each instance once. If the registry
  cannot be read, they keep their EC2 peers and log a warning. Ordinary
  `spawn launch --count` members still discover peers by tag only.
- **`spawn burst` installs spored** (#924). Burst's boot script never
  installed spored. It waited for an AMI that already had it, so on any other
  image there was no idle termination, no on-complete, no hybrid-registry
  registration and no DNS. Burst instances now boot with the same bootstrap as
  `spawn launch`, which downloads spored, verifies it and refuses to boot an
  unverified one, then starts it. Any Amazon Linux 2023 AMI works.
- **`spawn burst` picks an AMI that fits the instance type** (#924). Without
  `--ami`, burst took the newest self-owned `spawn-*` AMI of any architecture
  or OS. It now takes one only if it matches the instance type's architecture,
  is Linux, and matches GPU-ness. Otherwise it uses the Amazon Linux 2023 AMI
  `spawn launch` would pick. `--ami` with a Windows AMI is refused before
  anything is created.
- **A launch with an EC2 key pair but no SSH public key keeps the key pair's
  key** (#924). The shared Linux bootstrap (`pkg/launcher`) wrote an empty
  `authorized_keys` when it was given no public key. That erased the key
  cloud-init had installed from `KeyName`, so the key pair could not log in.
  This affected `spawn burst --key-name` and `launcher.Provision` callers that
  set `KeyName`. The bootstrap now leaves the file as it is when it has no key.
- **`spawn burst` honours `--region`, `--profile` and `SPORE_REGION`** (#939).
  Burst loaded its AWS config from the ambient chain only, so it launched its
  instances, created the registry table and looked up the AMI in whatever
  region `AWS_REGION` or the shared AWS config named, whatever `--region` said.
  It now resolves region and profile the way `spawn launch` does: `--region`,
  then `SPORE_REGION`, `AWS_REGION` and the spore config file, then the
  profile's region. The table, the AMI lookup and every instance use the same
  region, and burst stops before any AWS call if no region is set anywhere.
- **Other commands that ignored `--profile` and `--region`** (#939). `spawn bot`,
  `cancel` (instance termination), `cost`, `extend`, `fsx`, `onboard`,
  `team`, `validate`, the `service` and sweep dry-run price lookups, and
  instance-ID shell completion each loaded the ambient AWS config directly. They
  now go through the same resolution as `spawn launch`. With none of the flags
  or `SPORE_*` variables set, nothing changes. A test now fails any command
  that loads AWS config directly.
- **`spawn status` on a stopped instance no longer SSHes to your own machine**
  (#928). A stopped instance has no public IP, and status ran
  `ssh ec2-user@` with an empty host. ssh resolves that to localhost, so it
  offered your keys to your own sshd (`::1`) and failed with "Too many
  authentication failures". For an instance that is not running (stopped,
  stopping, pending), status now reads EC2 data and tags instead of SSH or SSM.
  It shows the state, when the instance stopped and why, the TTL deadline,
  compute and cost so far across every boot, and the EBS cost that keeps
  accruing while it is stopped. `--output json` returns the same facts with
  `"source": "ec2"`. A running instance with no public IP now uses SSM.
- **No spawn command connects to an empty, loopback or unspecified host**
  (#928). This covers `connect`, `status`, `config`, `extend`, `queue`,
  `plugin`, `service`, array commands and uploads. Every ssh and scp client now
  starts in one place, which refuses `""`, `localhost`, `127.0.0.0/8`, `::1`,
  `0.0.0.0` and `::`, including as a `-J` jump host or an scp operand. A test
  fails the build if any code starts ssh or scp elsewhere. `spawn extend` on a
  stopped instance no longer tries to reach it; spored reads the new deadline
  when the instance starts.
- **Compute from earlier boots is kept after a stop/start** (#928). An instance
  that ran about a minute, was stopped, then restarted reported 22s of compute,
  so its effective rate was meaningless for the stop/idle/restart pattern it is
  meant to show. spored now also keeps its running total on the instance's own
  disk (`/var/lib/spored/compute-checkpoint.json`). It writes that file every
  minute and again first on shutdown, before the network write that a stop's
  shutdown can lose. That network write (`spawn:compute-seconds`) is now
  retried, and a failure is logged. The next boot carries over the larger of
  the two totals. Each boot is now counted from when the kernel started, which
  is closer to what EC2 bills: it includes OS and cloud-init time, not only the
  time since spored started. A spored restart within one boot (an upgrade, a
  crash) is not counted twice. A restarted instance's billed time no longer
  includes the time it was stopped. That time is now shown as stopped, where
  only EBS bills.
- **`spawn stop` no longer says the TTL countdown pauses** (#928). It never did:
  the deadline is fixed at launch and keeps counting while the instance is
  stopped, as the docs say. `stop`, `hibernate` and `start` now print the
  deadline (local time with UTC) and say it still applies. If the deadline has
  already passed, they say starting will terminate the instance. `spawn start`
  also stopped overwriting `spawn:ttl` with a "remaining TTL" saved at stop.
  That tag is defined as the total from launch, and the overwrite changed its
  meaning. The stop/start help and reference pages say the same, and a test
  checks that no CLI string or doc in this repo says the TTL pauses.
- **Time spent hibernated is no longer counted as compute on Linux** (#945).
  A hibernated instance resumes into the same boot, so the compute clock in
  `/run/spored` and the checkpoint on disk both survive. `spored status`, and
  a spored restarted after the resume (an upgrade or a crash), measured from
  timestamps written before the hibernation and counted the whole hibernation
  as running time. That inflated compute cost and the total `--cost-limit` is
  enforced against. spored now measures running time on the kernel's
  monotonic clock, which stops while the instance is hibernated. `spored
  status` shows the hibernated time beside boot and stopped time, for example
  `(1m 0s compute · 10s boot · 8h 0m hibernated)`. It is included in the
  storage cost, because EBS bills while hibernated and compute does not. The
  minute or two EC2 bills while it saves memory to disk and restores it is
  counted as hibernated, so compute can be slightly low around a hibernation.
- **Time spent hibernated is no longer counted as compute on Windows** (#962).
  spored on Windows measured running time on Go's monotonic clock, which on
  Windows keeps counting through sleep and hibernation. So a hibernate and
  resume added the whole hibernation to `spawn:compute-seconds`, to the
  compute cost `spawn status` shows from it, and to the total `--cost-limit`
  is enforced against. spored now measures it on Windows' unbiased interrupt
  time, which excludes sleep and hibernation. This needs Windows Server 2016
  or later; older versions count as before. It has not yet been checked
  against a real EC2 hibernation of a Windows instance. `spored status` on
  Windows still reads the last checkpoint and does not show hibernated time.
  The resume-from-hibernation reset (#987) reads the same clocks, so it now
  covers Windows instances too.
- **The idle check's disk I/O signal was wrong on every instance type** (#937).
  It compared the total disk I/O since boot with a per-minute threshold, and its
  device filter skipped every NVMe disk while counting partitions such as
  `sda1` as extra disks. On Nitro instances the disk therefore always read 0,
  so a disk-bound job with low CPU and network could be idle-stopped. On
  instances with `xvd*`, `sd*` or `vd*` disks it always read as busy, so
  `--idle-timeout` never fired and the log said `Not idle: Disk I/O N bytes`
  every minute. spored now measures the I/O since the previous check, over
  every whole disk, whatever it is called, without partitions, loop,
  device-mapper, md RAID or zram devices. If the counters cannot be read or
  go backwards, it treats the disk as active, and if they cannot be read it
  logs one warning saying idle-stop will not trigger. The same applies on
  Windows, where a missing disk counter used to read as no I/O.
- **`spored status` now shows a `Disk I/O` line** (#937). It reads `unknown`,
  with the reason, when nothing was measured. A one-shot status has only one
  sample, so it is always unknown there. The JSON output gains
  `disk_bytes_per_min`, `disk_measured` and `disk_unknown`.
- **The Prometheus `spawn_disk_io_bytes_total` metric now counts NVMe disks** (#937).
  Before, it read 0 on Nitro instances and double-counted `sda`/`vda` disks
  with partitions. Its `device` label now lists every disk it counted, for
  example `nvme0n1,nvme1n1`, so existing series change label.
- **A CPU counter that goes backwards between samples now counts as unknown,
  not as a reading** (#937). Before, it produced a percentage that could be
  negative, which reads as idle.
- **An open SSH session, or a detached tmux or screen session, no longer keeps
  `--idle-timeout` from ever firing on Linux** (#953). spored counted any
  terminal that existed, and any login still open, as activity, however long
  nobody had typed in it. Leaving a session open, or a job's tmux session
  behind after the job finished, therefore kept the instance running until its
  TTL. A terminal now counts only while someone is typing in it: spored reads
  each terminal's last-input time, as `w` does, and a session with no input
  for 5 minutes no longer blocks idle-stop. The same applies to `docker exec -t`
  and SSM Session Manager shells. Output from a job does not count as input.
  This also stops spored relying on the column layout of `w`, which differs
  between distributions.
- **A Prometheus scrape could make a busy instance look idle and get it
  stopped** (#952). With `observability.metrics.enabled`, every scrape ran the
  idle check and read the CPU counter itself, at the same time as spored's
  once-a-minute check. That was a data race, and it also reset the starting
  point of the next measurement, so the next check only counted the traffic
  and disk I/O since the scrape. With a 15-second scrape interval, 200 KB a
  minute of real traffic could read as under the 100 KB threshold. Now only
  the once-a-minute check takes measurements, and the metrics endpoint,
  `spored status` and the webhooks read its last result. Every check also
  measures CPU, network and disk on every tick. Before, a check that stopped
  early (on an active session or busy CPU) skipped the later counters, so the
  next reading covered several minutes. The scrape no longer adds a
  `Not idle: ...` line to the log each time, and the `spawn_cpu_usage_percent`
  metric is omitted, not reported as 100%, until the first measurement.
- **A failed idle stop is no longer repeated every minute** (#948). When EC2
  refused to stop an idle instance (for example a one-time Spot instance, or
  a role without `ec2:StopInstances`), spored logged the error and tried
  again on the next check, once a minute until the TTL. Each attempt sent
  another `idle_stopped` notification for an instance that was still running,
  and deleted its DNS name again. Now `idle_stopped` and `idle_hibernated` are
  sent only after EC2 accepts the request. A failed attempt sends one
  `idle_stop_failed` notification with the error, and spored retries after 5,
  10, 20 and 40 minutes, then hourly. When a stop or hibernate fails, spored
  registers the DNS name and job-array entry again, so the running instance
  keeps its name. This also applies to `--on-complete stop` and `hibernate`.
  The pre-stop hook still runs only once.
- **A failed `--on-complete stop` or `hibernate` no longer repeats the
  completion announcement every minute** (#978). The completion file stays on
  disk, so when EC2 refused the stop, every one-minute check found it again and
  sent another `completion` notification, POSTed the completion webhook again,
  copied the command log to the serial console again and slept the grace
  period again, holding up the TTL and cost-limit checks each time. Now the
  notification, the webhook and the grace period happen once. A failed attempt
  sends one `completion_stop_failed` notification and is retried on the same
  backoff as a failed idle stop (5, 10, 20 and 40 minutes, then hourly), and
  the TTL and cost limit are still enforced while it waits. The idle stop and
  the on-complete stop share that backoff, so an instance EC2 will not stop is
  not asked twice and the failure is reported once. `--on-complete terminate`
  is still retried every minute.
- **A failed stop or hibernate no longer leaves every plugin marked stopped on
  a running instance** (#979). spored stops its plugins before asking EC2 to
  stop the instance. When EC2 refused, the instance kept running, but
  `spawn plugin status` reported each plugin as stopped and its health check
  was no longer running, so a plugin that failed afterwards was never noticed.
  Now, when the stop or hibernate fails, each plugin gets back the status it
  had (running or degraded) and its health check starts again. A plugin that
  was removed or reinstalled in the meantime is left as it is.
- **An instance resumed from hibernation now starts with a clean slate, as a
  stopped instance does** (#987). A hibernated instance resumes the same
  spored process, and spored carried its state from before the hibernation
  into the new run:
  - the next stop, hibernate or TTL terminate skipped the pre-stop hook (for
    example an `aws s3 sync`) and the task record;
  - the DNS name, deleted before the hibernate, came back only at the hourly
    renewal, and then with the public IP from the first boot;
  - the idle timer had already run out, so after a `spawn start` with no login
    the instance hibernated again within about a minute.

  spored now notices the resume from the kernel's clocks: the time the system
  has spent suspended jumps, and as a backup the wall clock jumps ahead of
  the monotonic clock. It then re-arms the pre-stop hook and the task record,
  restarts the idle timer and the idle warning, reads the public IP again and
  re-registers the DNS name with it, re-registers the job-array entry, and
  restores the plugins. It also resets the failed-stop backoff and the
  network, disk and CPU baselines, so the first idle check does not measure
  across the hibernation. After an `--on-complete hibernate`, the completion
  file that triggered it is still on disk. It no longer hibernates the resumed
  instance again, but a completion written after the resume is acted on as
  usual. Windows instances are not covered yet: their clocks do not show a
  resume (#962).
- **After a resume from hibernation with a new public IP, nothing in spored
  keeps using the old one** (#1019). #987 re-registered the DNS name with the
  new IP; three other things still had the IP from the first boot:
  - `spawn:ready-url` for a DCV desktop or web app on an instance with no DNS
    name named the old IP, so the URL `spawn` opens went nowhere. spored now
    rewrites it with the new IP, keeping the same access token;
  - `{{ instance.ip }}` in a plugin's steps (a health check, or a plugin
    installed after the resume) rendered the old IP;
  - the job-array entry in the hybrid registry was re-registered with the old
    IP.

  And an instance whose first boot had no public IP never registered its DNS
  name, even once it had one. spored now looks for a public IP again about
  once an hour, and after a resume, and registers the name once there is one.
- **A GPU that cannot be read no longer counts as an idle GPU, and Trainium,
  Inferentia, Gaudi and AMD accelerators are now checked** (#954). When
  `nvidia-smi` failed (for example a driver that no longer matched the kernel
  after an update, or "No devices were found"), or printed only `[N/A]`, the
  idle check read the GPU as 0% busy, so an instance running a GPU job with
  little CPU or network could be stopped. Accelerators other than NVIDIA were
  never measured. spored now finds accelerators from the hardware, not from
  which tools are installed. It measures NVIDIA GPUs with `nvidia-smi`, AMD
  GPUs with `rocm-smi`, Gaudi with `hl-smi`, and Trainium and Inferentia with
  `neuron-monitor`. If an accelerator is present but its tool is missing,
  fails, or prints something spored cannot read, it is treated as busy, and
  spored logs one warning saying idle-stop will not trigger. A Deep Learning
  AMI on an instance with no GPU is unaffected, although it has `nvidia-smi`.

### Security

- **`spawn pipeline collect` no longer writes outside `--output-dir`** (#989).
  It wrote each object to `<output-dir>/<object key>` without checking the
  key. Anyone able to write to the pipeline's `s3_bucket`, a stage command
  among them, could name an object `.../output/../../../../.bashrc` and have
  `collect` write it wherever that resolved on the machine that ran it. A key
  that would land outside its stage's directory in `--output-dir` is now
  refused, and then nothing is downloaded. Files are written through an
  `os.Root`, so a symlink already in the directory cannot lead out of it either.
- **A streaming pipeline's security group no longer opens SSH to the internet**
  (#929). It allowed tcp/22 from `0.0.0.0/0`. SSH, like the streaming ports, is
  now allowed only from the pipeline's own instances (the group itself).
- **The SAM-deployed TTL reaper's `ScanSelf=true` policy no longer grants
  unconditioned `ssm:SendCommand`** (#985). `lambda/ttl-reaper/template.yaml`
  kept its own copy of the scan-self permissions, and it had drifted from the
  one `spawn reaper deploy` uses: `ssm:SendCommand` was allowed on every
  resource with no condition, which is root shell on any SSM-managed instance
  in the account. It is now limited to `spawn:managed=true` instances and the
  `AWS-RunShellScript` document, as #818 already did elsewhere. The same copy
  also lacked the network-resource sweep's grants (#685), so that sweep hit
  AccessDenied every cycle in a `ScanSelf=true` deployment; they are added.
  A new gate parses the template and the cross-account role and fails unless
  every statement matches `pkg/reaperiam` in actions, resources and
  conditions. The cross-account role also drops `ec2:DescribeTags`, which the
  reaper never calls. Production runs `ScanSelf=false`, so the operated
  reaper's own role was not affected.
- **`spawn launch --ttl 1d` now enforces a one-day TTL** (#990). It used to tag
  `spawn:ttl=1d` and no `spawn:ttl-deadline`: the deadline was written only when
  Go's duration parser accepted the value, and it has no day unit, and spored
  read the tag the same way. The launch succeeded with nothing enforcing the
  TTL, so the instance was bounded only by the TTL reaper's 7-day max-age
  ceiling. Every TTL and lifecycle duration now goes through one parser, which
  accepts Go durations plus days (`1d`, `2d12h`, `1.5d`). That covers `--ttl`,
  `--idle-timeout`, `--pre-stop-timeout`, `--completion-delay`,
  `--webhook-timeout`, `--fsx-ttl`, `spawn extend`, `spored config set
  ttl/idle-timeout/completion-delay`, `spawn defaults set idle-timeout`, sweep
  `ttl`/`idle_timeout` (CLI and the detached-sweep Lambda), `spawn task`
  `lifecycle.ttl`, `spawn app`/`service`/`pool --ttl`, batch-queue and pipeline
  timeouts, the wizard, and the readers: spored, the TTL reaper (including
  `REAPER_MAX_AGE`), `spawn status` and cost estimates. Anything it cannot
  parse, plus a zero TTL or idle timeout, is rejected before any AWS call. A
  sweep `ttl: 3600` (a number, not a duration) is now an error rather than
  being dropped. Day-unit values are stored in tags in Go form (`24h0m0s`) so
  older readers agree.
- **The TTL reaper enforces `spawn:ttl` on instances that have no deadline
  tag** (#990): launch time + `spawn:ttl`, the deadline spored already applies
  in that case. This catches instances launched with a day-unit `--ttl` before
  this fix. A stopped instance runs no spored, so until now it lived until the
  max-age ceiling. `spawn:ttl=0` (TTL disabled) is still exempt.
- **The launch path can no longer write `spawn:ttl` without
  `spawn:ttl-deadline`.** `buildTags` returns an error instead. The #983 gate
  that every `spawn:ttl` writer also writes the deadline now requires the
  deadline write on the same code path, not just somewhere in the function, so
  the conditional write behind this bug fails it. A new test checks that every
  accepted TTL spelling produces a deadline.
- `pkg/aws.LaunchConfig.ValidateDurations` is new (an addition to the public
  surface). `Client.Launch` and `launcher.Provision` call it first.

## [0.130.1] - 2026-10-17

### Fixed

- **`spawn launch` works again: v0.129.0 and v0.130.0 failed every launch at
  "Setting up IAM role"** (#1069), with `MalformedPolicyDocument: The policy
  failed legacy parsing`, before creating any instance. #905 added a
  hybrid-registry grant whose resource used the `${aws:PrincipalAccount}` policy
  variable, which IAM's `PutRolePolicy` does not accept there. IAM Access
  Analyzer and `simulate-custom-policy` both accept the same document, and the
  emulator CI runs against does not model IAM's policy grammar. Every launch
  that sets up the spored role was affected: plain launches, `--iam-policy`,
  and `pkg/launcher.Provision` (lagotto, calque). `--instance-profile` was not.
  The policy now names the account literally, resolved at write time. A test
  runs every writer and checks what reached IAM, and a document still carrying
  the variable is refused before it is sent.

## [0.130.0] - 2026-10-16

### Added

- **Custom DNS zones work end to end** (#891). A spore can register under your own
  zone through your own updater, for example
  `<name>.<base36-account>.spore.example.edu`. Until now nothing told spored about
  either: it read a `SPORED_DNS_DOMAIN` that no launch set, so every spore
  registered under spore.host. The launch now writes `spawn:dns-domain` and
  `spawn:dns-endpoint` beside `spawn:dns-name`, and spored registers and renews
  there. Set them with `--dns-domain` / `--dns-api-endpoint`,
  `SPAWN_DNS_DOMAIN` / `SPAWN_DNS_API_ENDPOINT`, `dns.domain` /
  `dns.api_endpoint` in `~/.spawn/config.yaml`, or, for every launch in an
  account, the SSM parameters `/spawn/dns/domain` and `/spawn/dns/api_endpoint`.
  Both are validated before launch and again by spored: the endpoint must be an
  https Lambda Function URL, the only kind a SigV4-signed request can
  authenticate to. A bad value is reported in `spawn:dns-status`, never
  replaced with spore.host. `--dry-run` shows the target. `spawn dns register` /
  `delete` act in the zone the instance's tags name (new `--endpoint` flag), and
  `spawn dns list` shows each instance's own zone. See `docs/custom-dns.md`.
- `scripts/deploy-custom-dns.sh` now creates the updater's Function URL
  (`AuthType: AWS_IAM`) instead of only reading one, and grants
  `lambda:InvokeFunctionUrl` to each `--allow-account` (default: the deploying
  account). It never grants `lambda:InvokeFunction`, which would let a caller
  forge the account the updater authorizes.

### Changed

- **`--dns-domain` and `--dns-api-endpoint` are no longer deprecated** (#891).
  v0.129.0 deprecated them because nothing that registered DNS read them. They
  now reach spored.

### Fixed

- **`TestStatusScreen_UsesOneTimeFormat` no longer fails for part of every day.**
  Contributor-facing: when a deadline fell after UTC midnight but before local
  midnight, the status screen correctly showed the UTC date too ("(2026-10-17
  00:20 UTC)"), and the test double-counted that date as a bare timestamp. It
  now checks that no timestamp appears outside the local (UTC) form.
- **Plugins on a Windows instance are refused up front** (#1001). Plugins are
  Linux-only (their steps are shell scripts, and every registry plugin requires
  Linux), but `spawn plugin install` on a Windows spore ran local provision
  steps and then failed on the instance at the first step, because a Windows
  spore has no `sh`, and `spawn launch --os windows --plugin ...` dropped the
  plugin silently. Both now stop before doing anything and say why. A Windows
  spored also refuses an install request itself, so an older spawn reports
  the reason too. See the Windows guide.
- **`deploy-custom-dns.sh` no longer breaks the updater it adds a zone to**
  (#891). It overwrote `DEFAULT_DOMAIN` and `HOSTED_ZONE_ID` with the new zone's,
  so adding a zone made it the default. It also replaced its Route53 grant
  (`SpawnDNSPermissions`) with one on the new zone alone. On an updater the script
  created, that grant is the only one, so adding a zone revoked access to every
  zone added before it. spore.host's live updater happens to carry a second,
  hand-made grant for its zone (`Route53DNSUpdate`), which would have masked
  this there. The grant now covers
  every zone the updater serves, the defaults are set only when absent, and
  `DNS_ALLOWED_ACCOUNTS` is merged rather than replaced. The live updater still
  serves only the spore.host zone, so it was never used to add a second one.
  The script's closing message also pointed at a `CUSTOM_DNS.md` that did not
  exist.
- **`spawn notify register` (alias `spawn bot register`) now creates a
  cross-account role the bot can assume** (#1046). Without `--role-arn` it
  created `SpawnBotCrossAccount` trusting the prism-bot stack's auto-generated
  function role, which spore-bot no longer runs as, under the shared `spawn-bot`
  ExternalId, which spore-bot stopped accepting (spore-host#413), and stored no
  ExternalId in the registration. Every registration made that way was refused
  by the bot. The role now trusts `spore-bot-role`, a fixed name that a stack
  rebuild cannot change (`SPORE_BOT_LAMBDA_ROLE_ARN` still overrides it), under
  a per-account ExternalId that is also stored in each registration.
  An existing role with the old trust is no longer reused silently: register
  stops and says what is wrong. **`--repair`** rewrites the trust policy of a
  role spawn created, keeping a per-account ExternalId if it has one; it never
  modifies a role spawn did not create, such as one from
  `bot-cross-account-role.yaml`. After a repair, re-register the account's other
  instances so their registrations carry the ExternalId. A new
  **`--external-id`** flag goes with `--role-arn`.
- **`bot-cross-account-role.yaml`'s `BotLambdaRoleArn` defaults to
  spore-bot's role** (#1046). It had no default, and its example named a role
  that does not exist. Its `ExternalId` description no longer says the bot
  falls back to the shared value. It hasn't since spore-host#413.
- **`spawn plugin remove` of a plugin that is not installed says
  `plugin "<name>" not found on <instance>`** (#1038), instead of
  `remove API: HTTP 500 — internal error`. spored's plugin API now answers 404
  for a plugin that is not installed and 500 only for a real failure, on
  remove, push and status alike. `spawn plugin status` used to report a plugin
  whose state spored could not read as "not found"; it now reports the error.
- **A plugin's remote pre-flight conditions are now checked** (#1040). The
  `conditions.remote` block of a plugin spec, documented as "pre-flight checks
  before install", was validated and then ignored, so a `type: command` check
  such as "docker must already be present", or a `type: platform` OS
  requirement, never ran. spored now evaluates them on the instance before the
  plugin's install steps, and a failure marks the plugin failed with the
  condition's message, which `spawn plugin install` and `spawn plugin status`
  print. `spawn plugin install` also asks the instance to check them before it
  runs the plugin's local steps, so a failing check stops the install before
  anything is created on your machine (against an instance running an older
  spored it warns that it cannot check them first). A condition of a type spawn
  does not know now fails instead of being skipped, a platform `os` matches
  regardless of case, and condition commands, local and remote, are rendered
  with `{{ config.* }}` values like steps.

### Security

- **The local-file path guard works on Windows** (#1000). The check behind
  `spawn launch --user-data-file` / `--user-data @file` and spored's queue
  result uploads compared paths against `/etc`, `/proc`, ... only, so on Windows
  it refused nothing. It now uses the host's rules: on Windows it refuses the
  Windows directory, ProgramData and System Volume Information (any case, either
  separator), UNC and `\\?\` / `\\.\` paths, alternate data streams
  (`file:stream`), device names such as `NUL` and `CON`, and names ending in a
  dot or space. On every OS it now follows symlinks (and Windows junctions) and
  refuses a path that leads into a system directory, compares case-insensitively
  (macOS's `/ETC` is `/etc`), and refuses any `..` element rather than only those
  left after cleaning.
- **Queue result uploads check every file a pattern matches** (#1000), not
  just the pattern, so a wildcard such as `/e*c/passwd` or one through a
  symlink can no longer upload a system file from the spore.
- **On Windows the path guard follows directory junctions and mount points**
  (#1000). Since Go 1.23 Go's own link resolution stops at a junction, so a
  junction made with `mklink /J` (no privilege needed) to `C:\Windows` let
  `--user-data-file` or a queue result pattern read the Windows directory,
  including `System32\config\SAM`. The guard now asks Windows for the final path
  of the longest existing prefix, which follows junctions, mount points,
  symlinks and 8.3 short names, and refuses a link it cannot resolve.

## [0.129.0] - 2026-10-16

### Added

- `pkg/aws.CheckCostLimitEnforceable`, the check above, so callers can refuse
  before creating resources of their own.
- **`spawn --version`** (spore-host/truffle#189). It was rejected as an unknown
  flag; it now prints the same version, commit and build date as
  `spawn version` (without the network update check, so scripts can probe the
  binary offline).
- **`spawn list -o json` reports `ttl_deadline` and `ttl_remaining_seconds`**
  (#898) beside the existing `ttl`, which keeps meaning the configured TTL.
  `ttl_remaining_seconds` is `null` when there is no deadline or the instance is
  already terminated, and `0` once the deadline has passed.
- **`spawn burst --ttl` and `--idle-timeout`** (#917). Every burst instance now
  has a TTL, 8h unless you pass `--ttl`, and terminates when it expires.
  `--idle-timeout` also ends an instance once it has been idle that long. The
  TTL must be a Go duration such as `30m`, `2h` or `36h`, because that is what
  the TTL deadline is computed from. `1d` is rejected rather than launching an
  instance with no deadline.
- **`spawn burst --instance-profile`** (#905) attaches an existing IAM instance
  profile by name, with no IAM calls, as `spawn launch --instance-profile` does.
  Without it, burst uses the shared `spored-instance-profile`, creating it if
  needed.
- **The spored systemd-unit gates now find every unit instead of checking a
  list** (#946). `TestSporedUnit_NoPrivateTmp` scanned two named files and never
  saw the two units `cmd/app.go` writes for web-app and DCV instances, so a
  `PrivateTmp=true` there (which silently breaks `--on-complete` and `--pre-stop`,
  #66) would have passed. The test now discovers every `spored.service` file and
  every heredoc that writes `/etc/systemd/system/spored.service`, fails if it finds
  none or meets an emitter it cannot parse, and rejects any truthy `PrivateTmp=`,
  not only `true`. A new `TestSporedUnit_AfterNetworkOnline` requires each unit to
  order after and want `network-online.target`, which spored's shutdown flush of
  `spawn:compute-seconds` (#928) and DNS deregistration rely on. All four current
  units already comply.
- **CI fails when spored sends a notification spore-bot does not handle**
  (#1004). Every event spored passes to its notifier, including ones chosen
  through a variable or a struct field, is checked against spore-bot's list of
  handled events (vendored in `testdata/spore-bot/`, with a CI step that fails
  when the copy drifts from spore-host main). `fsx_mount_failed`,
  `fsx_dra_failed` and `region_vacated` each reached Slack, Discord and SMS as a
  generic fallback message before spore-bot caught up; a new event now has to be
  handled before it ships.

### Changed

- **`spawn list`'s TTL column shows the time left, not the configured TTL**
  (#898). A `--ttl 1h` instance seven minutes old now reads `53m` rather than
  `1h` for its whole life. The deadline is the one `spawn status` reports (the
  `spawn:ttl-deadline` tag, so `spawn extend` is reflected, else launch time +
  TTL); `none` means no TTL, `expired` means the deadline has passed, `-` a
  terminated instance.
- **`--command` logs no longer open with the shell-options notice** (#897).
  Every `/var/log/spawn-command.log` (what `spawn array logs` shows) began with
  two `spawn: shell options ... errexit is ON` lines, on every member, even when
  the command succeeded. The #707 warning now appears in the command log only
  when the command exits non-zero, after its output, naming the exit code; it
  is also noted once in `cloud-init-output.log` at boot.
- **The job-array and `--mpi` launch summary prints one connect hint instead of
  a raw `ssh -i <key> ec2-user@<ip>` line per member** (#897):
  `Connect: spawn connect arr-<index>   (e.g. spawn connect arr-0)`, following
  `--instance-names` when it is set. `spawn connect` picks the key and falls
  back to SSM, and the summary no longer prints your local key path.
- **The audit trail and diagnostic log lines no longer appear in the terminal
  by default (#895).** `spawn launch` (single, job array and `--mpi`), `cancel`,
  `extend` and `capacity-block` used to print a raw JSON audit record — carrying
  your AWS account ID — between their progress lines, and library diagnostics
  such as `2026/10/15 09:59:58 pricing: m8a.4xlarge in us-west-2 = $0.9738/hr`
  appeared alongside them. The audit trail is now appended to
  `~/.spawn/audit.log` (owner-only) and diagnostics to `~/.spawn/spawn.log`;
  `--verbose` also copies both to stderr. If `~/.spawn` cannot be written, audit
  records fall back to stderr rather than being lost. Warnings you need to act on
  (no on-demand price, unverified plugin source, an untagged bucket, and similar)
  are still printed.
- **`spawn status` and `spored status` no longer print the agent's idle-check
  and initialization log lines (#895)** — e.g. `Not idle: CPU usage could not be
  measured ...`, and the `Agent initialized ... (account: ...)` line. `spored
  status --verbose` prints them to stderr; `spawn status --verbose` relays them
  from an older spored that still emits them. Errors from spored are still
  shown.
- **The hibernation preflight error no longer ends in a full stop** (#923), matching
  its EFA and MPI siblings, so it reads cleanly when wrapped. The wording is
  otherwise unchanged. This is the only user-visible text change from making
  staticcheck blocking. Everything else in that change is dead-code removal,
  test fixes, two small migrations off deprecated APIs with identical output
  (`parser.ParseDir` and the `ecdsa.PublicKey.X/.Y` coordinates embedded in
  the Windows spored install script), and line-scoped `//lint:ignore`s, each
  with a reason.
- **CI runs the unit tests on Windows** (#971). A `windows-latest` job runs
  `go test -short` over every root-module package except those listed, each with
  a reason and an issue, in `scripts/windows-test-skips.txt` (today: two that do
  not compile on Windows until #941, plus #1000 and #1001). Windows-only tests,
  such as spored's `sys_windows.go` readings, were previously compiled and never
  run. Tests that exercise Linux-only behaviour skip on Windows (#1002).
- S3 uploads and downloads now use the AWS SDK's `feature/s3/transfermanager`
  instead of its deprecated predecessor, the S3 `manager` package (#932). This
  covers job and queue results uploaded by spored, pipeline stage inputs and
  outputs, `spawn pipeline` result downloads, schedule and batch-queue
  parameter files, and Windows ISO staging. Transfers behave as before.
  transfermanager's own defaults would change them, so spawn keeps the old
  ones: 5 MiB parts (64 MiB with 4 at a time for ISOs), single-request uploads
  below one part, 5 parts in flight, ranged downloads, and CRC32 upload
  checksums. One difference: an upload larger than one part is now buffered in
  memory, a part per worker plus one, where the old uploader read the file in
  place. That is about 35 MiB for a default transfer and about 384 MiB for an
  ISO. The AWS SDK modules move up to the versions transfermanager requires
  (`service/s3` v1.114).

### Deprecated

- **`spawn collect-results --output`/`-o`** is now a deprecated alias for the new
  `--output-file`/`-f`, the spelling `spawn sweep collect` already uses. The old
  flag shadowed the global `-o/--output` format flag, so a user following
  collect-results' deprecation notice to `spawn sweep collect ... --output
  results.csv` set the output *format* and silently got `results.json`; the
  `spawn sweep` help example showed exactly that invocation, and now uses
  `--output-file` (#935).
- **`pkg/aws.GenerateScopedS3Policy`** (#1049). Nothing calls it, in spawn or
  in spore-host-mcp, lagotto or calque, and it will be removed in a future
  minor release. Its signature is unchanged. Its results grants now name the
  bucket spawn actually uses, `spawn-results-<account>-<region>`, instead of
  `spawn-results-<region>`, so the policy is correct while it remains.

### Removed

- The `make test-integration-scheduler` and `make test-integration-queue` targets,
  with the tests they ran (#913). The scheduler tests wrote a DynamoDB schedule
  schema that the scheduler has never read and waited for a trigger that doesn't
  exist (schedules fire from EventBridge Scheduler), so they could not pass. The
  queue tests were `t.Skip` placeholders.

### Fixed

- **A `--cost-limit` on a Windows instance is refused instead of enforced at
  the Linux rate** (#1058). spored enforces the cap as the recorded hourly rate
  × compute time, and the only rate available is Linux's. Windows bills its
  licence on top: m7i.xlarge in us-east-1 is $0.2016/hr Linux against
  $0.3856/hr Windows, so a `--cost-limit 10` Windows instance could spend about
  $19 before the cap fired. Windows can't be priced until
  spore-host/truffle#185, so the launch now refuses with an error that points
  to `--ttl`. The refusal happens in preflight, before any resource is created,
  and `--dry-run` and `--estimate-only` report it too. This covers every
  `Launch` caller, including those that don't set the target OS: the AMI is
  checked when a cost limit is present. A Windows launch without a cost limit no
  longer records the Linux rate as `spawn:price-per-hour`.
- **`spored status` no longer counts its own run time as compute** (#971). On the
  checkpoint path — always, on Windows, which publishes no compute clock — the
  compute figure included how long the status command itself had been running,
  so a fresh instance read "2s … as of the last checkpoint" before any checkpoint
  existed. The boot and billed figures were also late by however long the
  CPU/network sampling took, which on Windows (a PowerShell process per sample)
  is seconds: 47s after launch reported 52s billed.
- `spawn sweep resume` accepts `--max-concurrent-auto` (#494), which until now
  only the deprecated `spawn resume` had, so following `spawn resume`'s
  deprecation notice lost the option. The two commands now declare their flags
  from one place, as `spawn collect-results` and `spawn sweep collect` do, and a
  test fails whenever a deprecated command has a flag its named replacement does
  not accept with the same meaning (#935).
- `go mod tidy` works again. An integration-tagged test still imported
  `pkg/orchestrator`, which #862 deleted, so tidy failed outright; the stale test is
  removed, and the root and `lambda/dashboard-api` modules are tidied (drops the
  orphaned `robfig/cron` and an unused SQS SDK requirement). `make check` and CI now
  fail when any module, root or nested, is not tidy (#907).
- **No colour escapes when stdout is not a terminal** (#898). `spawn list | cat`
  and `spawn list > file` wrote raw `\x1b[32m` sequences because only
  `--no-color` turned colour off. Colour is now also off when stdout is a pipe or
  file, when `NO_COLOR` is set, or when `TERM=dumb`. `--no-color` still works.
- **`--name` now sets the OS hostname, as its help always said** (#897).
  Nothing set it, so single instances, job-array members (`arr-3`) and `--mpi`
  members (`sim-0`) all kept the EC2 default `ip-172-31-...`, which is what
  `hostname` and `mpirun hostname` printed. The bootstrap now sets the hostname
  from the Name tag before `--command` starts (lowercased to a valid DNS label,
  with the member index appended when an `--instance-names` template would give
  members the same name), maps it to the private IP in `/etc/hosts`, and keeps
  it across reboots. MPI hostfiles still address peers by private IP.
- **`--mpi-command` now signals completion, so `--on-complete` fires across
  the MPI cohort** (#894). mpirun ran, but nothing recorded that it had
  finished, so every member rode the job out to idle or TTL even with
  `--on-complete terminate`. That left MPI, the most expensive launch type,
  without the cost backstop. It now behaves like `--command` on rank 0. Its
  output goes to `/var/log/spawn-command.log`, so `spawn array logs <name>
  --index 0` can read it (before, it was only in cloud-init's log). Its exit
  code goes to `/tmp/SPAWN_EXITCODE`. Once mpirun exits, pass or fail, rank 0
  signals every member over the cohort's SSH mesh, so each one runs its own
  `--on-complete`. If rank 0 gives up because a peer never accepts SSH, the
  members it can reach stop as well.
- **spored's hybrid-registry client now has a region** (#899). On every
  job-array member (the report was a `--mpi` cohort, but any `--count` array
  takes the same path) spored logged `Failed to register with hybrid registry:
  … Missing Region`: the DynamoDB client was built from the ambient AWS chain,
  which on an instance names no region, because the SDK does not read it from
  IMDS. It is now pinned to the instance's own region, where `spawn burst`
  creates the `spawn-hybrid-registry` table — not the spore.host infra account,
  which holds no such table. A local (hybrid) node uses its configured region
  for both registration and peer discovery, and still falls back to the ambient
  chain when its config names none. `spored run-queue` (`spawn launch
  --batch-queue`) had the same defect in its results uploader and now takes the
  instance's region from IMDS when nothing else sets one.
- **`spored run-pipeline-stage`'s S3 and EC2 clients now have a region**
  (#906). The pipeline stage runner's clients (stage data handoff, peer
  discovery, and the pipeline-definition download) used the default AWS
  credential chain. On an instance that chain names no region, because the SDK
  does not read one from IMDS, so every call would have failed with
  `Missing Region`. If nothing else sets a region, they now use the instance's
  region from IMDS; an explicit `AWS_REGION` or profile region still wins. No
  current spawn workflow runs this command: `spawn pipeline launch` stage
  instances run their command directly. #910 proposes removing the stage
  runner or wiring it in.
- **`spawn status` costs a spot instance at its spot price** (#896). The
  instance row said `(spot)` but every figure used the on-demand rate, about
  2.7x too high on an m8a.4xlarge. A spot launch now records the spot price for
  its zone, type and platform (`spawn:spot-price-per-hour`), and compute cost,
  cumulative cost, effective rate, max cost and the billable-resources row use
  it. Instances launched before this are priced at the current spot price
  (needs `ec2:DescribeSpotPriceHistory`). If no spot price can be found, the
  on-demand rate is used and the output says it overstates. The on-demand rate
  is still shown as the baseline, and `--cost-limit` is still enforced and
  reported at the on-demand rate.
- **A brand-new instance no longer shows `$0.00 (0s × ...)` and "100% lower than
  continuous on-demand"** (#896). `spored status` runs as a fresh process. It
  counted compute from its own start, which left only the
  `spawn:compute-seconds` checkpoint, and that is 0 until the daemon's first
  write. The daemon now publishes its compute clock (`/run/spored`), so status
  counts real running time, including the boot time EC2 bills for. When the
  clock is unavailable (older spored, Windows), status says the figure is from
  the last checkpoint. Compute and cumulative cost show `—` until any running
  time has been recorded. The effective rate and its saving show `—` for the
  first minute, and on the checkpoint fallback.
- **`spawn status` shows times in one format** (#896): local time with UTC in
  parentheses, e.g. `2026-10-15 10:59 MST (17:59 UTC)`. Before, the TTL line
  gave UTC and the termination deadline gave local time for the same moment.
  spored's UTC timestamps are re-rendered to match, and so is the
  lifecycle-mismatch warning.
- **A failed pipeline stage no longer leaves its instance running and billing**
  (#912). A stage whose command failed or timed out, or whose bootstrap failed,
  exited before the self-terminate step. The orchestrator never read the
  `FAILED` marker, so the stage stayed "running" and the orchestrator Lambda
  re-invoked itself every ~13 minutes until someone ran `spawn pipeline cancel`.
  The TTL reaper could not reclaim the instance because it carried no
  `spawn:managed` tag, and without that tag the instance's own role was not
  allowed to terminate it either. Now:
  - A stage instance terminates itself however its script exits. It reads its
    instance id from IMDSv2, because `ec2-metadata` does not exist on Ubuntu or
    Debian AMIs, so even successful stages there kept running. It is launched
    with shutdown behaviour `terminate` and halts as a last resort.
  - Stage instances carry `spawn:managed=true`, `spawn:launch-time`, `spawn:ttl`
    and `spawn:ttl-deadline`, the same tags `spawn launch` writes. The TTL is the
    stage's `timeout` (default 1h) plus 30 minutes for bootstrap, so the reaper
    backstops them.
  - The orchestrator reads `FAILED`, and also fails a stage whose instances have
    all gone without writing `COMPLETE` or that has outlived its TTL. It
    terminates that stage's instances and applies `on_failure`: `stop` skips the
    remaining stages and terminates the ones in flight, and `continue` skips only
    the failed stage's dependents, which used to wait forever.
  - A pipeline past its deadline (the sum of its stages' TTLs plus 30 minutes) is
    failed and its instances terminated, and the orchestrator stops re-invoking
    itself.
  - Exceeding `max_cost_usd` now terminates the pipeline's remaining instances.
    The status used to be set but never saved, and nothing was terminated.
  - Cleanup terminates each stage's instances in that stage's region. It used to
    send every terminate request to us-east-1.

  The `spawn-pipeline-orchestrator` Lambda has to be redeployed for any of this
  to take effect.
- **`--spot` with an idle timeout, `--on-idle`, `--on-complete stop|hibernate` or
  `--hibernate` now launches an instance EC2 can actually stop** (#922). Every
  `--spot` launch used to make a one-time Spot request, and EC2 refuses to stop
  an instance from one, so the idle timeout's default stop, `--on-idle`,
  `--on-complete stop|hibernate`, `spawn stop` and `spawn hibernate` all failed
  on Spot. A Spot launch whose lifecycle stops or hibernates now makes a
  **persistent** request that is interrupted by stopping (or hibernating, when
  configured with `--hibernate`). The request is tagged like the instance, and
  the instance carries `spawn:spot-request-type`. A `--spot` launch with a
  terminate-only lifecycle (`--ttl`, `--on-complete terminate`) is still
  one-time, as before.
- **Terminating a Spot instance cancels its Spot request first** (#922), so a
  persistent request cannot launch a replacement. This covers `spawn terminate`
  and every other CLI path that terminates (cleanup, cohorts, launch rollback),
  spored's own TTL, on-complete and cost-limit terminate, the TTL reaper, and
  the dashboard. If the CLI or dashboard cannot cancel a persistent request it
  does not terminate the instance; it prints the `aws ec2
  cancel-spot-instance-requests` command to run first. spored and the reaper
  terminate anyway and log the request id, because an instance past its
  deadline must not keep running. New grants of `ec2:CancelSpotInstanceRequests`,
  conditioned on `spawn:managed=true`, are added to: the spored instance role
  (re-applied on the next launch), the portal onboarding role, and the reaper's
  scan-self and cross-account (`spawn-ttl-reaper-ec2`) roles.
- **`spawn stop` and `spawn hibernate` on a one-time Spot instance say why up
  front** (#922). They used to print "Saving remaining TTL" and then fail with
  EC2's `UnsupportedOperation`. They now refuse before doing anything, and
  suggest `spawn terminate` or a stop-based lifecycle at launch.
- **`spawn status` describes what the idle timeout really does** (#922). On a
  one-time Spot instance it said "stops the instance when idle"; it now says the
  instance cannot be stopped and runs until its deadline. It also says
  "hibernates" for `--on-idle hibernate`.
- `make test-integration` builds again. The root `integration_test.go` had not
  compiled since the initial release (`undefined: config`, and a config loader
  with the wrong return type), and the stale `pkg/integration` orchestrator test
  from #907 is removed here too. The surviving multi-region sweep tests now clean
  up after themselves: they cancel with `--yes` (without it, `spawn cancel`
  prompted, got no answer, and terminated nothing), cancel from `t.Cleanup` so a
  failed assertion no longer skips it, and leak-check EC2 for anything still
  carrying the sweep's tag. `make test-integration` now builds `bin/spawn` first
  and runs with `-count=1`. `make check` and CI now `go vet` every module under
  every build tag found in its `//go:build` lines, without running anything, so
  tagged test code can't silently stop compiling again (#913).
- **The test suite compiles for Windows** (#925). `pkg/taskproto` and
  `pkg/service` each had a test file using Unix-only `syscall` APIs with no
  build constraint, so `go test ./...` could not build on Windows, although
  windows/amd64 ships. The Unix file-ownership tests are now constrained to
  non-Windows, and the fake-ssh fixture's process-group handling moved
  behind a per-OS helper. A new gate, `make check-goos` (also in `make check`
  and CI), runs `go vet ./...` for every GOOS that `.goreleaser.yaml` builds.
  It discovers the set from that file and fails if it finds none.
- **`spawn burst` instances can now run spored** (#905). Burst launched its
  instances with no IAM instance profile, so spored on them had no AWS
  credentials. It could not read its tags, register in the hybrid registry,
  register DNS or terminate itself. Burst now attaches the same
  `spored-instance-profile` that `spawn launch` uses.
- **`spawn burst` failed at launch every time** (#905). It sent its user-data
  script unencoded, and RunInstances rejects user-data that is not base64.
- **The TTL reaper, `spawn list` and `spawn array status` can see burst
  instances** (#917). Burst wrote five tags by hand and none of the ones
  `spawn launch` writes. Without `spawn:managed=true`, the reaper's max-age
  ceiling never applied, `spawn list` skipped the instances, and the spored role
  could not terminate them. Burst now launches each member through the same
  launch path as `spawn launch`, so it gets the same tags, and each member gets
  its own job-array index instead of all registering at index 0. Instances are
  named `spawn-burst-<id>-<index>`. `--job-array-name` now defaults to the job
  array ID, so the `spawn array status` command burst prints finds the
  instances. If a launch fails part-way, burst lists the instances already
  running and how to cancel them.
- **spored can use the hybrid registry** (#905). The spored role had no access
  to the `spawn-hybrid-registry` table, so no job-array member could register.
  Both spored policies now grant `PutItem`, `UpdateItem`, `DeleteItem` and
  `Query` on that one table, in the instance's own account. Existing roles get
  the grant on the next launch.
- **`make check`'s staticcheck step can fail** (#923). It was
  `staticcheck ./... || echo "staticcheck not installed"`, and because `||`
  fires on any non-zero exit, findings were reported as a missing tool and the
  step passed. 67 findings had accumulated on main. They are fixed, the step
  now fails on findings and skips (with a message) only when the binary is
  absent, and CI runs a pinned, checksum-verified staticcheck 2026.2.2 on
  every push and PR. CI previously ran no linter.
- **`spored status` reports cost-limit headroom the way the cap enforces it**
  (#959). Since #616 `--cost-limit` counts compute plus EBS (EBS on wall-clock time
  since launch, including while stopped), but the `Cost limit:` line and
  `cost_limit_used` / `cost_limit_remaining` in `-o json` were still computed from
  compute alone, so they overstated the remaining budget — most on a stopped
  instance, whose EBS kept accruing while the displayed figure stood still — and
  the cap could fire before "remaining" reached zero. Both now read the same
  figure the enforcement terminates on, and the line says `compute + EBS` instead
  of `compute-only`.
- **`spawn status -o json` marks EBS as `capped_by_cost_limit: true`** (#959). It
  said `false`, contradicting both the enforcement and the human footer on the
  same view. FSx and EFS stay `false`: terminating the instance does not stop them
  billing.
- **`launch --cost-limit` and `extend --keep-cost-limit` help say the instance is
  terminated at the cap, not stopped** (#959), and `--cost-limit` says it counts
  compute + EBS. The generated reference (`docs-gen/`) is regenerated to match.
- **A secret-scan finding on one branch no longer fails every open PR's Secret
  Scan** (#1024). The gitleaks job checked out with full history, which fetches
  every branch, and then scanned all of them, so a hit on one PR's branch (real or
  false positive) turned every other PR red until that branch was force-pushed.
  The scan now covers exactly the history of the commit under test: on a pull
  request, main plus every commit in the PR (a secret added and later removed in
  the PR is still caught); on a push or the weekly run, main's full history.
  `workflow_secret_scan_test.go` fails if the scan loses its HEAD scope or its
  full-history checkout.
- **Local plugin files work on Windows** (#997). `spawn plugin inspect` and
  `install` recognised a local file only by `./`, `../` or `/`, so
  `C:\plugins\demo.yaml` or `.\demo.yaml` was looked up as a registry name in
  spore-host/spore-plugins and failed. Any OS-absolute path, and `.\`/`..\` on
  Windows, is now a local file.
- **Pipeline stage inputs on a Windows spore no longer pull nested objects**
  (#998). The input pattern was matched against S3 keys with the host's path
  rules, so on Windows `output/*.txt` also matched `output/nested/result.txt`; it
  now matches the way S3 keys are written on every OS. Output keys for an
  absolute pattern are `/`-separated there too.
- **`spawn snapshot --from <raw image>` closes the image when it is done**
  (#999). The source file (or S3 object stream) was never closed; on Windows that
  also kept the file locked until spawn exited.
- **`-o/--output` rejects a value it does not render** (#975). It accepted
  anything, and every command treats an unknown format as `table`, so a typo
  (`-o jsno`) or a file name (`spawn sweep collect <id> --output results.csv`)
  printed the table or wrote the default file and exited 0. It now exits 1
  naming the valid formats (`table`, `json`), and on a command with
  `--output-file` (or `--output-dir`) it suggests that flag instead. Commands
  whose own `--output` is a file or directory are unaffected.
- **`spawn app` subcommands honour `--profile`, `--region` and `--account`**
  (found with #975). `spawn app`'s own `--catalog` hook replaced the root
  command's, so for `spawn app list`/`launch` those flags never reached the
  config layer and the profile/region came from the environment or config file
  instead. Every ancestor's hook now runs.
- **Per-region S3 buckets are reached in their own region** (#1015). Three
  commands signed requests for one region against a bucket in another, which
  real S3 answers with a redirect:
  - `spawn schedule create --region <r>` worked `spawn-schedules-<r>` with a
    us-east-1 client, so bucket creation or the parameter upload failed for any
    region but us-east-1.
  - `spawn collect-results` / `spawn sweep collect` read
    `spawn-results-<account>-<r>` with a us-east-1 client, logged a warning,
    and reported "No results found".
  - `spawn validate --infrastructure --region <r>` reported the binaries and
    schedules buckets as inaccessible.
- **CI action pins now say which version they actually run** (#994).
  `actions/checkout@3d3c42e…` was commented `# v6` in every workflow but is
  v7.0.1; likewise `goreleaser-action` `# v7` is v7.2.3 and
  `configure-aws-credentials` `# v6` is v6.3.0. `dependabot.yml` already cited a
  `TestActionsArePinnedToSHAs` that did not exist; it does now. The SHAs were
  right and are unchanged; only the comments were wrong — but those comments are
  what reviewers read and what Dependabot rewrites. A new
  `workflow_pins_test.go` now fails on any `uses:` not pinned to a full SHA with
  a `# vX.Y.Z` comment, and checks each comment against
  `scripts/action-pins.txt`, a table of what every pinned SHA really is,
  regenerated from GitHub by `scripts/refresh-action-pins.sh` (run it, and
  commit the table, whenever a pin changes — including on a Dependabot bump).
- **Scheduled sweeps can start** (#1026). Every one failed: when a schedule
  fired, `scheduler-handler` invoked `spawn-sweep-orchestrator` with a sweep ID
  for which it had written no record, so the orchestrator stopped at
  `sweep <id> not found`. Behind that, the params it had were the user's YAML in
  a bucket the orchestrator cannot read, under a bare key the orchestrator would
  have parsed as bucket `schedules`, and the schedule recorded no account to
  launch in. The handler now stages the params as JSON, writes the sweep record
  and then invokes — through the same `pkg/sweep` code `spawn launch
  --param-file` uses. `spawn schedule create` records the compute account and
  resolves a per-row AMI (#749) before upload. **Schedules created before this
  have no target account and are refused when they fire; recreate them.**
  Requires redeploying `scheduler-handler` and its role
  (`deployment/cloudformation/scheduler-handler-role.yaml`, which also still
  named the pre-#812 `sweep-orchestrator`).
- **A param file's `defaults: ami:` is honoured by detached sweeps** when
  `--ami` is not passed. A per-row AMI was resolved for every row without one,
  and the orchestrator merges a row over the defaults, so the file's AMI was
  never launched (#1026).
- **`spawn collect-results` / `spawn sweep collect` read results with the
  compute account's credentials** (#1034). `spawn-results-<account>-<region>` is
  named for, written from and created in the sweep's compute account. Collect
  read it with the infra credentials it fetches the sweep record with, so every
  list was denied: a warning per region, then `No results found` and exit 0.
  The sweep record still comes from the infra account. If the compute
  credentials (`SPAWN_COMPUTE_PROFILE`, default `spore-host-dev`) are for a
  different account than the sweep ran in, the command now fails and names both
  accounts.
- **`spawn plugin remove` now runs the plugin's `remote.stop` steps** (#1018).
  A plugin spec could declare them, and `spawn plugin inspect` listed them
  under "On removal", but nothing ran them, so a service the plugin started
  (a systemd unit, a daemon) kept running after the plugin was removed. They
  now run on the instance, with the plugin's config and pushed values, before
  spored forgets the plugin. If a stop step fails, the plugin is still
  removed, and `spawn plugin remove` warns that what it started may still be
  running. `remote.stop` does not run when the instance stops, hibernates or
  terminates: nothing runs `remote.start` again when the instance comes back,
  so for example a `tailscale down` there would keep the node off the
  tailnet after the next `spawn start`. `inspect` now says when each step
  runs. A plugin installed by an older spored has no stop steps on record, so
  removing it runs none, as before.
- **A Windows launch refuses the flags Windows cannot honour, instead of
  dropping them** (#1022). `--command`, `--user-data` and `--user-data-file`
  were accepted for `--os windows` (or a Windows AMI) and silently ignored: the
  Windows bootstrap runs no workload, so the job never started, `--on-complete`
  never fired, and the instance billed until its TTL. The same was true of
  `--plugin`/`--config`, `--session-timeout`, parameter sweeps
  (`--params`/`--param-file`), `--batch-queue`/`--queue-template`, `--strata-*`,
  `--mpi` and its options, and storage: `--efs-*`, `--attach-volume` (attached,
  never mounted) and `--fsx-*` (`--fsx-create` created and billed a Lustre
  filesystem nothing could mount). Each now fails the launch, and `--dry-run`,
  with a message naming every such flag and why. Every other launch flag is
  recorded as honoured on Windows, and a test fails when a new flag is added
  without that decision.
- **`--plugin` plugins load reliably** (#1023). The bootstrap wrote
  `/etc/spawn/plugins.json` after `systemctl start spored`, but spored reads it
  only once, at startup, so declared plugins loaded only when the bootstrap won
  a race against spored's first metadata reads. A slow disk or any extra step
  at that point silently dropped every plugin. The file is now written before
  spored is installed or started.
- **`TestJobArrayIDSuffixIsNotTheClock` no longer flakes** (#1044). It failed on
  the first duplicate among 2000 job-array ids, but the id's 32-bit random
  suffix collides about once in 2000 such runs (6 of 20000 measured). It now
  fails on more than one duplicate. A clock-derived suffix still fails it by
  hundreds, and a new test checks that the tolerance still catches a clock. The
  id format is unchanged: a production collision needs the same
  `--job-array-name` on the same day, and is about 10⁻⁶ even at 100 such launches.
- **The self-hosting `spore-bot.yaml` template matches the bot it deploys**
  (#1043). Audited against spore-host's `lambda/spore-bot`: it now takes the
  bot's secrets and settings as parameters (Discord public key, Slack signing
  and OAuth secrets, Teams app, Twilio, slash command) instead of leaving a
  self-hosted bot without them; creates the `spore-sms-pending` table and grants
  the write SMS notifications make (it failed silently before); allows `GET` on
  the Function URL for the OAuth install flow; turns on X-Ray tracing; and
  defaults `LambdaCodeKey` to the key the bot's `make upload` writes. Secret
  parameters are `NoEcho`; pass them with `UsePreviousValue` on stack updates.
  The `bot-cross-account-role.yaml` example role ARN and a comment naming
  `SPORE_BOT_NOTIFY_URL` (the variable is `SPORE_NOTIFY_URL`) are corrected.
- **`spawn extend --job-array-id/--job-array-name` now actually extends the
  instances** (#983). It used to write only `spawn:ttl`, set to the raw argument,
  and report "Job array TTL extended!". spored and the TTL reaper enforce
  `spawn:ttl-deadline`, which it never changed, so every instance still
  terminated at its original deadline. Each instance's deadline now moves
  forward from its own current deadline, exactly as `spawn extend <id>` does,
  and the new deadline is printed for each instance. The cost limit is raised
  per instance when it would otherwise stop the instance first, and
  `--cost-limit` / `--keep-cost-limit` work as on a single instance. That
  replaces the old advice to extend capped instances one at a time.
- **`spawn start` no longer claims to restore a paused TTL** (#983). It wrote the
  remaining time saved at stop into `spawn:ttl` and left the enforced deadline
  where it was. On an instance with a deadline that changed nothing. On an older
  instance without one it shortened the TTL. The TTL deadline is fixed at launch
  and does not pause while the instance is stopped, so `spawn start` now says so
  and shows the deadline (#928 tracks the matching `spawn stop` message). A
  new test fails if any code writes `spawn:ttl` without also writing or removing
  `spawn:ttl-deadline`.
- **Only hybrid job-array members register with the hybrid registry** (#904).
  spored registered every instance carrying `spawn:job-array-id` with the
  `spawn-hybrid-registry` DynamoDB table, so every ordinary `spawn launch
  --count` / `--mpi` member logged `Failed to register with hybrid registry`
  on boot: the instance role grants no DynamoDB access, and the table only
  exists where `spawn burst` has run. Nothing reads the registry for those
  arrays — EC2 peer discovery uses tags, and MPI peers come from the cohort
  control plane. spored now registers (and heartbeats and deregisters) only
  local nodes and instances launched by `spawn burst` (tagged
  `spawn:burst=true`); other members log that the registry was skipped. Peer
  discovery is unchanged.
- **Table columns line up in Japanese and other CJK locales** (#909). `spawn
  list` and every other table spawn prints padded columns by character count,
  so a double-width character such as those in `名前` or `IAMロール` shifted
  every later column left by one cell. Columns are now padded by the width a
  terminal displays, and colour escapes no longer count towards a cell's width,
  so a coloured `running` cell no longer pushes its column wider than its
  header. English output is unchanged.
- **Idle-stop never fired on instances whose network interface is not named
  `eth0` or `ens*`** (#919). On an m8a with Amazon Linux 2023 the NIC is
  `enp39s0`, which spored did not recognise, so it read zero bytes on every
  check, took every check for the first one, and assumed the instance was
  active. `--idle-timeout` therefore did nothing, and the log reported
  `Not idle: Network traffic 1000000 bytes` once a minute. spored now counts
  every physical interface, whatever it is called, and skips loopback, Docker,
  veth, bridge and VPN interfaces. When it cannot read any interface it logs one
  warning saying idle-stop will not trigger, rather than reporting invented
  traffic. The Prometheus network metric had the same filter and now reports
  the real interface.
- **`spawn status` and `spored status` no longer print `Network: 976.6 KB/min`
  when nothing was measured** (#919). That figure was the "assume active"
  placeholder. A one-shot status has no earlier sample to compare against, so
  it now prints `Network: unknown (single sample; assuming active)`, the same
  way the CPU line does. The JSON output gains `network_measured` and
  `network_unknown`.

### Security

- **The unit tests no longer make real AWS and DNS calls with your credentials**
  (#968). `TestNewAgent_StillActivates` called the real spored agent
  constructor, so on any machine with AWS credentials a plain `make check` sent a
  real Route53 UPSERT through the spore.host DNS API (registering
  `demo.<account>.spore.host -> 203.0.113.10`), an EC2 `CreateTags` and a
  DynamoDB `PutItem` — and passed whether they succeeded or not. The same
  audit found more: an FSx `DescribeFileSystems` poll from the FSx mount test,
  S3 `PutObject` uploads from the task-pool job-script tests (through the `aws`
  CLI), public-IP lookups to api.ipify.org/ifconfig.me/icanhazip.com from the
  local-provider tests, an STS `GetCallerIdentity` from the TTL reaper's package
  `init` in every one of its test runs, a DynamoDB `GetItem` from a
  dashboard-api handler test, and a DynamoDB `Scan` of the DNS account registry
  from two dns-updater handler tests. Each now goes through a fake.
  `make check`, CI and `scripts/nested-modules.sh` now run every short test
  under a hermetic wrapper (`test/hermetic`): ambient credentials are replaced
  with fakes, and `AWS_ENDPOINT_URL` and `HTTP(S)_PROXY` point at a local
  recorder, so a test that reaches AWS, the DNS API or any other off-box host
  fails and names the request. `pkg/agent` also installs this in its own
  `TestMain`, so `go test ./pkg/agent` is safe on its own.
- **The spored systemd unit written for web-app instances (`spawn app`) now sets
  `NoNewPrivileges=true`** (#959), like the canonical unit, the DCV unit and
  `scripts/spored.service` already did. spored runs as root and needs no privilege
  escalation; the setting stops a hook or job it spawns from gaining privileges
  through a setuid binary or file capabilities.

### Documentation

- **`docs/infra-account.md` describes spore-bot's deployment correctly** (#1043).
  It said a CloudFormation template in this repo deploys the bot. In fact its
  code ships from spore-host's `lambda/spore-bot` with `make deploy`, its role,
  runtime and Function URL are OpenTofu in spore-host's `infra/tofu/spore-bot`,
  and there has been no stack since 2026-06-14; `spore-bot.yaml` is only for
  self-hosting. The bot is no longer listed as API Gateway-fronted (it is a
  Function URL), `spore-sms-pending` is attributed to it, and the new facts are
  marked "per spore-host repo" until the next `spawn footprint` confirms them.

## [0.128.2] - 2026-10-13

### Deprecated

- **`spawn launch --dns-domain` and `--dns-api-endpoint`** (#872). Neither has
  ever reached the registration that happens. Both are now hidden and print a
  warning; custom-domain registration is #891.

### Removed

- **`spawn launch` no longer registers DNS itself; spored does** (#872). The
  launch SSHed into the instance and ran an unsigned `curl` against an API
  Gateway that was deleted long ago, carrying the identity-document fields the
  updater stopped reading in #173. It failed on every launch, and its warning
  blamed the user's account ("expected in accounts not wired for spore.host
  DNS"). spored registers the same name from the `spawn:dns-name` tag, signed,
  and keeps its lease renewed, and that was the registration that actually
  worked. The launch now reports spored's result: the record, the failure
  detail, or that registration is still pending. It also stops claiming to
  "skip" DNS for `--wait-for-ssh=false` and SSM-only launches, which spored
  registered all along. Also removed:
  the `dns.domain` / `dns.api_endpoint` config keys, `SPAWN_DNS_DOMAIN` /
  `SPAWN_DNS_API_ENDPOINT`, and the `/spawn/dns/domain` / `/spawn/dns/api_endpoint`
  SSM lookups. They fed only that path, and the domain was never sent. The keys
  are ignored if present.

### Fixed

- **`--no-dns` and `dns.enabled: false` actually skip DNS registration**
  (#872). They suppressed only the CLI's own attempt; the `spawn:dns-name` tag
  was written regardless, so spored registered the name anyway. They now
  withhold the tag.

- **`spawn dns register` and `spawn dns delete` work from a workstation**
  (#872). The DNS client fetched the instance identity document from IMDS
  before every request, so off-instance it failed before sending anything. The
  updater has ignored those fields since #173. The client no longer fetches
  them, and `--domain` now reaches the request instead of only the printed name.

- **Windows spored now signs its DNS requests, so Windows spores register DNS**
  (#883). spored signed only when `SPORE_DNS_SIGV4` was set, and only the Linux
  systemd units set it; the Windows service sets no environment. Windows spored
  therefore sent unsigned requests, which the `AuthType: AWS_IAM` DNS Function
  URL refuses with 403 whatever its resource policy says — observed on a
  v0.128.0 Windows spore — so Windows DNS, and DNS leases (#875), could not work.
  The opt-in existed to stage the #173 cutover, and its own comment said the
  flip and "the default-on switch happen in lockstep"; the switch never
  happened. Signing is now the default, with `SPORE_DNS_SIGV4=0` (or
  `false`/`off`/`no`) as an explicit opt-out for an unauthenticated endpoint.

- **DNS requests are signed for the Function URL's region, not the client's.**
  The signing region was the ambient one (`cfg.Region`), falling back to
  us-east-1 — correct only by accident, because `LoadDefaultConfig` does not read
  the region from IMDS, so on an instance it was usually empty and the fallback
  matched the URL. A spored environment with `AWS_REGION` set outside us-east-1
  would have signed for the wrong region and been refused. The region now comes
  from the endpoint's own hostname (`<id>.lambda-url.<region>.on.aws`), with the
  ambient region used only for a custom endpoint whose hostname carries none.
- **`spawn launch --os windows` without `--ami` is refused instead of launching
  Linux** (#884). AMI auto-detection only finds Amazon Linux 2023, so the launch
  booted Linux while the preview, the spored readiness check and the Windows
  guards all treated it as Windows. spawn does not choose a Windows version for
  you; the error names the SSM parameter that returns the latest Windows Server
  AMI to pass as `--ami`.

- **Windows rates are labelled as the Linux lower bound they are** (#884). The
  pricing source prices Linux only, so `--dry-run` and `--estimate-only` quoted a
  Windows instance at the Linux rate with nothing saying the Windows licence EC2
  adds was missing. Both now say so beside the figure, and the `-o json` plan
  carries `price_is_lower_bound: true`. Real Windows pricing is spore-host/truffle#185.

### Security

- **CI verifies the gitleaks binary before running it** (#885). The security
  workflow installed it with `curl | tar` and executed the result unchecked — the
  scanner deciding whether secrets are present was itself unverified, and a
  pinned version with no pinned hash runs a replaced release asset unnoticed. The
  expected SHA-256 is now pinned in the workflow (cross-checked against the
  published checksums when pinned) and checked with `sha256sum -c` before
  extraction. `TestWorkflowsVerifyWhatTheyDownload` forbids piping a download
  into tar or a shell and requires any `curl -o` download to be checked in the
  same step; its first version read physical lines and missed the original step,
  whose `| tar` sat after a line continuation.

## [0.128.1] - 2026-10-13

### Added

- `TestLaunchDecidesSporedVerificationByTargetOS` reads the launch call site's
  AST and fails if the verification decision is passed anything of `plat` — the
  original defect was one wrong value in one condition, which a test of the
  decision function alone cannot see. Five compiling reverts fail, including the
  tempting non-fix of skipping verification for Windows, which would also have
  made the false error disappear.


- The Windows verifier is **executed**, not string-matched. CI cannot run Windows
  PowerShell 5.1 but GitHub's runners ship PowerShell 7, and every API used
  exists in both. The test substitutes a fake `aws` for the script's `& $aws s3
  cp` and runs the whole generated install: verified install, a tampered source
  skipped in favour of the next, every source tampered, a matching checksum with
  a foreign signature (the bucket-write case), a missing `.sig`, a missing
  `.sha256`, and both DER integer shapes — 33-byte and short — that a fixed-width
  signature converter would silently reject on some launches. In CI a missing
  `pwsh` fails rather than skips. Also checked: the real v0.128.0 Windows binary
  verifies against the real key, and a tampered copy is refused.
- `.gitleaks.toml` allowlists the spored signing **public** key's coordinates in
  the generated `install-spored.ps1` by exact line shape, not by path, so a real
  secret added to that file is still caught — verified against two negative
  controls on the gitleaks version CI pins (8.21.2). The config now records that
  its semantics are version-dependent: 8.21 ORs a table's `paths` and `regexes`,
  later versions AND them, and 8.21 ignores the newer `[[allowlists]]` form
  altogether. A version validated only against a newer local gitleaks passed
  locally and failed CI with sixteen findings.
- Two gates on the rendered Windows user-data: `service install` must be
  reachable only through a successful verification, and the whole `<powershell>`
  block must parse under PowerShell's own parser — a syntax error there fails
  nothing in spawn, and would leave every Windows spore without spored.


- `TestOnlyTheReleaseWorkflowPublishesSpored` — nothing but the release workflow
  may write a spored binary to S3. Every shell script, Makefile and workflow is
  scanned, and commands are read as **logical** lines with arguments taken by
  position, so `aws s3 cp SRC DST` is a write exactly when `DST` is an `s3://`
  URL. The first version matched per physical line and **passed with the deleted
  script restored**, because that script put its `s3://` destination on a
  continuation line; the gate was blind to the one thing it existed to stop.

### Changed

- **`scripts/install-spored.ps1` is now generated**, from the same verifier the
  Windows launch bootstrap uses, and fails closed (#879). It used to install
  spored unverified. It is kept rather than deleted because Windows has no
  `spawn upgrade-spored` yet (#234), so it is the only manual path there is; a
  hand-maintained copy of the verifier is how Windows came to skip verification,
  so it is not hand-maintained. `TestInstallSporedPs1IsGenerated` fails if the
  committed file differs from the generator.

### Removed

- **`scripts/install-spored.sh`** (#879), a manual installer that downloaded
  spored and installed it as a root systemd service with no checksum or
  signature check. `spawn upgrade-spored` is the supported, verified way to
  (re)install spored on an existing Linux instance.


- **`scripts/deploy-spored.sh`** (#876). It uploaded `bin/spored-*` to every
  regional bucket with **no `.sha256` and no `.sig`**, while the boot installer
  refuses any spored whose checksum does not match or whose signature does not
  verify. One run would have left new binaries beside the old checksums, and
  every instance launched afterwards would have refused to install spored and
  booted with no TTL, idle-stop or cost-limit enforcement — the failure the
  release workflow's own comments record as having happened once (#440). It also
  covered 10 regions of the release's 11 and no Windows binary.

  Deleted rather than fixed: the `.sig` is made by a KMS key reachable only
  through the release workflow's OIDC role, so no script can produce a valid
  one. **spored ships only by tagging a release.**

### Fixed

- **`spawn launch` chose how to verify spored by the OS of your workstation,
  not the instance** (#882). The check was gated on `plat.OS`, which is
  `runtime.GOOS` — the machine running spawn. Launching Windows from macOS ran
  the Linux check against the Windows instance; SSM refused it with
  `UnsupportedPlatformType`, and the launch reported "spored never came up" about
  a spore that was healthy, exiting 1 (observed). **With `--terminate-on-error`
  that terminated a healthy instance.** From a Windows workstation, a Linux
  instance got no check at all, so a failed spored install was reported as a
  successful launch.

  The decision is now a function of the instance's OS (`config.TargetOS`) only;
  the workstation's OS is deliberately not a parameter. A Windows target is
  verified properly instead of skipped — asking the Service Control Manager
  through `AWS-RunPowerShellScript` whether the spored service is Running, with a
  10-minute budget for first boot — which matters now that the Windows install
  fails closed (#879) and so has a real failure to report.

### Security

- **Windows spores now verify spored before installing it, and install nothing
  if it does not verify** (advisory GHSA-q59f-6gc3-qj2p, #879). The Windows
  launch bootstrap copied `spored-windows-amd64.exe` out of S3 and registered it
  as a SYSTEM service with no checksum or signature check — the Linux bootstrap
  has verified both since GHSA-3x4q-99gh-fxhx, which described two such paths;
  this was a third. It also carried on when the download failed.

  It now downloads the `.sha256` and `.sig` beside the binary from the same
  source, checks both, and only then installs; a source that fails is skipped
  and the next tried, and if none verifies nothing is installed and the console
  says so. The material was always there: every release already publishes the
  Windows binary's checksum and KMS signature.

  Targets Windows PowerShell 5.1 on Windows 11+, deliberately not PowerShell 7:
  EC2Launch runs user-data with the built-in `powershell.exe`, and requiring 7
  would mean downloading an unverified installer before verifying spored. The
  key is embedded as its P-256 X/Y coordinates — .NET Framework 4.8 has no
  `ImportSubjectPublicKeyInfo` — derived at generation time from the same
  `SigningKeyPEM` Linux uses, so the two cannot verify against different keys.

## [0.128.0] - 2026-10-12

### Added

- **DNS leases: a record whose owner stops renewing it can now be expired from
  the zone alone, with no credentials in the account that owns it.**

  Every existing backstop for a stale A-record asks about the instance behind
  it. The DNS sweep (#438) calls `DescribeInstances` in the owning account, and
  DNS expiry (#466) relies on the portal prober assuming a role there. Both need
  credentials in that account, so for any account the reaper does not cover — an
  account spore.host must never hold credentials in, or any governed org that
  will not grant an external principal access — a record left by an abruptly
  terminated instance pointed at its IP **indefinitely**, under a spore.host
  name, after AWS had handed that IP to someone else. A self-hosted reaper
  (#625) does not help: it cannot touch spore.host's zone.

  A lease asks the owner instead. Three parts:

  - **spored** re-registers about hourly (1h ± 10m, jittered so a job array
    does not hit Route53's five-per-second limit), through the same code path
    as its boot registration so the two cannot drift, and re-tags
    `spawn:dns-status` only when it changes.
  - **the DNS updater** stamps the registration time into a
    `_spore-lease.<record>` TXT record **in the same Route53 batch** as the
    A-record, so the two are written atomically, and only under the
    SigV4-verified caller's own namespace (#173), so nobody can renew anyone
    else's record. It removes the lease on DELETE.
  - **the TTL reaper** reports leases unrenewed for six hours and, with
    `REAPER_DNS_LEASE_EXPIRE=true`, expires the A-record and its lease, for
    account subdomains it does **not** cover.

  The safety properties, each gated against a revert that compiles:

  - **Opt-in by the client.** Only a client that will renew may ask for a
    lease, and `spawn dns register` — a one-shot caller of the same client —
    does not; a lease it never renewed would expire a live record six hours
    later. A gate parses every non-test Go file and fails if anything other
    than spored's agent calls `SetRenewing(true)`, or if the agent does without
    calling `refreshDNSLease`.
  - **Old clients are unaffected.** A request with no `lease` field — what every
    released spored sends — gets no lease, proven through the real handler. And
    a non-renewing client re-registering a name **clears** an inherited lease,
    since otherwise that lease would age out under a live instance.
  - **Covered accounts are never expired by lease.** The `DescribeInstances`
    sweep is authoritative there, and a stale lease most likely means a hung
    spored on a live box.
  - **An unparseable lease is never acted on**; a lease dated in the future
    reads as fresh, so clock skew cannot expire a live record; and the reaper
    re-reads a lease immediately before deleting, so a renewal landing mid-run
    wins.
  - **Arrives off.** `DnsLeaseExpire` defaults to `false` and is live-wins, not
    always-asserted, so arming it later is not undone by a plain deploy.

  A wrongly expired record now self-heals on the next renewal, which removes
  the premise behind #466's caution that a deleted record "never self-heals" —
  for renewing clients. That comment in `expiry.go` is corrected, since it is
  still true of every pre-lease spored.

  The lease format is a contract between three separately built components, and
  `lambda/dns-updater` is its own module that cannot import `pkg/dns`.
  `TestDNSLeaseContractMatchesTheUpdater` parses the updater's copy with
  `go/ast` and fails if its name or format drifts.


- Four gates for the above, and a `print-explicit-params` target so they can
  assert on what `make deploy` **actually emits** rather than on `PARAM_MAP`
  membership — which is what the previous gate checked, and why it stayed green
  while `Version` was being dropped. The emission is defined once
  (`WRITE_EXPLICIT_FILE`) and called by both targets, so the gate cannot be
  asserting on a copy a deploy never runs.

  `TestVersionReachesCloudFormation` drives the real Makefile emission through
  the real merge script and rejects `""`, `"unknown"`, and a value with no
  digits. `TestPlainDeployEmitsNoModeSettingParameter` is the #650 guard applied
  to the Makefile's emission, which had no coverage before — the existing test
  drives the script with a hand-written override string, so it could not catch a
  Makefile that emitted too much. `TestAlwaysAssertContainsOnlySafeParameters`
  pins the membership so widening it requires editing the test.
  `TestVersionFallsBackOutsideAGitRepo` runs the Makefile in a temp directory.

  Verified against seven reverts that parse, including the dangerous one: adding
  `DryRun` to `ALWAYS_ASSERT` is caught by three independent tests, and making
  `EMIT_EXPLICIT` unconditional by two. Every pre-existing #650 guard passes
  unchanged — none had to be weakened.


- `TestNoDeployScriptCreatesAnUnauthenticatedAPIGatewayRoute` — a script that
  creates an API Gateway method or route must make the authorization decision
  explicit. It catches **both** ways to get `NONE`: naming it, and omitting
  `--authorization-type` entirely, which the API defaults to `NONE` and which is
  the form review misses.

  "Authenticated at the edge" is deliberately **not** the rule. On its first run
  this flagged two scripts that are legitimate — `setup-dashboard-api-gateway.sh`
  and `setup-websocket-api.sh` both leave the edge open and authenticate in the
  handler (`getUserFromRequest` / `getUserFromCredentialsHeader` /
  `verifySlackRequest`, and a ticket minted by `POST /api/ws-token`). Requiring
  edge auth would have needed an allowlist of things that are fine, which is the
  signal the rule is wrong (rule 6). So the rule is that leaving the edge open
  must be **stated** next to the call with an `authz-none-ok:` marker, the same
  convention as `env-replace-ok:` and `swallow-ok:`. The adjacency walk those
  gates shared is now one helper.

  Verified five ways: explicit `NONE` fails, an omitted flag fails, a marker that
  is present but **not adjacent** still fails, an adjacent marker passes (so the
  gate tests the stated decision rather than forbidding routes), and removing a
  real marker fails again.


- `TestNoLambdaHasTwoImplementations` — a Go lambda directory may not also ship
  a Python entry point. Keyed on `def lambda_handler(` at the start of a line,
  not on the presence of Python: a Go lambda may legitimately ship helpers
  (`lambda/ttl-reaper/scripts/merge_deploy_params.py`), and flagging those would
  be the allowlist-of-obviously-fine-things signal that means the rule is wrong
  (rule 6). Verified four ways — it fails when the file is restored, ignores a
  Python helper, ignores a docstring that merely mentions the handler, and fails
  when its own discovery of Go lambdas breaks.


- `TestRegionConstraintConversionCarriesEveryField` compares the two constraint
  structs through their **JSON forms** — they declare the same five tags, being
  copies of each other — which catches a dropped field, a swapped pair, and
  `AllowEmpty` flipped true in one assertion, plus a field-count check for a
  field added with `json:"-"` that would otherwise be invisible.
  `TestRegionFilteringUsesThePackageWithTheTests` pins the invariant that
  actually broke: the tested implementation must be the one in use, anchored on
  the invocation since the comment documenting the gate names the function.

  Four compiling reverts checked — a dropped field, two swapped fields,
  `AllowEmpty` true (a sweep with nowhere to launch succeeding), and a
  `json:"-"` field added to one struct and not the other. The first version of
  the test walked fields with `reflect.FieldByName` and was blocked by
  semgrep's `unsafe-reflect-by-name` rule; reaching for `nosemgrep` was the easy
  move, and the replacement is simpler than what it replaced.


- `docs/self-hosted-monitoring.md` — what `deployment/prometheus/` and
  `deployment/grafana/` are, and the **two defaults that make them do nothing**
  until changed: `spored`'s metrics server is `Enabled: false` by default, and
  nothing in spawn sets the `spawn:metrics-enabled` tag that the Prometheus EC2
  service-discovery job filters on (it is read in one place and written
  nowhere). Copying the configs and seeing empty graphs gave no clue which of
  the two was missing.

- `TestEveryDeploymentSubtreeIsReferenced` — every top-level entry under
  `deployment/` must be referenced from a doc, script, workflow, Makefile or
  non-test source. Twelve files of scrape config, alert rules and dashboards
  were findable only by listing the directory, and unreferenced config is not
  read: that tree shipped a receiver for a server nobody started and a scrape
  filter on a tag nothing sets, and neither was noticed.

  It took three attempts to make this gate able to fail, which is the point of
  rule 1. Version one passed with the new doc deleted, because `CHANGELOG.md`
  was in the searched text — a changelog records that something happened, it
  does not tell a reader what a config is for. Version two still passed, because
  the gate's own file names both trees in its comments and so **vouched for
  itself**. Only version three fails on a deleted doc, fails when a tree
  references itself from inside, and fails when its own text collection breaks.


- `TestEveryCommandIsBuiltByARelease` — every `package main` in the root module
  must appear as a `main:` in `.goreleaser.yaml`. The set is discovered from the
  filesystem rather than listed, because a gate naming the commands it knew
  about would have reported a clean bill of health for exactly the one nobody
  remembered. Verified three ways: it fails when an unshipped command is
  re-added, it **passes** once a build entry is added for that command (so it
  gates the relationship, not the existence of a directory), and it fails when
  its own parse of the release config stops matching.


- `docs/dns-updater-authorization.md` — who may register a record in the
  `spore.host` zone, where the entitled set comes from, the simulated IAM grant,
  and the two live steps that no script performs.

- Four gates, each watched failing against a revert that compiles: the refusal
  without caller credentials, the client being built with them, the STS client
  carrying `callerCreds` rather than the ambient chain, and every failure path in
  the credentials handler returning a **nil** provider — because handing back a
  half-built provider from a path where validation failed would let an unverified
  caller be acted as, which is the inverse of the original bug and worse.
  Note that the last property is partly enforced by the compiler: declaring the
  provider after the first early return does not build, since those returns
  reference it. The gate covers the compiling variant, where the early returns use
  a literal `nil` and the declaration drifts later.

- **Behavioural coverage for the live half of `pkg/staging`**, which had none.
  `staging_test.go`'s five tests covered only the functions this change removed,
  so deleting it would have left `resolveSchedulesBucket` untested — the function
  #804 had just made load-bearing, since the caller now persists which bucket it
  chose. Four tests against the Substrate emulator replace them: the shared name
  is preferred, the **returned** name is a bucket that actually exists (a function
  that creates the right bucket and reports the wrong name is precisely what broke
  scheduled sweeps), the account-scoped fallback works and carries the account id,
  and two calls return the same name. Swapping dead-code coverage for live-code
  coverage is why both changes belong together.

- **Three gates on the cert paths, which is what was actually missing.** The certs
  are written by shell at boot and read by Go at runtime, and nothing connected
  the two — which is how they drifted far enough apart that one side was dead and
  nobody noticed. `TestUserDataWritesThePathsTheAgentReads` requires every path
  the agent reads to be written by `cmd/app.go`;
  `TestUserDataWritesTheDCVCertWhereDCVReadsIt` pins DCV's own private directory,
  because a plausible-but-wrong path fails by silently falling back to
  self-signed; and `TestAgentDoesNotUseTheSupersededDCVCertPath` keeps `/etc/dcv/`
  from reappearing in Go. Same class as #790's Lambda names and #804's bucket —
  two sides constructing one identifier independently — except that here one side
  is shell, so the sharing has to be an assertion rather than a shared constant.
- **`scripts/deploy-cost-history-collector.sh`** (#848), so the one live Lambda
  nothing in the repo could ship is shippable. The function is invoked hourly by
  an enabled EventBridge rule and its output is served by
  `lambda/dashboard-api`'s `GET /api/cost-history`, so it was never dead code —
  there was simply no path from `lambda/cost-history-collector/` to the function
  running since 2026-02-20, and Dependabot had spent eight months bumping a module
  that could not reach production.
  Modelled on `deploy-scheduler-handler.sh` so the existing gates apply
  automatically — verified by reverting each: removing the account assertion fails
  `TestDeployScriptsAssertTheAccount` **naming the new script**, and removing the
  `put-retention-policy` call fails the census. It asserts the runtime on every
  deploy rather than only on create (the #716 trap, where a script declared
  `provided.al2023` and redeployed for ten months while the function stayed on
  `provided.al2`), stamps `spawn:version` from `git describe --dirty` so the
  deployed bytes can be traced to a commit, builds the **package** rather than
  `main.go`, and ignores `AWS_REGION` in favour of an explicit region.
  It deliberately does **not** create the hourly EventBridge rule: the live one
  already exists, and a deploy script that also owned the schedule could silently
  change how often a cost collector runs.

- **`TestDocsDoNotCitePathsThatDoNotExist`** — a doc that names a file in
  backticks must either name one that exists or declare itself historical. The
  forcing function is the point: a doc is either current, in which case its paths
  resolve, or it is history and says so, with no third state where it quietly
  describes a repo that no longer exists. `libs/` is excluded because it is a
  separate repository, and prose like "the cmd directory" is not matched because
  it is not a claim about a specific file.

- Making the reaper's call discovery real surfaced two actions nobody had reasoned
  about, because they had always been excluded: `sts:GetCallerIdentity` and the
  two Route53 calls. They are now recorded in a `grantedElsewhere` map with a
  reason each. `sts:GetCallerIdentity` needing no IAM permission is **proven**
  rather than assumed — the live production role grants only `sts:AssumeRole`, and
  the reaper calls GetCallerIdentity on every invocation and has been succeeding
  every ten minutes.

- **`TestNoTestFabricatesAnAMIForTheEC2API`** — every `ImageId` a test hands to
  the EC2 API must come from `RegisterTestAMI`. Discovery across the tree, so a
  new test cannot reintroduce the fixture; pure-logic tests that legitimately keep
  a literal id are untouched because they never assign it to `ImageId`. It divides
  labour with the tests themselves: a malformed id fails the gate, and a
  well-formed-but-unregistered one passes the gate and then fails the test loudly,
  now that the swallowing skips are `t.Fatalf`.
- **A sixth rule in `CLAUDE.md`: gate the mechanism, not a correlate of it.**
  Three gates written during this audit had to be deleted after they ran — "no
  local flag may shadow a persistent root flag" (twenty-odd commands do so by
  convention), "was a flag value supplied?" computed from whether any later flag
  appeared on the line (forty false positives), and "no test may skip on an
  emulator failure while containing an invented AMI id" (eight). In each case the
  striking detail in the story of how the bug was found was not the load-bearing
  one, and the precise rule was narrower and duller. The signal is an allowlist of
  things that are obviously fine: a list of what is *normal* means the rule is
  wrong, which is rule 4 inside out.

- **`PipelineStatus.IsTerminal` and `.Succeeded`**, with an exhaustive switch and
  no `default`, so a status added without being classified cannot fall through to
  "keep waiting" — which would be an unbounded poll rather than a visible error.
- **`TestEveryFlagVariableIsReadSomewhere`**, which catches the #775/#807 shape
  structurally: a flag variable with only its declaration and registration in
  non-test source is accepted from the user and discarded. It reads the source
  because cobra cannot tell a flag that is read from one that is not.
  Deprecated flags are exempt — that is what deprecating one means — and the
  exemption is taken from the code's own `MarkDeprecated` calls rather than a hand
  list. Three flags surfaced on the first run (`--use-reservation`, pipeline
  `--detached`, cleanup `--force`) and all three are deliberately deprecated
  no-ops. Neither `--wait` nor `logs --region` was, which is exactly why they were
  defects rather than decisions.
- **`TestPipelineLaunchActuallyWaits`** and **`TestEveryPipelineStatusIsClassified`**,
  both of which exist because two reverts went uncaught on the first attempt.
  Reference counting passed a revert to `if flagWait && false { … }` — three
  references each, dead code — so it detects a flag nobody looks at but not one
  looked at and ignored; the wiring needs asserting separately. And `IsTerminal`'s
  comment claimed a classification gate that I had not written: removing
  `StatusCancelled` from the terminal set as a deliberate revert failed nothing.
- **`TestExampleFlagsCanBeWrittenAsDocumented`** — every flag a command documents
  without a value must accept being written that way. #806's gate checks a named
  flag *exists*; it cannot tell that the flag demands an argument the example does
  not supply, and this closes that gap by resolving against pflag's own notion of
  arity.
  Its first version computed "was a value supplied?" from whether any later flag
  appeared anywhere on the line, which reported `--stack my-stack --region x` as
  giving `--stack` no value — about forty false positives. The question is only
  ever about the immediately following token, so it walks tokens now.
- **`TestNoTwoLiveFlagsShareOneVariable`** — this is what hid #808 from #807's
  unread-flag gate: both flags bound to `validateComplianceMode`, giving it a
  declaration and two registrations, so a reference count of three made it look
  read. Shared variables also clobber each other, whichever is parsed last
  winning.
  Aliases are the legitimate case and are exempt by the code's own declaration,
  not a hand list: a deprecated flag, or one whose usage says "Alias for --x".
  The first version exempted only deprecated flags and reported `launch
  --print-config`, which is a deliberate **live** alias of `--dry-run`.
- **`TestValidateCallsTheBaselineCheck`** — because testing the validator is not
  testing that anything validates. `TestNIST53BaselineValidation` exercises
  `isNIST53Baseline` directly and kept passing against a revert to
  `if !isNIST53Baseline(val) && false`, which left the function correct and the
  handler unguarded.

- **Four gates over every user-facing string in `cmd/` and `pkg/`**, reporting
  352 printed `spawn` invocations across 18,439 literals: commands resolve,
  resolved commands are **runnable**, named flags exist, and printed paths and
  `github.com/spore-host/...` URLs exist on disk.
  Literals come from `go/ast` rather than a regex, so comments are excluded and
  both raw and interpreted strings are handled.
  The hard part is telling an invocation from prose: "from the spawn repo" is
  indistinguishable from `spawn repo` to any parser, and resolving-or-not cannot
  be the test, because `spawn instances list` not resolving is precisely the bug.
  A string is treated as an invocation when its first word is a real subcommand
  **or** it is backtick/single-quote delimited.
  Three discriminator rules were removed after they produced false positives,
  each recorded in place: parentheses (parenthesised prose is common), trailing
  quotes (an apostrophe in "the instance's lifetime" looks like a closing quote),
  and treating everything before the first flag as a command path (that reads
  positional **arguments** as subcommands, and reported `spawn cost my-devbox` as
  a nonexistent command). The rule that works mirrors cobra: descend only while
  the current command is a group and the next word names one of its subcommands.
  Also handled: printf templates (`spawn notify workspace %s` is runnable once
  substituted) and `ShorthandLookup`, which **panics** on a name longer than one
  character — that panic was the gate's first output, a stack trace instead of a
  finding.

- Gates on version selection, which is the input that decides whether verification
  happens: semantic rather than lexical ordering (`1.10.0` beats `1.9.0`, which a
  string compare gets backwards and would silently install an older plugin);
  plugin-name isolation, so `demo-extra-1.2.3` is not read as a version of `demo`
  and `install tailscale` cannot resolve to a `tailscale-exit-node` release; drafts
  and prereleases excluded; API failures reported as errors and never as "no
  releases"; and pagination followed, bounded, and asserted with a full first page
  whose newest release is on page two.

- **`pkg/sporedinstall`** — the one implementation of download-verify-install for
  `spored`, plus the compiled-in signing public key (moved from
  `pkg/launcher/spored_pubkey.go`, which is deleted). Emits a shell function that
  requires a checksum, verifies the signature when a key is compiled in, puts the
  executable bit on only after every check has passed, and installs by atomic
  rename so a running `spored` cannot cause `ETXTBSY`.
- **Gates that execute the generated shell rather than matching strings in it.**
  `pkg/sporedinstall/install_test.go` stands up an `httptest` server serving a
  genuinely signed artifact, generates a real P-256 key with Go's crypto, and runs
  the shipped script against seven hostile sources — no checksum, wrong checksum,
  empty checksum, missing signature, a signature that does not match the binary,
  a corrupt signature, and an absent binary — asserting each exits non-zero **and
  installs nothing**. This is deliberate: the gates that existed asserted things
  like `strings.Contains(script, "sha256sum")`, which a comment satisfies and a
  fail-open checksum block satisfies equally. A substring cannot distinguish
  "verifies" from "mentions verifying", nor "aborts" from "warns and continues".
  It earned its keep immediately — against a revert that restored the fail-open,
  the executable test reported `exited 0 despite no checksum published` and that
  the binary had been installed, while the string assertions still passed.
- `TestEverySporedDownloaderUsesTheVerifiedInstaller` discovers every file that
  fetches a `spored-linux-*` artifact and requires it to call the shared
  installer, so a fourth path cannot be added quietly; plus
  `TestNoCallSiteSwallowsTheInstallerFailure` (a verified installer whose exit
  status is ignored is barely better than none) and
  `TestChmodHappensAfterVerification` (ordering, not presence — a `chmod +x`
  before the checksum leaves an executable attacker-controlled file in `/tmp`
  even on the refusal path).
- Eight gates on alias tenancy, driven against the Substrate Route53 emulator
  rather than a hand mock, because what is most likely to be wrong is how real
  record names and values come back — trailing dots, TXT quoting, octal escaping —
  and a fake that returns what the code expects cannot catch any of it. Two are
  handler-level on purpose: every unit test calls `authorizeAlias` directly, so
  deleting the call from `handler` leaves them all green, which is precisely the
  shape of the original bug (`aliasDNSName` validated the slug correctly; the
  caller just never checked ownership). Against that revert the handler-level gate
  reports the attack concretely — it writes `evil.mycelium-development.spore.host`
  and says so.
- Nineteen gates across both defects, each watched failing against a revert that
  compiles. The Slack verification is driven with genuinely HMAC-signed requests
  and ten hostile variants — no headers, signature over a different body, a
  different secret, replays six minutes either side of now, a truncated
  signature, the wrong scheme version. Two gates are ordering gates rather than
  presence gates (`TestSlackRotateVerifiesBeforeRotating`,
  `TestChmodHappensAfterVerification`'s sibling in spirit), because verifying
  after the credential has been rotated is a check that cannot prevent anything.
  `TestNoOwnershipCheckIsConditionalOnTheAttributeExisting` discovers the shape
  across the whole package instead of pinning the one line that was wrong, so the
  same fail-open cannot reappear in another handler.

- **`ManagedSSMTagCondition`**, separate from the `ec2:`-keyed one. A condition
  key belongs to the service of the **action**, so reusing
  `ManagedTagCondition` on an `ssm:` action yields a statement IAM accepts and
  never authorizes — a silent implicit deny (#652/#665).
- **Two gates on `pkg/reaperiam`, both verified against compiling reverts.**
  `TestEveryDestructiveActionIsContained` requires every destructive action to be
  bounded by a condition or a resource narrower than `*`; the existing coverage
  gates assert that everything the reaper *calls* is *granted*, which is
  necessarily blind to a grant being too broad. `fsx:DeleteFileSystem` is the one
  recorded exception, with #613 behind it.
  `TestSSMActionsUseSSMConditionKeys` fails an `ssm:` statement conditioned on an
  `ec2:` key — the trap this package documents twice and had never asserted.
  The containment gate caught a mistake of mine while being written: it listed
  `ec2:DeleteTags`, which the reaper does not hold, and its own
  "if it is not granted, drop it" check refused to let the list claim coverage it
  did not have.

- **`cmd/onboard_test.go`, the file's first test.** Four gates, each verified
  against a compiling revert that restores the original defect. They parse the
  policy JSON and assert on structure rather than substrings, because a
  `Contains(policy, "spawn:managed")` would have been satisfied by the statements
  that *did* carry the condition — and the defect was precisely that the ones that
  mattered did not. The requirement is expressed as a list of instance-mutating
  **actions** rather than of statements, so a future statement inherits it.

- **`spawn launch` warns when `--ttl` exceeds the reaper's default ceiling.**
  Now that the ceiling is unconditional, a TTL longer than it is silently
  ineffective — the instance is reclaimed at the ceiling regardless of the
  deadline. The warning names the threshold and says to raise `REAPER_MAX_AGE` for
  genuinely longer-lived instances, rather than letting an operator discover it
  when an instance disappears at seven days.
- **Three gates, each verified against a compiling revert.**
  `TestEvaluate_MaxAgeCeilingAppliesEvenWithinDeadline` asserts the behaviour
  directly — restoring the early return fails it.
  `TestDefaultReaperMaxAgeMatchesTheReaper` reads the reaper's Go constant **and**
  its CloudFormation parameter default, so the ceiling value the CLI warns against
  cannot drift from the one actually deployed; `pkg/ttl` and the reaper are
  separate modules, so the constant has to be duplicated, and an uncompared
  duplicate is the #790 defect.
  `TestReaperDocsAgreeThatTheCeilingBeatsAFarFutureDeadline` requires both the
  header and the parameter description to state the far-future case, because the
  two drifting apart is how the exemption survived.

### Changed

- **Region filtering is de-duplicated onto `pkg/regions`, the copy that has
  tests** (#811). There were two implementations: `cmd/launch_regions.go` had
  `applyRegionConstraints`, `filterInclude/Exclude/GeographicRegions`,
  `matchesAnyPattern`, `matchesWildcard`, `validateRegionConstraint`,
  `containsString` and `formatConstraint` written against
  `sweep.RegionConstraint`; `pkg/regions` had the same functions against its own
  near-identical struct. The wildcard matchers were character-for-character the
  same function with different identifier names, and `ValidateConstraint` matched
  down to its error strings.

  The **live** copy was the one in `cmd/`, with **no tests at all**. The
  unreferenced one in `pkg/regions` has `filter_test.go` — 393 lines covering six
  functions. So the logic deciding which AWS regions a launch targets ran
  untested while its tested twin sat dead, and the dead-code pass that found it
  (#811) recommended deleting the tested implementation. Routing through
  `pkg/regions` removes 121 lines from `cmd/` and brings those tests to bear on a
  live path instead.

  Equivalence was proven before the swap rather than read off the page: a
  differential run over `regions.AllAWSRegions` with every combination of 9
  include patterns, 9 exclude patterns and 7 geographic groups — **567 cases, 0
  mismatches**. The only behavioural differences are on paths this caller never
  takes (`pkg/regions` guards a nil constraint and honours `AllowEmpty`;
  `shouldApplyRegionConstraints` already guarantees non-nil and non-empty).
  `sweep.RegionConstraint` stays as the persisted type.


- **`BufferingPushClient`'s doc comment, which was the actual defect here.** It
  read "records pushes in memory *instead of* delivering them to a remote
  runtime", and #810 concluded from that — across three independent audit passes —
  that "pushed plugin data never reaches an instance" and that there is a
  `spawn plugin push` command which "appears to push, succeeds, and delivers
  nothing". Neither is true. There is no such command, and buffering **is** the
  delivery mechanism: `spawn plugin install` collects what local provision steps
  push and hands the whole map to spored in the install request, over an SSH
  tunnel to `127.0.0.1:7777/v1/plugins/install`. That ordering guarantee is the
  point — delivering values one at a time, as the removed transports would, could
  not give it. The comment now says so, for the same reason #809's does: a
  description that is fair about the type and misleading about the system is what
  three readers keyed on.

- **A fourth gate dropped for checking a correlate rather than a mechanism.** The
  first version asserted, by comparing source positions, that
  `downloadParams` consults the recorded bucket before the region template. A
  revert that computed the template first and then overrode it **passed** — and
  correctly, because computing-then-overriding is equally valid. The decision is
  now a pure `paramsBucket(recorded, region)` with a behavioural table test beside
  it, and the note replacing the old assertion says why. Rule 6, for the fourth
  time in this audit.
- Two matchers in the new gate were tightened after a revert slipped past them:
  `dynamodbav:"s3_params_bucket` is a **prefix** of
  `dynamodbav:"s3_params_bucket_unused"`, so renaming the field passed. Rule 3
  applies to a gate's own matcher as much as to the code it guards.

- **A gate removed after it ran.** The first version of `cmd/inert_flags_test.go`
  required that no local flag shadow a persistent root flag, on the theory that
  `logs --region` broke because the local flag won the binding. It reported more
  than twenty commands — `launch`, `list`, `doctor`, every `array` subcommand —
  declaring their own `--region`. That is the repo's convention, not a defect, and
  keeping the gate would have meant an allowlist of twenty entries each asserting
  that ordinary code is an exception: rule 4's anti-pattern inside out, where the
  list is what is normal and the gate is what is wrong. The reasoning is recorded
  in the file in place of the test. The actual defect was that the flag was never
  read, which the gate above catches without any reference to shadowing.

- **A second test that certified a defect has been inverted.**
  `TestCompareFedRAMPLevels` required the FedRAMP comparison text to contain
  `--fedramp`, a flag that exists nowhere in the CLI — so it mandated that the
  tool keep advertising something a user cannot type, and would have failed on
  the fix. It now requires the baseline flag that does exist, and fails if
  `--fedramp` reappears. That is the second instance of CLAUDE.md rule 5 in this
  batch, after #816's.

- **A test that asserted the vulnerability was correct behaviour has been
  inverted.** `TestResolveOfficialBare_NoManifestFetch` required that a bare
  official ref resolve successfully, not set `ManifestVerified`, and *never contact
  the release endpoint* — it failed the test if verification was attempted. That was
  an accurate description of the code and a false description of the design, and
  because it was written from the code it would have failed on the fix. It is now
  `TestResolveOfficialBare_ResolvesToLatestReleaseAndVerifies` and asserts the
  opposite, with the reason recorded in place (CLAUDE.md rule 5).
- Resolving an official ref from the default branch is now reachable only under
  `--insecure`. Three provenance tests that legitimately exercise that path use an
  explicitly-named `newInsecureTestResolver` rather than quietly relying on the old
  default, and the spec-path test now pins the **tagged** URL for a bare name, since
  fetching at the tag is the mechanism by which verification is reached.

- `sporedDCVRolePolicy` is now a `var` built with `fmt.Sprintf` rather than a
  `const`, so the control-tag deny renders from the same `reaperControlTagKeys`
  slice the dynamic policy uses and the two cannot drift on it. Unexported, so no
  public surface changes.

- **`spawn onboard --spored-profile` is deprecated in favour of `--spored-role`.**
  `iam:PassRole` names a role, not an instance profile. The old flag remains as an
  alias bound to the same variable, so existing invocations keep working — and
  now actually work, since the value is finally used as a role name.

- **The Lambda-name gate now keys on the property, not on how the constants
  happened to be spelled.** The version added for #790 required the identifier to
  end in `Function` and required a literal after `:function:`. Both describe the
  constants in `pkg/infrastructure`, not the property being gated — so it missed
  #812 twice: `defaultOrchestratorFuncName` ends in `FuncName`, and the name
  reaches AWS through `:function:%s` from a variable. Demonstrated rather than
  asserted: against the shipped bug the old pattern does not match and the new one
  does.
  The widened pattern is deliberately loose, so a second test pins the population
  it may match — exactly five non-test identifiers repo-wide, each with its
  expected value. A sixth now fails with instructions to narrow the pattern rather
  than grow the list, because a measurement nothing re-checks is rule 4's
  hand-maintained list one level up.

- **A test that asserted the old behaviour was replaced, not deleted.**
  `TestEvaluate_WithinDeadline_Spared` used a 30-day-old instance with a
  two-hour-future deadline and asserted it was spared. It was right about the
  code and wrong about the design, and it is why the gap lasted: a test that
  encodes observed behaviour cannot tell you the behaviour is wrong. It is now
  two tests — one for within-deadline-and-within-ceiling (still spared), one for
  the forged far-future case (reaped, reason `max-age`).

- **The docs gate asserts set equality in both directions** (#795). Pruning
  alone fixes one instance; an assertion that the fragment set and the command
  set agree cannot pass while they disagree, whatever the cause — a file deleted
  by hand, a command added without regenerating, or a generator that writes
  somewhere new. The second direction (a command with no committed fragment) was
  previously caught only incidentally, by `git diff` noticing an untracked file,
  which says nothing if the file was never generated.
  Pruning also refuses when the generated set is implausibly small, because a
  broken tree walk or an i18n failure would otherwise delete the entire published
  reference and `git diff` would report it faithfully but far too late — the same
  rule as `scripts/nested-modules.sh` refusing to pass on an empty discovery.

- **The deploy-script gates discover their targets instead of listing them**
  (#799). Each of four gates had hardcoded the same two filenames, so six
  deploying scripts were unguarded — including one in the very directory being
  scanned, which a `scripts/deploy-*.sh` glob missed because of its name.
  Discovery is now by what a script **does** (`aws lambda create-function`,
  `cloudformation deploy`, …) anywhere in the tree, and fails when it finds
  nothing rather than passing vacuously.
  Two refinements came out of widening it, both recorded in the code: the region
  gate now applies only to Lambda deploys, because a deliberately multi-region
  stack script is not the hazard it was written for; and legitimate `|| true`
  uses carry an explicit `# swallow-ok: <reason>` marker — a poll loop and an
  idempotent cleanup — rather than the gate being weakened to accept them.
- **The account gate now matches the assignment and the comparison**, not the
  bare identifier (#799). Renaming the assignment used to **pass**, because the
  script's own error message keeps the substring `EXPECTED_ACCOUNT` alive. Either
  half alone asserts nothing: an assignment never compared is dead, and a
  comparison against an unset variable is always false.

### Removed

- **`scripts/deploy-custom-dns.sh` no longer creates an API Gateway** (#870).
  Steps 5 and 6 built a REST API — `spawn-dns-api`, `POST /update-dns` at
  `--authorization-type NONE` — and added an `apigateway-invoke` permission to
  the function: **a public, unauthenticated endpoint into the DNS updater**,
  created on every run.

  It also could not work. The handler takes a `LambdaFunctionURLRequest`, while
  API Gateway REST sends an `APIGatewayProxyRequest` and expects a matching
  response shape, so a POST returned **502** rather than a handler response —
  verified against the resource the script created on 2026-10-12 before it was
  deleted again. It wrote no DNS, but it did invoke a billable function and was
  one handler-signature change away from being live.

  The section predated #173, which moved the updater to a Function URL with
  `AuthType: AWS_IAM`. Nothing consumed it: neither `/spawn/dns/*` SSM parameter
  was set, and spored registers through the Function URL.

  The script now reads the **Function URL** off the function and prints it,
  warning if its `AuthType` is not `AWS_IAM` — since the updater namespaces every
  record under the SigV4-verified caller, anything else leaves records unanchored.
  It also no longer tells the operator to publish `/spawn/dns/api_endpoint`,
  because the CLI path consuming that value is broken independently (#872).


- **`spawn launch --proximity-from` and `--cost-tier` are withdrawn** (#864),
  together with `pkg/regions/priority.go` (152 lines: `ScoreRegions`,
  `GetRegionsByPriority`, haversine distance and cost-tier scoring) and the
  `ProximityFrom`/`CostTier` fields on the region-constraint types and their
  validation.

  **This is a user-visible flag removal, so MINOR.** Both flags were accepted,
  validated and persisted, and never affected region selection — nothing read
  them. Withdrawn rather than finished, because each would have duplicated a
  live mechanism with a worse one:

  - `--cost-tier` would have ranked regions by a hand-assigned three-bucket
    table with no provenance and no collection date. truffle is this suite's
    pricing authority (#533) and already sorts by live on-demand price; a static
    table of that shape was measured wrong by up to 38x for a type it did not
    know (#543), and this project requires a static price reaching output to say
    that it is static, why, and when it was collected.
  - `--proximity-from` would have ordered regions by great-circle distance
    between hardcoded coordinates. `detectBestRegion` already selects by
    **measured TCP latency** to each region's EC2 endpoint over the SCP-aware
    enabled-region set (#732) — a better answer to the same question, and live.

  No migration is needed: no documentation, example or issue referenced either
  flag, and **zero of the 23 live sweep records carry `proximity_from` or
  `cost_tier`** (checked). `RegionMetadata.CostTier` and `Coordinates` are kept
  as reference data with a comment recording why they have no consumer, so a
  future reachability pass gets "why isn't it used" answered rather than
  rediscovering the question.


- **`pkg/compliance`'s report generator and standalone validator wrappers** —
  **1,852 lines** (746 non-test, 1,106 test), taking the package from 3,782
  lines to 1,930. There is no `spawn compliance` command and never was.

  - `report.go` + `report_test.go`: all seven exported functions
    (`GenerateComplianceReport`, `GenerateInstanceReport`,
    `GenerateSummaryReport`, `FormatReportJSON`, `FormatReportText`,
    `CompareBaselinesReport`, `CompareFedRAMPReport`) had **zero** callers
    outside the file and its own test.
  - thirteen standalone `Validate*Compliance` / `Enforce*Compliance` /
    `Validate*Instance` / `Get*Description` / `Compare*` functions across
    `nist80053.go`, `nist80171.go` and `fedramp.go`, plus the eleven test
    functions that covered only them.

  **The validator is untouched.** `NewValidator` and the four methods `cmd/`
  actually calls — `ValidateLaunchConfig`, `EnforceLaunchConfig` and
  `IsStrictMode` from `cmd/launch_single.go`, `ValidateInstances` from
  `cmd/validate.go` — and the three `*ControlSet` constructors they dispatch to
  all remain, as do every test of them. `ControlViolation.Error` is deliberately
  untouched: it is what makes the type satisfy `error`.

  The removed wrappers were **not** a second implementation, which is what
  distinguishes this from the `pkg/regions` case. Each was two lines over the
  live path — `controlSet := NIST80053ControlSet(baseline); return
  controlSet.ValidateLaunchConfig(cfg), nil` — and the live path is the
  better-covered one (10 test references against 1 for the wrapper), so nothing
  unique was lost. Checked before deleting, precisely because the `pkg/regions`
  finding an hour earlier was the opposite.


- **`lambda/dns-updater/handler.py`** — 472 lines of a complete Python
  implementation of a function deployed as Go. The live function is
  `provided.al2023` with handler `bootstrap`, read off the function itself, and
  nothing in the repo ever packaged the Python.

  The language mismatch was the smaller half. Its docstring described its
  security model as "Validates AWS-signed instance identity document" — the
  PKCS#7 identity-document scheme that #173 **replaced** with SigV4, and which
  `main.go` records as removed. It also hardcoded the production hosted zone ID.
  So a reader who found it had a plausible, deployable implementation of a
  superseded authentication design, for the one function whose entire security
  story is that its caller is SigV4-verified.


- **`pkg/observability/alerting`** — 234 non-test lines plus 291 of tests: an
  Alertmanager webhook bridge with a payload parser, alert formatting and two
  sender implementations, and **zero importers of any kind**. Nothing called
  `Bridge.Start`, so the receiver that
  `deployment/prometheus/alertmanager.yaml` posted to never existed at runtime
  and alerts routed to it went nowhere.

  Only the bridge. `pkg/observability/metrics` and `pkg/observability/tracing`
  are live (`cmd/spored/main.go`), and the `AlertingConfig` struct stays — it
  carries Prometheus/Alertmanager URLs for `pkg/provider/local.go`. #811
  proposed removing `deployment/prometheus/` and `deployment/grafana/` as "the
  config half" of this package; that is wrong, and they are kept — see below.


- **`cmd/orchestrator` and `pkg/orchestrator`** — 679 non-test lines plus 654 of
  tests, for a **second binary no release has ever produced**.
  `.goreleaser.yaml` builds `spawn` and `spored` and nothing else, so
  `spawn-orchestrator` could not be installed by any supported route; no sibling
  repo imported the package and it was not part of `api/public-surface.txt`.
  `deployment/spawn-orchestrator.service` goes with it — its `ExecStart` named
  `/usr/local/bin/spawn-orchestrator`, which nothing writes, and passed
  `/etc/spawn/orchestrator-config.yaml`, which the repo never contained.

  Nothing was broken by this, which is the point: it compiled, it vetted, it had
  more test lines than most live packages, and `make check` was green on all of
  it for as long as it existed.


- **`spawn stage`** (#805), with its three subcommands and ~1,600 lines. It
  uploaded to a bucket it never created, **nothing ever downloaded the staged
  data**, and no staging bucket exists in the account — so it had never run, while
  publishing "Savings: $85.70 (93% reduction)" to anyone who typed
  `spawn stage estimate`. That figure was the most concrete thing the feature
  produced.
  `pkg/staging/cost.go` and its tests went with it: the model is entirely
  staging-specific ("Data Staging Cost Comparison", `EstimateStagingCost`) and has
  nothing to estimate once the feature is gone. Recoverable from history if the
  idea is ever revived.
  **`pkg/staging` is NOT removed.** `NewClient`, `resolveSchedulesBucket`,
  `ensureBucket` and `UploadScheduleParams` are live on the `spawn schedule` path —
  three `spawn-schedules-*` buckets exist — so only the seven stage-only functions
  went, each verified to have zero callers outside the command first.
  `examples/README.md`'s staging quickstart said to run it; it now says what to do
  instead (`aws s3 cp`, or an FSx data-repository association via
  `--fsx-import-path`).
- **`pkg/plugin/push_ssh.go` and `push_ssm.go`** (#810) — two `PushClient`
  implementations, 179 lines, that nothing ever constructed. The SSH one
  duplicated the tunnel and token helpers `cmd/plugin.go` already has and uses.

- **`installDCVCert` and its `s3GetFile` helper** (#809). Dead — the definition
  and its own doc comment were the only references in the tree, across all three
  `GOOS` values — and dead because they had been **superseded**, not forgotten.
  They fetched the wildcard cert into `/etc/dcv/`, injected `tls-certificate` into
  `/etc/dcv/dcv.conf` and restarted `dcvserver`. `cmd/app.go`'s DCV user-data has
  since done this at boot, writing
  `/var/lib/dcv/.config/NICE/dcv/private/dcv.{pem,key}` — the path DCV actually
  reads — and `webready.go`'s own comment records that arrangement.
  The audit's reading of this, "DCV sessions always use a self-signed cert and
  `spawn-certs` is a read-only bucket with no live end", was wrong in both halves:
  the live reader is an `aws s3 cp` in generated shell, which a Go-only search
  cannot see. Wiring the function up would have been worse than leaving it — the
  wrong destination path, a `dcvserver` restart that drops live sessions, and two
  writers fighting over the same cert state. A comment where they were records
  this, so the next reader does not re-derive the same wrong conclusion from the
  same Go-only evidence.

- **`spawn autoscale` and its orchestrators are gone** (#772).
  **Breaking: 13 subcommands removed, so the next release bumps MINOR.**
  `health`, `launch`, `list`, `metric-activity`, `pause`, `resume`,
  `scaling-activity`, `schedule`, `set-metric-policy`, `set-scaling-policy`,
  `status`, `terminate`, `update` no longer exist, nor do the
  `/api/autoscale-groups*` dashboard endpoints.
  The production orchestrator had reconciled on `rate(1 minute)` since
  2026-02-14 — roughly 350,000 invocations — against a table that **has never
  held a record**. Both groups tables measured `Count=0` at deletion, so no user
  loses working behaviour.
  Removed from AWS as well: both `spawn-autoscale-orchestrator-*`
  CloudFormation stacks (two functions, two roles, two rules, two log groups and
  the two empty tables) and the `spawn-autoscale-ec2-role` stack in the compute
  account. Everything was CloudFormation-managed, including that role — deleting
  it directly would have caused stack drift and it would have returned. The
  control plane is **90 resources, down from 100**, re-measured with
  `spawn footprint` rather than decremented by hand.
  The EC2 role is the reason this is a removal rather than a tightening: it held
  `ec2:RunInstances`/`TerminateInstances`/`CreateTags` on `*` and `iam:PassRole`
  on `*` constrained only by `iam:PassedToService`, which constrains the service
  and not the role — so the holder could launch an instance carrying any
  EC2-assumable role in the account and read its credentials. Trust was the whole
  infra account, gated by an `sts:ExternalId` published in the template. A
  deployed, never-used privilege-escalation primitive is better deleted than
  narrowed.
- **`pkg/aws`'s autoscale coverage API is removed** — `AutoscaleCoverage`,
  `DetectAutoscaleCoverage`, `AutoscaleCoverageAdvice`, `AutoscaleFunctionName`
  (13 exported members). `pkg/aws` is a cross-repo package, so this is a wire
  contract change and **contributes to the MINOR bump**. It was added in
  #774/#775 earlier the same day and adopted by nobody — verified against local
  checkouts of all three consumers and an org-wide code search, both zero. It
  existed to warn when an autoscale orchestrator was running without coverage,
  which is not a question that can be asked any more.

### Fixed

- **The reaper Makefile's computed `VERSION` now actually reaches
  CloudFormation** (#869). `VERSION?=$(shell git describe …)` looked like
  automatic stamping, but a `?=` assignment has origin `file`, and
  `EMIT_EXPLICIT` only emits parameters whose `$(origin)` is `command line` or
  `environment` — so the value was computed and silently discarded, and the stack
  fell back to the template default. The production stack carried **no `Version`
  parameter at all** from 2026-06-08 until 2026-10-12, while the Makefile
  appeared to set it, defeating the point of #768.

  Fixed by distinguishing two classes of parameter rather than emitting
  everything, because **the obvious fix disarms the reaper**: `DRY_RUN` defaults
  to `true` here while production runs `false`, so asserting `?=` defaults is
  spawn#650 — which has already been reintroduced once by a fix for itself.

  - **always-assert** (`ALWAYS_ASSERT` = `Version`, `LogRetentionDays`): the repo
    owns the value, so it is sent on every deploy.
  - **live-wins** (everything else): unchanged, still origin-gated, so an
    unspecified parameter inherits what is deployed.

  Always-asserted entries are emitted *before* the origin-gated ones, because
  `merge_deploy_params.py` reads the file line by line into a dict — so the last
  line for a key wins and `make deploy VERSION=x` still beats the default.

- **The version fallback never fell back.** `git describe … | sed 's/^v//' ||
  echo unknown` takes its exit status from `sed`, which succeeds on empty input,
  so outside a git repository `VERSION` evaluated to the **empty string**, not
  `unknown` — confirmed by running the Makefile in a non-repo directory. Harmless
  while the value was being dropped; with Version now asserted on every deploy it
  would have stamped an empty string, which is worse than the template default it
  replaced. It now captures first and defaults with `${v:-unknown}`.


- `sweepFlagCoverage` classified both withdrawn flags as `sweepHonoured` —
  asserting that the sweep path honoured flags which did nothing. The gate over
  that table is sound and caught the removal immediately in the other direction
  (it fails when the list names a flag `launchCmd` no longer registers); it is
  the hand-written *classification* that was wrong.


- Deleting dead code **cascades**, and the second pass caught a wrong deletion.
  Removing the thirteen wrappers orphaned four more functions that had only ever
  been reachable through them. Three were then removed; the fourth,
  `GetBaselineControlCount`, was **restored** — it has no production caller, but
  `TestFedRAMPControlSet`, a test of the live path, uses it to assert that a
  FedRAMP control set holds the same number of controls as the NIST baseline it
  maps to. Deleting it would have forced that assertion to be weakened or the
  count reimplemented in the test. "Dead outside tests" is not the same as "safe
  to delete" when a live test needs it to say what it means, and the reason is now
  recorded at the function.


- Two stale citations in `tracked_binaries_test.go`. One justified the gate's
  scoping with "lambda/github-oauth is Python (handler.py)" — **there is no such
  directory**; the function that name refers to is one
  `scripts/lambda-runtimes.txt` records as deployed since 2026-01 and present in
  no repo at all. The scoping is right, the example was a phantom. The other
  cited `handler.py` as an example of a text file `file` calls "executable",
  which this change deletes; it now names a Python helper that still exists.


- **`--proximity-from` and `--cost-tier` claimed to do something they do not.**
  Both are accepted on `spawn launch`, **validated** (an invalid cost tier or
  proximity region is rejected, so they feel load-bearing) and persisted to the
  sweep record — but nothing reads them when regions are selected. Their only
  consumers, `regions.ScoreRegions` and `GetRegionsByPriority`, have no callers.
  And because either flag alone satisfies `shouldApplyRegionConstraints`,
  `--cost-tier low` on its own printed `Applied region constraints: N regions
  allowed` with N = every region and nothing filtered.

  The flags are unchanged for now — whether to implement the ordering or
  withdraw them is a product decision, tracked in #864 — but the help text and
  the sweep output now say the values are recorded and not applied, instead of
  implying an effect that does not happen. `pkg/regions/priority.go` is retained
  for the same reason: it is unreferenced, but it is the only implementation of
  what those two flags promise, and deleting it would turn a wiring gap into a
  design decision taken by accident.


- `deployment/prometheus/alertmanager.yaml` pointed at
  `http://localhost:8080/alertmanager/webhook`, a receiver spawn shipped code
  for but never started. It now carries a placeholder and says plainly that the
  receiver is yours to provide, with the two opt-in requirements stated at the
  top of the file rather than only in a doc.


- Two comments that cited things which were not true, both exposed by removing
  the orchestrator:

  - `pkg/taskpool/exec.go` justified its `0o700` workspace mode with "spored is
    `User=root` (`deployment/spawn-orchestrator.service`)" — a unit file for a
    *different* binary, and one no release produced. The conclusion holds, but
    for a different reason: the unit `pkg/launcher/bootstrap.go` actually writes
    sets no `User=` at all, so root comes from systemd's default. Now cites that.
  - the generated `spored.service` said SigV4 DNS signing was "harmless today
    (the DNS Function URL is still `AuthType: NONE`, which ignores the
    signature)". The cutover landed: the live function is `AuthType: AWS_IAM`
    (verified 2026-10-11), so an unsigned request is rejected at the URL and the
    instance never registers. The variable is load-bearing, and the comment
    described it as optional.


- **`spawn launch --dry-run` never mentioned `--cost-limit`** (#811). The preview
  printed `Max cost: ~$X (rate × ttl)` — compute only — and said nothing about
  the cap, so the authoritative-looking cost line sat exactly where a reader
  would expect to find it. `-o json` omitted it too, so a wrapper reading the
  plan could not see the ceiling governing the launch it was about to make. The
  cap and its scope now print beside that number, `Max cost:` says "compute
  only" out loud since the two lines mean different things, and the plan carries
  `cost_limit_usd`. `--estimate-only` states it as well, that being the other
  "what would this cost me" surface.

  The cause is worth recording: `costLimitScopeNote` existed for precisely this,
  with a doc comment saying it was "used in the launch preview", and its only
  callers were its own tests. `TestCostLimitScopeNoteStatesTheSplit` opened "the
  number is shown in the launch preview". Neither was true — so the note never
  printed and a green test was counted as coverage for behaviour that did not
  happen. The test was written from the comment, which is right; nobody checked
  that the comment was.

  Gated in four places, each watched failing against a revert that compiles: the
  render call removed, the field not populated from the config, the JSON tag
  dropped, and `omitempty` removed so an unset cap serializes as a `$0` ceiling.
  The populate-the-field revert initially **passed** the render gate, which is
  why it has a gate of its own: with the call present but the field always zero,
  the preview is silent about a cap that is in force, and the symptom is
  identical.


- **`scripts/deploy-custom-dns.sh` was unsafe to run, which is why
  `spawn-dns-updater` had not been deployed since 2026-06-25.** Three separate
  defects, each of which reported success:

  - it created and granted IAM permissions to `SpawnDNSLambdaRole`, a role that
    **does not exist** in the infra account. The function runs under
    `SpawnDNSLambdaExecutionRole`, so every grant the script made went to a role
    nothing used — a permission added here for a new feature would simply not
    have taken effect;
  - its update path passed `--environment Variables={HOSTED_ZONE_ID=...,DOMAIN=...}`,
    and `--environment` replaces the entire map, so running it would have deleted
    `DOMAIN_ZONES` — the only variable the live function has, and the one the
    handler prefers — while writing `DOMAIN`, which the handler never reads (it
    reads `DEFAULT_DOMAIN`). The environment is now merged into the deployed one,
    key by key for `DOMAIN_ZONES` so deploying one domain cannot unregister
    others;
  - it parsed `--region`, printed it, and then ignored it, because most of its
    `aws` calls carried no `--region`. The region is now exported, which fixes
    every call including ones added later.

  It also now asserts `--runtime` on the update path, not just on create:
  `update-function-code` does not change the runtime, which is how
  scheduler-handler sat on `provided.al2` for nine months (#716).

- `TestDeployScriptsDoNotWipeTheEnvironment` matched only the literal
  `Variables={}` — the empty map. That was the anecdote rather than the
  mechanism, and the gate passed `deploy-custom-dns.sh`'s *non-empty* literal,
  which is equally destructive. Generalized to any literal map on an update path,
  which found the same latent bug in two more scripts
  (`deploy-scheduler-handler.sh` and the dashboard websocket processor's
  `deploy.sh`, both verified against their live functions as displacing nothing
  today) and now requires a deliberate replacement to be stated with an adjacent
  `env-replace-ok:` comment, in the style of the existing `swallow-ok:` marker.
  Adjacency is enforced by walking up from the call, so a marker elsewhere in the
  file licenses nothing. It also could not see the marker at first: the shared
  `read()` helper strips comments on purpose (rule 3), so this one gate reads the
  raw file.


- **The dashboard's cross-account EC2 reads have never worked, and now do —
  with no IAM change at all** (#815). The member-account role's trust names
  `Cognito_SpawnDashboard_Auth_Role`, the identity-pool role the browser assumes,
  and that role already carries `sts:AssumeRole` scoped to exactly the one role
  ARN. **Both halves of the design were deployed and correct.** Only the code was
  wrong: `main.go` built one `aws.Config` from this Lambda's own credentials and
  passed it to every handler, so the AssumeRole was attributed to the Lambda.
  Measured with `simulate-principal-policy`:

  ```
  Cognito_SpawnDashboard_Auth_Role -> allowed
  SpawnDashboardLambdaRole         -> implicitDeny
  ```

  The caller's credentials — already validated against STS by
  `getUserFromCredentialsHeader`, then discarded — are now threaded through to
  `getEC2ClientForRegion`, which builds the STS client with them. That one line is
  the fix.

  **The obvious repair was rejected deliberately.** Adding the Lambda role to the
  trust and granting it `sts:AssumeRole` would create a standing role able to read
  **every** onboarded tenant's instances, assumed by a Lambda behind a public API,
  growing in reach with each account onboarded — the shape of
  GHSA-qc2h-6qqf-wcm4's fail-open ownership check and GHSA-7pm4-cqjg-vh2q's open
  front door, both fixed earlier in this audit. Acting as the caller inverts it:
  each user reads their own account with their own permissions, and this Lambda
  holds no cross-tenant access.

  A request that authenticated **without** usable credentials (the API Gateway IAM
  path identifies a caller but yields nothing to act with) now gets a named
  refusal rather than a silent fall-through to the ambient credential chain, which
  is this Lambda's role. The role session name carries the caller too, so
  CloudTrail in the member account shows which dashboard user read it rather than
  one shared identity.

- **Scheduled executions read a bucket they had never written to** (#804).
  `resolveSchedulesBucket` prefers the shared `spawn-schedules-<region>` and falls
  back to an account-scoped `spawn-schedules-<account>-<region>` when the shared
  name is taken by another account — and it **returns** which one it used.
  `cmd/schedule.go` discarded that, `ScheduleRecord` had no bucket field, and
  `lambda/scheduler-handler` rebuilt the name from the region alone. So in any
  account that took the fallback path, **every scheduled sweep failed** on a
  missing object. The record now carries `s3_params_bucket` and the handler
  prefers it, falling back to the region template only for records written before
  the field existed — which is right for those, since an account on the fallback
  path would already have been failing.
- **`spawn queue results` looked in the wrong bucket** (same issue). It derived
  `spawn-results-<region>` directly beneath a comment saying it "should match what
  was specified in the queue config", which it never loaded, while the writer
  uploads to the queue JSON's `result_s3_bucket`. It happened to work for the
  wizard default and the bundled examples, and not for
  `pkg/queue/templates/*.json`'s `{{S3_BUCKET}}`, for any other bucket a user
  names, or for the integration fixtures. It now takes `--config` (reading
  `result_s3_bucket`/`result_s3_prefix`) or `--bucket`, and **announces** the
  region-derived guess when given neither — guessing wrong produces an empty
  listing, which is indistinguishable from the queue having produced nothing.
  A `--config` that declares no bucket is an error rather than a silent fall back
  to the guess.

- **A 42 MB binary was committed by `git add -A` while writing this very PR**, and
  `TestNoTrackedCompiledBinaries` caught it in CI rather than locally. A
  single-package `go build ./lambda/<name>/` writes the executable into the
  **current** directory rather than next to its source, so the long-standing
  `lambda/<name>/<name>` ignore entries did not cover the repo root.
  `.gitignore` now covers both paths, and `TestEveryGoLambdaBinaryIsIgnored` —
  which already checked the per-directory half — checks the root half too, scoped
  to lambdas in the **root module**, since one with its own `go.mod` cannot be
  built from the repo root at all. Requiring it for all twelve was the first
  version, and that is rule 6's mistake inside the fix for rule 4: gating the set
  of lambda names, a correlate, rather than the set that can produce the binary.

- **Four gates reported a clean bill of health for sets they only partly
  covered** (#845) — the hand-maintained-list class, rule 4.
  - `templateSummary` in the reaper's deploy tests claimed "the 16 parameters the
    template declares". It declares **19**: `NetResources`, `Version` and
    `LogRetentionDays` had been added and the fixture never updated, so the
    merge-params machinery — whose job is to stop a deploy resetting a live
    parameter, the thing that disarmed the production reaper once — was never
    exercised for three of them, and `NetResources` is the staged **destructive**
    sweep (#685). Two assertions in the same file also said "want all 16
    declared". All three now derive from `template.yaml`.
  - **`apiServicePrefix` omitted route53, so a whole service was invisible to its
    own coverage gate.** `pkg/reaperiam` asserts that every AWS action the reaper
    calls is granted, and both halves of the discovery were anchored on a
    four-entry list (`ec2|fsx|ssm` in the regex, four keys in the map). The reaper
    has a `route53API` interface and calls two Route53 actions, so "every action
    the reaper calls" silently excluded a service. The service names are
    discovered now and an **unmapped** one fails rather than resolving to `""`.
    No live defect behind it — the SAM template grants Route53 scoped to the zone,
    the live role has it, and `spawn reaper deploy` exposes no DNS flags — but the
    gate would not have said either way.
  - The reaper's four sentinels were enumerated by hand in **two** gates, so a
    fifth would have had no spelling pin and no metric-filter requirement. They
    are discovered from `failures.go` now, with a floor, which also folds the
    rename case into the same check: a renamed constant is a literal the template
    no longer contains.
  - `TestEveryTemplateParameterIsInThePARAMMAP` matched the CFN key half of a
    `CfnKey:MAKE_VAR` pair and never the make-variable, so `DryRun:DRYRUN` passed
    while `make deploy DRY_RUN=true` was silently ignored — the exact failure the
    PARAM_MAP exists to prevent. It parses the pairs now and checks the
    make-variable against the SCREAMING_SNAKE convention, with `Environment:ENV`
    as the one documented departure.

- **Eleven tests reported coverage they did not have** (#843). They skipped, and
  every skip blamed the emulator — "substrate RunInstances unavailable",
  "substrate does not fully model the launch path" — each a true description of
  what happened and a false diagnosis. The ids they passed to `RunInstances`
  (`ami-base`, `ami-dcv`, `ami-x86-standard`) were invented; substrate has
  validated image ids since v0.120.0 and ships with none, and
  `testutil.RegisterTestAMI` already existed and was already used by five other
  tests. So the fixture was the defect and the skip hid it. A skip counts as a
  pass.
  Restored: three `DeleteAMI` tests covering **irreversible snapshot deletion**
  (including "retains a snapshot shared with another AMI" — an un-deleted
  snapshot is an EBS cost leak), three `WaitForSSMOnline` tests including
  `NoProfileIsDeadNotSlow`, two `spawn app launch` readiness-poll tests, two
  `verifySporedReady` tests, and `Provision` end to end — the headless path
  lagotto's capacity-poller takes (lagotto#19).
  Repo-wide skips: **14 → 3**. The three that remain name a genuine external
  requirement and the test that covers it instead.
- **A destructive-action guard that skipped instead of failing.**
  `TestMountDirNeverChownsAnExistingDirectory` exists to stop `chown` stripping
  `/tmp`'s sticky 1777 ownership. Moving the chown out of its `if [ ! -d ]` guard
  leaves the guard in place — the `mkdir` still needs it — so the guarded block
  contains no chown and the test reported "no chown emitted for /tmp at all —
  even safer than required" while the script chowned `/tmp` unconditionally.
  Verified by doing it. It now requires the chown to exist *and* to be inside the
  guard.

- **`spawn pipeline launch --wait` never waited** (#807). The flag had exactly two
  references in the tree — its declaration and its registration — while the launch
  path invokes the orchestrator with `InvocationType: Event` and returns. So
  `spawn pipeline launch p.json --wait && ./collect-results.sh` exited 0 at once
  and ran the next step against a pipeline that had not started, which is the
  precise thing the flag exists to prevent. It now polls until the pipeline is
  terminal and **exits non-zero unless it succeeded** — the second half is what
  makes the flag worth having, since a version that merely blocked would still let
  that `&&` succeed after a FAILED run. A transient read failure or a
  not-yet-written record is retried rather than reported as failure (the
  orchestrator writes the record asynchronously, so "not found" is normal right
  after launch), and `--wait-timeout` bounds the whole thing at 24h, because an
  unbounded wait on a wedged pipeline hangs CI forever — a different failure from
  the one being fixed but no better.
- **`spawn logs --region` was dropped, and shadowed the working global flag**
  (#807). The local flag had the same two-reference signature, and a local flag
  wins the binding — so `spawn logs x --region us-west-2` bound to the dead local
  one, the value never reached `SetSharedFlags`, and there was no working
  `--region` on that command at all. Removing the local flag lets the persistent
  root flag apply, as it does elsewhere.
- **`spawn validate --nist-800-171 -o json` silently printed a table** (#808).
  Both compliance flags were registered as `StringVar`, so pflag **required** a
  value — while the mode was decided by `Changed()` and the value never read.
  `--nist-800-171` has nothing to supply, so it ate the next token: `-o` became
  the flag's value, `json` became a positional, and the command printed a human
  table and exited 0. Three things failed as a result, all of them the tool's own
  words: the command's documented Example `spawn validate --nist-800-171`, the
  same invocation in its `-o json` Example, and the remedy its error message
  prints ("use --nist-800-171 or --nist-800-53=<low|moderate|high>").
  `--nist-800-171` is now a bool. `--nist-800-53` keeps its baseline but gains
  `NoOptDefVal = "low"`, so a bare `--nist-800-53` means what the handler already
  assumed when the value was empty.
- **An invalid `--nist-800-53` baseline is rejected at the flag**, naming the
  accepted values. It used to surface twelve frames down as
  `unknown compliance mode: nist-800-53-nonsense` — an internal string rather than
  the flag the user typed or the values it takes.
- **Passing both frameworks is refused** instead of 800-171 silently winning. A
  caller who passed both asked for something the command cannot do, and quietly
  doing half of it is the failure mode this issue is about.

- **The tool printed instructions that do not work, and the gate that was
  supposed to catch that covered one function** (#806). #790 fixed this for
  `spawn validate --infrastructure`'s recommendations; the gate I added then
  looked only at `pkg/infrastructure`, which is the same mistake in a new place —
  a gate scoped to where the author happened to be looking. Generalising it to
  every string literal in `cmd/` and `pkg/` found everything #806 reported **plus
  three more it had not**:
  - `spawn instances list` on the cost-leak path (`cmd/launch_jobarray.go`), the
    one instruction given when a failed cohort may still be billing. There is no
    `instances` command; it is `spawn list`.
  - `spawn fsx create` (`pkg/launcher/provision.go`) — the insidious one, because
    cobra prints group help and **exits 0** for an unknown subcommand of a group
    with no `RunE`, so a script following the advice reports success having
    created nothing. Durable FSx is `spawn launch --fsx-create
    --fsx-lifecycle=durable --fsx-ttl=<duration>`.
  - `spawn fsx`'s own summary advertised `info` and `export`; the subcommands are
    `list`, `show`, `delete`, and export is `delete --export-first`.
  - **New:** `spawn status --job-array-id` (`cmd/burst.go`) printed as "Monitor
    with:" — no such flag; it is `spawn array status <name>`.
  - **New:** the `spawn notify` examples used `--user` and `--workspace`, which
    exist on `register` but not on `deregister`/`list` (`--user-id`,
    `--workspace-id`).
  - **New:** the FedRAMP comparison table advertised `spawn launch
    --fedramp-low|moderate|high`. Those flags exist nowhere in the tree; the real
    control is `spawn validate --nist-800-53=<level>`.
  - `spawn config init --self-hosted` ×3 (`pkg/compliance`) — there is no `config
    init`, and self-hosted mode is a config-**file** key:
    `infrastructure.mode: self-hosted`.
  - `spawn validate --config <file>` — no such flag.
  - Five dangling doc paths, each verified absent on disk: `docs/lifecycle.md`
    (printed on **every** launch without `--ttl`/`--idle-timeout`), `docs/safety`
    ×2, `docs/compliance/<framework>.md` ×2, and
    `infra/amis/containers/<app>` — the last replaced with
    `docs/bring-your-own-app.md`, which does exist and is the right page.

- **A fail-open on the success path, found by a gate written to assert the
  refusal.** Resolving the owner returned "unclaimed" when the record listing came
  back empty, which authorizes the write. A real hosted zone always carries apex
  SOA and NS records that cannot be deleted, so an empty listing never means "no
  records under this slug" — it means the listing did not do what was asked (wrong
  zone ID, pagination bug, stubbed client). It now refuses. The error path already
  failed closed; this was the same hole reappearing one branch over, and it was
  only visible because the emulator returns an empty list where real Route53
  returns `NoSuchHostedZone`.
  The test fixture now seeds an apex record for the same reason: Substrate's
  `CreateHostedZone` does not create SOA/NS, so without it every test here
  exercised a zone shape that cannot exist in production.

- **Two gates in `pkg/aws` that my own change exposed as broken, both now
  property-based.** `TestSporedTagPermissionsScoped` matched statements by
  `Action` without reading `Effect`, so it treated the new control-tag `Deny` as
  an unconditioned grant — an `Action` match alone cannot tell a grant from a
  prohibition. `TestBuildInlinePolicy` asserted literal statement counts (6 and
  7), so every statement legitimately added to the spored baseline over the past
  year (#174, #173, #221, #622) failed it for no reason and taught whoever hit it
  to bump a number; it now measures the baseline and asserts the two properties
  that were meant — a known template adds statements, an unknown one adds none.
  Both were re-verified against compiling reverts to confirm they still catch the
  defects they were written for.
- **All four `make test-e2e-tier*` targets now pass `-count=1`** (#831). The e2e
  harness builds `bin/spawn` and execs it at runtime, and Go's test cache keys on
  source, build flags and environment — it cannot see a binary read at runtime —
  so a second invocation reported `ok (cached)` regardless of what `bin/spawn` had
  become. Replacing `bin/spawn` with a shell script whose entire body was `exit 1`
  still passed the whole Tier 0 surface. This was #792, fixed where it was noticed
  and left in the Makefile, which is where most people invoke it from. Tiers 2 and
  3 are the costly version of the same hole: they launch real billable instances,
  so a cached pass let a release be cut on a run that never touched AWS, with
  output indistinguishable from one that did.
  `TestEveryE2EMakeRecipePassesCountOne` discovers the recipes rather than naming
  the targets, so a future tier inherits the requirement, and fails when discovery
  finds nothing.
- The signature download now reads the base64 payload from stdin rather than as a
  positional argument (`base64 -d < file`). GNU coreutils accepts
  `base64 -d FILE` and BSD/macOS does not; redirection works on both. Not
  academic — it is how this was first tested.

- **Every scheduled sweep failed; `spawn schedule` has never worked** (#812).
  `lambda/scheduler-handler` invoked
  `arn:aws:lambda:…:function:sweep-orchestrator`, but the deployed function is
  `spawn-sweep-orchestrator` — verified against the live account, where the
  unprefixed name does not exist and the deployed handler had **no environment
  variables at all**, so nothing overrode the compiled-in default. Consistent with
  all three `spawn-schedules-*` buckets measuring 0 objects: no schedule has ever
  run far enough to stage its parameters.
  This is #790's mirror image — a *missing* prefix rather than an extra one.
- **A second failure was hiding behind the first:** the invoke ARN interpolated
  `schedule.Region`, the region the sweep's *instances* go to, while the
  orchestrator is deployed once in the infra account's `us-east-1`. Even with the
  name fixed, a schedule targeting any other region would have invoked an ARN in a
  region the orchestrator has never been deployed to. The region is now its own
  setting (`SPAWN_ORCHESTRATOR_REGION`), not derived from the schedule.
- **`scripts/deploy-scheduler-handler.sh` now sets the environment explicitly.**
  It previously set none, so the deployed configuration came entirely from
  compiled-in defaults — which cannot be read back from AWS, making skew between
  the repo and the live function invisible. Note `--environment` *replaces* rather
  than merges, which is deliberate: this script is meant to be the source of truth
  for the function's configuration.

- **`/api/cost-summary` survived the autoscale removal.** `handleGetCostSummary`
  and `calculateInstanceCosts` lived in `lambda/dashboard-api/autoscale.go` but
  have nothing to do with autoscale; deleting the file wholesale removed a live
  route, which the build caught. They now live in `cost_summary.go`, moved
  verbatim — changing cost arithmetic inside a removal would hide a behaviour
  change in a mechanical one.
  Recorded there, not fixed: the handler runs on a hardcoded, **undated**,
  us-east-1-only price table that defaults an unknown instance type to a
  **fabricated `$0.10/hr`**, then multiplies by 730 and presents it as a monthly
  estimate. truffle#114 established that a fabricated price is worse than no
  price because nothing downstream can tell the difference. Tracked in
  truffle#184.
- **Two IAM scripts no longer grant on deleted tables** — the
  `spawn-autoscale-groups-production` and `spawn-registry-production` ARNs are
  gone from `setup-user-accounts-table.sh` and `setup-dashboard-lambda-role.sh`.
  The second was already dead before this change: `dynamoRegistryTable` was
  declared in `lambda/dashboard-api/autoscale.go` and referenced nowhere, so the
  dashboard was granted access to a registry table it never read.

- **Three deploy scripts targeting a member account no longer assume one.**
  `setup-cross-account-role.sh`, `setup-sweep-cross-account-role.sh` and
  `setup-spawnd-iam-role.sh` create roles in whichever account is being onboarded,
  so there is no correct default account to assert. They now require
  `SPAWN_MEMBER_ACCOUNT` explicitly (`:?`, so unset is a hard failure) and refuse
  to run inside the infra account, where a cross-account role would trust itself.
- **`setup-user-accounts-table.sh` no longer reports success for a failed policy
  update.** `create-policy-version --set-as-default` ran with `&>/dev/null || true`,
  so a failure — the five-version limit being the common one — left permissions
  unchanged while the script printed success.
- **Four `attach-role-policy` calls no longer swallow errors.** `AttachRolePolicy`
  is idempotent, so `|| true` tolerated no real case while hiding `AccessDenied` —
  which would leave a role with no permissions, silently.

- **The Tier 0 e2e suite no longer tests whatever `bin/spawn` happens to
  contain** (#792). It preferred an existing `bin/spawn` unconditionally, and
  `bin/` is gitignored — exactly the artifact that accumulates locally. It
  misled this repo three times in one day, in both directions: a nine-hour-old
  binary made an alerts round-trip **pass vacuously** (the dangerous half — a
  failing test prompts investigation, a passing one does not), and after `spawn
  alerts` was deleted the coverage gate **failed** demanding a decision for a
  command that no longer existed, because it enumerates the command list from
  the binary. It now always builds from source, once per process, and prints
  which binary it built so a run's output says what was tested. A
  freshness-by-mtime heuristic was considered and rejected: what goes stale is
  not only behaviour but the command *inventory*, so the comparison would have to
  be right every time, whereas `go build` is right by construction.
- **`make check-docs` passed while shipping reference docs for a deleted
  command** (#795). The drift check is `git diff --exit-code docs-gen/`, which
  catches a fragment whose *content* changed but not one whose *command* is gone
  — nothing rewrote or removed the file, so there was no diff. `docs-gen/alerts.md`
  survived the removal of `spawn alerts` and would have kept describing four
  subcommands that do not exist. `gen-docs` now prunes orphaned fragments and
  names each one it removes.

- **Six deploy scripts asserted nothing about which AWS account they were
  pointed at** (#799). Credentials are whatever happens to be ambient, so each
  would have built a copy of production in a sandbox without complaint —
  `lambda/dashboard-api/deploy.sh`, its duplicate under `scripts/`, both
  dashboard-websocket scripts, `scripts/deploy-custom-dns.sh` and
  `scripts/setup-schedules-dynamodb.sh`. The DNS one already *printed* the
  account and never compared it.
- **`scripts/setup-schedules-dynamodb.sh` reported success for a failed stack
  update** (#799). `|| true` on both `update-stack` and
  `wait stack-update-complete` swallowed every failure, after which the script
  printed `✅ Stack updated successfully` unconditionally. It now matches the one
  error that genuinely means success — "No updates are to be performed" — and
  aborts on anything else.
- **Four Lambda deploy scripts stamped no `spawn:version` and set no log
  retention** (#799). An untagged function cannot be checked for version skew,
  and skew is the normal state here because the control plane does not upgrade
  with the repo. Measured consequence: the live `spawn-dashboard-api` carried
  `spawn:managed` and `spawn:purpose` but no version, so #768's "skew is readable
  in one API call" was false for it. Stamping now happens on **every** deploy
  rather than only on create.
- **`lambda/dashboard-api/deploy.sh` raced itself and skipped its own cleanup**
  (#799). It called `update-function-configuration` immediately after
  `update-function-code`, which Lambda rejects while the first update is in
  flight — exit 254 with the code updated and the configuration not, a state that
  looks like total failure. It now waits. Cleanup moved to a `trap` so it also
  runs on the error paths, where `set -e` had been leaving 27 MB of build
  artifacts behind.

### Security

- **The DNS updater authenticated its callers but never authorized them**
  (advisory GHSA-7pm4-cqjg-vh2q, #835). The Function URL is `AuthType: AWS_IAM`
  and the record is namespaced under the SigV4-verified caller account, so a
  caller cannot write outside its own subdomain — that part was right. But the
  function's resource policy is the one AWS auto-generates, `Principal: "*"`
  gated only on `lambda:FunctionUrlAuthType`, so any principal in any AWS account
  reached the handler with a verified identity. Since the record name is
  caller-chosen and the record lands in the real `spore.host` zone, a stranger
  with a free AWS account could publish `login.<their-base36>.spore.host`
  pointing anywhere — a phishing primitive hosted and paid for by spore.host.

  The verified account is now checked against a registry (`spawn-user-accounts`,
  overridable via `DNS_ACCOUNTS_TABLE`) plus an explicit `DNS_ALLOWED_ACCOUNTS`
  list, failing closed on a lookup error and on a lookup that returns nothing at
  all — an empty allowlist is a broken lookup, not a permissive one.

  **Deploying this does not close the hole**, and the ship-it-enforcing version
  would have caused an outage. Enforcement is behind `DNS_AUTHZ_MODE=enforce` and
  defaults to audit, which logs the would-be refusal and continues, because the
  zone serves four live A records under a subdomain belonging to an account that
  is *not* in the registry. The real fix is narrowing the resource policy, which
  is a live AWS change rather than a deploy; both steps, the simulation results
  and the rollback are in `docs/dns-updater-authorization.md`.

- The account-registry read is granted as its own inline policy scoped to one
  table, one attribute (`dynamodb:Attributes`) **and** to
  `dynamodb:Select = SPECIFIC_ATTRIBUTES`. The last is not belt-and-braces:
  `ForAllValues:StringEquals` is satisfied by an empty set, so a `Scan` naming no
  attributes passes the attribute condition and reads every field of every item.
  Simulated allow-and-deny eight ways before shipping.

- **`spawn plugin install <name>` performed no verification at all** (advisory
  GHSA-54x4-pc23-4m6j, #816). The shortest and most natural install form — a bare
  name with no `@version` — fetched the spec from the registry's `main` branch. The
  verification machinery is well built and genuinely fail-closed, but it is gated on
  a release tag having been resolved, and a bare name left that empty. So the form a
  person is most likely to type did no signature check, no checksum check, and
  emitted no warning, while a plugin spec runs as root on the instance.
  The gate was not wrong when written: bare-name resolution used to 404 against the
  official registry layout (#448), so the path was unreachable. Fixing that quietly
  armed this. Confirmed reachable today —
  `plugins/tailscale/plugin.yaml` on `main` returns HTTP 200.
  A bare official name now resolves to the plugin's **latest release tag**, which is
  exactly the input the verified path already takes. Verification itself is
  unchanged; it simply now runs. A plugin with no releases, or a releases API that
  cannot be read, **refuses** rather than falling back to the mutable branch — a
  rate limit must not be able to select the unverified path. `--insecure` still
  reaches the default branch, and now says so: the "unverified source" warning used
  to fire only for a third-party owner, so the official unverified path told the
  user nothing.

- **Any caller could publish or delete a DNS alias under another tenant's
  subdomain** (advisory GHSA-q9qq-223w-mqx6, #820). `lambda/dns-updater` builds
  the canonical record from the SigV4-verified caller account, so a caller can
  only write under its own base36 subdomain — and the comment saying so sits two
  lines above the friendly alias (#121), which was built from `req.AccountName`
  out of the **request body**, validated only as a DNS label. So any caller could
  UPSERT `{record}.{any-tenant-slug}.{domain}` onto its own instance, or DELETE
  it. The alias owner is now resolved server-side.

  Resolving it was the hard part, because there was nowhere obvious to resolve it
  *from*. The friendly name comes from `account:GetAccountInformation`, which for
  a member account needs org-management credentials this project must never hold;
  and the natural registry, the `spore-portal-accounts` table, is written by the
  portal rather than by spawn and currently holds zero items, so a lookup against
  it would authorize nothing and break the one tenant actually using aliases.
  Ownership is therefore derived from the zone's own state, which already encodes
  it: an existing alias CNAME points at `{record}.{base36}.{domain}`, and that
  base36 label decodes back to the owning account. A first use of an unclaimed
  slug takes it and records a `_spore-owner.{slug}` TXT marker, so ownership
  survives the last alias CNAME being deleted — otherwise that deletion is exactly
  when a slug would silently become available to a squatter. The explicit marker
  outranks an inferred owner, so one planted CNAME cannot contest a recorded claim.
- **`ip_address` is validated as an IPv4 literal** before anything is written
  (#835). It was previously checked only for non-emptiness, leaving the handler to
  rely on Route53 rejecting a malformed A-record value — a validation step in
  another service, reached only *after* the request had been authorized.
- **The Slack token-rotation endpoint was reachable unauthenticated from the
  public internet, and returned a live bot token in its response** (advisory
  GHSA-qc2h-6qqf-wcm4, #821). `POST /api/slack/token/rotate` was routed above
  `getUserFromRequest` alongside the two OAuth routes — but unlike those, with no
  comment saying why — and the API Gateway route is `$default` with
  `AuthorizationType: NONE`. It took a workspace's current bot token out of the
  request body, rotated it with Slack, replaced the stored one, and returned the
  **new** token as `{"ok":true,"token":"xoxb-…"}`. Possession of a leaked or
  already-stale token was therefore the only credential needed to mint a fresh
  valid one, while the legitimate owner lost theirs.
  Confirmed against production with a deliberately invalid token: the endpoint
  answered `500 {"error":"Token rotation failed: workspace not found for token"}`,
  showing the handler ran with nothing in front of it. Nothing was written,
  because the lookup fails before the update. `spore-bot-workspaces` holds seven
  real workspaces.
  Inbound requests are now verified with Slack's HMAC signature over the raw body,
  with a five-minute replay window checked as an **absolute** skew so a replay
  cannot be refreshed by dating it forward. The signing secret needed for this
  (`slackSigningSecret()`) already existed in Secrets Manager — it had simply never
  been used to verify anything.
  The response no longer contains the token, and the error no longer says whether
  a token matched a workspace, which was an oracle for whether a token is live.
- **A watch record with no owner was returned to any authenticated caller**
  (same advisory, #821). `handleGetWatch` read `user_id` out of the raw DynamoDB
  item and skipped the comparison entirely when the attribute was absent — or
  present but not a string. It was the only fail-open ownership check in the file
  set; `sweeps.go` and `instances.go` already used the direct comparison.
  `WatchInfo` now carries `UserID` (tagged `json:"-"`, since it is an
  authorization input and not dashboard data, and returning it would leak the
  owning principal's IAM ARN), so an absent owner is a mismatch rather than a
  bypass.

- **The TTL reaper executed instance-writable tag content as root, on any
  spawn-managed instance in the account** (advisory GHSA-8q3p-qcv8-2pr4, #830).
  The graceful pre-stop tier (#187) read the `spawn:pre-stop` tag and ran it
  through SSM `AWS-RunShellScript`, which executes as root, and the `spored`
  instance role holds `ec2:CreateTags` on `Resource: "*"` for every instance
  carrying `spawn:managed=true`. So any principal holding one spore's
  instance-role credentials — which includes any unprivileged local user on that
  spore, via IMDS — could rewrite **another** instance's `spawn:pre-stop` and
  `spawn:ttl-deadline` and obtain root command execution there when the reaper
  next collected it. In a team-shared account, that is cross-tenant root RCE.
  Two distinct paths reached root, and both are closed:
  the privilege drop was conditional (`if localUsername != ""`), so an instance
  with that tag missing — or *deleted*, which the same role could do — ran the
  hook as root; and the username was spliced into `su - %s -c …` unquoted while
  the hook body beside it was quoted, so `x -c '…' #` supplied its own `-c`.
  Running the hook as root was never the intent: #63 is "run the hook as this
  user". The reaper now refuses to run the hook at all unless the tag names a
  valid, non-`root` POSIX user, which cannot weaken the hard-deadline guarantee
  (#72) because the caller terminates either way — the only loss is the flush,
  which is already what happens when an instance isn't SSM-managed.
  The command construction moved into `buildPreStopCommand` so the guard can be
  tested where it lives; `tryGracefulPreStop` takes a concrete `*ssm.Client` and
  could not be driven at all.
- **The `spored` instance role can no longer write the tags the reaper acts on.**
  Both spored policies gained an explicit `Deny` on `ec2:CreateTags` and
  `ec2:DeleteTags` conditioned on `ForAnyValue:StringEquals` over `aws:TagKeys`
  for `spawn:pre-stop`, `spawn:pre-stop-timeout` and `spawn:local-username`.
  `Deny` rather than a narrower `Allow` because an explicit deny cannot be
  overridden by a later grant, and these two policies have already diverged in
  exactly that way once. `ForAnyValue` rather than `ForAllValues` because the
  latter would only fire when *every* requested key is a control key, so
  appending one innocuous tag would defeat it.
  **The list is deliberately only three keys.** The obvious longer version is
  wrong: spored legitimately writes `spawn:last-heartbeat` every cycle and both
  writes and deletes `spawn:ttl-deadline` on the `spored ttl set` path, so
  denying those would have broken idle detection and `spored ttl` while reading
  as a hardening. Deadline tampering stays contained by the unconditional max-age
  ceiling instead (#76), which is the right tool precisely because that key has a
  legitimate in-instance writer.
  Verified by 11 `simulate-custom-policy` evaluations across both documents: all
  three control keys `explicitDeny` on create and delete, including when mixed
  with a legitimate key in the same request, while `spawn:last-heartbeat`,
  `spawn:ttl`, `spawn:ttl-deadline` and `spawn:fsx-dra-status` all stay `allowed`.

  **Existing roles need remediation.** `spored-instance-role` is rewritten on
  each launch, so a launch from a build containing this change updates it — but
  until then a long-lived role keeps the old grant. Deploying the reaper is the
  independent and higher-priority half: it closes the root escalation on its own,
  without waiting for the policy to reach any instance.
- **Two spored install paths ran the agent as root without verifying it; one was
  on every app launch** (advisory GHSA-3x4q-99gh-fxhx, #819). `spored` is
  installed by curl-to-root from an S3 bucket and runs as root. The boot path has
  done this properly since spore-host#440 — probe for a signature before
  committing to a source, require a sha256, verify a detached signature against a
  key compiled into `spawn` rather than one served from the same bucket, fail
  closed at every step. Two other paths had none of it:
  - **`cmd/app.go`, twice** — a bare `curl`, a `chmod +x`, and
    `mv … || true`. No checksum, no signature, and the install failing was
    swallowed. This is the user-data of every `spawn app launch`.
  - **`cmd/upgrade_spored.go`** — sha256 only, and fail-**open**: a missing
    `.sha256` printed `WARNING: no checksum published; skipping verification` to
    stderr and installed anyway. A missing checksum is not a reason to skip
    verification — an attacker who can rewrite a bucket can equally delete a
    checksum object, so "verify if one happens to be published" is a check the
    attacker opts you out of. There was no signature verification at all.

  All three now call one shell function from the new `pkg/sporedinstall`, so there
  is a single implementation to get right instead of three to keep in step. Each
  was written by copying the shape of the *download* rather than the shape of the
  *verification*, which is why having three was the root cause rather than a
  detail.

- **The in-account TTL reaper could run arbitrary root commands on any
  SSM-managed instance** (advisory GHSA-2hfh-q6jv-qgjp, #818).
  `pkg/reaperiam.ScanSelfPolicyDocument` — the inline policy
  `spawn reaper deploy` writes — granted `ssm:SendCommand` on `Resource: "*"`
  with no condition, and the reaper invokes `DocumentName: "AWS-RunShellScript"`.
  That is arbitrary shell as root on anything SSM-managed in the account, which
  strictly dominates the `ec2:TerminateInstances` the *same document* tag-gates
  with considerable care.
  The correct shape already existed in
  `deployment/cloudformation/ttl-reaper-cross-account-role.yaml`; the scan-self
  copy never received it. It now matches: `SendCommand` scoped to
  `arn:aws:ec2:*:*:instance/*` with `ssm:resourceTag/spawn:managed`, split from a
  separate grant on the one document the reaper names (a document carries no tag,
  so conditioning that statement too would deny every call), with
  `GetCommandInvocation` left on `*` because it takes no resource-level ARN.
  **No deployed role needs remediation.** Checked both accounts: the production
  reaper's role has no `ssm:SendCommand` grant at all, and the cross-account role
  already carried the scoped form. No self-scan reaper has ever been deployed, so
  the over-broad grant existed only in the generator. Anyone who *has* run
  `spawn reaper deploy` should re-run it.
  A stale comment went with it: it claimed neither the template nor the
  cross-account role granted these actions, which stopped being true when the
  cross-account grant landed.

- **`spawn onboard` generated a role that could open a shell on any instance in a
  customer's account** (advisory GHSA-w7wx-6439-wff7, #817). Four defects in ~25
  lines of a file that had **no test at all**, which is most of why they survived:
  it builds the role the hosted portal assumes inside a BYOA account, so every
  grant is against someone else's infrastructure.
  - `ssm:StartSession` was granted on `arn:aws:ec2:*:*:instance/*` with **no tag
    condition**, while every destructive action beside it was gated on
    `spawn:managed`. A portal compromise meant an interactive root shell on
    arbitrary **non-spawn** production instances. Now conditioned on
    `ssm:resourceTag/spawn:managed` — the **`ssm:`** key, not `ec2:`, because a
    condition key belongs to the service of the *action* and an `ec2:` key is
    simply absent from an `ssm:` request context, making such a statement a
    silent implicit deny (#665). Split from the SSM *document* grant, since
    `StartSession` authorises against instance and document separately and a
    document carries no tag.
  - `ec2:CreateTags` was unconditioned on `*`, which is the #174 defect:
    `pkg/aws/iam.go` documents it as "a compromised spore could tag ANY instance
    `spawn:managed=true` and then terminate it", and that fix reached the spored
    policies but never this file. Two API calls terminated any instance in an
    onboarded account. Now split in two, because this role *launches*: tagging at
    `RunInstances` time cannot satisfy a resource-tag condition (the tag is what
    is being created), so that case uses `ec2:CreateAction`, and retagging an
    existing instance uses `ec2:ResourceTag/spawn:managed`.
  - `iam:PassRole` interpolated an instance **profile** name into a **role** ARN
    slot, so the grant matched nothing and the portal's `RunInstances` was denied.
    It now derives from `pkg/aws.SporedRoleName`. An IAM grant that cannot match
    is not a safe failure — it reads as working while the feature it enables is
    simply broken, which is why nobody noticed.
  - **No existing role needs remediation:** `spore-portal-accounts` measures
    `Count=0` and `spore-portal-onboard` does not exist in the compute account, so
    no account has ever been onboarded. That independently corroborates the
    `PassRole` finding.

- **The reaper's max-age ceiling now applies even inside a live deadline**, which
  is what its own documentation always said it did. `main.go`'s header describes
  the ceiling as firing "even for --no-timeout / missing / **far-future**
  deadlines — defense against escape hatches and tag tampering", but the code
  returned early for any instance within its `spawn:ttl-deadline`, and the
  template's `MaxAge` description mentioned only missing deadlines. Three
  documents, two intents, and the code implemented the narrow one.
  That mattered because the deadline tag is **writable from the instance**: the
  instance role grants `ec2:CreateTags` conditioned on `spawn:managed=true`, which
  the instance itself satisfies, so a spore could write a deadline decades out and
  become permanently unreapable — and forge `spawn:last-heartbeat` to keep the
  quiet-spored alarm silent while doing it. #76 tracks signing the tag; this makes
  the ceiling a real bound in the meantime, so the worst case is `REAPER_MAX_AGE`
  rather than forever.

- **The spored signature-verification gate could not fail** (#813). Emptying
  `sporedSigningPublicKeyPEM` — a one-literal, cleanly-compiling edit, and the
  documented way to disable signing — makes every launch download and run spored
  **unverified**, from the same bucket that serves its checksum. That is exactly
  the gap #440 closed. Both existing gates still passed:
  `TestBuildLinuxBootstrap_SigAwareFallback` matched `SPORED_SIG_VERIFY=1` as a
  bare substring, and that string also appears inside **two shell comments** in
  `linuxBootstrapBody`, which is appended unconditionally — so it is present in
  every bootstrap, signed or not. `TestBootstrap_SignatureEnabled` injects its own
  test key and never reads the production value, and **no test asserted the
  production key existed at all.**
  Fixed three ways: the assignment is now matched anchored at line start
  (`(?m)^SPORED_SIG_VERIFY=1$`); a new test asserts the compiled-in key is
  non-empty **and parses as an ECDSA P-256 public key**, because a truncated or
  placeholder value would satisfy a length check while failing `openssl` at boot
  on every instance; and the disabled path asserts the enabling assignment is
  *absent* rather than only that `=0` is present.
  Both reverts verified against compiling edits: an empty key now fails two tests,
  and a `REPLACE_ME` placeholder fails the PEM parse.
- **`TestBootstrap_SignatureDisabledByDefault` was permanently dead.** It opened
  with `if signatureVerificationEnabled() { t.Skip(...) }` and a key *is*
  compiled in, so it skipped on every run while counting as coverage for the
  disabled path. `sporedSigningPublicKeyPEM` is a `var` precisely so a test can
  exercise that path, so it now sets it, runs, and restores via `t.Cleanup`.

- **Removed an `sts:AssumeRole` grant into an account this project does not own**
  (advisory pending). Two committed scripts granted
  `sts:AssumeRole` on a role ARN in account `942542972736`, and the statement had
  reached the live `SpawnDashboardLambdaPolicy`, attached to the execution role of
  four production Lambda functions. Confirmed by simulation: before the fix that
  action evaluated **allowed**; it is now `implicitDeny`, with every other action
  on the role unchanged.
  It was inert — the role name is one nothing in the repo creates, and the live
  code assumes a role in a different account — so nothing depended on it, which is
  precisely why nothing would have noticed it being re-applied.
  Three structural fixes, so review is no longer the only control:
  `TestNoARNNamesAnAccountWeDoNotOwn` rejects any ARN naming an account outside an
  **allowlist** of accounts this project owns (an allowlist, not a denylist, so a
  typo'd or unfamiliar account is caught too — verified against both); the
  deploy-script discovery now includes `aws iam (create|put|attach|update)-`,
  bringing six IAM-mutating scripts under the account assertion for the first
  time; and the script that re-applied the policy no longer discards its own
  failure.
- **`SECURITY.md` added, with a working private reporting channel.** The suite had
  no vulnerability-reporting route anywhere — the umbrella's `SECURITY.md` is a
  security overview with no reporting section. GitHub private vulnerability
  reporting is now enabled on this repository so the documented link actually
  resolves.

### Documentation

- **A live, hourly Lambda that nothing in this repo can deploy** (#848).
  `docs/infra-account.md` asked only one of the two questions — "code with no
  deployment", answer none — and the inverse is not none.
  `spawn-cost-history-collector` is invoked every hour by an ENABLED EventBridge
  rule, was last modified **2026-02-20**, and the only references to it anywhere
  in the repository are its own `go.mod`, a `.gitignore` entry, a Dependabot
  entry and the changelog. No deploy script, no Makefile target, no CFN or SAM
  template — so the source cannot reach the function that is running, and
  Dependabot has been bumping a module that cannot be shipped. The existing gate
  reasons outward from hardcoded Lambda names in Go source, and nothing in Go
  source names this function, so it is invisible to that direction.
- **`docs/spawn-audit-2026-07-11.md` marked SUPERSEDED.** It is framed as a live
  prioritized report ("Phase 0 — Do now") and cites `cmd/launch.go` five times
  including its line count, plus `pkg/audit/context.go`. Neither file exists —
  `cmd/launch.go` was split up — so a reader following its own top-priority item
  finds nothing and cannot tell whether the finding was fixed, renamed or never
  real.
- **`docs/dcv-streaming-design-review.md`'s central risk corrected in place.**
  Section 3, "Idle detection can hang 'not idle' forever", and the table's
  "**unbounded grace**" are no longer true: `pkg/agent/agent.go` defines
  `const dcvGraceTimeout = 8 * time.Minute` and bounds exactly that, so the
  instance cannot "bill forever". The analysis is left in place because it is why
  the bound exists — deleting the question makes the answer look arbitrary.
- **The reaper's cross-account onboarding command could not work** — it passed
  `--parameter-overrides ReaperLambdaRoleArn=…` and the template declares no such
  parameter (`ReaperAccountId`, `ReaperRoleNamePattern`, `RoleName`, `TagPrefix`),
  so CloudFormation rejects it outright. This is the step a customer runs to put
  an account under the cost backstop. The instruction predated #476, which
  deliberately replaced the exact role ARN with a name pattern; the accompanying
  "chicken-and-egg, deploy the Lambda first to read its role ARN" paragraph was
  stale for the same reason and is gone, since a pattern-based trust does not
  reference a principal that must already exist.
- **The README's `.deb`/`.rpm` install commands 404'd.** GoReleaser puts the
  version in the asset name (`spawn_0.127.0_linux_amd64.deb`), so there is no
  version-less `releases/latest/download/` URL to fetch. Replaced with a tag
  resolution, verified end to end: the exact pipeline the README now gives
  resolves `0.127.0` and both assets return 200.
- **Every `spore.host/docs` link 404'd**; the live host is `docs.spore.host`
  (`/docs` → 404, `/docs/tools/spawn` → 404, `docs.spore.host/tools/spawn` → 200).
- **Three `examples/README.md` quickstarts could not run**: `spawn launch --params
  <file>` exits with "a spore name is required", so each now passes one.

- **`lambda/ttl-reaper/README.md` contradicted the code in two places**, both now
  gated rather than merely corrected. It said flatly that a reaper-kill "does
  **not** run the in-instance pre-stop hook", written before #187 added exactly
  that and never updated — so a reader auditing the reaper's blast radius would
  have concluded there was no SSM execution path at all. And it said
  "Within-deadline instances are always spared", which stopped being true when the
  max-age ceiling became unconditional (#76); the ceiling is the control against a
  spore writing itself a far-future deadline, so a page promising the opposite is
  the most misleading line available. `TestReaperDocsAgreeThatTheCeilingBeatsAFarFutureDeadline`
  now covers the README alongside `main.go` and `template.yaml` — it was the one
  page left out, and the one a reader reaches first.
- Four link references used in that README (`#76`, `#187`, `#247`, `#438`) had no
  definition, so they rendered as literal text.

- **`CLAUDE.md` gains a fifth gate rule: assert the documented intent, not the
  behaviour you observed.** A test written by reading the code can only confirm
  the code; if the behaviour is wrong, the test certifies it and will fail on the
  fix. This change is the worked example — `TestEvaluate_WithinDeadline_Spared`
  blessed an exemption that let an instance make itself unreapable, and was right
  about the code and wrong about the design. The rule says to write the assertion
  from the comment, parameter description or issue, run it, and if it fails
  resolve *which* is wrong before writing anything down — and to gate the
  agreement when two sources of intent conflict, because the drift is the root
  cause and the behaviour only the symptom.

- **Four stale figures corrected, all of them mine.** `CLAUDE.md` claimed 18
  Lambda functions where the page it calls authoritative said 17 — and both are
  now 15. The `Makefile`'s "The 12 nested modules" is replaced by a statement
  that the script discovers them, because a hand-written count is the
  anti-pattern rule 4 warns about and that one had drifted to 10 before anyone
  noticed. `docs/infra-account.md` said "Four have no reference in this repo"
  where three do, and "All 18 log groups" where the number had already changed —
  that claim is now made without a count.

## [0.127.0] - 2026-10-10

**Breaking:** `spawn alerts` is removed — see Removed below. Pre-1.0, a breaking change bumps MINOR.

### Added

- **Two gates on the class of defect behind #790.**
  `TestEveryHardcodedLambdaNameIsCreatedByADeployMechanism` reads the function
  names out of every deploy script and template in the repo and fails when Go
  source hardcodes a name none of them create — discovered, not listed, so the
  naming convention is not written down a second time to drift again. It found
  ten created names, including two nobody had enumerated by hand.
  `TestValidateRecommendationsNameOnlyRealCommandsAndPaths` resolves every
  command a recommendation mentions against the real cobra tree and checks every
  path it names exists. Checking the first word would not have been enough:
  `config` **is** a real command, and the defect was the subcommand after it.

- **`make check` now runs the nested-module loop, via the same script CI uses.**
  `go test ./...` does not descend into a directory with its own `go.mod`, so the
  12 modules under `lambda/` and `scripts/` were invisible to the local gate while
  CI checked them. A nested module that does not even **compile** passed
  `make check` and failed in CI.
  That gap cost two round trips in one session: #770's `LogRetentionDays` wired
  into no Makefile (caught by `lambda/ttl-reaper`'s own test, which only CI ran)
  and #771's missing `go.sum` entry after `pkg/aws` gained six SDK imports. Both
  were found after a push, where the fix is a second commit and another wait.
  Extracted to `scripts/nested-modules.sh` and called from **both** `make check`
  and the workflow, rather than duplicated — a local gate that differs from CI
  teaches people to push and find out, and two copies of the loop would drift
  back apart. The script refuses to pass when it discovers no modules, since a
  broken discovery is not a clean repo.

- **An idle reaper now says so, and can be told to remove itself on a schedule**
  (#772). `spawn reaper status` reports when the in-account reaper has run
  regularly and reclaimed nothing, and `spawn reaper teardown --if-idle-for 720h`
  removes it only if that is true.
  It distinguishes the two causes of "reclaimed nothing", because they have
  **opposite remedies**: a reaper that runs and finds nothing may be an unused
  feature (removal restores "leave no trace"), while a reaper that has not *run*
  is a broken schedule — and removing that one deletes something that was never
  given the chance to work. The second is reported as a broken schedule and is
  **never** offered for teardown. An unreadable log group reports "could not
  tell" and removes nothing, so a permission error can never be mistaken for
  idleness.
  Idleness is derived from the reaper's own CloudWatch logs, which needs **no new
  Lambda permission at all**. The obvious reading of "self-remove" — a reaper that
  deletes itself — would require adding `lambda:DeleteFunction`, `iam:DeleteRole`,
  `events:DeleteRule` and `s3:DeleteBucket` to the 11 EC2/FSx/SSM actions it has
  today. #613's audit of that reaper is quotable because its policy is minimal, in
  an account whose organisation forbids external trust; granting a scheduled
  function `iam:DeleteRole` to save a human one command is the wrong trade. So the
  capability lives in the CLI under the caller's own credentials — put it in a
  cron if you want it automatic.
  The 30-day window matches the log retention set in #770, so the honest answer is
  "idle for at least 30 days" rather than a number the evidence cannot support.

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

### Removed

- **`lambda/alert-evaluator` and the dashboard's `/api/alert-preferences`
  endpoints are gone** (#794), completing the alerting removal #783 began. The
  evaluator was **deployed and running hourly since 2026-02-20** — roughly 5,500
  invocations — reading `spawn-alert-preferences`, which has never held a record.
  Its SNS topic had **zero confirmed subscriptions**, so even a triggered alert
  had nowhere to go: the path was dead at both ends.
  The dashboard half goes with it deliberately. `POST /api/alert-preferences`
  was the only writer of that table, and keeping a user-facing endpoint that
  accepts configuration nothing consumes is precisely the defect #790 was about.
  **`spawn-cost-history` is unaffected** — `cost-history-collector` still writes
  it and the dashboard still reads it; only the evaluator's read of it went away.
  Also torn down in the shared infra account: the function, its
  `spawn-alert-evaluator-hourly` rule, and its log group. **Deliberately kept:**
  `SpawnDashboardLambdaRole`, which is shared by four still-live functions, and
  the `spawn-cost-alerts` topic and `spawn-alert-preferences` table, both
  declared in a template whose other resources are live. Those belong to #653.

- **The `spawn alerts` command and `lambda/alert-handler` are gone** (#783).
  **Breaking change — this is why 0.127.0 is a MINOR bump.** `spawn alerts
  create|list|delete|history` no longer exists.
  The evidence for removing rather than deploying: the handler is event-driven
  and **nothing emitted the event it handled** — `sweep-orchestrator`'s only
  `lambda.Invoke` is `reinvokeSelf` — so deploying it would have changed nothing
  without also building an emission path and a scheduler for the time and cost
  triggers. `spawn-alerts` has **never held a record**, so no user loses
  anything. And every commit touching the handler since the initial one was
  mechanical: CVE bumps, a Go version bump, a test migration. Removing it also
  retires the `spawn-alerts`/`spawn-alert-history` config keys,
  `SPAWN_DYNAMODB_ALERTS_TABLE`, `SPAWN_DYNAMODB_ALERT_HISTORY_TABLE` and
  `SPAWN_LAMBDA_ALERT_HANDLER_ARN`.
  Cost alerting via the dashboard is untouched: `alert-evaluator` reads
  `spawn-alert-preferences`, a different table, and is still deployed — though it
  has had nothing to do since February (#794).
  The two now-unread tables are left in place; deleting them is control-plane
  reaping, which nothing does yet (#653). The template that creates them stays
  because two of its four tables are live.
- **`deployment/cloudformation/self-hosted-stack.yaml` is gone** (#783). 354
  lines, one commit — the initial one — never edited and never deployed in either
  account. It declared 11 resources and **no functions**, so deploying it would
  have produced half a control plane, and it named a log group for the function
  removed above. Its informational value is superseded by
  `docs/infra-account.md`, which maps 103 resources measured from the live
  account rather than 11 imagined in 2026-02. #780 should start from the measured
  document.

### Changed

- **`spawn alerts create` now refuses an alert that cannot be delivered**
  (#790). It validated five trigger types, wrote the record, printed
  `Alert created: …` and exited 0 — while the only reader of that table,
  `lambda/alert-handler`, is deployed nowhere. Every alert ever created was
  silently inert, and `spawn alerts history` stayed empty, which reads like
  "nothing has gone wrong" rather than "nothing is watching". The command now
  checks that the consumer exists before writing and exits non-zero with what to
  do about it, so no dead record is left behind.
  **A check that cannot run does not refuse.** It costs one
  `lambda:GetFunction`, which a caller able to write to the alerts table may not
  hold, so `AccessDenied` or a network failure warns and proceeds — refusing
  there would break working installs to protect them from a defect they may not
  have. Only a definite "the function does not exist" refuses. A self-hosted
  install that points `SPAWN_LAMBDA_ALERT_HANDLER_ARN` (or
  `infrastructure.lambda.alert_handler_arn`) at its own deployed function is
  unaffected.
  The Tier 0 end-to-end suite now covers both halves: the create→list→delete
  round-trip seeds the consumer, and a new case asserts that with no consumer the
  command exits non-zero **and leaves no record behind** — the write always
  worked, so "it returned an error" alone would not distinguish the fix from a
  refusal that still wrote.

- **`spawn reaper teardown` now removes the artifact bucket** (#653). It removed
  the schedule, Lambda, permission and role, and **deliberately kept the bucket**,
  reporting what it kept. That was a defensible explicit choice and is not
  defensible under "leave no trace" — and it blocked idle self-removal (#772),
  because a reaper that tidies everything except a bucket has left a trace while
  reporting that it has not.
  The original concern — that emptying a bucket someone may have put other things
  in is a liberty — is **answered rather than overruled**. The bucket is removed
  only when it is tagged `spawn:managed=true` *and* contains nothing outside
  `ttl-reaper/`. A bucket failing either check is reported and kept, naming the
  offending object; `--force-artifacts` overrides the tag check but **not** the
  contents check, because emptying a repurposed bucket destroys data spawn never
  wrote. `--keep-artifacts` is the opt-out, and it *says* it kept the bucket
  rather than keeping it silently.
  `spawn reaper deploy` now tags the bucket at creation, so teardown has positive
  evidence instead of inferring ownership from the name — #755's lesson, where
  `spawn:created` was read in three places and written in none. Tagging failure is
  non-fatal: a deploy that cannot tag has still produced a working reaper, and the
  teardown handles an untagged bucket explicitly.

### Fixed

- **`spawn validate --infrastructure` no longer reports a deployed Lambda as
  missing** (#790). The scheduler handler is deployed as `scheduler-handler`,
  with no `spawn-` prefix — `scripts/deploy-scheduler-handler.sh` defaults to
  `SPAWN_LAMBDA_NAME:-scheduler-handler` — but the resolver reconstructed the
  name from the prefix convention and asked for `spawn-scheduler-handler`, which
  has never existed. Against the live shared account the command reported two
  errors, of which **one was false**, so the operator had to work out which half
  to believe. It now reports the one real error. Scheduling itself was never
  affected: `spawn schedule` has always invoked the unprefixed name.
  The same wrong name was also asserted by the resolver's unit test and seeded by
  the Substrate fixture, so three places agreed with the bug. The fixture now
  provisions from the same constants the resolver uses, while the unit test keeps
  literal ARNs on purpose — a test that restates the value under test cannot
  notice it being wrong.
- **A failing infrastructure report now points somewhere real** (#790). It
  recommended `spawn config deploy-infrastructure` and
  `spawn config init --self-hosted`; neither exists, and both had been there
  since the initial commit. Because `config` is an alias for `instance-config`,
  following the advice did not even produce an "unknown command" error — it tried
  to read runtime config over SSH from an instance named
  `deploy-infrastructure`. Missing Lambdas are now named one per line with the
  script that deploys each, missing tables with the setup script or template that
  creates them, and self-hosted mode with the config keys and
  `SPAWN_LAMBDA_*_ARN` variables that actually drive it. Where nothing in the
  repo deploys a function, it says so rather than inventing a command.

### Documentation

- **`CLAUDE.md` now records how to write a gate that can actually fail.** A large
  share of this repo's CI consists of gates — tests whose job is to fail when
  someone removes a property — and several were written here that could not fail,
  which is worse than having none because they are counted as coverage. The four
  rules are each drawn from a gate in this repo that broke them: verify against a
  revert that **compiles** (three "verified" reverts failed `go vet` first, so the
  gate never ran); test the invariant directly and not only through the
  constructor (#787's `!Determined` guard survived a real revert because no test
  could build the offending state); match the assignment or invocation rather than
  the bare identifier (four gates matched their own prose, including
  `spawn:version` inside a parameter `Description`); and discover the set rather
  than listing it (`lambda-deploy-census.sh` once named five functions in an
  account of twelve). Plus the corollary that a suspicious negative usually
  indicts the invocation, not the system — four false negatives in one session
  came from the probe, including `FilterLogEvents --limit` returning an empty
  *first* page with a continuation token.
- **`make check` is described accurately**: it also runs every nested module
  (#788), which the previous summary omitted.

- **`docs/infra-account.md` — a measured map of the deployed control plane.**
  `spawn` is not only a CLI: 18 Lambda functions, 23 DynamoDB tables, 21 S3
  buckets and 16 IAM roles live in the shared infra account, and nothing
  documented what they were or which code owned them. Every figure is measured
  from the live account rather than recalled, with the commands to re-measure.
  It records what is least obvious from the source: how each function is invoked
  (EventBridge schedule, API Gateway, or direct CLI invoke — the last explains
  why several have no resource policy); which live functions are deployed from
  *other* repos, so an unfamiliar name is not an orphan; that `lambda/alert-handler/`
  builds in CI but **has no live function anywhere**; and the deploy traps that
  have each cost a real failure, including that `--parameter-overrides` cannot
  carry a value containing a space and that SAM's `Enabled: !Ref` silently
  always resolves to ENABLED.

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

- **The reaper's cross-account trust no longer names a CloudFormation-generated
  role ARN** (#476). It named `…TTLReaperFunctionRole-ZJ84YZ2dCPei`, so recreating
  the reaper stack would have broken cross-account reaping for **every onboarded
  account at once** — with the failure mode being "expired instances are no longer
  terminated".
  `deployment/cloudformation/ttl-reaper-cross-account-role.yaml` now trusts
  `${ReaperAccountId}:root` with an `ArnLike` on `aws:PrincipalArn`
  (`spawn-ttl-reaper-*` by default), which survives the suffix being regenerated
  and stays far narrower than a bare `:root` — what this role grants is
  `ec2:TerminateInstances`. `ReaperLambdaRoleArn` is replaced by
  `ReaperAccountId` + `ReaperRoleNamePattern`.
  The exact-ARN form could not have been migrated gradually: **IAM rejects a trust
  policy naming a principal that does not exist yet**
  (`MalformedPolicyDocument: Invalid principal in policy`), so the new role would
  have had to exist before it could be trusted — an overlap migration per
  onboarded account. The pattern removes the problem rather than sequencing
  around it.

- **`spawn footprint -o json` emitted inconsistent key casing.** The top level was
  snake_case but its elements were PascalCase (`control_plane[].Service`),
  because `ControlPlaneResource` carried no json tags. A machine contract should
  not require knowing which half of the struct you are reading. Tagged now, so
  every key is snake_case.

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

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.147.0...HEAD
[0.147.0]: https://github.com/spore-host/spawn/compare/v0.146.0...v0.147.0
[0.146.0]: https://github.com/spore-host/spawn/compare/v0.145.0...v0.146.0
[0.145.0]: https://github.com/spore-host/spawn/compare/v0.144.0...v0.145.0
[0.144.0]: https://github.com/spore-host/spawn/compare/v0.143.0...v0.144.0
[0.143.0]: https://github.com/spore-host/spawn/compare/v0.142.0...v0.143.0
[0.142.0]: https://github.com/spore-host/spawn/compare/v0.141.0...v0.142.0
[0.141.0]: https://github.com/spore-host/spawn/compare/v0.140.0...v0.141.0
[0.140.0]: https://github.com/spore-host/spawn/compare/v0.139.1...v0.140.0
[0.139.1]: https://github.com/spore-host/spawn/compare/v0.139.0...v0.139.1
[0.139.0]: https://github.com/spore-host/spawn/compare/v0.138.0...v0.139.0
[0.138.0]: https://github.com/spore-host/spawn/compare/v0.137.0...v0.138.0
[0.137.0]: https://github.com/spore-host/spawn/compare/v0.136.0...v0.137.0
[0.136.0]: https://github.com/spore-host/spawn/compare/v0.135.2...v0.136.0
[0.135.2]: https://github.com/spore-host/spawn/compare/v0.135.1...v0.135.2
[0.135.1]: https://github.com/spore-host/spawn/compare/v0.135.0...v0.135.1
[0.135.0]: https://github.com/spore-host/spawn/compare/v0.134.0...v0.135.0
[0.134.0]: https://github.com/spore-host/spawn/compare/v0.133.0...v0.134.0
[0.133.0]: https://github.com/spore-host/spawn/compare/v0.132.2...v0.133.0
[0.132.2]: https://github.com/spore-host/spawn/compare/v0.132.1...v0.132.2
[0.132.1]: https://github.com/spore-host/spawn/compare/v0.132.0...v0.132.1
[0.132.0]: https://github.com/spore-host/spawn/compare/v0.131.1...v0.132.0
[0.131.1]: https://github.com/spore-host/spawn/compare/v0.131.0...v0.131.1
[0.131.0]: https://github.com/spore-host/spawn/compare/v0.130.1...v0.131.0
[0.130.1]: https://github.com/spore-host/spawn/compare/v0.130.0...v0.130.1
[0.130.0]: https://github.com/spore-host/spawn/compare/v0.129.0...v0.130.0
[0.129.0]: https://github.com/spore-host/spawn/compare/v0.128.2...v0.129.0
[0.128.2]: https://github.com/spore-host/spawn/compare/v0.128.1...v0.128.2
[0.128.1]: https://github.com/spore-host/spawn/compare/v0.128.0...v0.128.1
[0.128.0]: https://github.com/spore-host/spawn/compare/v0.127.0...v0.128.0
[0.127.0]: https://github.com/spore-host/spawn/compare/v0.126.1...v0.127.0
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
