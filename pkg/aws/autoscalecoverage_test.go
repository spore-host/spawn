package aws

import (
	"strings"
	"testing"
)

// TestAutoscaleCoverageAdviceDistinguishesTheFourStates is the point of the
// type: "covered", "deployed but disabled", "deployed but unscheduled", and "not
// there" are four different situations with four different remedies, and
// collapsing any of them into the others is how a check becomes useless.
//
// spawn#624 is the precedent — a coverage check that printed the same warning in
// every account carried no information, so users learned to scroll past it.
func TestAutoscaleCoverageAdviceDistinguishesTheFourStates(t *testing.T) {
	tests := []struct {
		name       string
		cov        AutoscaleCoverage
		wantEmpty  bool
		mustSay    []string
		mustNotSay []string
	}{{
		name:      "covered says nothing",
		cov:       AutoscaleCoverage{Covered: true, Determined: true, How: "fn invoked by rule"},
		wantEmpty: true,
	}, {
		// The state that motivated the whole check (#772): disabling the
		// per-minute rules would have produced exactly this.
		name: "deployed but schedule DISABLED names the rule and how to fix it",
		cov: AutoscaleCoverage{
			Determined: true,
			Function:   "spawn-autoscale-orchestrator-production",
			Rule:       "spawn-autoscale-orchestra-AutoScaleOrchestratorFunc-Zp5BP1f94xIl",
			RuleState:  "DISABLED",
			Region:     "us-east-1",
		},
		mustSay: []string{
			"is deployed",
			"DISABLED",
			"recorded and never acted on",
			"enable-rule",
			"spawn-autoscale-orchestra-AutoScaleOrchestratorFunc-Zp5BP1f94xIl",
		},
		// Must not claim it is absent — it is there, just inert.
		mustNotSay: []string{"no autoscale orchestrator found"},
	}, {
		name: "deployed but nothing invokes it",
		cov: AutoscaleCoverage{
			Determined: true,
			Function:   "spawn-autoscale-orchestrator-production",
			Region:     "us-east-1",
		},
		mustSay:    []string{"NOTHING invokes", "us-east-1", "recorded and never acted on"},
		mustNotSay: []string{"DISABLED", "enable-rule"},
	}, {
		name: "absent names the function AND the region it looked for",
		cov:  AutoscaleCoverage{Determined: true, Region: "us-west-2", Looked: "spawn-autoscale-orchestrator-production"},
		// The region is load-bearing: `spawn autoscale` ignores --region, so an
		// absent result is as likely to mean "looked in the wrong region" as
		// "not deployed". A confident warning that omits where it looked is the
		// cry-wolf failure this check exists to avoid.
		mustSay: []string{"spawn-autoscale-orchestrator-production", "us-west-2", "AWS_REGION"},
	}, {
		name:    "undetermined must hedge, not accuse",
		cov:     AutoscaleCoverage{Why: "lambda:ListFunctions: AccessDenied"},
		mustSay: []string{"could not determine"},
		// "I could not tell" is not "there is none" — conflating them is #624.
		mustNotSay: []string{"recorded and never acted on", "no autoscale orchestrator ("},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoscaleCoverageAdvice(tc.cov)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("want no advice when covered, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("want advice, got none")
			}
			for _, want := range tc.mustSay {
				if !strings.Contains(got, want) {
					t.Errorf("advice is missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.mustNotSay {
				if strings.Contains(got, bad) {
					t.Errorf("advice wrongly contains %q, which describes a DIFFERENT state:\n%s", bad, got)
				}
			}
		})
	}
}

// TestAutoscaleCoverageAdviceNeverBlamesWhenUndetermined is the single property
// most worth protecting. A probe that could not run must not report an absence:
// that is how a check starts crying wolf, and a warning users learn to ignore is
// worse than no warning at all.
func TestAutoscaleCoverageAdviceNeverBlamesWhenUndetermined(t *testing.T) {
	got := AutoscaleCoverageAdvice(AutoscaleCoverage{Why: "events:ListRules: AccessDenied", Function: "fn"})
	if strings.Contains(got, "never acted on") {
		t.Errorf("an undetermined probe claimed a definite failure:\n%s", got)
	}
	if !strings.Contains(got, "could not determine") {
		t.Errorf("an undetermined probe must say so:\n%s", got)
	}
}

// TestAutoscaleFunctionNameIsEnvScoped guards the false alarm the first version
// of this check produced.
//
// It matched a shared PREFIX and took whichever orchestrator the API returned
// first. The moment staging's schedule was disabled, a user working in
// production was warned that staging was DISABLED — about an environment they
// were not using, while production was healthy. A warning about the wrong thing
// is the #624 cry-wolf failure, reintroduced by the check meant to prevent it.
func TestAutoscaleFunctionNameIsEnvScoped(t *testing.T) {
	if got := AutoscaleFunctionName("staging"); got != "spawn-autoscale-orchestrator-staging" {
		t.Errorf("AutoscaleFunctionName(staging) = %q", got)
	}
	if got := AutoscaleFunctionName("production"); got != "spawn-autoscale-orchestrator-production" {
		t.Errorf("AutoscaleFunctionName(production) = %q", got)
	}
	// An unset env must not match everything; it resolves to production.
	if got := AutoscaleFunctionName(""); got != "spawn-autoscale-orchestrator-production" {
		t.Errorf("AutoscaleFunctionName(\"\") = %q, want the production name rather than a prefix", got)
	}
	if AutoscaleFunctionName("staging") == AutoscaleFunctionName("production") {
		t.Error("staging and production resolve to the same function name, so the check cannot " +
			"tell them apart — which is what warned a production user about staging")
	}
}
