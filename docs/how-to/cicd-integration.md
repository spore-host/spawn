# How-To: CI/CD Integration

Integrate spawn into continuous integration and deployment pipelines.

## Overview

### Common CI/CD Patterns

1. **Automated Testing** - Run tests on every commit
2. **Build Verification** - Test builds on target platforms
3. **Performance Benchmarking** - Track performance over time
4. **Deployment Testing** - Validate deployments before production
5. **Scheduled Jobs** - Nightly builds, data processing

---

## GitHub Actions Integration

### Problem
Run compute-intensive tests in CI without tying up GitHub Actions runners.

### Solution: Offload to spawn

**Basic workflow:**
```yaml
# .github/workflows/test-spawn.yml
name: Test with spawn

on:
  push:
    branches: [main, develop]
  pull_request:

jobs:
  test-on-spawn:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS credentials
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Install spawn
        run: |
          curl -sL https://github.com/yourorg/spawn/releases/latest/download/spawn-linux-amd64 \
            -o /usr/local/bin/spawn
          chmod +x /usr/local/bin/spawn

      - name: Launch test instance
        id: launch
        run: |
          INSTANCE_ID=$(spawn launch \
            --instance-type c7i.2xlarge \
            --ttl 30m \
            --wait-for-ssh \
            --tags ci=true,pr=${{ github.event.pull_request.number }},commit=${{ github.sha }} \
            --quiet)

          echo "instance_id=$INSTANCE_ID" >> $GITHUB_OUTPUT

      - name: Upload code to instance
        run: |
          INSTANCE_IP=$(spawn status ${{ steps.launch.outputs.instance_id }} --json | jq -r '.network.public_ip')

          tar czf code.tar.gz .
          scp code.tar.gz ec2-user@$INSTANCE_IP:~/

          ssh ec2-user@$INSTANCE_IP "tar xzf code.tar.gz"

      - name: Run tests
        run: |
          INSTANCE_IP=$(spawn status ${{ steps.launch.outputs.instance_id }} --json | jq -r '.network.public_ip')

          ssh ec2-user@$INSTANCE_IP "cd ~ && ./run-tests.sh"

      - name: Download test results
        if: always()
        run: |
          INSTANCE_IP=$(spawn status ${{ steps.launch.outputs.instance_id }} --json | jq -r '.network.public_ip')
          scp ec2-user@$INSTANCE_IP:~/test-results.xml ./test-results.xml

      - name: Publish test results
        uses: EnricoMi/publish-unit-test-result-action@v2
        if: always()
        with:
          files: test-results.xml

      - name: Terminate instance
        if: always()
        run: |
          aws ec2 terminate-instances --instance-ids ${{ steps.launch.outputs.instance_id }}
```

**Using artifacts for code transfer:**
```yaml
jobs:
  test-on-spawn:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Package code
        run: tar czf code.tar.gz .

      - name: Upload to S3
        run: |
          aws s3 cp code.tar.gz s3://my-ci-bucket/builds/${{ github.sha }}.tar.gz

      - name: Launch and test
        run: |
          spawn launch \
            --instance-type c7i.2xlarge \
            --ttl 30m \
            --iam-policy s3:ReadOnly \
            --on-complete terminate \
            --user-data "
              aws s3 cp s3://my-ci-bucket/builds/${{ github.sha }}.tar.gz /home/ec2-user/code.tar.gz
              cd /home/ec2-user
              tar xzf code.tar.gz
              ./run-tests.sh
              aws s3 cp test-results.xml s3://my-ci-bucket/results/${{ github.sha }}.xml
            "

      - name: Download results
        run: |
          aws s3 cp s3://my-ci-bucket/results/${{ github.sha }}.xml ./test-results.xml
```

---

## Parallel Testing with Job Arrays

### Problem
Test suite takes 2 hours on single machine. Want to parallelize.

### Solution: Distribute tests across spawn array

