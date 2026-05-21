# Integration Tests for Multi-Region Sweeps

This document describes the integration test suite for v0.9.0 multi-region features.

## Overview

The integration tests verify the following features:
- **Issue #41**: Per-region max concurrent limits
- **Issue #40**: Spot instance type flexibility with fallback
- **Issue #42**: Regional cost breakdown tracking
- **Issue #43**: Multi-region result collection
- Multi-region basic launch functionality

## Prerequisites

1. **AWS Credentials**: Configure AWS profiles for `spore-host-infra` and `spore-host-dev`
2. **Permissions**: Ensure you have permissions to:
   - Launch EC2 instances in `spore-host-dev` account
   - Access DynamoDB in `spore-host-infra` account
   - Invoke Lambda functions
3. **Build**: Run `go build -o bin/spawn .` to build the CLI

## Running Tests

### Run All Integration Tests

```bash
go test -v -tags=integration -timeout 30m ./...
```

### Run Specific Test

```bash
go test -v -tags=integration -timeout 30m -run TestMultiRegionBasicLaunch ./...
```

### Test List

- `TestMultiRegionBasicLaunch` - Verifies basic multi-region sweep functionality
- `TestPerRegionConcurrentLimits` - Validates per-region concurrent limits
- `TestInstanceTypeFallback` - Tests instance type fallback patterns
- `TestRegionalCostTracking` - Verifies cost calculation per region
- `TestDynamoDBConnection` - Sanity check for DynamoDB access

## Test Behavior

**IMPORTANT**: These tests launch real EC2 instances and will incur costs (minimal, as they use t3.micro spot instances with 30-minute TTL).

- Each test automatically cancels sweeps for cleanup
- Instances are set to terminate after 30 minutes (TTL)
- Tests use spot instances to minimize cost

## Expected Output

Successful test run:
```
=== RUN   TestMultiRegionBasicLaunch
    integration_test.go:32: Launching multi-region sweep...
    integration_test.go:34: Sweep ID: sweep-abc123
    integration_test.go:45: Region us-east-1: Launched=1, Failed=0
    integration_test.go:45: Region us-west-2: Launched=1, Failed=0
    integration_test.go:51: Canceling sweep for cleanup...
--- PASS: TestMultiRegionBasicLaunch (30.12s)
PASS
```

## Cost Estimate

Running the full test suite once:
- ~10 t3.micro spot instances launched
- ~30 minutes runtime each (test duration + TTL)
- **Estimated cost**: $0.10 - $0.20

## Troubleshooting

### "Failed to load AWS config"
- Ensure AWS profiles are configured: `aws configure --profile spore-host-infra`

### "Failed to query sweep status"
- Check DynamoDB table exists: `spawn-sweep-orchestration`
- Verify IAM permissions for DynamoDB access

### "Launch command failed"
- Ensure `./bin/spawn` binary exists
- Check EC2 launch permissions in `spore-host-dev` account

### Tests timeout
- Increase timeout: `go test -v -tags=integration -timeout 60m ./...`
- Lambda may take time to process sweeps

## Skipping Tests

To skip expensive integration tests during development:

```bash
# Run only unit tests
go test -v -short ./...

# Explicitly skip integration tests
go test -v ./...  # (integration tag not specified)
```

## Cleanup

If tests fail and leave resources running:

1. **List active sweeps**:
   ```bash
   ./bin/spawn list-sweeps
   ```

2. **Cancel sweep**:
   ```bash
   ./bin/spawn cancel --sweep-id <sweep-id>
   ```

3. **Check for orphaned instances**:
   ```bash
   ./bin/spawn list
   ```

## CI/CD Integration

For CI/CD pipelines, add integration tests as a separate job:

```yaml
integration-tests:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v2
    - uses: actions/setup-go@v3
    - name: Configure AWS
      run: |
        aws configure set aws_access_key_id ${{ secrets.AWS_ACCESS_KEY_ID }}
        # ... configure profiles
    - name: Run Integration Tests
      run: go test -v -tags=integration -timeout 30m ./...
```

## Future Enhancements

- Add `TestMultiRegionResultCollection` once result upload pipeline is ready
- Add performance benchmarks for large sweeps
- Add chaos testing (simulate Lambda timeouts, network issues)
