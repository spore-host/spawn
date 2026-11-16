// Package sdkanswer answers AWS SDK v2 calls in-process, for unit tests that
// need a real SDK client (so the client's own middleware runs, including the
// libs/tags/tagtest Detector, spawn#1652) but no network.
//
// The answer runs at the end of the Initialize step, after every other
// Initialize middleware and before serialization, so nothing is sent: a call
// returns what the test's Func returns. A Func that returns a nil output and
// no error gets the operation's zero output (for the services in clientTypes).
package sdkanswer

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/imagebuilder"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// Func answers one call: service is the SDK service ID ("IAM", "EC2", ...),
// op the operation name, in the operation's input. A nil out with a nil error
// is the operation's zero output.
type Func func(service, op string, in any) (out any, err error)

// MiddlewareID is the ID the answerer registers in the Initialize step.
const MiddlewareID = "spawn/sdkanswer"

// Install makes every client built from cfg answer through f.
func Install(cfg *aws.Config, f Func) {
	cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(MiddlewareID,
			func(ctx context.Context, in middleware.InitializeInput, _ middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
				service, op := awsmiddleware.GetServiceID(ctx), awsmiddleware.GetOperationName(ctx)
				out, err := f(service, op, in.Parameters)
				if err != nil {
					return middleware.InitializeOutput{}, middleware.Metadata{}, err
				}
				if out == nil {
					if out, err = zeroOutput(service, op); err != nil {
						return middleware.InitializeOutput{}, middleware.Metadata{}, err
					}
				}
				return middleware.InitializeOutput{Result: out}, middleware.Metadata{}, nil
			}), middleware.After)
	})
}

// Config is an aws.Config for region whose clients answer through f, with
// static credentials, no retries, and an HTTP client that refuses every
// request (nothing reaches it while f answers).
func Config(region string, f Func) aws.Config {
	cfg := aws.Config{
		Region:      region,
		Credentials: aws.AnonymousCredentials{},
		HTTPClient:  refuse{},
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	Install(&cfg, f)
	return cfg
}

// Err is an API error with code, as a service returns it.
func Err(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " (sdkanswer)"}
}

type refuse struct{}

func (refuse) Do(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("sdkanswer: no network in unit tests")
}

// clientTypes maps a service ID to its client, whose methods give each
// operation's output type.
var clientTypes = map[string]reflect.Type{
	"CloudWatch Logs": reflect.TypeOf(&cloudwatchlogs.Client{}),
	"DynamoDB":        reflect.TypeOf(&dynamodb.Client{}),
	"EC2":             reflect.TypeOf(&ec2.Client{}),
	"EventBridge":     reflect.TypeOf(&eventbridge.Client{}),
	"FSx":             reflect.TypeOf(&fsx.Client{}),
	"IAM":             reflect.TypeOf(&iam.Client{}),
	"imagebuilder":    reflect.TypeOf(&imagebuilder.Client{}),
	"Lambda":          reflect.TypeOf(&lambda.Client{}),
	"S3":              reflect.TypeOf(&s3.Client{}),
	"SNS":             reflect.TypeOf(&sns.Client{}),
	"SQS":             reflect.TypeOf(&sqs.Client{}),
	"STS":             reflect.TypeOf(&sts.Client{}),
}

func zeroOutput(service, op string) (any, error) {
	ct, ok := clientTypes[service]
	if !ok {
		return nil, fmt.Errorf("sdkanswer: no zero output for service %q; answer %s yourself", service, op)
	}
	// nosemgrep: go.lang.security.audit.unsafe-reflect-by-name.unsafe-reflect-by-name -- op is the SDK operation a test's own client is calling, in a test-only answerer, not untrusted input
	m, ok := ct.MethodByName(op)
	if !ok || m.Type.NumOut() != 2 || m.Type.Out(0).Kind() != reflect.Pointer {
		return nil, fmt.Errorf("sdkanswer: %s has no operation %s", service, op)
	}
	return reflect.New(m.Type.Out(0).Elem()).Interface(), nil
}
