package aws

import (
	"context"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

// AutoscaleFunctionName is the orchestrator Lambda's name for an environment.
// The caller's env decides it, which is load-bearing: an account can hold
// SEVERAL orchestrators.
//
// The first version matched a shared prefix and took whichever function came
// back first. That produced a real false alarm the moment staging's schedule was
// disabled — a user working in PRODUCTION was warned that "the autoscale
// orchestrator spawn-autoscale-orchestrator-staging … is DISABLED", about an
// environment they were not using, while production's schedule was healthy.
// That is the cry-wolf failure this check exists to avoid (#624), reintroduced
// by the check itself.
func AutoscaleFunctionName(env string) string {
	if env == "" {
		env = "production"
	}
	return "spawn-autoscale-orchestrator-" + env
}

// AutoscaleCoverage is what could be determined about whether anything will act
// on this account's autoscale groups.
//
// This exists because the answer was previously unobtainable (#772). `spawn
// autoscale create` and `add-schedule` write into the group's DynamoDB record;
// a SCHEDULED Lambda reads those records and acts. Nothing in the CLI referenced
// that schedule, so the two halves could come apart silently:
//
//	$ spawn autoscale create …        # succeeds
//	$ spawn autoscale add-schedule …  # succeeds
//	# …and nothing ever scales, with nothing saying why
//
// That is the same composition failure as #755 — two individually-correct
// properties combining into a feature that is inert and quiet about it — and it
// is the reason the two per-minute rules in #772 could not simply be switched
// off. A thing that cannot be observed cannot safely be turned off.
//
// Deliberately the same shape as ReaperCoverage, including Determined: "there is
// no orchestrator" and "I could not tell" are different answers, and conflating
// them was spawn#624, where a check that warned in every account carried no
// information and users learned to scroll past it.
type AutoscaleCoverage struct {
	// Covered is true only when positive evidence was found: an orchestrator
	// function AND an enabled schedule that invokes it. False with
	// Determined=true means "looked, and there is none".
	Covered bool
	// Determined is false when a probe could not be completed (e.g. no
	// permission), in which case nothing may be claimed either way.
	Determined bool
	// How names the evidence.
	How string
	// Why explains an undetermined result.
	Why string
	// Function is the orchestrator found, if any. Set even when not Covered, so a
	// caller can say "deployed but not scheduled" rather than a bare "no".
	Function string
	// Rule and RuleState describe the schedule found targeting Function.
	// RuleState is "DISABLED" for the precise case that motivated this check.
	Rule      string
	RuleState string
	// Looked is the function name the probe searched for, so an absent result
	// names what was missing rather than gesturing at a category.
	Looked string
	// Region is where the probe actually looked, and it is reported in the
	// advice for a specific reason: `spawn autoscale` resolves its AWS config
	// from the ambient default chain and ignores --region, so running it without
	// AWS_REGION set probes a region that may hold nothing. That produced a
	// confident "no autoscale orchestrator runs in this account" against an
	// account that has one. Naming the region makes that false negative
	// self-diagnosing instead of misleading.
	Region string
}

// DetectAutoscaleCoverage reports whether this account will actually act on an
// autoscale group.
//
// BOTH halves are required, and that is the whole point. A deployed orchestrator
// with a disabled rule is indistinguishable from a healthy one if you only check
// that the function exists — and it is exactly the state #772 would have created
// by disabling the per-minute rules. So coverage means: the function is there
// AND something enabled invokes it.
func DetectAutoscaleCoverage(ctx context.Context, cfg awssdk.Config, env string) AutoscaleCoverage {
	var probeErrs []string
	want := AutoscaleFunctionName(env)
	region := cfg.Region
	if region == "" {
		region = "<no region resolved>"
	}

	// 1. The orchestrator function.
	var fnName, fnARN string
	lam := lambda.NewFromConfig(cfg)
	var marker *string
	// Bounded, as in DetectReaperCoverage: an account with 500+ functions is not
	// worth paging forever for a yes/no.
	for page := 0; page < 10; page++ {
		out, err := lam.ListFunctions(ctx, &lambda.ListFunctionsInput{Marker: marker})
		if err != nil {
			probeErrs = append(probeErrs, "lambda:ListFunctions: "+err.Error())
			break
		}
		for _, fn := range out.Functions {
			// EXACT match on the environment's function, not a prefix. See
			// AutoscaleFunctionName for the false alarm a prefix caused.
			if awssdk.ToString(fn.FunctionName) == want {
				fnName = awssdk.ToString(fn.FunctionName)
				fnARN = awssdk.ToString(fn.FunctionArn)
				break
			}
		}
		if fnName != "" || out.NextMarker == nil || awssdk.ToString(out.NextMarker) == "" {
			break
		}
		marker = out.NextMarker
	}

	if fnName == "" {
		if len(probeErrs) == 0 {
			// Definitive: looked at every function and none is an orchestrator.
			return AutoscaleCoverage{Determined: true, Region: region, Looked: want}
		}
		return AutoscaleCoverage{Why: strings.Join(probeErrs, "; "), Region: region, Looked: want}
	}

	// 2. An ENABLED rule that invokes it.
	//
	// Found by walking rules and matching their TARGETS, rather than by guessing
	// the rule's name. The live rule is called
	// "spawn-autoscale-orchestra-AutoScaleOrchestratorFunc-Zp5BP1f94xIl" — a
	// CloudFormation-generated name with a random suffix, which no name-based
	// match could predict. Guessing a generated name is the #476 trap.
	eb := eventbridge.NewFromConfig(cfg)
	rules, err := eb.ListRules(ctx, &eventbridge.ListRulesInput{})
	if err != nil {
		probeErrs = append(probeErrs, "events:ListRules: "+err.Error())
		return AutoscaleCoverage{Why: strings.Join(probeErrs, "; "), Function: fnName, Region: region}
	}

	for _, r := range rules.Rules {
		name := awssdk.ToString(r.Name)
		targets, err := eb.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: r.Name})
		if err != nil {
			probeErrs = append(probeErrs, "events:ListTargetsByRule("+name+"): "+err.Error())
			continue
		}
		for _, t := range targets.Targets {
			if awssdk.ToString(t.Arn) != fnARN {
				continue
			}
			state := string(r.State)
			if state == "ENABLED" {
				return AutoscaleCoverage{
					Covered:    true,
					Determined: true,
					How: fmt.Sprintf("%s invoked by %s (%s)", fnName, name,
						awssdk.ToString(r.ScheduleExpression)),
					Function:  fnName,
					Rule:      name,
					RuleState: state,
					Region:    region,
				}
			}
			// Found, but not enabled. This is the motivating case, and it is
			// reported as NOT covered with the reason named — a disabled schedule
			// is functionally identical to no schedule, and saying "no
			// orchestrator" here would be wrong and unhelpful.
			return AutoscaleCoverage{
				Determined: true,
				Function:   fnName,
				Rule:       name,
				RuleState:  state,
				Region:     region,
			}
		}
	}

	if len(probeErrs) > 0 {
		return AutoscaleCoverage{Why: strings.Join(probeErrs, "; "), Function: fnName, Region: region}
	}
	// The function exists and nothing invokes it.
	return AutoscaleCoverage{Determined: true, Function: fnName, Region: region}
}