**Workflow:**
```yaml
# .github/workflows/parallel-tests.yml
name: Parallel Tests

on: [push]

jobs:
  prepare-tests:
    runs-on: ubuntu-latest
    outputs:
      test-matrix: ${{ steps.split.outputs.matrix }}
    steps:
      - uses: actions/checkout@v3

      - name: Split tests into chunks
        id: split
        run: |
          # Split test files into 10 chunks
          find tests/ -name "*.py" | split -n l/10 - chunk-

          # Create matrix JSON
          MATRIX=$(ls chunk-* | jq -R -s -c 'split("\n")[:-1]')
          echo "matrix=$MATRIX" >> $GITHUB_OUTPUT

  run-tests:
    needs: prepare-tests
    runs-on: ubuntu-latest
    strategy:
      matrix:
        chunk: ${{ fromJson(needs.prepare-tests.outputs.test-matrix) }}
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Run test chunk
        run: |
          # Upload test chunk to S3
          aws s3 cp ${{ matrix.chunk }} s3://ci-bucket/chunks/${{ github.sha }}/${{ matrix.chunk }}

          # Launch instance to run tests
          spawn launch \
            --instance-type c7i.large \
            --ttl 30m \
            --on-complete terminate \
            --iam-policy s3:FullAccess \
            --user-data "
              # Download code
              git clone ${{ github.repositoryUrl }} /home/ec2-user/repo
              cd /home/ec2-user/repo
              git checkout ${{ github.sha }}

              # Download test chunk
              aws s3 cp s3://ci-bucket/chunks/${{ github.sha }}/${{ matrix.chunk }} /tmp/tests.txt

              # Run tests
              cat /tmp/tests.txt | xargs pytest --junit-xml=results.xml

              # Upload results
              aws s3 cp results.xml s3://ci-bucket/results/${{ github.sha }}/${{ matrix.chunk }}.xml
            "

      - name: Download results
        run: |
          aws s3 cp s3://ci-bucket/results/${{ github.sha }}/${{ matrix.chunk }}.xml ./results-${{ matrix.chunk }}.xml

      - name: Upload results artifact
        uses: actions/upload-artifact@v3
        with:
          name: test-results
          path: results-*.xml

  aggregate-results:
    needs: run-tests
    runs-on: ubuntu-latest
    steps:
      - name: Download all results
        uses: actions/download-artifact@v3
        with:
          name: test-results

      - name: Publish combined results
        uses: EnricoMi/publish-unit-test-result-action@v2
        with:
          files: results-*.xml
```

---

## GPU-Accelerated CI

### Problem
Train models in CI to verify training pipeline doesn't break.

### Solution: GPU instances on demand

**Workflow:**
```yaml
# .github/workflows/train-model.yml
name: Train Model

on:
  pull_request:
    paths:
      - 'models/**'
      - 'training/**'

jobs:
  train:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Train model
        run: |
          # Package code
          tar czf code.tar.gz .
          aws s3 cp code.tar.gz s3://ci-bucket/code/${{ github.sha }}.tar.gz

          # Launch GPU instance
          INSTANCE_ID=$(spawn launch \
            --instance-type g5.xlarge \
            --ami ami-gpu-pytorch \
            --ttl 2h \
            --spot \
            --iam-policy s3:FullAccess \
            --tags ci=true,pr=${{ github.event.pull_request.number }} \
            --user-data "
              # Download and extract code
              aws s3 cp s3://ci-bucket/code/${{ github.sha }}.tar.gz /home/ec2-user/code.tar.gz
              cd /home/ec2-user
              tar xzf code.tar.gz

              # Train model (5 epochs only for CI)
              python train.py --epochs 5 --output-dir /tmp/model

              # Upload model
              aws s3 sync /tmp/model s3://ci-bucket/models/${{ github.sha }}/

              # Signal completion
              spored complete --status success
            " \
            --on-complete terminate \
            --wait-for-ssh \
            --quiet)

          echo "Training on instance: $INSTANCE_ID"

      - name: Wait for training
        run: |
          # Wait for model to appear in S3
          timeout 120m bash -c '
            until aws s3 ls s3://ci-bucket/models/${{ github.sha }}/model.pt; do
              echo "Waiting for training..."
              sleep 30
            done
          '

      - name: Download model
        run: |
          aws s3 sync s3://ci-bucket/models/${{ github.sha }}/ ./model/

      - name: Validate model
        run: |
          python validate_model.py ./model/model.pt

      - name: Comment PR with results
        uses: actions/github-script@v6
        with:
          script: |
            github.rest.issues.createComment({
              issue_number: context.issue.number,
              owner: context.repo.owner,
              repo: context.repo.repo,
              body: '✅ Model training succeeded on commit ${{ github.sha }}'
            })
```

