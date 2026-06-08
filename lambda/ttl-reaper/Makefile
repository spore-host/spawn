.PHONY: build deploy deploy-staging deploy-production test clean logs validate

BINARY_NAME=bootstrap
ENV?=production

build:
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o $(BINARY_NAME) main.go
	zip function.zip $(BINARY_NAME)

# Deploy into the infra account (966362334030). Override on the command line:
#   make deploy DRY_RUN=false NOTIFY_URL=https://hooks.slack.com/... \
#     ROLE_ARNS='arn:aws:iam::ACCT_A:role/spawn-ttl-reaper-ec2,arn:aws:iam::ACCT_B:role/spawn-ttl-reaper-ec2'
# ROLE_ARNS must list one cross-account role per spore-launching account.
DRY_RUN?=true
NOTIFY_URL?=
SCAN_SELF?=false
ROLE_ARNS?=arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2
deploy: build
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-ttl-reaper-$(ENV) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides Environment=$(ENV) DryRun=$(DRY_RUN) NotifyUrl=$(NOTIFY_URL) ScanSelf=$(SCAN_SELF) "RoleArns=$(ROLE_ARNS)" \
		--resolve-s3

deploy-staging:
	$(MAKE) deploy ENV=staging

deploy-production:
	$(MAKE) deploy ENV=production

test:
	go test -v ./...

clean:
	rm -f $(BINARY_NAME) function.zip
	rm -rf .aws-sam

logs:
	sam logs --stack-name spawn-ttl-reaper-$(ENV) --tail

validate:
	sam validate --template-file template.yaml
