package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/spore-host/cohort"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/mpicohort"
	"github.com/spore-host/spawn/pkg/taskcohort"
)

// sdkLaunchError is a RunInstances rejection as aws.Client.Launch returns it:
// a *aws.LaunchError around the SDK's error, whose text is "failed to launch
// instance: CODE: operation error EC2: RunInstances, https response error
// StatusCode: 400, RequestID: ..., api error CODE: <AWS message>".
type sdkLaunchError struct {
	code string
	sdk  error
}

func (e sdkLaunchError) Error() string {
	return fmt.Sprintf("failed to launch instance: %s: %v", e.code, e.sdk)
}

func (e sdkLaunchError) Unwrap() []error { return []error{&aws.LaunchError{Code: e.code}, e.sdk} }

const sdkRequestID = "5f3c0d2e-9b1a-4c7e-8d6f-1a2b3c4d5e6f"

// runInstancesRejection is the SDK's error for a 400 from RunInstances, built
// from the SDK's own error types so its text is the SDK's.
func runInstancesRejection(code, msg string) sdkLaunchError {
	return sdkLaunchError{code: code, sdk: &smithy.OperationError{
		ServiceID: "EC2", OperationName: "RunInstances",
		Err: &awshttp.ResponseError{
			RequestID: sdkRequestID,
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusBadRequest}},
				Err:      &smithy.GenericAPIError{Code: code, Message: msg},
			},
		},
	}}
}

// #1462: a job-array or MPI member's failure line reads like a pool worker's:
// the AWS code once and AWS's message once, without the SDK's operation,
// status and RequestID wrapping. Both classifiers are checked, because the
// point is that the two paths agree.
func TestCohortMemberReasonCarriesTheAWSMessageOnce(t *testing.T) {
	const code = "InvalidParameterValue"
	const msg = "Value (spawn-instance-ja-1) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name"
	err := runInstancesRejection(code, msg)
	if !strings.Contains(err.Error(), sdkRequestID) || strings.Count(err.Error(), code) != 2 {
		t.Fatalf("fixture %q is not the SDK's shape (the code twice, a RequestID)", err)
	}
	for name, classify := range map[string]func(error) cohort.Fault{
		"mpicohort (job array, MPI)": mpicohort.Classifier{}.Classify,
		"taskcohort (pool)":          taskcohort.Classifier{}.Classify,
	} {
		t.Run(name, func(t *testing.T) {
			f := classify(err)
			reason := cohortMemberReason(cohort.Record{Terminal: &f, ReachedPhase: cohort.PhaseLaunchAcked})
			if n := strings.Count(reason, msg); n != 1 {
				t.Errorf("member reason has AWS's message %d times, want once:\n%s", n, reason)
			}
			if n := strings.Count(reason, code); n != 1 {
				t.Errorf("member reason has the code %d times, want once:\n%s", n, reason)
			}
			for _, sdk := range []string{"RequestID", sdkRequestID, "operation error", "StatusCode"} {
				if strings.Contains(reason, sdk) {
					t.Errorf("member reason carries the SDK's %q:\n%s", sdk, reason)
				}
			}
		})
	}
}