---

## GitLab CI Integration

### Problem
Integrate spawn into GitLab CI pipelines.

### Solution: GitLab CI spawn runner

**Basic pipeline:**
```yaml
# .gitlab-ci.yml
stages:
  - test
  - build

variables:
  AWS_REGION: us-east-1

before_script:
  - curl -sL https://github.com/yourorg/spawn/releases/latest/download/spawn-linux-amd64 -o /usr/local/bin/spawn
  - chmod +x /usr/local/bin/spawn

test:
  stage: test
  script:
    - |
      # Launch test instance
      INSTANCE_ID=$(spawn launch \
        --instance-type c7i.2xlarge \
        --ttl 30m \
        --wait-for-ssh \
        --tags ci=true,pipeline=$CI_PIPELINE_ID,job=$CI_JOB_ID \
        --quiet)

      # Get instance IP
      INSTANCE_IP=$(spawn status $INSTANCE_ID --json | jq -r '.network.public_ip')

      # Upload code
      tar czf code.tar.gz .
      scp code.tar.gz ec2-user@$INSTANCE_IP:~/
      ssh ec2-user@$INSTANCE_IP "tar xzf code.tar.gz && ./run-tests.sh"

      # Download results
      scp ec2-user@$INSTANCE_IP:~/test-results.xml ./test-results.xml

      # Terminate
      aws ec2 terminate-instances --instance-ids $INSTANCE_ID

  artifacts:
    reports:
      junit: test-results.xml

build:
  stage: build
  script:
    - spawn launch --instance-type c7i.xlarge --ttl 1h --user-data "make build"
  only:
    - main
```

**Multi-platform builds:**
```yaml
# .gitlab-ci.yml
build:linux-amd64:
  stage: build
  script:
    - |
      spawn launch \
        --instance-type c7i.large \
        --ttl 30m \
        --on-complete terminate \
        --user-data "
          git clone $CI_REPOSITORY_URL /build
          cd /build
          git checkout $CI_COMMIT_SHA
          make build-linux-amd64
          aws s3 cp ./bin/app s3://builds/$CI_COMMIT_SHA/app-linux-amd64
        "

build:linux-arm64:
  stage: build
  script:
    - |
      spawn launch \
        --instance-type c7g.large \
        --ttl 30m \
        --on-complete terminate \
        --user-data "
          git clone $CI_REPOSITORY_URL /build
          cd /build
          git checkout $CI_COMMIT_SHA
          make build-linux-arm64
          aws s3 cp ./bin/app s3://builds/$CI_COMMIT_SHA/app-linux-arm64
        "
```

---

## Jenkins Integration

### Problem
Integrate spawn into Jenkins build pipeline.

### Solution: Jenkins spawn plugin (or shell script)

