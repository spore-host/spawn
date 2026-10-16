package plugin

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// fakeHost is a ConditionHost whose command runner succeeds unless the script
// is "exit 1", and records what it was asked to run.
func fakeHost(goos string, ran *[]string) ConditionHost {
	return ConditionHost{
		GOOS: goos,
		RunCommand: func(_ context.Context, script string) error {
			*ran = append(*ran, script)
			if script == "exit 1" {
				return errors.New("exit status 1")
			}
			return nil
		},
	}
}

// conditionCases is a passing and a failing condition for each type. The gate
// below discovers the types from the validator (ConditionTypes), so a type the
// validator accepts without a case here fails rather than going untested.
var conditionCases = map[string]struct{ pass, fail Condition }{
	"command": {
		pass: Condition{Type: "command", Run: "true", Message: "tool must be present"},
		fail: Condition{Type: "command", Run: "exit 1", Message: "tool must be present"},
	},
	"platform": {
		pass: Condition{Type: "platform", OS: "linux", Message: "requires a Linux instance"},
		fail: Condition{Type: "platform", OS: "windows", Message: "requires a Windows instance"},
	},
}

// Every condition type the spec validator accepts is evaluated: its passing
// case passes and its failing case fails with the author's message (#1040).
func TestCheckCondition_EveryValidatedTypePassesAndFails(t *testing.T) {
	types := ConditionTypes()
	if len(types) == 0 {
		t.Fatal("ConditionTypes() discovered no condition types; discovery is broken")
	}
	for _, typ := range types {
		tc, ok := conditionCases[typ]
		if !ok {
			t.Errorf("condition type %q is accepted by Validate but has no pass/fail case here", typ)
			continue
		}
		var ran []string
		host := fakeHost("linux", &ran)
		if err := CheckCondition(context.Background(), tc.pass, NewTemplateContext(), host); err != nil {
			t.Errorf("%s: passing condition failed: %v", typ, err)
		}
		err := CheckCondition(context.Background(), tc.fail, NewTemplateContext(), host)
		if err == nil {
			t.Errorf("%s: failing condition passed", typ)
		} else if !strings.HasPrefix(err.Error(), tc.fail.Message+": ") {
			t.Errorf("%s: error %q does not lead with the condition's message %q", typ, err, tc.fail.Message)
		}
	}
	for typ := range conditionCases {
		if !validConditionTypes[typ] {
			t.Errorf("conditionCases has %q, which Validate does not accept", typ)
		}
	}
}

// A type the validator does not know fails rather than being skipped. Validate
// rejects it too, but the evaluator is also reached by specs that were never
// validated (spored's launch-time declarations), so it must not pass silently.
func TestCheckCondition_UnknownTypeFails(t *testing.T) {
	for _, typ := range []string{"", "file", "Command", "platfrom"} {
		if validConditionTypes[typ] {
			t.Fatalf("%q unexpectedly a valid condition type; pick another", typ)
		}
		var ran []string
		err := CheckCondition(context.Background(), Condition{Type: typ, Run: "true", OS: "linux"}, NewTemplateContext(), fakeHost("linux", &ran))
		if err == nil || !strings.Contains(err.Error(), "unknown condition type") {
			t.Errorf("type %q: err = %v, want an unknown-type failure", typ, err)
		}
		if len(ran) != 0 {
			t.Errorf("type %q: ran %q", typ, ran)
		}
	}
}

// A failure without a message still says which condition type failed.
func TestCheckCondition_NoMessageNamesTheType(t *testing.T) {
	var ran []string
	err := CheckCondition(context.Background(), Condition{Type: "command", Run: "exit 1"}, NewTemplateContext(), fakeHost("linux", &ran))
	if err == nil || !strings.Contains(err.Error(), `condition "command" failed`) {
		t.Errorf("err = %v", err)
	}
}

