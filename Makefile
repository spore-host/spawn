.PHONY: all build build-spawn build-spored build-all clean install test test-coverage test-short test-integration test-e2e test-e2e-tier0 test-e2e-tier1 test-e2e-tier2 test-e2e-tier3 check check-fmt vuln gen-docs check-docs check-release-version

# Version
VERSION ?= 0.1.0

# Build directory
BUILD_DIR = bin

all: build

# Regenerate the committed command/flag reference fragments (docs-gen/) from the
# CLI itself. Run after adding/renaming/removing a command or flag; the docs
# drift gate (check-docs) fails CI otherwise.
gen-docs:
	go run . gen-docs --out docs-gen

# Drift gate: regenerate and fail if the committed reference is stale. Run in CI
# on PRs and before a release so the reference can never ship out of sync.
check-docs: gen-docs
	git diff --exit-code docs-gen/ || { echo "::error::docs-gen/ is stale — run 'make gen-docs' and commit"; exit 1; }

# Regenerate docs/reference/cli-iam-actions.md, the IAM actions each spawn
# command can call, discovered from the code (internal/iamcoverage, #254). The
# test of the same name fails when the committed copy is stale.
.PHONY: cli-iam-actions
cli-iam-actions:
	go test ./internal/iamcoverage -run TestCLIActionInventoryIsCurrent -update -count=1
	@echo "docs/reference/cli-iam-actions.md regenerated — review the diff before committing."

# Regenerate the committed snapshot of the exported API that OTHER REPOS compile
# against (api/public-surface.txt). Run when a change to one of those packages is
# intended, and commit the result in the same change — the gate in
# api_surface_test.go fails CI otherwise, and reports whether what you changed is
# an addition (safe) or a removal/signature change (not safe in a PATCH). #679
.PHONY: api-snapshot
api-snapshot:
	go test -run TestPublicAPISurface -update .
	@echo "api/public-surface.txt regenerated — review the diff before committing."

# Release guard: build with the real release ldflags and confirm the binaries
# report the tag. Run automatically by the release workflow; run it by hand
# before tagging to catch a stale CHANGELOG without burning a tag.
#   make check-release-version TAG=v0.98.0
check-release-version:
	@scripts/check-release-version.sh $(TAG)

# Build for current platform
build: build-spawn build-spored

build-spawn:
	@echo "Building spawn..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/spawn .

build-spored:
	@echo "Building spored..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/spored ./cmd/spored/

# Build for all platforms
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64

# Linux AMD64 (x86_64)
build-linux-amd64:
	@echo "Building for Linux AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-linux-amd64 .
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/spored-linux-amd64 ./cmd/spored/

# Linux ARM64 (Graviton)
build-linux-arm64:
	@echo "Building for Linux ARM64 (Graviton)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/spawn-linux-arm64 .
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/spored-linux-arm64 ./cmd/spored/

# macOS AMD64 (Intel)
build-darwin-amd64:
	@echo "Building for macOS AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-darwin-amd64 .
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/spored-darwin-amd64 ./cmd/spored/

# macOS ARM64 (M1/M2)
build-darwin-arm64:
	@echo "Building for macOS ARM64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/spawn-darwin-arm64 .
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/spored-darwin-arm64 ./cmd/spored/

# Windows AMD64
build-windows-amd64:
	@echo "Building for Windows AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-windows-amd64.exe .
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/spored-windows-amd64.exe ./cmd/spored/

# Install locally
install: build
	@echo "Installing spawn and spored..."
	@cp $(BUILD_DIR)/spawn /usr/local/bin/
	@cp $(BUILD_DIR)/spored /usr/local/bin/
	@echo "Installed to /usr/local/bin/"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)

# Run tests
test:
	@echo "Running tests..."
	go test -v ./...

# Run tests with coverage
test-coverage:
	@echo "Running tests with coverage..."
	go test -v -coverprofile=coverage.out ./...
	@echo ""
	@echo "Coverage Summary:"
	@go tool cover -func=coverage.out | tail -1
	@echo ""
	@echo "Generating HTML coverage report..."
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report saved to: coverage.html"
	@echo ""
	@echo "To view report: open coverage.html"

# Run short tests only
test-short:
	@echo "Running short tests..."
	go test -v -short ./...

# Run the race detector on the concurrency-sensitive packages (pkg/agent holds
# the monitor loop + background goroutines; 2026-06 audit M-corr). Scoped rather
# than -race on the whole suite to keep it fast and non-flaky.
test-race: hermetic-exec
	@echo "Running race detector on pkg/agent..."
	go test -race -count=1 -exec $(HERMETIC_EXEC) ./pkg/agent/...