**Jenkinsfile:**
```groovy
// Jenkinsfile
pipeline {
    agent any

    environment {
        AWS_REGION = 'us-east-1'
        AWS_CREDENTIALS = credentials('aws-credentials-id')
    }

    stages {
        stage('Test') {
            steps {
                script {
                    // Launch spawn instance
                    def instanceId = sh(
                        script: '''
                            spawn launch \
                                --instance-type c7i.2xlarge \
                                --ttl 30m \
                                --wait-for-ssh \
                                --tags ci=true,build=${BUILD_NUMBER} \
                                --quiet
                        ''',
                        returnStdout: true
                    ).trim()

                    echo "Launched instance: ${instanceId}"

                    try {
                        // Get instance IP
                        def instanceIp = sh(
                            script: "spawn status ${instanceId} --json | jq -r '.network.public_ip'",
                            returnStdout: true
                        ).trim()

                        // Upload code
                        sh """
                            tar czf code.tar.gz .
                            scp code.tar.gz ec2-user@${instanceIp}:~/
                        """

                        // Run tests
                        sh """
                            ssh ec2-user@${instanceIp} 'tar xzf code.tar.gz && ./run-tests.sh'
                        """

                        // Download results
                        sh """
                            scp ec2-user@${instanceIp}:~/test-results.xml ./test-results.xml
                        """
                    } finally {
                        // Always terminate instance
                        sh "aws ec2 terminate-instances --instance-ids ${instanceId}"
                    }
                }
            }
        }

        stage('Build') {
            when {
                branch 'main'
            }
            steps {
                sh '''
                    spawn launch \
                        --instance-type c7i.xlarge \
                        --ttl 1h \
                        --on-complete terminate \
                        --user-data "
                            git clone ${GIT_URL} /build
                            cd /build
                            git checkout ${GIT_COMMIT}
                            make build
                            aws s3 cp ./bin/app s3://builds/${BUILD_NUMBER}/app
                        "
                '''
            }
        }
    }

    post {
        always {
            junit 'test-results.xml'
        }
    }
}
```

**Shared library for spawn:**
```groovy
// vars/spawnTest.groovy
def call(Map config) {
    def instanceId = sh(
        script: """
            spawn launch \
                --instance-type ${config.instanceType ?: 'c7i.large'} \
                --ttl ${config.ttl ?: '30m'} \
                --wait-for-ssh \
                --tags ci=true,build=${env.BUILD_NUMBER} \
                --quiet
        """,
        returnStdout: true
    ).trim()

    try {
        def instanceIp = sh(
            script: "spawn status ${instanceId} --json | jq -r '.network.public_ip'",
            returnStdout: true
        ).trim()

        // Run test command
        sh """
            ssh ec2-user@${instanceIp} '${config.testCommand}'
        """
    } finally {
        sh "aws ec2 terminate-instances --instance-ids ${instanceId}"
    }
}

// Usage in Jenkinsfile:
// spawnTest(instanceType: 'c7i.2xlarge', ttl: '30m', testCommand: './run-tests.sh')
```

---

## Performance Benchmarking

### Problem
Track performance regressions over time in CI.

### Solution: Automated benchmarking with spawn

**Workflow:**
```yaml
# .github/workflows/benchmark.yml
name: Performance Benchmark

on:
  push:
    branches: [main]
  schedule:
    - cron: '0 2 * * *'  # Daily at 2 AM

jobs:
  benchmark:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Run benchmark
        run: |
          # Launch instance
          INSTANCE_ID=$(spawn launch \
            --instance-type c7i.4xlarge \
            --ttl 1h \
            --wait-for-ssh \
            --on-complete terminate \
            --quiet)

          INSTANCE_IP=$(spawn status $INSTANCE_ID --json | jq -r '.network.public_ip')

          # Upload code
          tar czf code.tar.gz .
          scp code.tar.gz ec2-user@$INSTANCE_IP:~/
          ssh ec2-user@$INSTANCE_IP "tar xzf code.tar.gz"

          # Run benchmark
          ssh ec2-user@$INSTANCE_IP "cd ~ && ./benchmark.sh" > benchmark-results.json

      - name: Store results
        run: |
          # Store in S3 with timestamp
          TIMESTAMP=$(date +%Y%m%d-%H%M%S)
          aws s3 cp benchmark-results.json s3://benchmarks/results/${TIMESTAMP}.json

          # Extract key metrics
          THROUGHPUT=$(jq -r '.throughput' benchmark-results.json)
          LATENCY_P99=$(jq -r '.latency_p99' benchmark-results.json)

          # Store in CloudWatch for trending
          aws cloudwatch put-metric-data \
            --namespace "CI/Benchmarks" \
            --metric-name "Throughput" \
            --value "$THROUGHPUT" \
            --unit "Count/Second"

          aws cloudwatch put-metric-data \
            --namespace "CI/Benchmarks" \
            --metric-name "LatencyP99" \
            --value "$LATENCY_P99" \
            --unit "Milliseconds"

      - name: Check for regressions
        run: |
          # Compare to baseline
          aws s3 cp s3://benchmarks/baseline.json baseline.json

          python << 'EOF'
          import json

          with open('baseline.json') as f:
              baseline = json.load(f)

          with open('benchmark-results.json') as f:
              current = json.load(f)

          # Check for >10% regression
          throughput_change = (current['throughput'] - baseline['throughput']) / baseline['throughput']
          latency_change = (current['latency_p99'] - baseline['latency_p99']) / baseline['latency_p99']

          if throughput_change < -0.10:
              print(f"❌ Throughput regression: {throughput_change:.1%}")
              exit(1)

          if latency_change > 0.10:
              print(f"❌ Latency regression: {latency_change:.1%}")
              exit(1)

          print("✅ No performance regressions detected")
          EOF
```

