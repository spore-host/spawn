# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`spawn status` now shows a "Lifecycle protection" summary** for a running
  managed instance: in-instance (spored) enforcement, the out-of-band reaper
  backstop (described as "if deployed" — it isn't authoritatively visible from the
  launch account), the hard termination deadline (from the launch-anchored
  `spawn:ttl-deadline` tag, with time remaining), a worst-case compute-cost ceiling
  by that deadline (on-demand rate, compute only), and the idle timeout. Surfaces
  the safety model the docs describe directly in the CLI.

### Security
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

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.91.1...HEAD
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
