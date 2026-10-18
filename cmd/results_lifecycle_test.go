package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/provenance"
)

func TestRenderLifecyclePlanDryRunSaysWhatAndWhatItCosts(t *testing.T) {
	other := provenance.Rules()[0]
	id := "expire-scratch"
	other.ID = &id
	p := &provenance.LifecyclePlan{Bucket: "b", Desired: provenance.Rules(), Changed: true, Others: append(provenance.Rules()[:0:0], other)}
	var b bytes.Buffer
	renderLifecyclePlan(&b, p, false, false)
	out := b.String()
	for _, want := range []string{
		"dry run; nothing is changed",
		"Installs spawn's tiering rules",
		"spawn:regen-class=cheap -> Standard-IA at 30 days, Glacier IR at 90 days",
		"spawn:regen-class=moderate -> Standard-IA at 60 days",
		"requires spawn:retain=false",
		"kept exactly as they are (1)",
		"expire-scratch",
		"could expire an output",
		"$0.01 per 1,000 into Standard-IA",
		"$0.02 per 1,000 into Glacier IR",
		"minimum storage duration",
		"static",
		provenance.RequestPricesAsOf,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Written, and read back") {
		t.Errorf("a dry run claims to have written:\n%s", out)
	}
}

func TestRenderLifecyclePlanNoChange(t *testing.T) {
	var b bytes.Buffer
	renderLifecyclePlan(&b, &provenance.LifecyclePlan{Bucket: "b", Desired: provenance.Rules(), Current: provenance.Rules()}, true, false)
	if !strings.Contains(b.String(), "already installed and current") || strings.Contains(b.String(), "Written") {
		t.Errorf("no-op plan:\n%s", b.String())
	}
}

func TestRenderRetainResultsListsWhatWasLeftAlone(t *testing.T) {
	var b bytes.Buffer
	renderRetainResults(&b, []provenance.RetainResult{
		{Key: "out/a", Outcome: provenance.RetainSet},
		{Key: "out/notes", Outcome: provenance.RetainNotOutput},
		{Key: "out/c", Outcome: provenance.RetainError, Error: "AccessDenied"},
	}, "true")
	out := b.String()
	for _, want := range []string{"left alone (no spawn:run-id", "out/notes", "FAILED out/c: AccessDenied", "set on 1, already so on 0, left alone 1, failed 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("retain summary lacks %q:\n%s", want, out)
		}
	}
}
