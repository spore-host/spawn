package aws

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

type pagedRules struct {
	pages [][]string
	calls int
}

func (p *pagedRules) ListRules(_ context.Context, in *eventbridge.ListRulesInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error) {
	i := 0
	if tok := awssdk.ToString(in.NextToken); tok != "" {
		i = int(tok[0] - '0')
	}
	p.calls++
	out := &eventbridge.ListRulesOutput{}
	for _, n := range p.pages[i] {
		out.Rules = append(out.Rules, ebtypes.Rule{Name: awssdk.String(n)})
	}
	if i+1 < len(p.pages) {
		out.NextToken = awssdk.String(string(rune('0' + i + 1)))
	}
	return out, nil
}

// #1161: ListRules returns at most 100 rules per call; the footprint read only
// the first page, so a spawn rule on page two was silently absent.
func TestFootprintReadsEveryPageOfRules(t *testing.T) {
	p := &pagedRules{pages: [][]string{{"other-1", "other-2"}, {"other-3"}, {"spawn-ttl-reaper"}}}
	rules, err := listAllRules(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 3 {
		t.Errorf("read %d pages of 3", p.calls)
	}
	found := false
	for _, r := range rules {
		if awssdk.ToString(r.Name) == "spawn-ttl-reaper" {
			found = true
		}
	}
	if !found {
		t.Errorf("the rule on the last page was not returned: %v", rules)
	}
}

// #1161: cmd/footprint.go's help and footer called 'scheduler-handler' a blind
// spot while footprintExactNames matches it. Every exact name the command's text
// quotes must be described, in the same sentence, as matched — so the docs cannot
// again claim a gap the scanner does not have.
func TestFootprintTextDoesNotCallAMatchedNameABlindSpot(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRootFromPkgAWS(t), "cmd", "footprint.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Collapse the Fprintf-per-line footer into running text before splitting.
	src := regexp.MustCompile(`\\n"\)\s*fmt\.Fprintf\(w, "`).ReplaceAllString(string(b), " ")
	sentences := regexp.MustCompile(`\.(\s|\\n)`).Split(src, -1)
	checked := 0
	for name := range footprintExactNames {
		for _, s := range sentences {
			if !strings.Contains(s, "'"+name+"'") {
				continue
			}
			checked++
			if !strings.Contains(s, "matched") {
				t.Errorf("cmd/footprint.go quotes %q, which the scanner matches by exact name, in a sentence that does not say so:\n%s", name, strings.TrimSpace(s))
			}
		}
	}
	if checked == 0 {
		t.Log("cmd/footprint.go quotes no exact-match name; nothing to check")
	}
}

// busFootprint is an account with the TTL reaper's instance-event bus (#1287).
type busFootprint struct {
	rules map[string][]string // bus ("" = default) -> rule names
	buses []string
}

func (b *busFootprint) ListRules(_ context.Context, in *eventbridge.ListRulesInput, _ ...func(*eventbridge.Options)) (*eventbridge.ListRulesOutput, error) {
	out := &eventbridge.ListRulesOutput{}
	for _, n := range b.rules[awssdk.ToString(in.EventBusName)] {
		out.Rules = append(out.Rules, ebtypes.Rule{Name: awssdk.String(n)})
	}
	return out, nil
}

func (b *busFootprint) ListEventBuses(context.Context, *eventbridge.ListEventBusesInput, ...func(*eventbridge.Options)) (*eventbridge.ListEventBusesOutput, error) {
	out := &eventbridge.ListEventBusesOutput{}
	for _, n := range b.buses {
		out.EventBuses = append(out.EventBuses, ebtypes.EventBus{Name: awssdk.String(n)})
	}
	return out, nil
}

// TestFootprintSeesTheReapersEventBusAndItsRule (#1287): the footprint read
// rules on the default bus only, so the instance-event bus the reaper stack
// creates, and the rule on it, were invisible to it.
func TestFootprintSeesTheReapersEventBusAndItsRule(t *testing.T) {
	f := &busFootprint{
		buses: []string{"default", "spawn-ttl-reaper-production-instance-events", "someone-elses-bus"},
		rules: map[string][]string{
			"": {"spawn-ttl-reaper-instance-terminated", "unrelated"},
			"spawn-ttl-reaper-production-instance-events": {"spawn-ttl-reaper-production-instance-terminated"},
			"someone-elses-bus":                           {"their-rule"},
		},
	}
	rows, notes := scanEvents(context.Background(), f, "us-east-1")
	if len(notes) != 0 {
		t.Fatalf("notes %v", notes)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Kind+" "+r.Name] = r.Detail
	}
	for _, want := range []string{
		"event-bus spawn-ttl-reaper-production-instance-events",
		"rule spawn-ttl-reaper-production-instance-terminated",
		"rule spawn-ttl-reaper-instance-terminated",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("footprint is missing %s; got %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("footprint reported %d rows, want exactly the 3 spawn ones: %v", len(got), got)
	}
	if d := got["rule spawn-ttl-reaper-production-instance-terminated"]; !strings.Contains(d, "on bus spawn-ttl-reaper-production-instance-events") {
		t.Errorf("a custom-bus rule does not say which bus: %q", d)
	}
}