// AutoscaleCoverageAdvice is the sentence to show someone whose autoscale group
// depends on an orchestrator that will not run. Empty when covered.
//
// It names what will not happen rather than describing the mechanism, because
// the user's question is "why is my group not scaling", not "what is
// EventBridge".
func AutoscaleCoverageAdvice(c AutoscaleCoverage) string {
	if c.Covered {
		return ""
	}
	switch {
	case !c.Determined:
		return fmt.Sprintf("could not determine whether the autoscale orchestrator runs in this "+
			"account (%s) — if your group does not scale, this is the first thing to check", c.Why)

	case c.Function != "" && c.Rule != "" && c.RuleState != "ENABLED":
		// The exact state #772 would have created.
		return fmt.Sprintf("the autoscale orchestrator %s is deployed, but its schedule %s is %s — "+
			"so groups and schedules you create here are recorded and never acted on. "+
			"Re-enable it with: aws events enable-rule --name %s",
			c.Function, c.Rule, c.RuleState, c.Rule)

	case c.Function != "":
		return fmt.Sprintf("the autoscale orchestrator %s is deployed in %s, but NOTHING invokes "+
			"it — no EventBridge rule targets it, so groups and schedules you create here are "+
			"recorded and never acted on", c.Function, c.Region)

	default:
		return fmt.Sprintf("no autoscale orchestrator (%s) found in %s, so groups and schedules you "+
			"create here are recorded and never acted on. Deploy it from "+
			"lambda/autoscale-orchestrator (make deploy) — or, if it is deployed in a different "+
			"region, note that `spawn autoscale` uses the ambient AWS region and ignores "+
			"--region, so set AWS_REGION", c.Looked, c.Region)
	}
}
