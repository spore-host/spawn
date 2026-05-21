.PHONY: build clean test deploy upload stack-deploy cross-account-role

# Configurable — override with environment variables
S3_BUCKET     ?= spawn-binaries-us-east-1
FUNCTION_NAME ?= spore-bot
STACK_NAME    ?= spore-bot
REGION        ?= us-east-1
CFN_TEMPLATE  := ../../deployment/cloudformation/spore-bot.yaml
ROLE_TEMPLATE := ../../deployment/cloudformation/bot-cross-account-role.yaml

## build: Compile Lambda binary and zip for deployment
build:
	@echo "Building spore-bot Lambda (linux/arm64)..."
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap .
	zip function.zip bootstrap
	@echo "Build complete: function.zip"

## clean: Remove build artifacts
clean:
	rm -f bootstrap function.zip

## test: Run unit tests
test:
	go test . -v

## upload: Build and upload zip to S3
upload: build
	@echo "Uploading to s3://$(S3_BUCKET)/spore-bot/function.zip..."
	AWS_PROFILE=spore-host-infra aws s3 cp function.zip \
		s3://$(S3_BUCKET)/spore-bot/function.zip \
		--region $(REGION)
	@echo "Upload complete"

## stack-deploy: Deploy or update the CloudFormation stack
stack-deploy:
	AWS_PROFILE=spore-host-infra aws cloudformation deploy \
		--stack-name $(STACK_NAME) \
		--template-file $(CFN_TEMPLATE) \
		--capabilities CAPABILITY_IAM \
		--parameter-overrides \
			Environment=production \
			LambdaCodeBucket=$(S3_BUCKET) \
			LambdaCodeKey=spore-bot/function.zip \
		--region $(REGION)
	@echo "Stack deployed. Function URL:"
	@AWS_PROFILE=spore-host-infra aws cloudformation describe-stacks \
		--stack-name $(STACK_NAME) --region $(REGION) \
		--query 'Stacks[0].Outputs[?OutputKey==`LambdaFunctionUrl`].OutputValue' \
		--output text

## deploy: Full deploy (upload + stack update)
deploy: upload stack-deploy

## cross-account-role: Deploy cross-account role in another account
## Usage: BOT_LAMBDA_ROLE_ARN=arn:... TAG_PREFIX=prism make cross-account-role
cross-account-role:
	@if [ -z "$(BOT_LAMBDA_ROLE_ARN)" ]; then echo "BOT_LAMBDA_ROLE_ARN is required"; exit 1; fi
	aws cloudformation deploy \
		--stack-name spawn-bot-cross-account \
		--template-file $(ROLE_TEMPLATE) \
		--capabilities CAPABILITY_NAMED_IAM \
		--parameter-overrides \
			BotLambdaRoleArn=$(BOT_LAMBDA_ROLE_ARN) \
			TagPrefix=$(or $(TAG_PREFIX),spawn) \
		--region $(REGION)
	@echo "Cross-account role ARN:"
	@aws cloudformation describe-stacks --stack-name spawn-bot-cross-account --region $(REGION) \
		--query 'Stacks[0].Outputs[?OutputKey==`RoleArn`].OutputValue' --output text
