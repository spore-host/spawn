# Testing Guide

Comprehensive testing guide for spawn's hybrid compute system.

## Overview

Spawn includes three levels of tests:
1. **Unit Tests** - Fast, no AWS dependencies
2. **Integration Tests** - Require AWS credentials and services
3. **End-to-End Tests** - Full workflow testing

## Quick Start

```bash
# Run all unit tests (fast, no AWS required)
go test -short ./...

# Run integration tests (requires AWS credentials)
go test -tags=integration ./pkg/integration/...

# Run all tests with coverage
go test -cover ./...

# Run tests for specific package
go test -v ./pkg/provider/...
```

## Unit Tests

Unit tests are fast and require no AWS credentials. They test individual components in isolation.

### Running Unit Tests

```bash
# All unit tests
go test -short ./...

# Specific packages
go test -short ./pkg/provider/...
go test -short ./pkg/config/...
go test -short ./pkg/orchestrator/...
go test -short ./pkg/registry/...

# With verbose output
go test -short -v ./...

# With coverage report
go test -short -cover ./...
go test -short -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Unit Test Coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| `pkg/provider` | ~75% | Provider abstraction, local provider |
| `pkg/config` | ~80% | Config parsing, validation |
| `pkg/orchestrator` | ~70% | Scaling logic, cost tracking |
| `pkg/registry` | ~60% | Registry operations (mocked) |

### Writing Unit Tests

**Table-Driven Tests (Preferred):**
```go
func TestLocalProvider_GetIdentity(t *testing.T) {
    tests := []struct {
        name      string
        configYAML string
        wantID    string
        wantErr   bool
    }{
        {
            name: "valid config",
            configYAML: `instance_id: test-01`,
            wantID: "test-01",
            wantErr: false,
        },
        // More test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
        })
    }
}
```

**Best Practices:**
- Use `t.TempDir()` for test files
- Clean up resources with `defer`
- Use `t.Helper()` for test helpers
- Test error paths, not just happy path
- Keep tests focused and independent

## Integration Tests

Integration tests require AWS credentials and test against real AWS services (DynamoDB, S3, SQS).

### Setup

1. **AWS Credentials:**
```bash
# Configure AWS CLI (if not already done)
aws configure

# Or use environment variables
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

# Or use AWS profile
export AWS_PROFILE=myprofile
```

2. **DynamoDB Table:**
```bash
# Create registry table (if not exists)
aws dynamodb create-table \
  --table-name spawn-hybrid-registry \
  --attribute-definitions \
    AttributeName=job_array_id,AttributeType=S \
    AttributeName=instance_id,AttributeType=S \
  --key-schema \
    AttributeName=job_array_id,KeyType=HASH \
    AttributeName=instance_id,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST
```

3. **S3 Bucket (Optional):**
```bash
# Create test bucket for queue tests
aws s3 mb s3://spawn-test-bucket-$RANDOM
```

### Running Integration Tests

```bash
# All integration tests
go test -tags=integration ./pkg/integration/...

# Specific test
go test -tags=integration -run TestHybridWorkflow ./pkg/integration/...

# With verbose output
go test -tags=integration -v ./pkg/integration/...

# Skip slow tests
go test -tags=integration -short ./pkg/integration/...
```

### Integration Test Suite

**Hybrid Workflow (`hybrid_test.go`):**
- Local provider creation
- DynamoDB registration
- Peer discovery
- Heartbeat mechanism
- Multi-instance coordination
- TTL expiration

**Orchestrator (`orchestrator_test.go`):**
- Config loading
- Manual mode operation
- Scaling decisions
- Registry table creation

### Test Data Cleanup

Integration tests create temporary data in AWS:

```bash
# List test job arrays in DynamoDB
aws dynamodb scan \
  --table-name spawn-hybrid-registry \
  --filter-expression "begins_with(job_array_id, :prefix)" \
  --expression-attribute-values '{":prefix":{"S":"integration-test-"}}'

# Delete test entries (automatic via TTL, or manual)
aws dynamodb delete-item \
  --table-name spawn-hybrid-registry \
  --key '{"job_array_id":{"S":"test-123"},"instance_id":{"S":"test-01"}}'
```

## End-to-End Tests

End-to-end tests verify complete workflows with real workloads.

### Prerequisites

1. **Spawn AMI** - Pre-built AMI with spored installed
2. **S3 Bucket** - For queue/result storage
3. **SQS Queue** - For orchestrator monitoring
4. **IAM Permissions** - EC2 launch, DynamoDB access

### E2E Test: Local + Cloud Hybrid

```bash
# 1. Create test queue
cat > /tmp/test-queue.yaml <<EOF
jobs:
  - id: job1
    command: "echo 'Job 1' && sleep 10"
  - id: job2
    command: "echo 'Job 2' && sleep 10"
  - id: job3
    command: "echo 'Job 3' && sleep 10"
EOF

aws s3 cp /tmp/test-queue.yaml s3://test-bucket/queue.yaml

