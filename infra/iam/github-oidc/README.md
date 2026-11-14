# GitHub Actions OIDC trusts and the spored signing key

The release and the Lambda deploy reach AWS through GitHub's OIDC provider, by
way of two roles created by hand in 966362334030. `GitHubActions-spore-host-release`
also holds `kms:Sign` on the spored signing key, and that key is the
authenticity root for every spored binary, spored manifest and bootstrap body
an instance accepts (`pkg/sporedinstall.SigningKeyPEM`). These files are the
reviewable record of what the live roles and key must allow (#1601):

- `trusts.json` holds the expected trust policy of each role. It uses the same
  format as spore-host's `infra/iam/github-oidc/trusts.json` (spore-host#709),
  which lists these two roles as well. CI fails if the two copies disagree.
- `signing-key.json` names the key, the only principals that may sign with it,
  and the Deny that enforces that limit when the key policy delegates to IAM.

| Role | `sub` it must pin | Assumed by | Codified |
|---|---|---|---|
| `GitHubActions-spore-host-release` | StringLike `repo:spore-host/spawn:ref:refs/tags/v*` | `release.yaml` (a push of a `v*` tag) | no, hand-managed |
| `GitHubActions-spawn-lambda-deploy` | `repo:spore-host/spawn:environment:production` | `deploy-lambdas.yaml` (`production` environment) | `scripts/setup-lambda-deploy-ci-role.sh` |

Every trust follows the same rules:
- one Allow statement;
- the principal is `token.actions.githubusercontent.com` in the role's own account;
- the action is `sts:AssumeRoleWithWebIdentity`;
- `aud` is StringEquals `sts.amazonaws.com`;
- `sub` is StringEquals one exact ref or environment of one repository.

The one StringLike allowed is the tag name, for the tag-triggered release. A
`sub` of `repo:spore-host/spawn:*` or `refs/heads/*`, or an `aud`-only trust,
would let any branch, including a Dependabot branch or a maintainer's
scratch branch, obtain `kms:Sign`. spawn does not customize the OIDC sub claim:
`gh api repos/spore-host/spawn/actions/oidc/customization/sub` reports
`use_default: true`, so the default forms above are the ones GitHub issues.

## In CI

- `go -C scripts run ./oidctrust check` checks both files against the rules,
  and the signers and the Deny against each other. It makes no network call.
- `oidc_trust_test.go` checks the workflows against these files:
  - every role a workflow assumes is listed here;
  - a tag-`sub` role is assumed only in a workflow whose sole trigger is a push
    of the tags it covers, by a job with no environment;
  - an environment-`sub` role is assumed only by a job in that environment;
  - no such workflow runs on a pull request;
  - `aws kms sign` runs only after the job assumes a listed signer;
  - every workflow listed in `used_by` really assumes its role.
- The `oidc_trust_sync` step checks that spore-host's copy lists both roles
  with the same trusts.

## Comparing against live (read-only)

The comparison needs `iam:GetRole` and `kms:GetKeyPolicy` in 966362334030. The
checker makes no AWS call itself. Run this from the repository root:

```bash
aws iam get-role --role-name GitHubActions-spore-host-release  > /tmp/release.json
aws iam get-role --role-name GitHubActions-spawn-lambda-deploy > /tmp/lambda.json
aws kms get-key-policy --region us-east-1 --policy-name default \
  --key-id arn:aws:kms:us-east-1:966362334030:key/7b28df72-c92f-4d0c-97e3-7696e04bafd3 > /tmp/key-policy.json
for id in $(gh api repos/spore-host/spawn/rulesets --jq '.[].id'); do
  gh api "repos/spore-host/spawn/rulesets/$id"
done | jq -s . > /tmp/rulesets.json

go -C scripts run ./oidctrust live \
  --role GitHubActions-spore-host-release=/tmp/release.json \
  --role GitHubActions-spawn-lambda-deploy=/tmp/lambda.json \
  --key-policy /tmp/key-policy.json \
  --rulesets /tmp/rulesets.json
```

`live` reports four kinds of finding:

- **Trust differences.** For each role, any statement present on one side and
  not the other, plus any rule the live trust breaks.
- **Key policy.** Every Allow that reaches `kms:Sign`, whether by `kms:Sign`,
  `kms:*`, a wildcard or a `NotAction`, must name only a signer, or the account
  root. If it names the account root (`"Enable IAM User Permissions"` in the
  default key policy), the policy must also hold `required_deny`. Without that
  Deny, any role or user in 966362334030 whose identity policy allows
  `kms:Sign` or `kms:*` can sign spored.
- **Tag ruleset.** An active ruleset with target `tag`, covering
  `refs/tags/v*`, with the `creation`, `update` and `deletion` rules. The
  release role trusts any `v*` tag, so whoever can create one can sign. On
  2026-11-14, `gh api repos/spore-host/spawn/rulesets` returned `[]` and `main`
  had no branch protection, so every account with write access to spawn could
  push a `v*` tag.
- **Signer identity policy.** This is not compared. Read it by hand:
  `aws iam list-role-policies` / `get-role-policy` on the release role. Its
  `kms:Sign` should name only this key.

Fix whichever side is wrong. If the live role or key allows more than these
files list, fix the live side. If the trust was deliberately changed, update
this file and spore-host's copy, in reviewed PRs.

### Closing the gaps found (owner actions)

Add the Deny to the key policy. Write the live policy with `required_deny`
appended into `/tmp/new-policy.json`, then run:

```bash
aws kms put-key-policy --region us-east-1 --policy-name default \
  --key-id arn:aws:kms:us-east-1:966362334030:key/7b28df72-c92f-4d0c-97e3-7696e04bafd3 \
  --policy file:///tmp/new-policy.json
```

The Deny covers only `kms:Sign`, so it cannot lock anyone out of the key's
administration.

Then protect `v*` tags so only repository admins can create, move or delete
them:

```bash
gh api -X POST repos/spore-host/spawn/rulesets --input - <<'JSON'
{"name": "release tags", "target": "tag", "enforcement": "active",
 "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
 "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}],
 "bypass_actors": [{"actor_type": "RepositoryRole", "actor_id": 5, "bypass_mode": "always"}]}
JSON
```

`actor_id` 5 is the repository admin role. Once this ruleset is in place, a
leaked token with only `contents: write` can no longer create a release tag
(#1602).

A stronger alternative to the tag `sub`: run the release job in a `release`
environment with a required reviewer and a `v*` tag deployment policy, as the
Lambda deploy already does, and change the trust's `sub` to
`repo:spore-host/spawn:environment:release`. That puts a human approval
between a tag and `kms:Sign`, at the cost of approving each release. This
inventory, spore-host's copy and `oidc_trust_test.go` would change in the
same PR.