# Run integration tests. LAUNCHES REAL, BILLABLE INSTANCES (the root package's
# multi-region sweep tests): requires AWS credentials. Depends on build because
# those tests exec ./bin/spawn, and -count=1 because Go's test cache cannot see a
# binary read at runtime (#831). The test-integration-scheduler and
# test-integration-queue targets were removed with the tests they ran (#913):
# the scheduler tests drove a DynamoDB schema and trigger that never existed, and
# the queue tests were t.Skip placeholders.
test-integration: build
	@echo "Running all integration tests..."
	@echo "Note: These tests require AWS credentials, launch billable instances, and may take 20-30 minutes"
	go test -v -count=1 -tags=integration -timeout 45m ./...

# E2E test suite — four independently runnable tiers.
# Tier 0 needs NO AWS account (runs the real spawn binary against the Substrate
# emulator). Tiers 1–3 require AWS credentials (AWS_PROFILE=spore-host-dev or env
# vars) and a compiled spawn binary at ./bin/spawn (run 'make build' first).

# -count=1 on every tier below is load-bearing, not habit (#831, #792). These
# tests exec bin/spawn at runtime, and Go's test cache cannot see a binary read at
# runtime — so without it a second run reports `ok (cached)` whatever bin/spawn has
# become. Replacing bin/spawn with a script whose whole body is `exit 1` passed the
# entire Tier 0 surface that way. On tiers 2 and 3 it is worse than embarrassing:
# they launch real billable instances, so a cached pass lets a release be cut on a
# run that never touched AWS. TestEveryE2EMakeRecipePassesCountOne gates this.

# Tier 0 — CLI against the Substrate emulator: no AWS account, deterministic,
# safe for CI. Exercises the full command surface (args → cobra → AWS client →
# Substrate) asserting JSON, exit codes, and emulator state.
test-e2e-tier0: build
	@echo "Running E2E tier 0 (CLI against Substrate, no AWS account)..."
	go test -count=1 -tags=e2e_tier0 -timeout 30m ./test/e2e/

# Tier 1 — AWS API-only, no instances launched, ~free, ~5 min
test-e2e-tier1: build
	@echo "Running E2E tier 1 (API-only, no instances)..."
	go test -v -count=1 -tags=e2e_tier1 -run TestTier1 -timeout 10m ./test/e2e/

# Tier 2 — Single instance per test, ~$0.50 total, ~20 min
test-e2e-tier2: build
	@echo "Running E2E tier 2 (single-instance tests, ~\$$1)..."
	go test -v -count=1 -tags=e2e_tier2 -run TestTier2 -timeout 80m ./test/e2e/

# Tier 3 — Multi-instance (job arrays, sweeps, FSx, MPI), ~$2-5 total, ~35 min
test-e2e-tier3: build
	@echo "Running E2E tier 3 (multi-instance tests, ~\$$2-5)..."
	go test -v -count=1 -tags=e2e_tier3 -run TestTier3 -timeout 60m ./test/e2e/

# Run all E2E tiers sequentially
test-e2e: test-e2e-tier1 test-e2e-tier2 test-e2e-tier3

# Formatting gate: REPORT drift, don't silently fix it.
#
# Distinct from `check`, which runs `gofmt -w` and rewrites files in place. That
# is convenient locally but it is not a gate — it mutates the tree and always
# succeeds, so drift committed by an editor that doesn't format on save was never
# caught by anything. Three files sat unformatted on main for months because CI
# had no formatting check at all, and `make check` reformatted them on every run,
# turning them into recurring unrelated diffs in whatever PR came next.
#
# Excludes vendor/ and lists offenders with a diff, so the fix is obvious.
# Mechanical fix for the duplicate-group conflict changelog_test.go rejects. Two
# PRs each adding their own `### Fixed` merge cleanly for git and badly for the
# format; this merges them in Keep-a-Changelog order. Touches [Unreleased] only.
.PHONY: changelog-fix
changelog-fix:
	python3 scripts/changelog-consolidate.py CHANGELOG.md

check-fmt:
	@files=$$(gofmt -l . 2>/dev/null | grep -v '^vendor/' || true); \
	if [ -n "$$files" ]; then \
	  echo "::error::these files are not gofmt-clean — run 'gofmt -w' on them:"; \
	  echo "$$files" | sed 's/^/  /'; \
	  echo; gofmt -d $$files; \
	  exit 1; \
	fi; \
	echo "✓ gofmt clean"

