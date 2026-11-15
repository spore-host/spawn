# Release tap token (Homebrew / Scoop auto-publish)

The release workflow (`.github/workflows/release.yaml`) runs GoReleaser. Its
last phase pushes the updated formula and manifest to the tap repos, so that
`brew upgrade` and `scoop update` serve the new version.

Since 2026-11-15 that push authenticates with a token from the GitHub App
**`spore-host-release`**. The step "Mint the tap and bucket token" mints it
with `actions/create-github-app-token`, just before GoReleaser runs:

- `owner: spore-host`, `repositories: homebrew-tap,scoop-bucket`;
- `permission-contents: write`, and nothing else;
- it lasts one hour and belongs to no person.

The app's credentials are org-level and readable only by the five repos that
publish to the tap (spawn, truffle, lagotto, spore-host-mcp, calque):

- `RELEASE_APP_PRIVATE_KEY`, an org secret;
- `RELEASE_APP_CLIENT_ID`, an org variable, not secret.

The expected scope is in
[`infra/github-tokens/tokens.json`](../infra/github-tokens/tokens.json), and CI
holds the workflow to it.

## Why (spawn#280)

The tap tokens used to be personal access tokens, which **expired silently**
between releases. One set on 2026-05-27 worked for v0.68.1 (06-25) and
returned 401 on the next release (v0.69.0, 07-08): the tap push failed, and
before PR #281 the spored S3 upload was skipped too. The replacement was a
classic `repo`-scoped token, which is over-scoped and tied to an account.

A token minted per run can't expire between releases.

## The app's installation must never include spore-host/spawn

A token that can write spawn could push a `v*` tag, and with it reach the
spored signing key (#1602). Every workflow in five repositories can read this
key. So the app is installed on `homebrew-tap` and `scoop-bucket` only.
`TestAppTokensAreMintedNoWiderThanTheInventory` fails a minting step that
leaves `repositories:` unset, because it would then mint for spawn.

## Rotating the key

1. Org → Settings → Developer settings → GitHub Apps → `spore-host-release` →
   **Generate a private key**.
2. Set it:
   `gh secret set RELEASE_APP_PRIVATE_KEY --org spore-host --visibility selected --repos spawn,truffle,lagotto,calque,spore-host-mcp < key.pem`
3. Delete the old key on the app's page, then delete `key.pem`.

## Verify

A release's "Mint the tap and bucket token" step fails loudly in either case:

- the app isn't installed on a repository it names;
- the key or client ID is wrong.

If the step is green and GoReleaser's brew and scoop phases are green, the
taps were published.
