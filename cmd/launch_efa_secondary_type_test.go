package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// --efa-secondary-type (#1404): what needs no AWS call is checked when the
// config is built. The default leaves the config as it always was; efa needs
// --efa and is refused with --efa-interfaces 1; --efa alone (0 = every card)
// is accepted here and settled by resolveEFAInterfaces.
func TestBuildLaunchConfig_EFASecondaryTypeFlag(t *testing.T) {
	prevEFA, prevN, prevType, prevSec := efaEnabled, efaInterfaces, instanceType, efaSecondaryType
	t.Cleanup(func() { efaEnabled, efaInterfaces, instanceType, efaSecondaryType = prevEFA, prevN, prevType, prevSec })
	instanceType = "c8gn.48xlarge"

	if def := launchCmd.Flags().Lookup("efa-secondary-type").DefValue; def != "efa-only" {
		t.Errorf("--efa-secondary-type defaults to %q, want efa-only (nothing changes unless asked)", def)
	}
	efaEnabled, efaInterfaces, efaSecondaryType = true, 0, "efa-only"
	cfg, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EFASecondaryType != "" {
		t.Errorf("default EFASecondaryType = %q, want unset (today's layout)", cfg.EFASecondaryType)
	}

	for _, tc := range []struct {
		efa     bool
		n       int
		value   string
		wantErr string
	}{
		{false, 0, "efa", "requires --efa"},
		{true, 1, "efa", "one EFA interface"},
		{true, 2, "interface", "must be efa-only or efa"},
	} {
		efaEnabled, efaInterfaces, efaSecondaryType = tc.efa, tc.n, tc.value
		if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("--efa=%t --efa-interfaces %d --efa-secondary-type %s: err = %v, want %q", tc.efa, tc.n, tc.value, err, tc.wantErr)
		}
	}
	for _, n := range []int{0, 2} {
		efaEnabled, efaInterfaces, efaSecondaryType = true, n, "efa"
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Fatalf("--efa --efa-interfaces %d --efa-secondary-type efa: %v", n, err)
		}
		if cfg.EFASecondaryType != "efa" {
			t.Errorf("--efa-interfaces %d: EFASecondaryType = %q, want efa", n, cfg.EFASecondaryType)
		}
	}
}

// --efa with efa on a type with one EFA card: --efa-interfaces 0 resolves to 1,
// which has no secondary card, so the launch (and its dry-run) is refused.
func TestResolveEFAInterfaces_SecondaryTypeNeedsMultiCard(t *testing.T) {
	stubEFACapacity(t, cardsCapacity("c7i.48xlarge", 1))
	cfg := &spawnaws.LaunchConfig{InstanceType: "c7i.48xlarge", Region: "us-east-1", EFAEnabled: true, EFASecondaryType: "efa"}
	if _, err := resolveEFAInterfaces(context.Background(), nil, cfg); err == nil || !strings.Contains(err.Error(), "c7i.48xlarge") {
		t.Errorf("efa on a 1-card type: err = %v, want a refusal naming the type", err)
	}

	stubEFACapacity(t, cardsCapacity("c8gn.48xlarge", 2))
	cfg = &spawnaws.LaunchConfig{InstanceType: "c8gn.48xlarge", Region: "us-east-1", EFAEnabled: true, EFASecondaryType: "efa"}
	if _, err := resolveEFAInterfaces(context.Background(), nil, cfg); err != nil || cfg.EFAInterfaces != 2 {
		t.Errorf("efa on c8gn.48xlarge: err = %v, EFAInterfaces = %d, want accepted with 2", err, cfg.EFAInterfaces)
	}
}

// The dry-run shows each card's interface type: on c8gn.48xlarge, card 0 efa
// at device 0 and card 1 efa at device 0 with a private IP, or efa-only with
// none by default; -o json carries the secondary type.
func TestLaunchDryRunShowsEFASecondaryType(t *testing.T) {
	for _, tc := range []struct {
		secondary string
		card1     string
		jsonType  string
	}{
		{"", "card 1 / device 0: efa-only (no IP)", `"efa_secondary_type":"efa-only"`},
		{"efa", "card 1 / device 0: efa (private IP)", `"efa_secondary_type":"efa"`},
	} {
		cfg := spawnaws.LaunchConfig{Name: "p", InstanceType: "c8gn.48xlarge", Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 2, EFASecondaryType: tc.secondary}
		out, plan := renderEFAPlan(t, cfg, cardsCapacity("c8gn.48xlarge", 2))
		for _, want := range []string{"card 0 / device 0: efa (primary)", tc.card1,
			"Public IP:    Elastic IP allocated after launch and associated to the primary interface"} {
			if !strings.Contains(out, want) {
				t.Errorf("EFASecondaryType %q: dry-run output missing %q:\n%s", tc.secondary, want, out)
			}
		}
		b, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.jsonType) {
			t.Errorf("EFASecondaryType %q: -o json missing %s: %s", tc.secondary, tc.jsonType, b)
		}
	}
}