# Go vulnerability check
vuln:
	@echo "Running govulncheck..."
	@govulncheck ./...
	@echo "✓ No known vulnerabilities"

# Pre-commit checks (fast)
#
# Step 1 formats in place (the convenient local behavior) and THEN gates, so a
# file gofmt can't fix on its own still fails rather than passing quietly. The
# gate is what CI runs; keeping both here means `make check` and CI agree.
check:
	@echo "Running pre-commit checks..."
	@echo "1. Formatting code..."
	@gofmt -w .
	@$(MAKE) --no-print-directory check-fmt
	@echo "2. Running go vet..."
	@go vet ./...
	@echo "2b. Vetting every GOOS the release builds..."
	@$(MAKE) --no-print-directory check-goos
	@echo "3. Running staticcheck..."
	@$(MAKE) --no-print-directory check-staticcheck
	@echo "3b. Running golangci-lint (every module)..."
	@$(MAKE) --no-print-directory check-lint
	@echo "4. Running short tests (hermetic: no credentials, no network)..."
	@$(MAKE) --no-print-directory test-hermetic
	@echo "5. Nested modules (build + vet + test)..."
	@$(MAKE) --no-print-directory check-nested
	@echo "6. Module tidiness (root + nested)..."
	@$(MAKE) --no-print-directory check-tidy
	@echo "7. Build tags (go vet under every discovered tag, no execution)..."
	@$(MAKE) --no-print-directory check-build-tags
	@echo "✓ All checks passed!"

# Short tests under the hermetic `go test -exec` wrapper (test/hermetic, #968).
# Ambient AWS credentials are stripped and replaced with fakes, and AWS_ENDPOINT_URL
# and HTTP(S)_PROXY point at a loopback recorder, so a test that reaches AWS, the
# spore.host DNS API (not an SDK client) or any other off-box host FAILS and is
# named. Before this, TestNewAgent_StillActivates registered a real spore.host DNS
# record on every `make check` run with credentials — and passed either way.
HERMETIC_EXEC := $(CURDIR)/$(BUILD_DIR)/hermetic-exec
.PHONY: hermetic-exec test-hermetic
hermetic-exec:
	@mkdir -p $(BUILD_DIR)
	@go build -o $(HERMETIC_EXEC) ./test/hermetic
test-hermetic: hermetic-exec
	go test -short -exec $(HERMETIC_EXEC) ./...

# `go vet ./...` once per GOOS in .goreleaser.yaml, discovered, not listed (#925).
# Plain `go vet` checks only the host GOOS, and `go build` never type-checks
# tests, so two test files that could not compile for Windows went unnoticed
# while windows/amd64 shipped. The SAME script CI runs.
.PHONY: check-goos
check-goos:
	@scripts/release-goos-vet.sh

# staticcheck fails the build on findings; only a MISSING binary is skipped (#923).
# This used to be `staticcheck ./... || echo "not installed"`, and `||` fires on
# any non-zero exit — staticcheck exits 1 when it reports findings, so the step
# printed "not installed" about an installed tool and passed, while 67 findings
# sat on main behind it. CI installs a pinned, checksum-verified binary (the pin
# lives there, in one place) and runs staticcheck unconditionally, so a skip
# here cannot hide a finding from a PR.
.PHONY: check-staticcheck
check-staticcheck:
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not installed, skipping (CI runs the version pinned in .github/workflows/ci.yml)"; \
	fi

# golangci-lint over every module, root and nested, via the SAME script CI runs
# (#1028). .golangci.yml is a ratchet — it enables only the linters that pass
# today, and lint_ratchet_test.go keeps it from narrowing; see
# docs/lint-ratchet.md for what is still off and why.
#
# Keep GOLANGCI_LINT_VERSION in step with the pin (version AND sha256) in the
# Lint step of .github/workflows/ci.yml. A different local version can report
# different findings, so it warns rather than failing.
#
# `make lint` fails when golangci-lint is missing — you asked for it.
# `make check` runs `check-lint`, which skips a missing binary the same way
# check-staticcheck does; CI installs the pinned one and runs `make lint`, so
# that skip cannot hide a finding from a PR.
GOLANGCI_LINT_VERSION := 2.15.0
.PHONY: lint check-lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed (CI pins v$(GOLANGCI_LINT_VERSION)): https://golangci-lint.run/" >&2; \
		exit 1; }
	@golangci-lint version 2>/dev/null | grep -q "version $(GOLANGCI_LINT_VERSION) " || \
		echo "warning: CI pins golangci-lint $(GOLANGCI_LINT_VERSION); local $$(golangci-lint version 2>/dev/null | awk '{print $$4}') may report different findings"
	@scripts/lint.sh
