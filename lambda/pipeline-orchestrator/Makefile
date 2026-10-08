.PHONY: build deploy clean test

# The deployed function's name, in ONE place. It must match template.yaml's
# `FunctionName:` exactly — there is no Environment suffix. update-code, logs and
# invoke-test each used to hardcode `spawn-pipeline-orchestrator-production`,
# which has never existed in any account, so all three failed on a name rather
# than doing anything (#754). SAM has no say in this: FunctionName is explicit in
# the template, so `Environment` names the STACK, never the function.
FUNCTION := spawn-pipeline-orchestrator
STACK    := spawn-pipeline-orchestrator

# Build for Lambda (ARM64) into build/, which must contain NOTHING but the bootstrap
# binary: `sam deploy` zips the CodeUri directory with no filtering (samcli os.walk,
# dotfiles included) and there is no .samignore. This template previously declared no
# CodeUri at all, which packaged whatever cwd happened to be (#645).
# function.zip stays OUTSIDE build/ for the same reason — update-code needs it.
build:
	mkdir -p build
	@# Build the PACKAGE (`.`), not `main.go`: naming files compiles only those files, so
	@# a second source file would ship a binary silently missing it (see ttl-reaper).
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o build/bootstrap .
	@# -j so the zip has bootstrap at its root, not build/bootstrap.
	zip -j function.zip build/bootstrap

# Deploy to AWS (default: production)
deploy: build
	sam deploy \
		--template-file template.yaml \
		--stack-name $(STACK) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=production \
		--resolve-s3

# Deploy to staging
deploy-staging: build
	sam deploy \
		--template-file template.yaml \
		--stack-name $(STACK)-staging \
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
	rm -rf build

# Run Go tests
test:
	go test -v ./...

# Update Lambda code only (faster than full deploy).
#
# This is a DEBUG shortcut, not a deploy. template.yaml pins the function's code
# to an S3 object, so CloudFormation considers that object the truth: the next
# `make deploy` silently reverts whatever this pushed, with no warning and no
# failure. Ship real changes with `make deploy` (#754).
update-code: build
	@echo "WARNING: out-of-band code push — the next 'make deploy' reverts it (#754)." >&2
	aws lambda update-function-code \
		--function-name $(FUNCTION) \
		--zip-file fileb://function.zip

# View logs
logs:
	aws logs tail /aws/lambda/$(FUNCTION) --follow

# Invoke Lambda with test event
invoke-test:
	aws lambda invoke \
		--function-name $(FUNCTION) \
		--payload '{"pipeline_id": "test-pipeline-001"}' \
		--invocation-type Event \
		response.json
	cat response.json
