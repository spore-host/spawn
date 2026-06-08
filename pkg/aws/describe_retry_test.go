package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/testutil"
)

// fakeAPIError implements smithy.APIError for classifier testing.
type fakeAPIError struct{ code string }

func (e *fakeAPIError) Error() string                 { return e.code }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.code }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestIsInstanceNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"smithy NotFound", &fakeAPIError{code: "InvalidInstanceID.NotFound"}, true},
		{"smithy other", &fakeAPIError{code: "UnauthorizedOperation"}, false},
		{"string fallback NotFound", errors.New("operation error EC2: DescribeInstances ... InvalidInstanceID.NotFound: ..."), true},
		{"unrelated string", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInstanceNotFound(tc.err); got != tc.want {
				t.Errorf("isInstanceNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDescribeInstanceWithRetry_Found(t *testing.T) {
	env := testutil.SubstrateServer(t)
	id := launchTestInstance(t, env)

	inst, err := DescribeInstanceWithRetry(context.Background(), env.EC2Client(), id)
	if err != nil {
		t.Fatalf("DescribeInstanceWithRetry: %v", err)
	}
	if inst == nil || inst.InstanceId == nil || *inst.InstanceId != id {
		t.Errorf("got instance %v, want id %s", inst, id)
	}
}

// TestDescribeInstanceWithRetry_NotFoundExhausts verifies that a genuinely
// missing instance eventually errors (rather than looping forever) — we bound it
// with a short context so the backoff exits quickly.
func TestDescribeInstanceWithRetry_NotFoundExhausts(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := DescribeInstanceWithRetry(ctx, env.EC2Client(), "i-000000000nonexistent")
	if err == nil {
		t.Fatal("expected an error for a nonexistent instance")
	}
}
