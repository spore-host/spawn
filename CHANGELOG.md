# Changelog

All notable changes to **spawn** are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

---

Older releases are summarized in the
[GitHub Releases](https://github.com/spore-host/spawn/releases) for this repo.

[Unreleased]: https://github.com/spore-host/spawn/compare/v0.43.0...HEAD
[0.43.0]: https://github.com/spore-host/spawn/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/spore-host/spawn/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/spore-host/spawn/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/spore-host/spawn/compare/v0.39.0...v0.40.0
