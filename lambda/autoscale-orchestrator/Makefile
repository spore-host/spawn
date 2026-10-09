.PHONY: build deploy clean test

# The version stamped onto the function as spawn:version (#654). `git describe`
# rather than a bare tag: it yields 0.126.1 on a tag, 0.126.1-3-gabc when ahead,
# and a -dirty suffix for uncommitted changes — so the stamp says "deployed from
# uncommitted code" out loud instead of claiming a release. Leading v stripped to
# match pkg/reaperdeploy.Tags.
# Days to keep this function's CloudWatch logs (#653). Configurable; the default
# matches what the template previously hardcoded, so behaviour is unchanged.
LOG_RETENTION_DAYS?=30
# Reconciliation schedule state (#772). Default matches the template, so it
# changes nothing unless set: `make deploy SCHEDULE_STATE=DISABLED` turns the
# schedule off properly, through CloudFormation. Doing it with
# `aws events disable-rule` would be stack drift and would be silently reverted
# by the next deploy.
#
# ScheduleRate is deliberately NOT passed here, and must not be added without
# switching to a params file. `--parameter-overrides` cannot carry a value
# containing a SPACE, and every valid EventBridge rate has one
# ("rate(1 minute)"). Passing it inline failed a real staging deploy with
# `Parameter ScheduleExpression is not valid` and rolled the stack back. The
# ttl-reaper Makefile documents this trap at length and uses a file:// params
# file for exactly this reason. The template default is correct, so the rate is
# changed by editing the template until someone needs it per-environment.
# Explicit, never ambient. A `sam deploy` with no --region resolves one from the
# environment, and getting it wrong does not fail — it CREATES A SECOND STACK in
# the wrong region while the real one sits untouched. #650 is the same family:
# an unspecified value silently taking a default disarmed the live reaper.
REGION?=us-east-1

# Passed explicitly even though it equals the template default, because
# `--parameter-overrides` is not additive: a parameter omitted there is reset to
# the template default rather than keeping its live value. It is benign only as
# long as the two happen to agree, which is luck rather than design (#650).
EC2_ROLE_ARN?=arn:aws:iam::435415984226:role/spawn-autoscale-ec2

SCHEDULE_STATE?=ENABLED
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
		--region $(REGION) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=$(ENV) Version=$(VERSION) LogRetentionDays=$(LOG_RETENTION_DAYS) ScheduleState=$(SCHEDULE_STATE) EC2RoleArn=$(EC2_ROLE_ARN) \
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
