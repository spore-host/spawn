# Release tap token (Homebrew / Scoop auto-publish)

The release workflow (`.github/workflows/release.yaml`) runs GoReleaser, whose
last phase pushes the updated formula/manifest to the tap repos so `brew upgrade`
/ `scoop update` serve the new version. That push authenticates with two repo
secrets on `spore-host/spawn`:

- `HOMEBREW_TAP_GITHUB_TOKEN` → pushes to `spore-host/homebrew-tap`
- `SCOOP_BUCKET_GITHUB_TOKEN` → pushes to `spore-host/scoop-bucket`

## Why this doc exists (spawn#280)

These were a **classic PAT that silently expired** between releases: set
2026-05-27, worked for v0.68.1 (06-25), then 401'd on the *next* release (v0.69.0,
07-08) — the tap push failed and (before PR #281) also skipped the spored S3
upload. They're currently re-set to a working classic `repo`-scoped token, but
that is over-scoped and account-tied. Replace it with a least-privilege
**fine-grained PAT** and set an expiry reminder.

## Request — mint the fine-grained PAT (org owner / maintainer, ~2 min)

`spore-host` is a GitHub **Organization**, so this must be a fine-grained PAT with
the org as resource owner (and may need org-owner approval if the org gates PAT
access).

1. GitHub → Settings → Developer settings → **Fine-grained tokens** → **Generate new token**.
2. **Resource owner:** `spore-host`.
3. **Repository access → Only select repositories:** `spore-host/homebrew-tap`
   **and** `spore-host/scoop-bucket` (both).
4. **Permissions → Repository permissions → Contents: Read and write.** Nothing
   else (Metadata:read is auto-added — fine). This is all GoReleaser's
   clone→commit→push needs.
5. **Expiration:** pick a date and **set a calendar reminder ~1 week before** to
   rotate — the whole point is to not let it lapse unnoticed again.
6. Generate; copy the token (`github_pat_…`). If the org gates fine-grained PATs,
   an org owner approves the request under Org → Settings → Personal access tokens.

## Install the token (either you or hand it to the assistant)

Set both secrets on `spore-host/spawn` to the new PAT:

```bash
printf '%s' "<the-new-PAT>" | gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo spore-host/spawn
printf '%s' "<the-new-PAT>" | gh secret set SCOOP_BUCKET_GITHUB_TOKEN --repo spore-host/spawn
```

(One PAT scoped to both repos can back both secrets; or mint two if you prefer
per-tap isolation.)

## Verify

The token must succeed on the exact call GoReleaser makes first (a 401 here is
what failed v0.69.0):

```bash
GH_TOKEN=<the-new-PAT> gh api repos/spore-host/homebrew-tap --jq .default_branch   # -> main
GH_TOKEN=<the-new-PAT> gh api repos/spore-host/scoop-bucket --jq .default_branch   # -> main
```

Both returning `main` (not 401) means the next `v*` tag will auto-publish the taps.
No need to re-run a release just to test — the next release exercises it.