---

## Load Testing in CI

### Problem
Validate application can handle expected load.

### Solution: spawn job array for load generation

**Workflow:**
```yaml
# .github/workflows/load-test.yml
name: Load Test

on:
  pull_request:
    types: [labeled]

jobs:
  load-test:
    if: contains(github.event.pull_request.labels.*.name, 'load-test')
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Deploy test environment
        run: |
          # Deploy application to test environment
          ./deploy-test-env.sh

          # Get application URL
          APP_URL=$(./get-test-env-url.sh)
          echo "Testing: $APP_URL"

      - name: Run load test
        run: |
          # Launch 100 load generators
          spawn launch \
            --instance-type c7i.large \
            --array 100 \
            --ttl 30m \
            --spot \
            --on-complete terminate \
            --iam-policy s3:FullAccess \
            --user-data "
              # Install load testing tool
              sudo yum install -y wrk

              # Run load test (each instance generates load)
              wrk -t 4 -c 100 -d 5m --latency ${APP_URL} > /tmp/results.txt

              # Upload results
              aws s3 cp /tmp/results.txt s3://load-test-results/${{ github.sha }}/worker-\${TASK_ARRAY_INDEX}.txt

              spored complete --status success
            "

      - name: Wait for completion
        run: |
          # Wait for all results
          timeout 10m bash -c '
            until [ $(aws s3 ls s3://load-test-results/${{ github.sha }}/ | wc -l) -eq 100 ]; do
              echo "Waiting for load test to complete..."
              sleep 10
            done
          '

      - name: Aggregate results
        run: |
          # Download all results
          aws s3 sync s3://load-test-results/${{ github.sha }}/ ./results/

          # Aggregate with Python
          python << 'EOF'
          import re
          import glob
          import json

          results = []
          for file in glob.glob('results/worker-*.txt'):
              with open(file) as f:
                  content = f.read()

              # Parse wrk output
              rps_match = re.search(r'Requests/sec:\s+([\d.]+)', content)
              latency_match = re.search(r'99.000%\s+([\d.]+)', content)

              if rps_match and latency_match:
                  results.append({
                      'rps': float(rps_match.group(1)),
                      'latency_p99_ms': float(latency_match.group(1))
                  })

          total_rps = sum(r['rps'] for r in results)
          avg_latency = sum(r['latency_p99_ms'] for r in results) / len(results)

          summary = {
              'total_rps': total_rps,
              'avg_latency_p99_ms': avg_latency,
              'workers': len(results)
          }

          print(json.dumps(summary, indent=2))

          with open('load-test-summary.json', 'w') as f:
              json.dump(summary, f)

          # Check if meets requirements
          if total_rps < 10000:
              print(f"❌ Failed: {total_rps:.0f} RPS < 10000 RPS requirement")
              exit(1)

          if avg_latency > 100:
              print(f"❌ Failed: {avg_latency:.1f}ms p99 latency > 100ms requirement")
              exit(1)

          print("✅ Load test passed")
          EOF

      - name: Comment PR
        uses: actions/github-script@v6
        with:
          script: |
            const fs = require('fs');
            const summary = JSON.parse(fs.readFileSync('load-test-summary.json', 'utf8'));

            github.rest.issues.createComment({
              issue_number: context.issue.number,
              owner: context.repo.owner,
              repo: context.repo.repo,
              body: `## Load Test Results\n\n` +
                    `- **Total RPS:** ${summary.total_rps.toFixed(0)}\n` +
                    `- **P99 Latency:** ${summary.avg_latency_p99_ms.toFixed(1)}ms\n` +
                    `- **Workers:** ${summary.workers}\n\n` +
                    `✅ All requirements met`
            })
