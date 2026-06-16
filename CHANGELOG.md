# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.56.0...HEAD
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
