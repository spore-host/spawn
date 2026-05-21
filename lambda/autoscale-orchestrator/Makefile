.PHONY: build deploy clean test

BINARY_NAME=bootstrap
ENV?=production

build:
	GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o $(BINARY_NAME) main.go
	zip function.zip $(BINARY_NAME)

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
	rm -rf .aws-sam

logs:
	sam logs --stack-name spawn-autoscale-orchestrator-$(ENV) --tail

validate:
	sam validate --template-file template.yaml
