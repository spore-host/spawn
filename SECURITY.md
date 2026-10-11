# Reporting a security issue in spawn

**Please do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting:
**[Report a vulnerability](https://github.com/spore-host/spawn/security/advisories/new)**.
It is enabled on this repository, so the report stays private between you and the
maintainers until a fix is published.

If that is unavailable to you, open a public issue containing **only** the words
"security report, need a private channel" and nothing else, and a maintainer will
open a private advisory and invite you to it.

## What is in scope

`spawn` launches and manages real, billable AWS infrastructure, so the
interesting surface is wider than the CLI binary:

- the `spawn` CLI and the `spored` in-instance agent
- the IAM policies and roles `spawn` **generates** (`pkg/aws/iam.go`,
  `cmd/onboard.go`, the `scripts/setup-*.sh` role creators) — an over-broad
  generated policy is a vulnerability even though no AWS resource is misconfigured
  in this repository
- the Lambda functions under `lambda/` and the CloudFormation templates under
  `deployment/`
- the EC2 user-data and bootstrap scripts `spawn` renders (`pkg/userdata/`,
  `pkg/launcher/bootstrap.go`)
- the `spored` push API, the `:443` session proxy, and plugin installation

Particularly welcome: anything that lets an instance escape its own lifecycle
controls, lets one tenant reach another's resources, or lets code run on an
instance without a verified signature.

## What is out of scope

- Findings that require credentials you were already given, acting within the
  permissions they grant.
- Cost: an expensive-but-correct launch is not a vulnerability. A launch that
  evades TTL, idle-stop or the out-of-band reaper **is** — those are the
  controls that bound spend.
- Reports against a spore.host-operated AWS account rather than this code.
  Account configuration is not in this repository; report it the same way and it
  will be routed.

## What to expect

There is no formal SLA — this is a small project. A report will be
acknowledged, and if it is valid you will be credited in the advisory unless you
ask otherwise.

## Security model

`spawn` never holds your AWS credentials and never runs your compute: everything
runs in your own account with your own credentials. The full trust and data-flow
map lives in the suite-level overview,
[spore-host/SECURITY.md](https://github.com/spore-host/spore-host/blob/main/SECURITY.md).

Supporting detail for the pieces most often asked about:

- what is deployed in the shared infra account, and which code owns each
  piece — [`docs/infra-account.md`](docs/infra-account.md)
- lifecycle enforcement and the out-of-band reaper —
  [`lambda/ttl-reaper/README.md`](lambda/ttl-reaper/README.md)
