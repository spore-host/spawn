package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	smithy "github.com/aws/smithy-go"
)

// iamProfilePropagationFake is a smithy.APIError with the exact code+message
// shape RunInstances returns for the spawn#572 transient: InvalidParameterValue
// whose message says the IAM instance profile name is invalid — the profile is
// visible on the IAM control plane (CreateOrGetInstanceProfile already
// confirmed it via GetInstanceProfile) but hasn't yet propagated to EC2's
// separate consumption-side path.
type iamProfilePropagationFake struct{}

func (e *iamProfilePropagationFake) Error() string {
	return "operation error EC2: RunInstances, https response error StatusCode: 400, " +
		"RequestID: e09cc19e-0815-460d-9278-f86378fdfb34, api error InvalidParameterValue: " +
		"Value (spawn-instance-0346c413) for parameter iamInstanceProfile.name is invalid. " +
		"Invalid IAM Instance Profile name"
}
func (e *iamProfilePropagationFake) ErrorCode() string { return "InvalidParameterValue" }
func (e *iamProfilePropagationFake) ErrorMessage() string {
	return "Value (spawn-instance-0346c413) for parameter iamInstanceProfile.name is invalid. " +
		"Invalid IAM Instance Profile name"
}
func (e *iamProfilePropagationFake) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// iamProfilePropagationFakeWithMsg is InvalidParameterValue with an arbitrary,
// non-IAM-profile message — a genuinely different, non-transient
// InvalidParameterValue (bad instance type, bad AMI, ...) that must NOT be
// retried.
type iamProfilePropagationFakeWithMsg struct{ msg string }

func (e *iamProfilePropagationFakeWithMsg) Error() string {
	return "api error InvalidParameterValue: " + e.msg
}
func (e *iamProfilePropagationFakeWithMsg) ErrorCode() string    { return "InvalidParameterValue" }
func (e *iamProfilePropagationFakeWithMsg) ErrorMessage() string { return e.msg }
func (e *iamProfilePropagationFakeWithMsg) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}

func TestIsTransientIAMInstanceProfilePropagation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"exact repro from #572 (code + message match)",
			&iamProfilePropagationFake{},
			true,
		},
		{
			"different InvalidParameterValue (bad instance type)",
			&iamProfilePropagationFakeWithMsg{msg: "Invalid value 'bogus.type' for instanceType"},
			false,
		},
		{
			"different InvalidParameterValue (bad AMI)",
			&iamProfilePropagationFakeWithMsg{msg: "The image id '[ami-bogus]' does not exist"},
			false,
		},
		{
			"unrelated capacity error must not match",
			&fakeAPIError{code: "InsufficientInstanceCapacity"},
			false,
		},
		{
			"non-API error, unrelated",
			errors.New("connection refused"),
			false,
		},
		{
			"non-API error, string mentions both markers (fallback path)",
			errors.New("api error InvalidParameterValue: ... Invalid IAM Instance Profile name"),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientIAMInstanceProfilePropagation(tc.err); got != tc.want {
				t.Errorf("isTransientIAMInstanceProfilePropagation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// fakeRunInstancesAPI lets tests drive RunInstances without a real EC2 client:
// it fails with a configured error for the first N calls, then succeeds.
type fakeRunInstancesAPI struct {
	failTimes int // number of calls that should fail before succeeding
	failErr   error
	calls     int
}

func (f *fakeRunInstancesAPI) RunInstances(_ context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.calls++
	if f.calls <= f.failTimes {
		return nil, f.failErr
	}
	return &ec2.RunInstancesOutput{}, nil
}

// stubSleep replaces runInstancesSleep for the duration of the test so the
// retry-loop tests don't pay the real backoff wall-clock time, and records the
// durations the loop asked to sleep between attempts.
func stubSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	orig := runInstancesSleep
	runInstancesSleep = func(d time.Duration) { got = append(got, d) }
	t.Cleanup(func() { runInstancesSleep = orig })
	return &got
}

// TestRunInstancesWithRetry_SucceedsAfterTransientFailures is the "fails N
// times then succeeds" case: the launch ultimately succeeds after retrying
// through the transient IAM-propagation error.
func TestRunInstancesWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	sleeps := stubSleep(t)
	api := &fakeRunInstancesAPI{failTimes: 2, failErr: &iamProfilePropagationFake{}}

	out, err := runInstancesWithRetry(context.Background(), api, &ec2.RunInstancesInput{})
	if err != nil {
		t.Fatalf("runInstancesWithRetry: unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("runInstancesWithRetry: expected a non-nil result on eventual success")
	}
	if api.calls != 3 {
		t.Errorf("RunInstances called %d times, want 3 (2 failures + 1 success)", api.calls)
	}
	if len(*sleeps) != 2 {
		t.Errorf("slept %d times, want 2 (one per failed attempt)", len(*sleeps))
	}
}

// TestRunInstancesWithRetry_GivesUpEventually is the "always fails" case: the
// retry loop must NOT retry forever — it gives up with a clear error after the
// bounded backoff schedule is exhausted.
func TestRunInstancesWithRetry_GivesUpEventually(t *testing.T) {
	stubSleep(t)
	api := &fakeRunInstancesAPI{failTimes: 1000, failErr: &iamProfilePropagationFake{}}

	_, err := runInstancesWithRetry(context.Background(), api, &ec2.RunInstancesInput{})
	if err == nil {
		t.Fatal("runInstancesWithRetry: expected an error when the transient never clears")
	}
	wantCalls := len(iamInstanceProfilePropagationBackoff) + 1 // initial attempt + one per backoff step
	if api.calls != wantCalls {
		t.Errorf("RunInstances called %d times, want %d (bounded, not infinite)", api.calls, wantCalls)
	}
}

// TestRunInstancesWithRetry_NoRetryOnOtherError verifies a DIFFERENT (non-
// transient) error — e.g. a genuinely bad InvalidParameterValue, or any other
// AWS error class — fails immediately with no retry. This is the guard against
// the fix becoming a blanket retry-on-any-launch-error mechanism.
func TestRunInstancesWithRetry_NoRetryOnOtherError(t *testing.T) {
	cases := map[string]error{
		"different InvalidParameterValue message": &iamProfilePropagationFakeWithMsg{msg: "Invalid value 'bogus.type' for instanceType"},
		"unrelated capacity error":                &fakeAPIError{code: "InsufficientInstanceCapacity"},
		"plain non-API error":                     errors.New("dial tcp: connection refused"),
	}
	for name, wantErr := range cases {
		t.Run(name, func(t *testing.T) {
			sleeps := stubSleep(t)
			api := &fakeRunInstancesAPI{failTimes: 1000, failErr: wantErr}

			_, err := runInstancesWithRetry(context.Background(), api, &ec2.RunInstancesInput{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if api.calls != 1 {
				t.Errorf("RunInstances called %d times, want exactly 1 (no retry)", api.calls)
			}
			if len(*sleeps) != 0 {
				t.Errorf("slept %d times, want 0 (no retry means no backoff)", len(*sleeps))
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("returned error does not wrap/equal the original: %v", err)
			}
		})
	}
}
