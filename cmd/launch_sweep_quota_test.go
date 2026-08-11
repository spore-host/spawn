package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestSweepQuotaCombos_DedupesAndAppliesBaseFallback(t *testing.T) {
	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "g7e.2xlarge", "spot": true},
			{"instance_type": "g7e.2xlarge", "spot": true}, // duplicate of the first
			{"instance_type": "g7e.4xlarge", "spot": true},
			{}, // omits instance_type entirely -> falls back to baseConfig
		},
	}
	baseConfig := &aws.LaunchConfig{InstanceType: "c7i.xlarge", Spot: false}

	combos, err := sweepQuotaCombos(paramFormat, baseConfig)
	if err != nil {
		t.Fatalf("sweepQuotaCombos: %v", err)
	}

	want := map[sweepQuotaCombo]bool{
		{instanceType: "g7e.2xlarge", spot: true}: true,
		{instanceType: "g7e.4xlarge", spot: true}: true,
		{instanceType: "c7i.xlarge", spot: false}: true,
	}
	if len(combos) != len(want) {
		t.Fatalf("got %d combos, want %d: %v", len(combos), len(want), combos)
	}
	for _, c := range combos {
		if !want[c] {
			t.Errorf("unexpected combo %+v", c)
		}
	}
}

func TestSweepQuotaCombos_BaseSpotAppliesToEveryEntry(t *testing.T) {
	// A base-level Spot:true should OR into every entry, even one that doesn't
	// set its own "spot" key — mirrors the real launchParameterSweep merge
	// semantics (an entry silently inherits defaults it doesn't override).
	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{
			{"instance_type": "g7e.2xlarge"},
		},
	}
	baseConfig := &aws.LaunchConfig{Spot: true}

	combos, err := sweepQuotaCombos(paramFormat, baseConfig)
	if err != nil {
		t.Fatalf("sweepQuotaCombos: %v", err)
	}
	if len(combos) != 1 || !combos[0].spot {
		t.Errorf("got %+v, want a single spot=true combo", combos)
	}
}

func TestSweepQuotaCombos_MissingInstanceTypeErrors(t *testing.T) {
	paramFormat := &ParamFileFormat{
		Params: []map[string]interface{}{
			{},
		},
	}
	baseConfig := &aws.LaunchConfig{} // no fallback InstanceType either
	if _, err := sweepQuotaCombos(paramFormat, baseConfig); err == nil {
		t.Error("want error when neither the param set nor baseConfig has an instance_type")
	}
}

func TestSweepQuotaCombos_NoParamsErrors(t *testing.T) {
	paramFormat := &ParamFileFormat{}
	baseConfig := &aws.LaunchConfig{InstanceType: "c7i.xlarge"}
	if _, err := sweepQuotaCombos(paramFormat, baseConfig); err == nil {
		t.Error("want error for an empty param file")
	}
}

// TestCombosFromLaunchConfigs_Dedupes is the spawn#494 (`spawn resume
// --max-concurrent-auto`) counterpart to TestSweepQuotaCombos_*: extracting
// combos from already-built *aws.LaunchConfig entries (resume's pending
// configs are already fully merged, unlike launch's raw param sets) must
// still dedupe correctly.
func TestCombosFromLaunchConfigs_Dedupes(t *testing.T) {
	configs := []*aws.LaunchConfig{
		{InstanceType: "g7e.2xlarge", Spot: true},
		{InstanceType: "g7e.2xlarge", Spot: true}, // duplicate
		{InstanceType: "g7e.4xlarge", Spot: true},
		{InstanceType: "c7i.xlarge", Spot: false},
	}

	combos, err := combosFromLaunchConfigs(configs)
	if err != nil {
		t.Fatalf("combosFromLaunchConfigs: %v", err)
	}
	want := map[sweepQuotaCombo]bool{
		{instanceType: "g7e.2xlarge", spot: true}: true,
		{instanceType: "g7e.4xlarge", spot: true}: true,
		{instanceType: "c7i.xlarge", spot: false}: true,
	}
	if len(combos) != len(want) {
		t.Fatalf("got %d combos, want %d: %v", len(combos), len(want), combos)
	}
	for _, c := range combos {
		if !want[c] {
			t.Errorf("unexpected combo %+v", c)
		}
	}
}

func TestCombosFromLaunchConfigs_MissingInstanceTypeErrors(t *testing.T) {
	configs := []*aws.LaunchConfig{{}}
	if _, err := combosFromLaunchConfigs(configs); err == nil {
		t.Error("want error when a launch config has no instance_type")
	}
}

func TestCombosFromLaunchConfigs_EmptyErrors(t *testing.T) {
	if _, err := combosFromLaunchConfigs(nil); err == nil {
		t.Error("want error for no launch configs")
	}
}
