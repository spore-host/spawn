.PHONY: build deploy clean test

# The version stamped onto the function as spawn:version (#654). `git describe`
# rather than a bare tag: it yields 0.126.1 on a tag, 0.126.1-3-gabc when ahead,
# and a -dirty suffix for uncommitted changes — so the stamp says "deployed from
# uncommitted code" out loud instead of claiming a release. Leading v stripped to
# match pkg/reaperdeploy.Tags.
# Days to keep this function's CloudWatch logs (#653). Configurable; the default
# matches what the template previously hardcoded, so behaviour is unchanged.
LOG_RETENTION_DAYS?=30
# Reconciliation schedule (#772). Defaults match the template, so these change
# nothing unless set: `make deploy SCHEDULE_STATE=DISABLED` turns the schedule
# off properly, through CloudFormation. Doing it with `aws events disable-rule`
# would be stack drift and would be silently reverted by the next deploy.
SCHEDULE_STATE?=ENABLED
SCHEDULE_RATE?=rate(1 minute)
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)

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
		--parameter-overrides Environment=$(ENV) Version=$(VERSION) LogRetentionDays=$(LOG_RETENTION_DAYS) ScheduleState=$(SCHEDULE_STATE) 'ScheduleRate=$(SCHEDULE_RATE)' \
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
