# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Friendly account-name DNS: instances are tagged with `spawn:account-name` — a
  DNS-safe slug of the AWS account's friendly name (from `aws account
  put-account-name`) — and the dns-updater registers a CNAME
  `{name}.{account-name}.spore.host` → the canonical base36 A-record, so the
  legible FQDN resolves. base36 stays authoritative (holds the IP); the name is a
  true alias. Best-effort end to end: when the account has no name, the caller
  lacks `account:GetAccountInformation`, or the slug isn't a valid DNS label, it
  silently falls back to base36-only (unchanged). Spans the launch tag (#121),
  spored's DNS registration, and the dns-updater Lambda (spore-host#357).

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

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.44.2...HEAD
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
