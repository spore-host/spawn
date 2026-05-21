module github.com/spore-host/spawn/lambda/dashboard-api

go 1.25.5

require (
	github.com/aws/aws-lambda-go v1.51.1
	github.com/aws/aws-sdk-go-v2 v1.41.3
	github.com/aws/aws-sdk-go-v2/config v1.32.11
	github.com/aws/aws-sdk-go-v2/credentials v1.19.11
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue v1.20.29
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.53.5
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.294.0
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.21
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.8
	github.com/google/uuid v1.6.0
	github.com/scttfrdmn/strata v0.22.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.6 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.19 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.19 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.19 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.32.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.16 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/s3 v1.96.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssm v1.68.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.16 // indirect
	github.com/aws/smithy-go v1.24.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
