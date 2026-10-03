.PHONY: build deploy clean test

BINARY_NAME=build/bootstrap
ENV?=production

# Build into build/, which must contain NOTHING but the bootstrap binary: `sam deploy`
# zips the CodeUri directory with no filtering (samcli os.walk, dotfiles included) and
# there is no .samignore. With CodeUri: . this shipped the dev binary, a nested
# function.zip and every source file — 75.7 MiB deployed for a ~7 MiB artifact (#645).
# function.zip stays OUTSIDE build/ for the same reason.
build:
	mkdir -p build
	@# Build the PACKAGE (`.`), not `main.go`: naming files compiles only those files, so
	@# adding a second source file would ship a binary silently missing it (see ttl-reaper).
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o $(BINARY_NAME) .
	@# -j so the zip has bootstrap at its root, not build/bootstrap.
	zip -j function.zip $(BINARY_NAME)

deploy: build
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-autoscale-orchestrator-$(ENV) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=$(ENV) \
		--resolve-s3

deploy-staging:
	$(MAKE) deploy ENV=staging

deploy-production:
	$(MAKE) deploy ENV=production

test:
	go test -v ./...

clean:
	rm -f $(BINARY_NAME) function.zip
	rm -rf build
	rm -rf .aws-sam

logs:
	sam logs --stack-name spawn-autoscale-orchestrator-$(ENV) --tail

validate:
	sam validate --template-file template.yaml