# 2. Start local instance
export SPAWN_CONFIG=/tmp/local-config.yaml
cat > $SPAWN_CONFIG <<EOF
instance_id: e2e-local-01
region: local
job_array:
  id: e2e-test-${RANDOM}
  index: 0
EOF

spored run-queue s3://test-bucket/queue.yaml &
LOCAL_PID=$!

# 3. Burst cloud instances
spawn burst \
  --count 2 \
  --instance-type t3.micro \
  --job-array-id $(grep id $SPAWN_CONFIG | awk '{print $2}')

# 4. Monitor progress
watch -n 5 'aws dynamodb query \
  --table-name spawn-hybrid-registry \
  --key-condition-expression "job_array_id = :id" \
  --expression-attribute-values "{\":id\":{\"S\":\"$(grep id $SPAWN_CONFIG | awk \"'\"'{print $2}\"'\")\"}}\"'

# 5. Wait for completion
wait $LOCAL_PID

# 6. Verify results
aws s3 ls s3://test-bucket/results/
```

### E2E Test: Auto-Burst Orchestrator

```bash
# 1. Create large queue (200 jobs)
cat > /tmp/large-queue.yaml <<EOF
jobs:
EOF
for i in {1..200}; do
  echo "  - id: job$i" >> /tmp/large-queue.yaml
  echo "    command: \"echo 'Job $i' && sleep 5\"" >> /tmp/large-queue.yaml
done

# Create SQS queue and enqueue jobs
aws sqs create-queue --queue-name e2e-test-queue
QUEUE_URL=$(aws sqs get-queue-url --queue-name e2e-test-queue --query QueueUrl --output text)

# Convert queue to SQS messages
spawn queue to-sqs --file /tmp/large-queue.yaml --queue-url $QUEUE_URL

# 2. Start orchestrator
cat > /tmp/orch-config.yaml <<EOF
job_array_id: e2e-auto-burst
queue_url: $QUEUE_URL
region: us-east-1

burst_policy:
  mode: auto
  queue_depth_threshold: 50
  max_cloud_instances: 5
  instance_type: t3.micro
  ami: ami-12345  # Your spawn AMI
  spot: true
  cost_budget: 1.0
EOF

spawn-orchestrator run /tmp/orch-config.yaml &
ORCH_PID=$!

# 3. Monitor auto-scaling
watch -n 10 'echo "Queue Depth:"; \
aws sqs get-queue-attributes \
  --queue-url $QUEUE_URL \
  --attribute-names ApproximateNumberOfMessages; \
echo "Cloud Instances:"; \
aws ec2 describe-instances \
  --filters "Name=tag:spawn:job-array-id,Values=e2e-auto-burst" \
  --query "Reservations[*].Instances[*].[InstanceId,State.Name]" \
  --output table'

# 4. Wait for queue to drain
while [ $(aws sqs get-queue-attributes --queue-url $QUEUE_URL --attribute-names ApproximateNumberOfMessages --query 'Attributes.ApproximateNumberOfMessages' --output text) -gt 0 ]; do
  echo "Waiting for queue to drain..."
  sleep 30
done

# 5. Verify scale-down
sleep 360  # Wait 6 minutes for scale-down delay
aws ec2 describe-instances \
  --filters "Name=tag:spawn:job-array-id,Values=e2e-auto-burst" \
  --query "Reservations[*].Instances[*].[InstanceId,State.Name]" \
  --output table

# Should show terminated instances

# 6. Cleanup
kill $ORCH_PID
aws sqs delete-queue --queue-url $QUEUE_URL
```

## Performance Testing

### Load Testing

Test orchestrator with high queue depth:

```bash
# Create queue with 10,000 jobs
for i in {1..10000}; do
  aws sqs send-message \
    --queue-url $QUEUE_URL \
    --message-body "{\"job_id\":\"$i\"}"
done

# Start orchestrator and measure:
# - Time to scale up to max instances
# - Time to drain queue
# - Cost incurred
```

### Peer Discovery Latency

Test DynamoDB peer discovery performance:

```bash
go test -v -run TestPeerDiscoveryLatency ./pkg/registry/...
```

### Heartbeat Overhead

Test heartbeat system overhead:

```bash
# Run 100 instances sending heartbeats
# Measure DynamoDB write capacity consumed
```

## CI/CD Testing

### GitHub Actions

```yaml
name: Tests

on: [push, pull_request]

jobs:
  unit-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.21'
      - name: Run unit tests
        run: go test -short -cover ./...

  integration-tests:
    runs-on: ubuntu-latest
    needs: unit-tests
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
      - name: Configure AWS credentials
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1
      - name: Run integration tests
        run: go test -tags=integration ./pkg/integration/...
```

### Pre-commit Hook

```bash
# .git/hooks/pre-commit
#!/bin/bash
set -e

echo "Running pre-commit checks..."

# Format code
gofmt -w .

# Run linters
go vet ./...
staticcheck ./...

# Run unit tests
go test -short ./...

