package infrastructure

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The consumer exists: alerts are deliverable and nothing is refused.
func TestCheckAlertDelivery_HandlerDeployed(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createLambdaFunction(t, env.LambdaClient(), alertHandlerFunction)

	d := checkAlertDelivery(context.Background(),
		NewResolver(defaultInfraConfig(), testRegion, testAccountID),
		env.LambdaClient())

	if !d.Determined {
		t.Fatalf("Determined = false, want true; why: %s", d.Why)
	}
	if !d.Deliverable {
		t.Errorf("Deliverable = false, want true; why: %s", d.Why)
	}
}

// The consumer is absent: this is the state the live shared account is in, and
// the one `spawn alerts create` must refuse.
func TestCheckAlertDelivery_HandlerMissing(t *testing.T) {
	env := testutil.SubstrateServer(t)
	// Deliberately create nothing.

	d := checkAlertDelivery(context.Background(),
		NewResolver(defaultInfraConfig(), testRegion, testAccountID),
		env.LambdaClient())

	if !d.Determined {
		t.Fatalf("Determined = false, want true — an absent function is a known answer, not an unknown one; why: %s", d.Why)
	}
	if d.Deliverable {
		t.Error("Deliverable = true with no function deployed")
	}
	if d.UndeliverableAlertError() == nil {
		t.Error("UndeliverableAlertError() = nil for an undeliverable alert")
	}
}

// errLambda fails every call with a supplied error.
type errLambda struct{ err error }

func (e errLambda) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	return nil, e.err
}

// A check that could not run must NOT refuse.
//
// This is the assertion that keeps the fix from being worse than the bug: the
// check costs one lambda:GetFunction, which a caller able to write to the alerts
// table may not hold. Refusing on AccessDenied would break working installs to
// protect them from a defect they might not have — the inverse of #624, where a
// check that could not tell reported a verdict anyway.
func TestCheckAlertDelivery_UndeterminedDoesNotRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  alertDeliveryAPI
	}{
		{"access denied", errLambda{errors.New("AccessDeniedException: not authorized to perform lambda:GetFunction")}},
		{"no network", errLambda{errors.New("dial tcp: lookup lambda.us-east-1.amazonaws.com: no such host")}},
		{"no client", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := checkAlertDelivery(context.Background(),
				NewResolver(defaultInfraConfig(), testRegion, testAccountID), tc.api)

			if d.Determined {
				t.Errorf("Determined = true for %q; a failure that is not a 404 says nothing about the function", tc.name)
			}
			if d.Why == "" {
				t.Error("Why is empty, so the warning would explain nothing")
			}
		})
	}
}

// A real typed ResourceNotFoundException must be recognised, not just the
// string form the emulator happens to produce. Without this, the typed branch
// is never exercised and could be wrong in production while every test passes.
func TestCheckAlertDelivery_TypedNotFoundIsRecognised(t *testing.T) {
	d := checkAlertDelivery(context.Background(),
		NewResolver(defaultInfraConfig(), testRegion, testAccountID),
		errLambda{&lambdatypes.ResourceNotFoundException{}})

	if !d.Determined || d.Deliverable {
		t.Errorf("typed ResourceNotFoundException gave Determined=%v Deliverable=%v, want true/false",
			d.Determined, d.Deliverable)
	}
}

// A self-hosted install pointing at its own deployed function is deliverable.
// Without this, "refuse when the shared function is missing" would be
// indistinguishable from "always refuse".
func TestCheckAlertDelivery_RespectsConfiguredARN(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createLambdaFunction(t, env.LambdaClient(), "my-own-alert-handler")

	cfg := defaultInfraConfig()
	cfg.Lambda.AlertHandlerARN = "arn:aws:lambda:us-east-1:123456789012:function:my-own-alert-handler"

	d := checkAlertDelivery(context.Background(),
		NewResolver(cfg, testRegion, testAccountID), env.LambdaClient())

	if !d.Determined || !d.Deliverable {
		t.Errorf("configured ARN gave Determined=%v Deliverable=%v, want true/true; why: %s",
			d.Determined, d.Deliverable, d.Why)
	}
	if d.FunctionName != "my-own-alert-handler" {
		t.Errorf("FunctionName = %q, want the configured function", d.FunctionName)
	}
}

// Decide's three outcomes, asserted directly.
//
// Exactly one of (refuse, warn) may be set. An earlier version of this fix put
// the branch in cmd/, where it could only be reached by a command that needs
// credentials — so the branch that must NOT refuse was untestable, which is the
// property most worth being sure of.
func TestDeliverabilityDecide(t *testing.T) {
	tests := []struct {
		name       string
		d          Deliverability
		wantRefuse bool
		wantWarn   bool
	}{
		{
			name:       "deliverable: silent",
			d:          Deliverability{Determined: true, Deliverable: true},
			wantRefuse: false, wantWarn: false,
		},
		{
			name:       "provably undeliverable: refuse",
			d:          Deliverability{Determined: true, Deliverable: false, FunctionName: "x", Why: "gone"},
			wantRefuse: true, wantWarn: false,
		},
		{
			name:       "undetermined: warn, never refuse",
			d:          Deliverability{Determined: false, FunctionName: "x", Why: "AccessDenied"},
			wantRefuse: false, wantWarn: true,
		},
		{
			// Unreachable through CheckAlertDelivery, which never sets
			// Deliverable without Determined — and tested anyway, because
			// Deliverability is exported and a caller can construct it. This is
			// the #787 lesson: a guard reachable only through its constructor is
			// not proven by tests that use the constructor.
			name:       "undetermined but marked deliverable: still warns, still does not refuse",
			d:          Deliverability{Determined: false, Deliverable: true, FunctionName: "x", Why: "unknown"},
			wantRefuse: false, wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refuse, warn := tt.d.Decide()
			if (refuse != nil) != tt.wantRefuse {
				t.Errorf("refuse = %v, want non-nil=%v", refuse, tt.wantRefuse)
			}
			if (warn != "") != tt.wantWarn {
				t.Errorf("warn = %q, want non-empty=%v", warn, tt.wantWarn)
			}
			if refuse != nil && warn != "" {
				t.Error("both refuse and warn set; at most one may be")
			}
		})
	}
}
