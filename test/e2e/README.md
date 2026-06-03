# spawn end-to-end test tiers

Four independently runnable tiers, ordered by fidelity and cost. Each drives the
**real spawn binary** (not in-process helpers) so the actual user surface —
argument parsing → cobra → `RunE` → AWS client → AWS API — is exercised.

| Tier | Backend | Cost | What it covers |
|------|---------|------|----------------|
| **0** | Substrate emulator | $0, no AWS account | Broad CLI surface: every command's happy path, JSON output, exit codes, and resulting control-plane state. Runs in CI on every push/PR. |
| **1** | Real AWS, API-only | ~$0 | Things needing real AWS API responses (pricing, quotas, instance-type offerings); `--estimate-only`; EFA/placement-group region regressions. |
| **2** | Real AWS, 1 instance | ~$1 | Instance/SSH/spored-dependent surface: `connect`, remote `config`, `extend`→reload, completion polling, `--command`/`--on-complete`/`--pre-stop`, plugin install on a live instance. |
| **3** | Real AWS, N instances | $2–$5 | MPI + placement groups, job arrays, real sweeps, queue execution, slurm submit. |

## Running

```bash
make test-e2e-tier0          # no AWS account needed
make test-e2e-tier1          # AWS_PROFILE=spore-host-dev (or env creds)
make test-e2e-tier2
make test-e2e-tier3
```

Tiers 1–3 require AWS credentials and a built binary (`make build`). Tier 0 builds
the binary itself and needs no credentials.

## Tier 0 — how it works

spawn's AWS client uses `config.LoadDefaultConfig`, which honors the SDK v2
`AWS_ENDPOINT_URL` env var. The harness (`substrate_cli.go`) starts a Substrate
server and runs the spawn binary with `AWS_ENDPOINT_URL` pointed at it, so the
binary's real AWS calls hit the emulator. Tests assert stdout JSON, exit codes,
and resulting emulator state (e.g. `DescribeInstances` shows the launched
instance, then gone after `terminate`).

### What Tier 0 does NOT cover (by design)

Substrate emulates the AWS **control plane**, not instance semantics. The
following are exclusive to Tiers 2–3:

- Real instance boot, SSH, `spored`, and user-data execution.
- Completion-file polling and `status --check-complete` round-trips on a live
  instance.
- Capacity exhaustion / real pricing.
- SQS-backed queue/orchestrator paths (SDK v2 ↔ Substrate SQS protocol mismatch).

Tier 0 asserts **spawn's behavior given AWS responses** — exactly the
internal-bug surface (e.g. it caught a nil-pointer panic on a `RunInstances`
response that omitted `Placement`). It does not assert AWS itself.
