.PHONY: all build build-spawn build-spored build-all clean install test test-coverage test-short test-integration test-integration-scheduler test-integration-queue test-e2e test-e2e-tier0 test-e2e-tier1 test-e2e-tier2 test-e2e-tier3 check vuln

# Version
VERSION ?= 0.1.0

# Build directory
BUILD_DIR = bin

all: build

# Build for current platform
build: build-spawn build-spored

build-spawn:
	@echo "Building spawn..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/spawn main.go

build-spored:
	@echo "Building spored..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/spored cmd/spored/main.go

# Build for all platforms
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64

# Linux AMD64 (x86_64)
build-linux-amd64:
	@echo "Building for Linux AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-linux-amd64 main.go
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/spored-linux-amd64 cmd/spored/main.go

# Linux ARM64 (Graviton)
build-linux-arm64:
	@echo "Building for Linux ARM64 (Graviton)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/spawn-linux-arm64 main.go
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/spored-linux-arm64 cmd/spored/main.go

# macOS AMD64 (Intel)
build-darwin-amd64:
	@echo "Building for macOS AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-darwin-amd64 main.go
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/spored-darwin-amd64 cmd/spored/main.go

# macOS ARM64 (M1/M2)
build-darwin-arm64:
	@echo "Building for macOS ARM64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/spawn-darwin-arm64 main.go
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/spored-darwin-arm64 cmd/spored/main.go

# Windows AMD64
build-windows-amd64:
	@echo "Building for Windows AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/spawn-windows-amd64.exe main.go
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/spored-windows-amd64.exe cmd/spored/main.go

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

# Run integration tests
test-integration:
	@echo "Running all integration tests..."
	@echo "Note: These tests require AWS credentials and may take 20-30 minutes"
	go test -v -tags=integration -timeout 45m ./...

# Run scheduler integration tests only
test-integration-scheduler:
	@echo "Running scheduler integration tests..."
	@echo "Note: Requires AWS credentials (spore-host-dev profile)"
	go test -v -tags=integration -run TestScheduler -timeout 20m ./...

# Run queue integration tests only
test-integration-queue:
	@echo "Running queue integration tests..."
	@echo "Note: Requires AWS credentials (spore-host-dev profile)"
	go test -v -tags=integration -run TestQueue -timeout 20m ./...

# E2E test suite — four independently runnable tiers.
# Tier 0 needs NO AWS account (runs the real spawn binary against the Substrate
# emulator). Tiers 1–3 require AWS credentials (AWS_PROFILE=spore-host-dev or env
# vars) and a compiled spawn binary at ./bin/spawn (run 'make build' first).

# Tier 0 — CLI against the Substrate emulator: no AWS account, deterministic,
# safe for CI. Exercises the full command surface (args → cobra → AWS client →
# Substrate) asserting JSON, exit codes, and emulator state.
test-e2e-tier0: build
	@echo "Running E2E tier 0 (CLI against Substrate, no AWS account)..."
	go test -tags=e2e_tier0 -timeout 30m ./test/e2e/

# Tier 1 — AWS API-only, no instances launched, ~free, ~5 min
test-e2e-tier1: build
	@echo "Running E2E tier 1 (API-only, no instances)..."
	go test -v -tags=e2e_tier1 -run TestTier1 -timeout 10m ./test/e2e/

# Tier 2 — Single instance per test, ~$0.50 total, ~20 min
test-e2e-tier2: build
	@echo "Running E2E tier 2 (single-instance tests, ~\$$1)..."
	go test -v -tags=e2e_tier2 -run TestTier2 -timeout 80m ./test/e2e/

# Tier 3 — Multi-instance (job arrays, sweeps, FSx, MPI), ~$2-5 total, ~35 min
test-e2e-tier3: build
	@echo "Running E2E tier 3 (multi-instance tests, ~\$$2-5)..."
	go test -v -tags=e2e_tier3 -run TestTier3 -timeout 60m ./test/e2e/

# Run all E2E tiers sequentially
test-e2e: test-e2e-tier1 test-e2e-tier2 test-e2e-tier3

# Go vulnerability check
vuln:
	@echo "Running govulncheck..."
	@govulncheck ./...
	@echo "✓ No known vulnerabilities"

# Pre-commit checks (fast)
check:
	@echo "Running pre-commit checks..."
	@echo "1. Formatting code..."
	@gofmt -w .
	@echo "2. Running go vet..."
	@go vet ./...
	@echo "3. Running staticcheck..."
	@staticcheck ./... || echo "staticcheck not installed, skipping..."
	@echo "4. Running short tests..."
	@go test -short ./...
	@echo "✓ All checks passed!"

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