// CheckConditions stops at the first failure and runs nothing after it.
func TestCheckConditions_StopsAtFirstFailure(t *testing.T) {
	var ran []string
	err := CheckConditions(context.Background(), []Condition{
		{Type: "command", Run: "true"},
		{Type: "platform", OS: "windows", Message: "needs windows"},
		{Type: "command", Run: "echo after"},
	}, NewTemplateContext(), fakeHost("linux", &ran))
	if err == nil || !strings.HasPrefix(err.Error(), "needs windows: ") {
		t.Fatalf("err = %v, want the platform failure", err)
	}
	if len(ran) != 1 || ran[0] != "true" {
		t.Errorf("ran %q, want only the first command", ran)
	}
}

// A command condition is template text (Validate scans it for config
// references), so it is rendered like a run step, shell-escaped.
func TestCheckCondition_CommandIsRenderedShellEscaped(t *testing.T) {
	var ran []string
	tmplCtx := NewTemplateContext()
	tmplCtx.Config["bin"] = "it's"
	if err := CheckCondition(context.Background(), Condition{Type: "command", Run: "command -v {{ config.bin }}"}, tmplCtx, fakeHost("linux", &ran)); err != nil {
		t.Fatal(err)
	}
	if want := `command -v 'it'\''s'`; len(ran) != 1 || ran[0] != want {
		t.Errorf("ran %q, want %q", ran, want)
	}
	// An unresolvable reference is a failure, not a literal "{{" run.
	ran = nil
	if err := CheckCondition(context.Background(), Condition{Type: "command", Run: "test {{ config.absent }}"}, tmplCtx, fakeHost("linux", &ran)); err == nil || len(ran) != 0 {
		t.Errorf("err = %v, ran %q; want a render failure and nothing run", err, ran)
	}
}

// An empty command condition fails rather than passing as `sh -c ""` would.
func TestCheckCondition_EmptyCommandFails(t *testing.T) {
	var ran []string
	if err := CheckCondition(context.Background(), Condition{Type: "command"}, NewTemplateContext(), fakeHost("linux", &ran)); err == nil {
		t.Error("empty command condition passed")
	}
}

// The platform check normalises case and whitespace (as the Windows refusal
// does), and a platform condition without os imposes no constraint.
func TestCheckCondition_PlatformMatching(t *testing.T) {
	for _, tc := range []struct {
		os, goos string
		pass     bool
	}{
		{"linux", "linux", true},
		{"Linux", "linux", true},
		{" linux ", "linux", true},
		{"linux", "windows", false},
		{"linux", "darwin", false},
		{"windows", "windows", true},
		{"", "windows", true},
		{"linux", "", true}, // an unknown OS is Linux, as for an absent spawn:os tag
		{"windows", "", false},
	} {
		var ran []string
		err := CheckCondition(context.Background(), Condition{Type: "platform", OS: tc.os}, NewTemplateContext(), fakeHost(tc.goos, &ran))
		if (err == nil) != tc.pass {
			t.Errorf("platform os=%q on %q: err = %v, want pass=%v", tc.os, tc.goos, err, tc.pass)
		}
	}
}

// The controller host runs commands with the caller's environment, which the
// registry's local conditions rely on (e.g. `test -n "$GITHUB_TOKEN"`).
func TestLocalConditions_RealHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command conditions run sh")
	}
	t.Setenv("SPAWN_COND_TEST_VAR", "set")
	e := NewLocalExecutor(nil)
	ctx := context.Background()
	pass := []Condition{
		{Type: "command", Run: `test -n "$SPAWN_COND_TEST_VAR"`},
		{Type: "platform", OS: runtime.GOOS},
	}
	if err := e.CheckLocalConditions(ctx, pass, NewTemplateContext()); err != nil {
		t.Errorf("passing local conditions failed: %v", err)
	}
	for _, c := range []Condition{
		{Type: "command", Run: `test -n "$SPAWN_COND_TEST_UNSET_VAR"`, Message: "var must be set"},
		{Type: "platform", OS: "plan9", Message: "needs plan9"},
		{Type: "nope", Message: "unknown"},
	} {
		err := e.CheckLocalConditions(ctx, []Condition{c}, NewTemplateContext())
		if err == nil || !strings.HasPrefix(err.Error(), c.Message+": ") {
			t.Errorf("local %s condition: err = %v, want a failure led by %q", c.Type, err, c.Message)
		}
	}
}
