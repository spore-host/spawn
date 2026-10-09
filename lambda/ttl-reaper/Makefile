.PHONY: build deploy deploy-staging deploy-production test clean logs validate

BINARY_NAME=build/bootstrap
ENV?=production
# The version stamped onto the function as spawn:version (#654). `git describe`
# rather than a bare tag on purpose: it yields v0.126.1 on a tag, v0.126.1-3-gabc
# when ahead, and a -dirty suffix when the tree has uncommitted changes — so the
# stamp says "deployed from uncommitted code" out loud instead of claiming a
# release. The leading v is stripped to match pkg/reaperdeploy.Tags.
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
# The reaper stack lives in us-east-1. Explicit, because every AWS call below
# must target the same region the stack is in (#650 follow-up).
REGION?=us-east-1

# Build into build/, which must contain NOTHING but the bootstrap binary: `sam deploy`
# zips the CodeUri directory with no filtering (samcli os.walk, dotfiles included) and
# there is no .samignore. With CodeUri: . this shipped the dev binary, a nested
# function.zip and every source file — 75.7 MiB deployed for a ~7 MiB artifact (#645).
# function.zip stays OUTSIDE build/ for the same reason.
build:
	mkdir -p build
	@# Build the PACKAGE (`.`), not `main.go`. Naming files builds only those files, so
	@# adding a second source file (expiry.go, #466) to a `go build main.go` recipe
	@# fails outright if it is referenced — and would ship a binary missing it if not.
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o $(BINARY_NAME) .
	@# -j so the zip has bootstrap at its root, not build/bootstrap.
	zip -j function.zip $(BINARY_NAME)

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

# Map each Make variable to its CloudFormation parameter name. Only variables the
# CALLER actually set are sent as overrides; everything else inherits whatever is
# deployed (spawn#650).
#
# $(origin V) is what makes that distinction possible: it reports "command line"
# or "environment" for a value the caller supplied, and "file" for the `?=`
# defaults above. Before this, all 13 defaults were asserted on every deploy, so
# the documented `make deploy` would have set DryRun=true on a stack running
# DryRun=false — DISARMING the live production reaper, silently, since it keeps
# running and reporting and merely stops terminating anything. It would also have
# turned DnsSweep off, blanked the DNS zone/domain, and detached AlarmTopicArn so
# every failure alarm fired into nothing.
#
# The Makefile comments above explain the opposite hazard (an omitted parameter
# becomes UsePreviousValue, which is why #438 was unreachable from here). Passing
# everything explicitly fixed that and created this. Read-then-write fixes both.
PARAM_MAP = \
	Environment:ENV \
	DryRun:DRY_RUN \
	NotifyUrl:NOTIFY_URL \
	ScanSelf:SCAN_SELF \
	RoleArns:ROLE_ARNS \
	DnsZoneId:DNS_ZONE_ID \
	DnsDomain:DNS_DOMAIN \
	DnsSweep:DNS_SWEEP \
	DnsExpire:DNS_EXPIRE \
	NetResources:NET_RESOURCES \
	AccountsTable:ACCOUNTS_TABLE \
	AccountsTableKeyArn:ACCOUNTS_TABLE_KEY_ARN \
	AlarmsEnabled:ALARMS_ENABLED \
	AlarmTopicArn:ALARM_TOPIC_ARN \
	MaxAge:MAX_AGE \
	Regions:REGIONS \
	Schedule:SCHEDULE \
	Version:VERSION

# Emit one `CfnKey=value` line per EXPLICITLY-supplied parameter. Deliberately not
# JSON: Make has no comma literal (`$(,)` is undefined and silently expands to
# nothing, which produced `{"a":"1" "b":"2"}` — valid-looking and unparseable), and
# single-quoting each line is what lets a value contain a space or a comma.
# Schedule is `rate(10 minutes)` and Regions is comma-separated, so both matter.
define EMIT_EXPLICIT
$(if $(filter command line environment,$(origin $(2))),printf '%s\n' '$(1)=$($(2))' >> .deploy-explicit.txt;,)
endef

deploy: build
	@# Read the live stack first, so an unspecified parameter keeps what is
	@# deployed instead of being reset to this Makefile's idea of a default.
	@# --region is NOT optional here. Without it the describe falls back to the
	@# profile's default region, the stack is "not found", the `||` branch writes
	@# an EMPTY live set, and every parameter falls back to the TEMPLATE default —
	@# which for DryRun is 'true'. That disarmed the live production reaper, i.e.
	@# precisely the failure #650 was written to prevent, reintroduced by the fix.
	@aws cloudformation get-template-summary --region $(REGION) \
		--template-body file://template.yaml --output json > .deploy-summary.json
	@# Separate the two failure modes instead of collapsing them into '[]': a
	@# genuinely absent stack (first deploy) is fine, but a describe that fails for
	@# any OTHER reason must abort rather than silently reset the world.
	@if ! aws cloudformation describe-stacks --region $(REGION) \
		--stack-name spawn-ttl-reaper-$(ENV) \
		--query 'Stacks[0].Parameters' --output json > .deploy-live.json 2>/tmp/.deploy-live.err; then \
		if grep -q 'does not exist' /tmp/.deploy-live.err; then \
			echo "no existing stack spawn-ttl-reaper-$(ENV) in $(REGION) — using template defaults"; \
			echo '[]' > .deploy-live.json; \
		else \
			echo "ERROR: could not read the live stack; refusing to deploy with template defaults" >&2; \
			cat /tmp/.deploy-live.err >&2; \
			exit 1; \
		fi; \
	fi
	@: > .deploy-explicit.txt
	@$(foreach pair,$(PARAM_MAP),$(call EMIT_EXPLICIT,$(word 1,$(subst :, ,$(pair))),$(word 2,$(subst :, ,$(pair)))))
	@python3 scripts/merge_deploy_params.py .deploy-summary.json .deploy-live.json \
		.deploy-explicit.txt > $(PARAMS_FILE)
	@rm -f .deploy-summary.json .deploy-live.json .deploy-explicit.txt
	@# Echo the mode-setting parameters so a deploy can't quietly leave a feature
	@# off — but never NotifyUrl, which is a Slack webhook secret. These are read
	@# back from the MERGED file, so this shows what will actually be sent rather
	@# than what this Makefile happens to default to.
	@awk -F': ' '/^(DryRun|ScanSelf|DnsSweep|DnsExpire|NetResources|AlarmsEnabled|DnsZoneId|DnsDomain|AlarmTopicArn|MaxAge|Schedule|Version):/ \
		{ printf "  %s %s\n", $$1, $$2 }' $(PARAMS_FILE)
	sam deploy \
		--template-file template.yaml \
		--stack-name spawn-ttl-reaper-$(ENV) \
		--region $(REGION) \
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
	rm -rf build
	rm -rf .aws-sam

logs:
	sam logs --stack-name spawn-ttl-reaper-$(ENV) --tail

validate:
	sam validate --template-file template.yaml
