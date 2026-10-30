package taskcohort

import (
	"context"
	"fmt"
	"strings"
	"testing"

	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/cohort"
)

// The Fault is what `pool create` prints for a worker that did not come up
// (#1448), so it carries the AWS code and AWS's message: from the AWS error
// when Launch did not wrap it in a LaunchError, and with the deadline noted
// when the deadline ended a retry of it.
func TestClassifierFaultCarriesTheAWSReason(t *testing.T) {
	rejection := &smithy.GenericAPIError{
		Code:    "InvalidParameterValue",
		Message: "Value (spawn-instance-pool-b566ee05f2b9a8c1) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name",
	}
	cases := map[string]struct {
		err      error
		wantMsgs []string
	}{
		"AWS error, not a LaunchError": {
			err:      fmt.Errorf("store launch config object: %w", rejection),
			wantMsgs: []string{rejection.Message},
		},
		"retry ended by the launch deadline": {
			err:      fmt.Errorf("RunInstances retry stopped (%w) after 3 attempt(s) rejected with: %w", context.DeadlineExceeded, rejection),
			wantMsgs: []string{rejection.Message, context.DeadlineExceeded.Error()},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := Classifier{}.Classify(tc.err)
			if f.Class != cohort.FaultTerminal || f.Code != "InvalidParameterValue" {
				t.Errorf("Fault = %s/%s, want terminal/InvalidParameterValue", f.Class, f.Code)
			}
			for _, want := range tc.wantMsgs {
				if !strings.Contains(f.Message, want) {
					t.Errorf("Fault message %q does not contain %q", f.Message, want)
				}
			}
		})
	}
}
