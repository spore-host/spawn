.PHONY: build deploy deploy-staging deploy-production test clean logs validate

BINARY_NAME=bootstrap
ENV?=production

build:
	@# Build the PACKAGE (`.`), not `main.go`. Naming files builds only those files, so
	@# adding a second source file (expiry.go, #466) to a `go build main.go` recipe
	@# fails outright if it is referenced — and would ship a binary missing it if not.
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o $(BINARY_NAME) .
	zip function.zip $(BINARY_NAME)

# Deploy into the infra account (966362334030). Override on the command line:
#   make deploy DRY_RUN=false NOTIFY_URL=https://hooks.slack.com/... \
#     ROLE_ARNS='arn:aws:iam::ACCT_A:role/spawn-ttl-reaper-ec2,arn:aws:iam::ACCT_B:role/spawn-ttl-reaper-ec2'
# ROLE_ARNS must list one cross-account role per spore-launching account.
#
# Every template parameter the DNS features need is passed explicitly. `sam deploy`
# marks any parameter it is NOT given as UsePreviousValue, so an omitted one is
# unreachable from here: on an existing stack it silently keeps whatever is
# deployed, and on a fresh stack it falls back to the template Default. DNS_SWEEP
# in particular defaults to 'false', so leaving it out made #438 impossible to
# enable via `make deploy` even though the code and the README documented it.
#
# Overrides go through a generated params file, NOT inline Key=Value pairs. sam
# matches the WHOLE --parameter-overrides string against one regex, and an empty
# value makes that match fail — so with several pairs and one empty (NOTIFY_URL /
# DNS_ZONE_ID / DNS_DOMAIN are all empty by default) sam either rejects the line
# outright or, worse, silently falls through to another pattern and mis-keys every
# parameter (observed: NotifyUrl swallowed as the literal key "ParameterKey").
# Repeating the flag doesn't help either — the last occurrence wins and the rest are
# dropped. A file:// params file is the only form that handles empty values.
DRY_RUN?=true
NOTIFY_URL?=
SCAN_SELF?=false
ROLE_ARNS?=arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2
DNS_ZONE_ID?=
DNS_DOMAIN?=
DNS_SWEEP?=false
# DNS expiry (#466): deletes records, so it is passed explicitly for the same reason
# as DNS_SWEEP above — an omitted parameter is unreachable from here. Off by default.
DNS_EXPIRE?=false
ACCOUNTS_TABLE?=spore-portal-accounts
ACCOUNTS_TABLE_KEY_ARN?=
# Failure alarms (#469). Defaults ON, unlike every other flag here: the others change
# what the reaper DOES, so they start off. This one only changes what it REPORTS, and
# the reason #469 exists is that the answer was "nothing".
ALARMS_ENABLED?=true
ALARM_TOPIC_ARN?=
PARAMS_FILE=.deploy-params.yaml
deploy: build
	@printf 'Environment: "%s"\nDryRun: "%s"\nNotifyUrl: "%s"\nScanSelf: "%s"\nRoleArns: "%s"\nDnsZoneId: "%s"\nDnsDomain: "%s"\nDnsSweep: "%s"\nDnsExpire: "%s"\nAccountsTable: "%s"\nAccountsTableKeyArn: "%s"\nAlarmsEnabled: "%s"\nAlarmTopicArn: "%s"\n' \
		'$(ENV)' '$(DRY_RUN)' '$(NOTIFY_URL)' '$(SCAN_SELF)' '$(ROLE_ARNS)' '$(DNS_ZONE_ID)' '$(DNS_DOMAIN)' '$(DNS_SWEEP)' '$(DNS_EXPIRE)' '$(ACCOUNTS_TABLE)' '$(ACCOUNTS_TABLE_KEY_ARN)' '$(ALARMS_ENABLED)' '$(ALARM_TOPIC_ARN)' > $(PARAMS_FILE)
	@# Echo the mode-setting parameters so a deploy can't quietly leave a feature off
	@# — but never NotifyUrl, which is a Slack webhook secret. ALARM_TOPIC_ARN is an
	@# ARN, not a credential, and alarms wired to nothing is worth seeing here.
	@echo "deploying: dry-run=$(DRY_RUN) scan-self=$(SCAN_SELF) dns-sweep=$(DNS_SWEEP) dns-expire=$(DNS_EXPIRE) zone='$(DNS_ZONE_ID)' domain='$(DNS_DOMAIN)' alarms=$(ALARMS_ENABLED) alarm-topic='$(ALARM_TOPIC_ARN)'"
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-ttl-reaper-$(ENV) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides file://$(PARAMS_FILE) \
		--resolve-s3

deploy-staging:
	$(MAKE) deploy ENV=staging

deploy-production:
	$(MAKE) deploy ENV=production

test:
	go test -v ./...

clean:
	rm -f $(BINARY_NAME) function.zip $(PARAMS_FILE)
	rm -rf .aws-sam

logs:
	sam logs --stack-name spawn-ttl-reaper-$(ENV) --tail

validate:
	sam validate --template-file template.yaml
