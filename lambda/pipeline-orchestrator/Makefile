.PHONY: build deploy clean test

# Build for Lambda (ARM64)
build:
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap main.go
	zip function.zip bootstrap

# Deploy to AWS (default: production)
deploy: build
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-pipeline-orchestrator \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=production \
		--resolve-s3

# Deploy to staging
deploy-staging: build
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-pipeline-orchestrator-staging \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=staging \
		--resolve-s3

# Test locally (requires SAM CLI)
test-local:
	echo '{"pipeline_id": "test-pipeline-001"}' | \
		sam local invoke PipelineOrchestratorFunction

# Clean build artifacts
clean:
	rm -f bootstrap function.zip

# Run Go tests
test:
	go test -v ./...

# Update Lambda code only (faster than full deploy)
update-code: build
	aws lambda update-function-code \
		--function-name spawn-pipeline-orchestrator-production \
		--zip-file fileb://function.zip

# View logs
logs:
	aws logs tail /aws/lambda/spawn-pipeline-orchestrator-production --follow

# Invoke Lambda with test event
invoke-test:
	aws lambda invoke \
		--function-name spawn-pipeline-orchestrator-production \
		--payload '{"pipeline_id": "test-pipeline-001"}' \
		--invocation-type Event \
		response.json
	cat response.json
