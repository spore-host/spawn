package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// alertDeliveryAPI is the slice of Lambda the delivery check needs.
//
// Unexported, and the exported entry point takes an aws.Config instead, so that
// cmd/ does not need an aws-sdk-go-v2/service/lambda import. cmd/ is meant to be
// a thin CLI layer that delegates AWS work to pkg/*, and TestNoNewAWSSDKImportsInCmd
// (#326/#327) enforces that — it caught the first version of this change, where
// cmd/alerts.go built the Lambda client itself. The allowlist there is allowed to
// shrink and never to silently grow, so the client construction moved here.
type alertDeliveryAPI interface {
	GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
}

// Deliverability reports whether an alert created now could actually be
// delivered.
//
// Determined is separate from the answer, for the same reason it is on
// Idleness and ReaperCoverage: "it cannot be delivered" and "I could not tell"
// are different claims, and here the difference decides whether a command
// refuses to do its job. A caller without lambda:GetFunction must not lose the
// ability to create alerts over a check that could not run — that would turn a
// permission gap into a hard failure, which is a worse bug than the one being
// fixed.
type Deliverability struct {
	// Determined is false when the check could not be made at all.
	Determined bool
	// Deliverable is true when the function that consumes these records exists.
	Deliverable bool
	// FunctionName is the consumer that was looked for.
	FunctionName string
	// Why explains an undetermined result, or a negative one.
	Why string
}

// CheckAlertDelivery reports whether anything exists to consume the records
// `spawn alerts create` writes.
//
// #790: `spawn alerts create` validated five trigger types, wrote to
// `spawn-alerts`, printed "Alert created" and returned 0 — while the only reader
// of that table, lambda/alert-handler, is deployed nowhere. So every alert ever
// created was silently undeliverable, and `spawn alerts history` was permanently
// empty, which reads like "nothing has gone wrong" rather than "nothing is
// watching". The live `spawn-alert-evaluator` is not a substitute: it reads
// `spawn-alert-preferences` and covers cost alerts only.
//
// Checking the CONSUMER rather than the write is the point. The write succeeds;
// that was never the problem.
func CheckAlertDelivery(ctx context.Context, r *Resolver, cfg awssdk.Config) Deliverability {
	return checkAlertDelivery(ctx, r, lambda.NewFromConfig(cfg))
}

// checkAlertDelivery is the testable half, taking the narrow interface.
func checkAlertDelivery(ctx context.Context, r *Resolver, api alertDeliveryAPI) Deliverability {
	name := extractFunctionName(r.GetAlertHandlerARN())
	d := Deliverability{FunctionName: name}

	if api == nil {
		d.Why = "no Lambda client configured"
		return d
	}

	_, err := api.GetFunction(ctx, &lambda.GetFunctionInput{
		FunctionName: awssdk.String(name),
	})
	if err == nil {
		d.Determined, d.Deliverable = true, true
		return d
	}

	if isFunctionNotFound(err) {
		d.Determined = true
		d.Why = fmt.Sprintf("%s does not exist, and it is the only consumer of the alerts table", name)
		return d
	}

	// Anything else — AccessDenied, throttling, no network — is undetermined.
	// Not knowing is not the same as knowing it is broken.
	d.Why = err.Error()
	return d
}

// isFunctionNotFound distinguishes a genuinely absent function from every other
// failure.
//
// Typed first, string second. The typed check is correct against the real API;
// the string fallback is kept because the Substrate test double does not always
// return the modelled error type, and a check that only works against one of the
// two would be verified by tests that cannot fail.
func isFunctionNotFound(err error) bool {
	var nfe *lambdatypes.ResourceNotFoundException
	if errors.As(err, &nfe) {
		return true
	}
	return strings.Contains(err.Error(), "ResourceNotFoundException")
}

// Decide turns a Deliverability into the action to take: refuse, or proceed
// with a caveat, or proceed silently. At most one return value is non-zero.
//
// The three-way branch lives here rather than at the call site so it can be
// tested directly. A switch in cmd/ would only be exercised through a command
// that needs AWS credentials, which in practice means not exercised at all —
// and the whole point of this change is a code path that nobody ran.
func (d Deliverability) Decide() (refuse error, warn string) {
	switch {
	case !d.Determined:
		return nil, d.UndeterminedAlertWarning()
	case !d.Deliverable:
		return d.UndeliverableAlertError(), ""
	default:
		return nil, ""
	}
}

// UndeliverableAlertError explains why an alert was refused.
//
// A refusal, not a warning. Printing "Alert created" for a record nothing will
// ever read is the failure being fixed, and a warning that scrolls past leaves
// the operator believing they are covered — which is the state that makes an
// unnoticed sweep failure expensive.
func (d Deliverability) UndeliverableAlertError() error {
	return fmt.Errorf(
		"refusing to create an alert that cannot be delivered: %s\n\n"+
			"Alerts written by this command are consumed by one Lambda function, and it is not\n"+
			"deployed, so the alert would never fire and `spawn alerts history` would stay empty.\n"+
			"Nothing in the spawn repository deploys it today; spore-host/spawn#783 tracks the\n"+
			"decision to either deploy it or withdraw this command.\n\n"+
			"If you run your own control plane, point spawn at your deployed function with\n"+
			"SPAWN_LAMBDA_ALERT_HANDLER_ARN or the infrastructure.lambda.alert_handler_arn key\n"+
			"in ~/.spawn/config.yaml",
		d.Why)
}

// UndeterminedAlertWarning is shown when the check could not be made. The alert
// is still created: see the note on Determined.
func (d Deliverability) UndeterminedAlertWarning() string {
	return fmt.Sprintf(
		"warning: could not verify that alerts can be delivered (%s).\n"+
			"  The alert was created. If %s is not deployed it will never fire.",
		d.Why, d.FunctionName)
}
