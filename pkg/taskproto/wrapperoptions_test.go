package taskproto

import (
	"errors"
	"strings"
	"testing"
)

func optsSpec() *TaskSpec {
	return &TaskSpec{TaskID: "t1", Command: []string{"true"}}
}

func validOpts() WrapperOptions {
	return WrapperOptions{
		ResultsPrefix: "s3://b/tasks/t1",
		Region:        "us-east-1",
		RunID:         "run-1",
	}
}

// TestGeneratorsRejectAnEmptyRunID is the point of spawn#764.
//
// An empty run_id is accepted by the on-instance protocol and reads downstream as
// "unattributable", so a waiter cannot tell THIS attempt's completion record from
// a previous attempt's at the same S3 key (spawn#608). Before this change, the
// signature was positional and `GenerateWrapper(spec, bucket, region, false, "")`
// was the quickest way to make a consumer compile after a parameter was added —
// it passed every test and shipped the bug. The doc comment already warned about
// it, which is the evidence that documenting it was not enough.
//
// All three generators must refuse, not just the one the issue named.
func TestGeneratorsRejectAnEmptyRunID(t *testing.T) {
	opts := validOpts()
	opts.RunID = ""

	for name, gen := range map[string]func(*TaskSpec, WrapperOptions) (string, error){
		"GenerateWrapper":         GenerateWrapper,
		"GeneratePooledJobScript": GeneratePooledJobScript,
		"GenerateFlushScript":     GenerateFlushScript,
	} {
		script, err := gen(optsSpec(), opts)
		if !errors.Is(err, ErrMissingRunID) {
			t.Errorf("%s with an empty RunID returned err=%v, want ErrMissingRunID", name, err)
		}
		if script != "" {
			t.Errorf("%s returned a script alongside the error; a partial script could still be "+
				"installed by a caller that ignores err", name)
		}
	}
}

// TestGeneratorsRejectAnEmptyResultsPrefix covers the other required field. An
// empty prefix would make every script write its completion record to a
// malformed S3 path, which is the #646 regression in a different disguise.
func TestGeneratorsRejectAnEmptyResultsPrefix(t *testing.T) {
	opts := validOpts()
	opts.ResultsPrefix = ""

	for name, gen := range map[string]func(*TaskSpec, WrapperOptions) (string, error){
		"GenerateWrapper":         GenerateWrapper,
		"GeneratePooledJobScript": GeneratePooledJobScript,
		"GenerateFlushScript":     GenerateFlushScript,
	} {
		if _, err := gen(optsSpec(), opts); !errors.Is(err, ErrMissingResultsPrefix) {
			t.Errorf("%s with an empty ResultsPrefix returned err=%v, want ErrMissingResultsPrefix", name, err)
		}
	}
}

// TestEmptyRegionIsAllowed pins the deliberate NON-requirement.
//
// Region is used only for the ECR login of a private-registry image, so a
// host-command task legitimately has none. Validating it would make the check
// fire on correct input — and a check that cries wolf is one people route
// around, which is how the original bug survived a documented warning.
func TestEmptyRegionIsAllowed(t *testing.T) {
	opts := validOpts()
	opts.Region = ""

	script, err := GenerateWrapper(optsSpec(), opts)
	if err != nil {
		t.Fatalf("an empty Region must be accepted (no container ⇒ no ECR login), got %v", err)
	}
	if script == "" {
		t.Error("no script returned")
	}
}

// TestWrapperOptionsCarriesTheRunIDIntoTheScript confirms the options actually
// reach the emitted script, rather than Validate passing while the fields are
// dropped on the floor — the failure a signature change can introduce silently.
func TestWrapperOptionsCarriesTheRunIDIntoTheScript(t *testing.T) {
	opts := validOpts()
	opts.RunID = "run-distinctive-42"

	for name, gen := range map[string]func(*TaskSpec, WrapperOptions) (string, error){
		"GenerateWrapper":         GenerateWrapper,
		"GeneratePooledJobScript": GeneratePooledJobScript,
		"GenerateFlushScript":     GenerateFlushScript,
	} {
		script, err := gen(optsSpec(), opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(script, "run-distinctive-42") {
			t.Errorf("%s does not carry RunID into the script", name)
		}
		if !strings.Contains(script, opts.ResultsPrefix) {
			t.Errorf("%s does not carry ResultsPrefix into the script", name)
		}
	}
}

// TestGPUIsReadByTheContainerGeneratorsOnly pins the one field the three
// generators do NOT treat alike, so the asymmetry is a documented decision
// rather than something a reader has to infer.
func TestGPUIsReadByTheContainerGeneratorsOnly(t *testing.T) {
	spec := &TaskSpec{TaskID: "t1", Command: []string{"true"}, Container: "nvidia/cuda:latest"}

	opts := validOpts()
	opts.GPU = true

	for name, gen := range map[string]func(*TaskSpec, WrapperOptions) (string, error){
		"GenerateWrapper":         GenerateWrapper,
		"GeneratePooledJobScript": GeneratePooledJobScript,
	} {
		script, err := gen(spec, opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(script, "--gpus all") {
			t.Errorf("%s with GPU=true does not pass --gpus all; a GPU box would run the "+
				"container with no GPU attached (spawn#606)", name)
		}
	}

	// The flush hook does not run the container, so GPU must make no difference
	// to it. Asserted rather than assumed: if it ever starts reading GPU, that is
	// a behaviour change worth failing on.
	withGPU, err := GenerateFlushScript(spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.GPU = false
	withoutGPU, err := GenerateFlushScript(spec, opts)
	if err != nil {
		t.Fatal(err)
	}
	if withGPU != withoutGPU {
		t.Error("GenerateFlushScript's output depends on opts.GPU, but it does not run the " +
			"container; either the dependency is a bug or the field doc is now wrong")
	}
}