check-lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		$(MAKE) --no-print-directory lint; \
	else \
		echo "golangci-lint not installed, skipping (CI runs v$(GOLANGCI_LINT_VERSION), pinned in .github/workflows/ci.yml)"; \
	fi

# Every nested module, via the SAME script CI runs (#136). Deliberately not a
# count: a hand-written number here is the anti-pattern rule 4 warns about, and
# this one had drifted from 12 to 10 before anyone noticed.
#
# `go test ./...` does not descend into a directory with its own go.mod, so
# without this `make check` is weaker than CI — and that gap cost two round trips
# in one session (#770's unwired template parameter, #771's missing go.sum entry),
# both found only after a push. One script, two callers, no drift.
.PHONY: check-nested
check-nested:
	@scripts/nested-modules.sh

# Every module, root and nested, must be `go mod tidy`-clean (#907). Tidy is
# the only tool here that loads packages under every build tag, so it is what
# catches an integration-tagged test importing a package that no longer exists
# — which nothing else in check or CI compiles. Same script CI runs.
.PHONY: check-tidy
check-tidy:
	@scripts/check-tidy.sh

# Every module vetted under every custom build tag it uses (#913), so code behind
# `//go:build integration` / `e2e_tier*` can't stop compiling unseen — the root
# integration_test.go had never compiled. Tags are discovered from //go:build
# lines, not listed. Compile-only: vet runs no tests, which matters because the
# integration and tier 1-3 tests launch billable instances. Same script CI runs.
.PHONY: check-build-tags
check-build-tags:
	@scripts/check-build-tags.sh

# Development
dev: build
	@echo "Built for development"

# Create release archives
release: build-all
	@echo "Creating release archives..."
	@mkdir -p release
	tar czf release/spawn-$(VERSION)-linux-amd64.tar.gz -C $(BUILD_DIR) spawn-linux-amd64 spored-linux-amd64
	tar czf release/spawn-$(VERSION)-linux-arm64.tar.gz -C $(BUILD_DIR) spawn-linux-arm64 spored-linux-arm64
	tar czf release/spawn-$(VERSION)-darwin-amd64.tar.gz -C $(BUILD_DIR) spawn-darwin-amd64 spored-darwin-amd64
	tar czf release/spawn-$(VERSION)-darwin-arm64.tar.gz -C $(BUILD_DIR) spawn-darwin-arm64 spored-darwin-arm64
	@echo "Release archives created in release/"

.DEFAULT_GOAL := build

# Real-AWS smoke for the paths unit tests cannot reach (see
# scripts/hardware-sensitive.txt for which those are and why). Roughly $0.05 at
# the default 2-node width; every instance gets a TTL, is terminated explicitly,
# and is leak-checked afterwards.
#
# NODES=4 make smoke        wider MPI check
# SKIP_EFA=1 make smoke     skip the EFA leg
.PHONY: smoke smoke-needed
smoke: build
	@SPAWN=bin/spawn scripts/hardware-smoke.sh

# Does anything changed since the last tag actually warrant a smoke? Answers the
# question that currently gets answered from memory at tagging time.
smoke-needed:
	@scripts/hardware-smoke-needed.sh

# Are all Lambda runtimes supported? Offline by default so it can gate a PR;
# --deployed also checks the accounts we own (spawn#716).
.PHONY: lambda-runtimes lambda-runtimes-deployed
lambda-runtimes:
	@scripts/lambda-runtime-census.sh

# Version census (#654). The offline half gates PRs: every deploy mechanism must
# stamp spawn:version, so a new Lambda cannot arrive unreadable. The --deployed
# half is operator-facing and needs the infra profile — these functions are in
# spore.host's account, so a user's CLI cannot see them (the user-facing half of
# #654 is `spawn reaper status`, for the self-hosted reaper in their own account).
.PHONY: lambda-versions lambda-versions-deployed
lambda-versions:
	@scripts/lambda-deploy-census.sh

lambda-versions-deployed:
	@scripts/lambda-deploy-census.sh --deployed

lambda-runtimes-deployed:
	@scripts/lambda-runtime-census.sh --deployed
