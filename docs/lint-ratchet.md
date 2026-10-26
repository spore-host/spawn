# golangci-lint ratchet

`.golangci.yml` does not describe the lint policy spawn wants. It describes the
policy spawn **passes today**, and it only moves in one direction (#1028).

- CI's Lint job runs `make lint` (golangci-lint, version and sha256 pinned in
  `.github/workflows/ci.yml`, the same version as `GOLANGCI_LINT_VERSION` in the
  Makefile) over the root module and every nested module (`scripts/lint.sh`).
- `lint_ratchet_test.go` fails if `.golangci.yml` enables a different set of
  linters or formatters from the lists below, sets `default` to anything but
  `none`, uses `disable`, adds path exclusions or exclusion rules, or stops
  being run by CI.

So a linter cannot be switched off without editing this file in the same
reviewed PR, and switching one on means recording it here, which raises the
floor.

## To widen

1. Pick a linter from the tables below and fix its findings, in every module
   (`golangci-lint run --default=none -E <linter> ./...` in each).
2. Add it to `linters.enable` in `.golangci.yml`.
3. Move it from its table into the "Enabled" list below.
4. `make lint` and `go test -run 'TestGolangci|TestLintGate' .`

One linter per PR keeps each review about one kind of change. If a finding is a
real bug rather than a style point, fix it in its own PR with a test, and leave
the linter off until then (as `nilnesserr` was until #1211).

## Enabled (the floor)

<!-- ratchet:enabled:begin -->
- `asasalint`
- `asciicheck`
- `bidichk`
- `copyloopvar`
- `durationcheck`
- `exptostd`
- `fatcontext`
- `gocheckcompilerdirectives`
- `gochecksumtype`
- `govet`
- `ineffassign`
- `loggercheck`
- `makezero`
- `mirror`
- `misspell`
- `nilnesserr`
- `nolintlint`
- `reassign`
- `recvcheck`
- `sloglint`
- `testifylint`
- `tparallel`
- `unconvert`
- `wastedassign`
<!-- ratchet:enabled:end -->

Formatters:

<!-- ratchet:formatters:begin -->
- `gofmt`
<!-- ratchet:formatters:end -->

`misspell` ignores three words, each a deliberate misspelling in a test; the
reasons are in `.golangci.yml`. `govet`, `copyloopvar`, `unconvert` and
`wastedassign` had 2, 9, 6 and 5 findings when this was committed; they were
fixed (behaviour-neutral) to enable them.

## Still off

Counts are findings from golangci-lint v2.15.0 with that linter's default
settings, uncapped (`max-issues-per-linter: 0`, `max-same-issues: 0`,
`uniq-by-line: false`), measured 2026-10-19 on the commit that added this file.
"root" is the root module; "nested" is the nine modules under `lambda/` and
`scripts/` combined. They go stale as code changes, so re-measure before
starting on one.

### golangci-lint's `standard` set

These are the linters golangci-lint enables by default, so anyone who runs it
without this config sees them.

| linter | root | nested | plan |
|---|---:|---:|---|
| `errcheck` | 468 | 8 | Most are unchecked `fmt.Fprint*` to a terminal (#1028 measured 320 of 358). Decide whether to exclude those via `errcheck.exclude-functions` (a reviewed narrowing, recorded here), then fix the rest: `Close`, `os.Setenv` in tests and so on. |
| `staticcheck` | 32 | 4 | Already gated in CI by the standalone `staticcheck` (#923), which honours the repo's 12 justified `//lint:ignore` directives; golangci-lint does not, so 17 of these (SA1019, ST1005) are those suppressions. The other 19 are QF quick-fixes (QF1001 De Morgan, QF1011, QF1002/3/8), which the standalone tool does not run. Fix the QF findings, then either restrict golangci's `staticcheck.checks` to the QF family or duplicate the suppressions as `//nolint:staticcheck` with the same reasons. |
| `unused` | 0 | 4 | All four are in `lambda/dashboard-api/slack_token.go` (`getFreshBotToken` and three attribute helpers), the Slack token-rotation path. Unreferenced is not the same as unwanted: decide whether that path is meant to be wired in before deleting it. |

### Next candidates (correctness, small counts)

| linter | root | nested | notes |
|---|---:|---:|---|
| `nosprintfhostport` | 2 | 0 | `cmd/connect.go`, `pkg/agent/dcv.go`: `https://%s:port` URLs; an IPv6 host would need brackets. |
| `canonicalheader` | 1 | 0 | One test header spelled as AWS spells it. |
| `bodyclose` | 7 | 0 | `cmd/plugin.go` and its test; check each before trusting it — bodyclose has false positives on bodies closed elsewhere. |
| `errorlint` | 23 | 0 | `==` / type assertions on errors that may be wrapped. |
| `nilerr` | 21 | 17 | Returns nil after checking a non-nil error; some are intended (best-effort paths), so each needs a reason. |
| `predeclared` | 19 | 2 | Locals and helpers named `min`, `max`, `any`, `close`, `real`, `new`. Mechanical renames. |
| `thelper` | 13 | 0 | Test helpers without `t.Helper()`. |
| `dupword` | 8 | 0 | Repeated words, mostly in comments. |
| `usetesting` | 32 | 2 | `os.MkdirTemp`/`os.Setenv` in tests where `t.TempDir`/`t.Setenv` exist. |
| `usestdlibvars` | 15 | 23 | Literal HTTP methods/status codes. |
| `intrange` | 103 | 7 | `for i := 0; i < n; i++` → `for i := range n`. Mechanical. |
| `noctx` | 131 | 7 | HTTP requests and `exec.Command` without a context. |
| `unparam` | 53 | 12 | |
| `gocritic` | 42 | 3 | |
| `contextcheck` | 30 | 13 | |
| `exhaustive` | 28 | 5 | |
| `errchkjson` | 34 | 12 | |
| `gosec` | 498 | 22 | semgrep already runs in CI; overlap needs sorting out first. |
| `revive` | 452 | 21 | Needs a chosen rule set; the default rules are mostly style. |
| `modernize` | 915 | 72 | Mostly mechanical (`golangci-lint run --fix`), but large; split by analyzer. |

### Not planned

Style or opinion linters, which would need a decision that spawn wants that
style before their counts mean anything. Listed so the record is complete:
`wsl_v5` 14943/1491, `exhaustruct` 4286/480, `nlreturn` 3230/401,
`paralleltest` 2992/285, `goconst` 2354/97, `varnamelen` 2127/189,
`noinlineerr` 2034/150, `lll` 1736/190, `depguard` 1529/255 (needs a config to
mean anything), `mnd` 950/211, `err113` 880/73, `gochecknoglobals` 803/54,
`godot` 675/116, `tagliatelle` 597/162, `forbidigo` 653/27, `cyclop` 494/46,
`wrapcheck` 499/24, `testpackage` 468/2, `perfsprint` 337/49, `funlen` 270/30,
`nonamedreturns` 222/23, `tagalign` 101/31, `gocognit` 105/10, `funcorder`
111/1, `inamedparam` 98/9, `nestif` 65/3, `gochecknoinits` 57/7, `prealloc`
34/2, `musttag` 28/3, `forcetypeassert` 26/1, `gocyclo` 23/2, `maintidx` 22/1,
`nilnil` 19/2, `dogsled` 20/0, `embeddedstructfieldcheck` 9/3, `godox` 8/2,
`ireturn` 7/2, `godoclint` 6/2, `errname` 7/0, `dupl` 4/2, `gosmopolitan` 6/0,
`whitespace` 5/0, `containedctx` 3/0, `interfacebloat` 3/0,
`testableexamples` 2/0, `goprintffuncname` 1/0, and `gomoddirectives` 0/3
(it rejects the nested modules' `replace ../..`, which is how they build).

Linters that report nothing but check for libraries spawn does not use
(`ginkgolinter`, `zerologlint`, `promlinter`, `protogetter`, `rowserrcheck`,
`sqlclosecheck`, `spancheck`, and so on) are left off. Enabling them would add
entries without adding checks.

## Not covered

- **Build tags.** golangci-lint loads the default build only, so files behind
  `//go:build integration` or `e2e_tier*` are not linted. They are vetted by
  `scripts/check-build-tags.sh`.
- **Version bumps.** Raising the pinned golangci-lint can add checks inside an
  enabled linter (`govet`'s `inline` analyzer, for example, reported 2 findings
  at v2.15.0 that `go vet` does not). Fix those in
  the bump's PR.
