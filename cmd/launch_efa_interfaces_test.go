package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// cardsCapacity is an n-card EFA capacity as DescribeInstanceTypes reports it
// for c8gn.48xlarge (2) and p5en.48xlarge (16).
func cardsCapacity(instanceType string, n int) spawnaws.EFACapacity {
	c := spawnaws.EFACapacity{InstanceType: instanceType, MaxInterfaces: n}
	for i := 0; i < n; i++ {
		c.Cards = append(c.Cards, spawnaws.EFANetworkCard{Index: int32(i), EFA: true, EFAOnly: true})
	}
	return c
}

func stubEFACapacity(t *testing.T, c spawnaws.EFACapacity) *int {
	t.Helper()
	calls := 0
	prev := efaCapacityFor
	efaCapacityFor = func(context.Context, *spawnaws.Client, string, string) (spawnaws.EFACapacity, error) {
		calls++
		return c, nil
	}
	t.Cleanup(func() { efaCapacityFor = prev })
	return &calls
}

// --efa-interfaces bounds that need no AWS: negative, and without --efa.
func TestBuildLaunchConfig_EFAInterfacesFlag(t *testing.T) {
	prevEFA, prevN, prevType := efaEnabled, efaInterfaces, instanceType
	t.Cleanup(func() { efaEnabled, efaInterfaces, instanceType = prevEFA, prevN, prevType })
	instanceType = "c8gn.48xlarge"

	efaEnabled, efaInterfaces = true, -1
	if _, err := buildLaunchConfig(nil); err == nil {
		t.Error("--efa-interfaces -1 accepted")
	}
	efaEnabled, efaInterfaces = false, 2
	if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), "--efa") {
		t.Errorf("--efa-interfaces without --efa: err = %v, want a refusal naming --efa", err)
	}
	efaEnabled, efaInterfaces = true, 2
	cfg, err := buildLaunchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EFAInterfaces != 2 {
		t.Errorf("EFAInterfaces = %d, want the flag's 2", cfg.EFAInterfaces)
	}
}

// --efa means every EFA the type has (#661); N above it is an error; 1 is
// today's single interface.
func TestResolveEFAInterfaces(t *testing.T) {
	stubEFACapacity(t, cardsCapacity("p5en.48xlarge", 16))
	for _, tc := range []struct {
		req, want int
		wantErr   bool
	}{{0, 16, false}, {1, 1, false}, {16, 16, false}, {17, 0, true}} {
		cfg := &spawnaws.LaunchConfig{InstanceType: "p5en.48xlarge", Region: "us-east-1", EFAEnabled: true, EFAInterfaces: tc.req}
		_, err := resolveEFAInterfaces(context.Background(), nil, cfg)
		if (err != nil) != tc.wantErr {
			t.Errorf("--efa-interfaces %d: err = %v, wantErr %v", tc.req, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && cfg.EFAInterfaces != tc.want {
			t.Errorf("--efa-interfaces %d resolved to %d, want %d", tc.req, cfg.EFAInterfaces, tc.want)
		}
	}
}

func TestResolveEFAInterfaces_NoEFANoCall(t *testing.T) {
	calls := stubEFACapacity(t, cardsCapacity("x", 2))
	cfg := &spawnaws.LaunchConfig{InstanceType: "t3.micro"}
	if _, err := resolveEFAInterfaces(context.Background(), nil, cfg); err != nil || *calls != 0 || cfg.EFAInterfaces != 0 {
		t.Errorf("non-EFA launch: err=%v calls=%d EFAInterfaces=%d", err, *calls, cfg.EFAInterfaces)
	}
}

// Every --efa launch resolves the count — --mpi or not, real or --dry-run —
// because both go through ensureAMIAndPreflight.
func TestEnsureAMIAndPreflightResolvesEFAInterfaces(t *testing.T) {
	src, err := os.ReadFile("launch_single.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func ensureAMIAndPreflight(")
	if start < 0 {
		t.Fatal("ensureAMIAndPreflight not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	if !strings.Contains(body[start:start+end], "resolveEFAInterfaces(ctx, awsClient, config)") {
		t.Error("ensureAMIAndPreflight does not call resolveEFAInterfaces: --efa would launch one interface on a multi-card type")
	}
}

func renderEFAPlan(t *testing.T, cfg spawnaws.LaunchConfig, c spawnaws.EFACapacity) (string, launchPlan) {
	t.Helper()
	plan := describeLaunchPlan(&cfg)
	if err := applyEFAPlan(&plan, cfg, c); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderLaunchPlanTable(&buf, plan)
	return buf.String(), plan
}

// The dry-run shows the interface layout Launch would send and that an Elastic
// IP will be associated.
func TestLaunchDryRunShowsEFALayoutAndElasticIP(t *testing.T) {
	cfg := spawnaws.LaunchConfig{Name: "p", InstanceType: "p5en.48xlarge", Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 16}
	out, plan := renderEFAPlan(t, cfg, cardsCapacity("p5en.48xlarge", 16))
	for _, want := range []string{
		"EFA:          enabled, 16 of 16 EFA interface(s), one per network card",
		"card 0 / device 0: efa (primary)",
		"card 1 / device 0: efa-only",
		"card 15 / device 0: efa-only",
		"Public IP:    Elastic IP allocated after launch and associated to the primary interface",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	if len(plan.EFALayout) != 16 || !plan.ElasticIP {
		t.Errorf("plan = %d interfaces, ElasticIP=%v", len(plan.EFALayout), plan.ElasticIP)
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"efa_interfaces":16`, `"efa_max_interfaces":16`, `"elastic_ip":true`, `"efa_layout":[`} {
		if !bytes.Contains(b, []byte(k)) {
			t.Errorf("-o json missing %s: %s", k, b)
		}
	}

	// The layout shown is the one Launch sends: the same function, compared
	// interface by interface.
	nis, err := spawnaws.EFALaunchInterfaces(cfg, cardsCapacity("p5en.48xlarge", 16))
	if err != nil {
		t.Fatal(err)
	}
	for i, ni := range nis {
		if want := fmt.Sprintf("card %d / device %d", aws.ToInt32(ni.NetworkCardIndex), aws.ToInt32(ni.DeviceIndex)); !strings.HasPrefix(plan.EFALayout[i], want) {
			t.Errorf("layout[%d] = %q, want %q…", i, plan.EFALayout[i], want)
		}
	}
}

// --efa-interfaces 1: one interface, its own public IP, no Elastic IP line.
func TestLaunchDryRunSingleEFAHasNoElasticIP(t *testing.T) {
	cfg := spawnaws.LaunchConfig{InstanceType: "c8gn.48xlarge", Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1}
	out, plan := renderEFAPlan(t, cfg, cardsCapacity("c8gn.48xlarge", 2))
	if !strings.Contains(out, "1 of 2 EFA interface(s)") || strings.Contains(out, "Elastic IP") || plan.ElasticIP {
		t.Errorf("single-interface preview:\n%s", out)
	}
}