```

---

## Scheduled Jobs

### Problem
Run nightly data processing jobs.

### Solution: GitHub Actions scheduled workflows

**Workflow:**
```yaml
# .github/workflows/nightly-processing.yml
name: Nightly Data Processing

on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM UTC daily
  workflow_dispatch:  # Allow manual trigger

jobs:
  process:
    runs-on: ubuntu-latest
    steps:
      - name: Configure AWS
        uses: aws-actions/configure-aws-credentials@v2
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      - name: Launch processing job
        run: |
          DATE=$(date +%Y-%m-%d)

          spawn launch \
            --instance-type m7i.4xlarge \
            --ttl 4h \
            --spot \
            --on-complete terminate \
            --iam-policy s3:FullAccess,dynamodb:ReadOnly \
            --tags job=nightly-processing,date=$DATE \
            --user-data "
              # Download data
              aws s3 sync s3://raw-data/$DATE /data/raw/

              # Process
              python /usr/local/bin/process_data.py --input /data/raw --output /data/processed

              # Upload results
              aws s3 sync /data/processed s3://processed-data/$DATE/

              # Write completion marker
              echo '{\"date\": \"$DATE\", \"status\": \"success\"}' | \
                aws s3 cp - s3://processed-data/$DATE/COMPLETE

              spored complete --status success
            "

      - name: Notify on failure
        if: failure()
        uses: dawidd6/action-send-mail@v3
        with:
          server_address: smtp.gmail.com
          server_port: 465
          username: ${{ secrets.EMAIL_USERNAME }}
          password: ${{ secrets.EMAIL_PASSWORD }}
          subject: 'Nightly processing failed'
          body: 'Nightly data processing job failed. Check GitHub Actions logs.'
          to: ops-team@example.com
```

---

## Best Practices

### 1. Always Terminate Instances
```yaml
# Use on-complete or always block
--on-complete terminate

# Or in workflow:
- name: Cleanup
  if: always()
  run: aws ec2 terminate-instances --instance-ids $INSTANCE_ID
```

### 2. Use Spot for CI
```bash
# Save 70% on CI costs
spawn launch --spot ...
```

### 3. Tag CI Instances
```bash
spawn launch --tags \
  ci=true,\
  pipeline=$CI_PIPELINE_ID,\
  commit=$COMMIT_SHA,\
  pr=$PR_NUMBER
```

### 4. Use Artifacts/S3 for Data Transfer
```yaml
# Don't scp large files
# Upload to S3, download in instance
```

### 5. Set Appropriate TTLs
```bash
# Short TTLs for safety
spawn launch --ttl 30m ...  # Tests
spawn launch --ttl 2h ...   # Builds
spawn launch --ttl 8h ...   # Nightly jobs
```

---

## Troubleshooting

### CI Job Hangs

**Problem:** CI job doesn't complete.

**Cause:** Instance not terminating.

**Solution:**
```yaml
# Always use timeout
- name: Run tests
  timeout-minutes: 30
  run: ...

# Always cleanup
- name: Cleanup
  if: always()
  run: aws ec2 terminate-instances --instance-ids $INSTANCE_ID
```

### High CI Costs

**Problem:** CI costs unexpectedly high.

**Cause:** Instances not terminated, using on-demand instead of spot.

**Solution:**
```bash
# Use spot instances
spawn launch --spot ...

# Set aggressive TTLs
spawn launch --ttl 30m ...

# Use on-complete terminate
spawn launch --on-complete terminate ...

# Monitor with tags
aws ec2 describe-instances --filters "Name=tag:ci,Values=true"
```

---

## See Also

- [How-To: Cost Optimization](cost-optimization.md) - Reduce CI costs
- [How-To: Spot Instances](spot-instances.md) - Use spot in CI
- [spawn launch](../reference/commands/launch.md) - Launch flags
- [GitHub Actions Documentation](https://docs.github.com/en/actions)
