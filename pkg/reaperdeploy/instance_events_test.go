package reaperdeploy

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"gopkg.in/yaml.v3"
)

func deployForTest(t *testing.T) (*fakeLambda, *fakeEvents, *Deployer) {
	t.Helper()
	lamF, evF := &fakeLambda{}, &fakeEvents{}
	d := newTestDeployer(&fakeIAM{}, lamF, evF, &fakeS3{})
	if _, err := d.Deploy(context.Background(), Options{
		AccountID: "123456789012", Region: "eu-west-1", Version: "0.145.0", Artifact: "/tmp/x.zip",
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	return lamF, evF, d
}

// TestDeploySubscribesTheReaperToTerminatedInstances: `spawn reaper deploy`
// puts the terminated-instance rule on this account's default bus in the
// deploy region, targets the function, and lets only that rule invoke it
// (#1287). Same account throughout: no trust, no cross-account target.
func TestDeploySubscribesTheReaperToTerminatedInstances(t *testing.T) {
	lamF, evF, _ := deployForTest(t)

	rule := evF.rules[InstanceEventRuleName]
	if rule == nil {
		t.Fatalf("Deploy wrote rules %v, none named %s", keys(evF.rules), InstanceEventRuleName)
	}
	if got := awssdk.ToString(rule.EventPattern); got != InstanceEventPattern {
		t.Errorf("rule pattern %s, want %s", got, InstanceEventPattern)
	}
	if rule.ScheduleExpression != nil || rule.EventBusName != nil {
		t.Errorf("the instance-event rule must be a pattern rule on the default bus: %+v", rule)
	}
	if !slices.Contains(evF.targeted, InstanceEventRuleName) {
		t.Errorf("no PutTargets for %s (targeted %v)", InstanceEventRuleName, evF.targeted)
	}
	var perm bool
	for _, p := range lamF.permitted {
		if awssdk.ToString(p.StatementId) != InstanceEventRuleName {
			continue
		}
		perm = true
		if got, want := awssdk.ToString(p.SourceArn), "arn:aws:events:eu-west-1:123456789012:rule/"+InstanceEventRuleName; got != want {
			t.Errorf("invoke permission SourceArn %s, want %s", got, want)
		}
		if awssdk.ToString(p.Principal) != "events.amazonaws.com" {
			t.Errorf("invoke permission principal %s", awssdk.ToString(p.Principal))
		}
	}
	if !perm {
		t.Error("no invoke permission for the instance-event rule: EventBridge would fail every invocation")
	}
	// The schedule is unaffected.
	if evF.putSchedule != DefaultSchedule {
		t.Errorf("schedule %q, want %q", evF.putSchedule, DefaultSchedule)
	}
}

// TestTeardownRemovesEveryRuleAndPermissionDeployCreated is the leave-no-trace
// half, discovered rather than listed: every rule Deploy put is deleted and
// detargeted by Teardown, and every invoke permission it added is removed.
func TestTeardownRemovesEveryRuleAndPermissionDeployCreated(t *testing.T) {
	lamF, evF, d := deployForTest(t)
	if len(evF.rules) < 2 || len(lamF.permitted) < 2 {
		t.Fatalf("Deploy put %d rules and %d permissions; want at least the schedule's and the instance event's",
			len(evF.rules), len(lamF.permitted))
	}
	if _, err := d.Teardown(context.Background(), TeardownOptions{}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for name := range evF.rules {
		if !slices.Contains(evF.deleted, name) {
			t.Errorf("Teardown left rule %s", name)
		}
		if !slices.Contains(evF.untargeted, name) {
			t.Errorf("Teardown did not remove %s's targets (DeleteRule fails on a rule with targets)", name)
		}
	}
	for _, p := range lamF.permitted {
		if id := awssdk.ToString(p.StatementId); !slices.Contains(lamF.unpermitted, id) {
			t.Errorf("Teardown left invoke permission %s", id)
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestInstanceEventPatternMatchesTheTemplates: the self-hosted rule's pattern
// is the operated reaper's, on both of its rules.
func TestInstanceEventPatternMatchesTheTemplates(t *testing.T) {
	// interface{} on purpose: the comparison must see every key, so a pattern
	// broadened with a key a struct would not declare still fails. Both inputs
	// are this repository's own files.
	var want interface{} // nosemgrep
	if err := json.Unmarshal([]byte(InstanceEventPattern), &want); err != nil {
		t.Fatalf("InstanceEventPattern is not JSON: %v", err)
	}
	found := 0
	for _, path := range []string{
		"../../lambda/ttl-reaper/template.yaml",
		"../../deployment/cloudformation/ttl-reaper-instance-events.yaml",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Resources map[string]struct {
				Type       string `yaml:"Type"`
				Properties struct {
					EventPattern interface{} `yaml:"EventPattern"`
				} `yaml:"Properties"`
			} `yaml:"Resources"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for name, res := range doc.Resources {
			if res.Type != "AWS::Events::Rule" || res.Properties.EventPattern == nil {
				continue
			}
			found++
			// Round-trip through JSON so YAML's map types compare with JSON's.
			jb, _ := json.Marshal(res.Properties.EventPattern)
			var got interface{} // nosemgrep
			_ = json.Unmarshal(jb, &got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: pattern %s, want %s", path, name, jb, InstanceEventPattern)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d pattern rules in the templates, want 2", found)
	}
}

// eventsWithStates answers DescribeRule with a per-rule state.
type eventsWithStates struct {
	fakeEvents
	states map[string]ebtypes.RuleState
}

func (f *eventsWithStates) DescribeRule(_ context.Context, in *eventbridge.DescribeRuleInput, _ ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	st, ok := f.states[awssdk.ToString(in.Name)]
	if !ok {
		return nil, &ebtypes.ResourceNotFoundException{}
	}
	return &eventbridge.DescribeRuleOutput{State: st}, nil
}

func TestInspectReportsTheInstanceEventRule(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states map[string]ebtypes.RuleState
		want   bool
	}{
		{"enabled", map[string]ebtypes.RuleState{RuleName: ebtypes.RuleStateEnabled, InstanceEventRuleName: ebtypes.RuleStateEnabled}, true},
		{"disabled", map[string]ebtypes.RuleState{RuleName: ebtypes.RuleStateEnabled, InstanceEventRuleName: ebtypes.RuleStateDisabled}, false},
		{"absent (deployed before #1287)", map[string]ebtypes.RuleState{RuleName: ebtypes.RuleStateEnabled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDeployer(&fakeIAM{}, &fakeLambda{exists: true}, &fakeEvents{}, &fakeS3{})
			d.Events = &eventsWithStates{states: tc.states}
			info, err := d.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.InstanceEvents != tc.want {
				t.Errorf("InstanceEvents = %v, want %v", info.InstanceEvents, tc.want)
			}
		})
	}
}
