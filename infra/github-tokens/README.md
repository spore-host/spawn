# Cross-repository GitHub tokens

`tokens.json` lists every secret other than `GITHUB_TOKEN` that spawn's
workflows read (#1602), with what each token may reach. GitHub doesn't show a
secret's token type or scope to a workflow or through the API, so this file is
the reviewable record and the live tokens are compared against it by hand.

Why it matters: unlike `GITHUB_TOKEN`, a push, tag or dispatch made with a PAT
or an App token **does** trigger workflows. If a token that can write
`spore-host/spawn` leaked from any job holding it, the holder could push a
`v*` tag. That runs `release.yaml`, which assumes
`GitHubActions-spore-host-release` and calls `kms:Sign` on the key every
spawn instance trusts (`infra/iam/github-oidc`, #1601).

| Secret | Target (only) | Permissions | Read by |
|---|---|---|---|
| `HOMEBREW_TAP_GITHUB_TOKEN` | `spore-host/homebrew-tap` | Contents: write, Metadata: read | `release.yaml` step "Run GoReleaser" |
| `SCOOP_BUCKET_GITHUB_TOKEN` | `spore-host/scoop-bucket` | Contents: write, Metadata: read | `release.yaml` step "Run GoReleaser" |
| `DOCS_SYNC_TOKEN` | `spore-host/spore-host` | Contents: write (what `POST /dispatches` needs), Metadata: read | `release.yaml` step "Notify docs site of new release" |

## In CI

`github_tokens_test.go` fails in each of these cases:
- **An unlisted secret.** A workflow reads a secret, other than `GITHUB_TOKEN`, that isn't listed here.
- **A token broader than one repository.** A listed token is a classic PAT, reaches `spore-host/spawn`, reaches more than one repository, or holds a permission other than contents/pull-requests write or metadata read.
- **A token read outside its listed steps.** A token is read anywhere except the step `env:`/`with:` of the steps listed in `used_by`, for example in a job- or workflow-level `env:`, where every step and third-party action would see it.
- **A stale `used_by`.** A listed step no longer reads the token.
- **A pull-request trigger.** A workflow that reads one of these tokens runs on a pull request.

## Comparing against live (owner, about 5 minutes)

1. **Fine-grained tokens with access to the org.** Go to
   <https://github.com/organizations/spore-host/settings/personal-access-tokens/active>.
   For each token behind one of these secrets, check:
   - *Repository access* is exactly the one target in the table;
   - *Permissions* are exactly the ones in the table;
   - an expiry is set.

   A token that isn't on this page is not a fine-grained token on the
   org, so look for it under
   <https://github.com/settings/tokens> (classic). A classic token fails this
   inventory whatever its scopes. Its `repo` scope reaches every repository
   its owner can write, and when the owner is a spawn admin it also bypasses
   any tag ruleset. `docs/release-tap-token.md` records that the tap tokens
   were re-set to a classic `repo` token after spawn#280, so expect to find
   one.
2. **If you still have a token's value,** run this:
   ```bash
   curl -sI -H "Authorization: Bearer $T" https://api.github.com/rate_limit \
     | grep -iE '^(x-oauth-scopes|github-authentication-token-expiration):'
   ```
   An `x-oauth-scopes:` header means the token is classic, and the header lists
   its scopes. A fine-grained token sends no such header.
3. **Org PAT policy.** Under Settings → Personal access tokens → Settings, set
   *Restrict access via personal access tokens (classic)* and *Require
   approval of fine-grained personal access tokens*. That stops a classic token
   from reaching any spore-host repository, whoever owns it.
4. **Who can read each secret.** All three are org secrets with selected-repository
   visibility, not secrets on spawn. On 2026-11-14:
   ```bash
   gh api orgs/spore-host/actions/secrets/DOCS_SYNC_TOKEN/repositories --jq '.repositories[].full_name'
   # calque lagotto spawn spore-host truffle
   gh api orgs/spore-host/actions/secrets/HOMEBREW_TAP_GITHUB_TOKEN/repositories --jq '.repositories[].full_name'
   # calque lagotto spawn spore-host-mcp truffle   (SCOOP_BUCKET_GITHUB_TOKEN: the same)
   ```
   Every repository on these lists is somewhere the token can leak from. Drop
   any that doesn't publish to that target.

The `v*` tag ruleset in `infra/iam/github-oidc/README.md` limits the damage
from a leaked token whose owner is not a bypass actor. It does not replace
scoping the tokens, because an admin-owned classic token bypasses the ruleset.

## Replacing them (proposed)

- **`DOCS_SYNC_TOKEN` can go entirely.** spore-host's `sync-cli-docs.yaml`
  could poll on a schedule (hourly, say) for a spawn, truffle or lagotto
  release newer than the one it vendored, using its own `GITHUB_TOKEN`. Then no
  release job holds a token for spore-host at all, and the secret can be
  deleted from every repository above.
- **The tap and bucket tokens become a GitHub App.** Install the App only on
  `homebrew-tap` and `scoop-bucket`, and **never on spawn**. The release job
  mints a token per run with `actions/create-github-app-token`
  (`repositories: homebrew-tap` / `scoop-bucket`, `permission-contents:
  write`). Each token lasts an hour, belongs to no person, and can't silently
  expire between releases, which is how spawn#280 happened. The App's private
  key becomes the secret. It can mint tokens only for the repositories the App
  is installed on.
- `GITHUB_TOKEN` can't replace any of the three: it can't write another
  repository or dispatch to one.

Each replacement changes `tokens.json` (`kind: github-app`, or the entry
removed) in the same PR as the workflow.
