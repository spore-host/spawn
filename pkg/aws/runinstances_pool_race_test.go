package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	smithy "github.com/aws/smithy-go"
)

// simClockKey carries one caller's simulated clock through ctx, so concurrent
// retry loops each advance their own time through the runInstancesSleep seam.
type simClockKey struct{}

type simClock struct{ now time.Duration }

func simNow(ctx context.Context) *simClock { return ctx.Value(simClockKey{}).(*simClock) }

// propagatingEC2 is RunInstances against an instance profile EC2 cannot use
// until propagateAt (on each caller's simulated clock) has passed: before
// that every call is the #572 rejection, after it every call succeeds. It
// records each call's input so a test can check that a retry is the same
// request.
type propagatingEC2 struct {
	propagateAt time.Duration
	mu          sync.Mutex
	inputs      map[string][]*ec2.RunInstancesInput // ClientToken → inputs sent
}

func (f *propagatingEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	f.inputs[*in.ClientToken] = append(f.inputs[*in.ClientToken], in)
	f.mu.Unlock()
	if simNow(ctx).now < f.propagateAt {
		return nil, &iamProfilePropagationFake{}
	}
	return &ec2.RunInstancesOutput{}, nil
}

// TestRunInstancesWithRetry_PoolWorkersOutlastTheProfileRace replays the
// CloudTrail sequence from #1448: four pool workers launch at once against a
// just-created profile, all four are rejected with "Invalid IAM Instance
// Profile name", and the profile becomes usable a few seconds later (about 7s
// in us-west-2). Every worker must come up. The longer case is the IAM
// propagation tail beyond what CloudTrail showed, which the schedule must
// also cover.
func TestRunInstancesWithRetry_PoolWorkersOutlastTheProfileRace(t *testing.T) {
	orig := runInstancesSleep
	runInstancesSleep = func(ctx context.Context, d time.Duration) error {
		simNow(ctx).now += d
		return nil
	}
	t.Cleanup(func() { runInstancesSleep = orig })

	for _, propagateAt := range []time.Duration{7 * time.Second, 45 * time.Second} {
		t.Run(propagateAt.String(), func(t *testing.T) {
			api := &propagatingEC2{propagateAt: propagateAt, inputs: map[string][]*ec2.RunInstancesInput{}}
			const workers = 4
			errs := make([]error, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ctx := context.WithValue(context.Background(), simClockKey{}, &simClock{})
					token := fmt.Sprintf("worker-%d", i)
					_, errs[i] = runInstancesWithRetry(ctx, api, &ec2.RunInstancesInput{ClientToken: &token})
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("worker-%d lost the race with the profile usable %s after the first rejection: %v", i, propagateAt, err)
				}
			}
			for token, sent := range api.inputs {
				if len(sent) < 2 {
					t.Errorf("%s: %d RunInstances call(s); every worker's first call is rejected, so a success needs a retry", token, len(sent))
				}
				for _, in := range sent[1:] {
					if in != sent[0] {
						t.Errorf("%s: a retry sent a different request; it must resend the same input under the same ClientToken", token)
					}
				}
			}
		})
	}
}

// TestRunInstancesWithRetry_StoppedRetryKeepsTheAWSError is the empty reason in
// #1448 ("terminal/ at phase=launch-acked"): the pool's launch budget ended
// while the #572 rejection was still being retried, and the context error that
// came back carried no AWS code. Whether the context ends during the backoff
// or before the next call, the error must still name the rejection.
func TestRunInstancesWithRetry_StoppedRetryKeepsTheAWSError(t *testing.T) {
	cases := map[string]func(cancel context.CancelFunc) func(context.Context, time.Duration) error{
		"context ends during the backoff": func(cancel context.CancelFunc) func(context.Context, time.Duration) error {
			return func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
		},
		"context ends before the next call": func(cancel context.CancelFunc) func(context.Context, time.Duration) error {
			return func(context.Context, time.Duration) error { cancel(); return nil }
		},
	}
	for name, sleep := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			orig := runInstancesSleep
			runInstancesSleep = sleep(cancel)
			t.Cleanup(func() { runInstancesSleep = orig })

			api := ctxAwareRejectingEC2{}
			_, err := runInstancesWithRetry(ctx, api, &ec2.RunInstancesInput{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error does not say the retry was cut short: %v", err)
			}
			var le *LaunchError
			if !errors.As(newLaunchError(err), &le) || le.Code != "InvalidParameterValue" {
				t.Errorf("LaunchError code = %q, want InvalidParameterValue (the rejection being retried); error: %v", le.Code, err)
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || !strings.Contains(apiErr.ErrorMessage(), "Invalid IAM Instance Profile name") {
				t.Errorf("the AWS message is not reachable from the error: %v", err)
			}
		})
	}
}

// ctxAwareRejectingEC2 rejects with the #572 error while ctx is live and
// fails as the SDK does, without an AWS code, once it has ended.
type ctxAwareRejectingEC2 struct{}

func (ctxAwareRejectingEC2) RunInstances(ctx context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operation error EC2: RunInstances, %w", err)
	}
	return nil, &iamProfilePropagationFake{}
}

// The exported window is what callers size their deadlines from; it must be
// the schedule the loop actually sleeps through.
func TestInstanceProfilePropagationRetryWindowIsTheSchedule(t *testing.T) {
	sleeps := stubSleep(t)
	api := &fakeRunInstancesAPI{failTimes: 1000, failErr: &iamProfilePropagationFake{}}
	_, _ = runInstancesWithRetry(context.Background(), api, &ec2.RunInstancesInput{})
	var slept time.Duration
	for _, d := range *sleeps {
		slept += d
	}
	if slept == 0 || slept != InstanceProfilePropagationRetryWindow() {
		t.Errorf("the retry loop sleeps %s in total, InstanceProfilePropagationRetryWindow says %s", slept, InstanceProfilePropagationRetryWindow())
	}
}