echo "Pre-commit checks passed!"
```

Make executable:
```bash
chmod +x .git/hooks/pre-commit
```

## Test Coverage

### Generate Coverage Report

```bash
# Generate coverage
go test -coverprofile=coverage.out ./...

# View in terminal
go tool cover -func=coverage.out

# View in browser
go tool cover -html=coverage.out
```

### Coverage Goals

| Component | Current | Goal |
|-----------|---------|------|
| Provider | 75% | 80% |
| Config | 80% | 85% |
| Orchestrator | 70% | 80% |
| Registry | 60% | 75% |
| Overall | 70% | 80% |

## Debugging Tests

### Verbose Output

```bash
# Show all test output
go test -v ./...

# Show only failures
go test ./... 2>&1 | grep -A 10 "FAIL"
```

### Run Single Test

```bash
go test -v -run TestLocalProvider_GetIdentity ./pkg/provider/
```

### Debug with Delve

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug test
dlv test ./pkg/provider/ -- -test.run TestLocalProvider_GetIdentity
```

### Test Timeouts

```bash
# Set custom timeout
go test -timeout 30m ./pkg/integration/...

# Disable timeout (for debugging)
go test -timeout 0 ./pkg/provider/...
```

## Test Data

### Test Fixtures

Test fixtures are in `testdata/` directories:

```
pkg/
  provider/
    testdata/
      local-config.yaml
      ec2-tags.json
  orchestrator/
    testdata/
      orchestrator-config.yaml
      queue-empty.json
      queue-full.json
```

### Generating Test Data

```bash
# Generate test config
cat > pkg/provider/testdata/local-config.yaml <<EOF
instance_id: test-01
region: local
ttl: 1h
EOF

# Generate test queue
python3 -c "
import yaml
jobs = [{'id': f'job{i}', 'command': f'echo job{i}'} for i in range(100)]
print(yaml.dump({'jobs': jobs}))
" > testdata/test-queue.yaml
```

## Troubleshooting Tests

### Issue: Tests fail with "AWS credentials not found"

**Solution:**
```bash
# Set AWS credentials
aws configure
# Or
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
```

### Issue: Integration tests timeout

**Solutions:**
1. Increase timeout: `go test -timeout 10m ...`
2. Check AWS service availability
3. Check network connectivity
4. Run with `-v` to see progress

### Issue: DynamoDB "ResourceNotFoundException"

**Solution:**
```bash
# Create table
go test -tags=integration -run TestRegistryTableCreation ./pkg/integration/
```

### Issue: Tests leave AWS resources

**Solution:**
```bash
# Terminate test instances
aws ec2 terminate-instances --instance-ids $(
  aws ec2 describe-instances \
    --filters "Name=tag:spawn:job-array-id,Values=*test*" \
    --query "Reservations[*].Instances[*].InstanceId" \
    --output text
)

# Delete test DynamoDB entries
aws dynamodb scan --table-name spawn-hybrid-registry \
  --filter-expression "begins_with(job_array_id, :prefix)" \
  --expression-attribute-values '{":prefix":{"S":"test-"}}' \
  --query 'Items[*].[job_array_id.S,instance_id.S]' \
  --output text | while read job_id inst_id; do
  aws dynamodb delete-item \
    --table-name spawn-hybrid-registry \
    --key "{\"job_array_id\":{\"S\":\"$job_id\"},\"instance_id\":{\"S\":\"$inst_id\"}}"
done
```

## Test Metrics

### Current Status

- **Total Tests:** 87
- **Unit Tests:** 65
- **Integration Tests:** 22
- **Average Run Time:** Unit: 2.5s, Integration: 45s
- **Flaky Tests:** 0
- **Coverage:** 70%

### Test Health Dashboard

```bash
# Run all tests and collect metrics
go test -json ./... > test-results.json

# Parse results
go run scripts/test-metrics.go test-results.json
```

## Contributing Tests

### Guidelines

1. **Add tests for new features** - Every new feature needs tests
2. **Update existing tests** - When behavior changes
3. **Write table-driven tests** - For multiple similar cases
4. **Test error paths** - Not just happy path
5. **Use meaningful test names** - Describe what's being tested
6. **Keep tests independent** - Tests shouldn't depend on each other
7. **Clean up resources** - Use `defer` for cleanup

### Test Review Checklist

- [ ] Tests pass locally
- [ ] Tests pass in CI
- [ ] Coverage doesn't decrease
- [ ] Tests are independent
- [ ] Error cases covered
- [ ] Test names are descriptive
- [ ] Fixtures in `testdata/`
- [ ] Integration tests tagged correctly

## Resources

- [Go Testing Package](https://pkg.go.dev/testing)
- [Table-Driven Tests](https://dave.cheney.net/2019/05/07/prefer-table-driven-tests)
- [AWS SDK Testing](https://aws.github.io/aws-sdk-go-v2/docs/unit-testing/)
- [Test Fixtures](https://dave.cheney.net/2016/05/10/test-fixtures-in-go)
