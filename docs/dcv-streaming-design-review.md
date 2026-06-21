# DCV Application Streaming — Design Review & Failure-Mode Analysis

**Status:** review note (no code changes). Written to ground a decision on the
v0.34.0 "Interactive Application Streaming (DCV)" epic
([spore-host/spore-host #282], with #285/#286/#289/#290).

**Why this note exists:** prior work on DCV "went around and around without
resolution." This document maps what is actually built, traces the full
handshake, and isolates *why* it churned — so the next round of work targets the
cause (un-diagnosable, un-testable failure) rather than re-tuning timings on a
real instance.

---

## TL;DR

The DCV flow is **functionally complete, not stubbed** — token verifier, session
wait, tag handshake, CLI polling, and the browser launch page are all real code.
It churned because it is **fire-once, silent on failure, and only exercisable on
a real GPU/DCV instance** — and every distinct failure surfaces to the user as
the *same* message: `(timed out — DCV login screen will appear)`.

That combination is a debugging trap: each attempt costs a ~3–4 minute real
launch, fails opaquely, and offers no signal about *which* of ~five layers
broke. The AMI comments in `libs/catalog/catalog.yaml` ("rebuilt … definitive:
all fixes", "rebuilt … DCV-GL NVIDIA Xorg + kiosk-wm") are themselves a record of
that loop.

**The fix is to make failure legible and the logic testable without a real
instance — not to keep adjusting the handshake on live hardware.**

---

## What exists (component inventory)

| Component | File | Status |
|---|---|---|
| `spawn app launch` flow | `cmd/app.go` | Full |
| DCV user-data builder | `cmd/app.go` `buildDCVUserData()` | Full |
| spored token verifier (`:8444`) | `pkg/agent/agent.go` `startDCVAuthVerifier()` | Full |
| spored session-wait + token + tag write | `pkg/agent/agent.go` `setupDCVAuth()` | Full, **fire-once** |
| CLI ready-url poll | `cmd/app.go` (≈261–311) | Full |
| Browser session HTML | `cmd/app.go` `writeSessionHTML()` | Full |
| Reconnect path | `cmd/connect.go` (≈571–766) | Full |
| DCV-aware idle detection | `pkg/agent/agent.go` `isIdle()` | Full, **unbounded grace** |
| App catalog | `libs/catalog/catalog.yaml` | Full; AMIs only for paraview/chimerax |
| Packer DCV AMIs | `spore-host/infra/amis/` | Partial (built mainly for some regions) |
| Idle-detection unit tests | `pkg/agent/agent_test.go` | Present (4 tests) |
| Handshake integration test | — | **None** (needs a real DCV instance) |

The catalog has GPU AMIs for **paraview** and **chimerax** across ~9 regions;
**igv / qgis / fiji / ds9** have `amis: {}` and fall back to a stock AL2023 image
with no DCV installed (so those apps can't actually stream today without a
manual install).

---

## The handshake, end to end

1. `spawn app launch <app>` resolves the app in the catalog, picks an AMI
   (catalog GPU AMI, else AL2023 fallback), sets up the spored IAM role, builds
   DCV user-data, and launches with `spawn:dcv-session-id` set.
2. On the instance, user-data: updates spored from S3 → installs the wildcard TLS
   cert from `s3://spawn-certs-<region>/...` → starts `dcvserver` → starts
   `spored monitor` → `dcv create-session`.
3. spored's `NewAgent()` sees `DCVSessionID != ""` and fires
   **`go setupDCVAuth()`** (one goroutine, once).
4. `setupDCVAuth()`: starts the `:8444` verifier → polls `dcv list-sessions` up
   to 36×5s for the session → generates a 32-hex token → writes
   `spawn:ready-url`, `spawn:ready-token`, `spawn:ready-status` tags.
5. The CLI polls `ListInstances` up to 60×5s for `spawn:ready-url`, extracts the
   token, and writes a local session HTML that redirects the browser to
   `https://<host>:8443/?authToken=<token>#<session>`.
6. The browser hits DCV on `:8443`; DCV POSTs the token to spored's `:8444`
   verifier, which answers `<auth result="yes">`.

When it works, it works. The problem is every step from 2–4 can fail invisibly.

---

## Root-cause analysis: why it churned

### 1. Failures are silent and indistinguishable (the core problem)
- `setupDCVAuth()` (`agent.go:707`): `out, _ := exec.Command("dcv", "list-sessions").Output()` — **the error is discarded.** If `dcv` isn't on PATH or `dcvserver` isn't up, the loop just spins for the full 3 minutes.
- After 36 failed polls the loop **falls through and writes a `ready-url` anyway** (`agent.go:706–712` → `733`) — for a session that may not exist. So "success" (a tag is written) and "DCV never came up" look identical to the CLI.
- The CLI's only output on any failure is `(timed out — DCV login screen will appear)` (`cmd/app.go:308`). IAM-denied tag write, missing `dcv` binary, crashed server, missing TLS cert, and a simple timing race **all produce that one line.**
- **Effect:** every debug cycle = launch a real instance (~3–4 min), see the same opaque timeout, SSH in to guess the layer, tweak, relaunch. That is the "around and around."

### 2. Fire-once, no retry
- `setupDCVAuth()` runs as a single startup goroutine (`agent.go:157`); it is **not** re-driven by the monitor loop. Any transient failure (slow `dcvserver`, a momentary `ec2:CreateTags` throttle) is **permanent** — the instance never gets a `ready-url`.
- `writeReadyTags()` correctly skips setting `dcvReadyURLWritten` on a CreateTags error (so it *could* retry) — but nothing calls it again.

### 3. Idle detection can hang "not idle" forever
- In `isIdle()` (`agent.go:818–823`): when the X11 activity file is absent and `getDCVConnectionCount()` returns `-1` (DCV not ready / `dcv` missing / parse error), it returns **not-idle with no time cap.** If DCV never becomes healthy, the instance **never idles and bills until TTL** — a silent cost leak that looks like "idle timeout is broken."

### 4. Two unsynchronized timers
- CLI polls 5 min; spored writes after ≤3 min (longer if `dcv` is missing). There's no shared state machine — the CLI can give up before spored writes, producing non-deterministic "sometimes it works."

### 5. Untestable without real hardware
- The only DCV tests are 4 idle-detection unit tests. The **handshake itself has no test**, because the token/session logic is entangled with shelling out to `dcv` and to live EC2. So there's no fast feedback loop at all — only the slow real-instance one.

### 6. AMI/environment reality is uneven
- Only paraview/chimerax have AMIs; other apps fall back to a DCV-less AL2023 image. The repeated "rebuilt … all fixes" AMI comments show display-stack churn (Xorg/DCV-GL/kiosk-wm) that is **separate from** the handshake churn — two different loops that were easy to conflate when both surfaced as "it didn't work."

---

## Recommended approach (sequence, each independently shippable)

The throughline: **make failure legible and the logic testable off-instance
first; do the real-instance run last, as a confirmation, not a hunt.**

1. **Distinct, recorded failure states.** Replace the silent `out, _ :=` and the
   fall-through-after-36-polls with explicit outcomes — `dcv-not-installed`,
   `dcvserver-not-running`, `session-never-created`, `tag-write-denied`,
   `ready` — and write the reason into `spawn:ready-status`. The CLI then reports
   *why* instead of one generic timeout. (Pure observability; low risk.)
2. **Retry + bounded idle grace.** Drive `setupDCVAuth` (or just the
   tag-write/health probe) from the monitor loop so transient failures recover;
   cap the idle-detection grace window so a never-ready DCV eventually falls back
   to CPU/network idle checks instead of billing forever.
3. **Extract and unit/Tier-0 test the state machine.** Separate the decision
   logic (token gen, status selection, "is the session present?") from the `dcv`
   shell-out and the EC2 calls, so it's unit-testable; add a Tier 0 test of the
   CLI poll + tag read against Substrate (the tagging path is already proven by
   the #259 cleanup tests).
4. **One real-instance confirmation run** on a known-good AMI (paraview in
   us-east-1). With steps 1–3 in place, this either works or tells you the exact
   failing layer — minutes, not days.

Step 0, before any of this: **confirm the test environment** — which catalog
AMIs are still valid, and whether a paraview/chimerax launch in us-east-1 still
streams. If the AMIs have rotted, that's a *separate* (infra/amis) track from the
handshake and should not be debugged through the CLI.

---

## Open questions for the maintainer

- Is the goal browser-based streaming of the curated catalog apps, or a general
  "bring any app" `--app`/DCV mode? (Affects how much catalog/AMI work is in
  scope vs. just the handshake.)
- Are the paraview/chimerax AMIs still good, or is an AMI rebuild its own
  prerequisite task?
- Is `spawn:ready-token` (#289) meant to fully replace the DCV login screen
  (seamless auth), or augment it? `cmd/app.go:522` notes the token path was left
  "empty until #289".

*No code was changed in producing this note.*
