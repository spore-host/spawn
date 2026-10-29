package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// testEFACards is MaximumEfaInterfaces as DescribeInstanceTypes reports it for
// the types these tests name.
var testEFACards = map[string]int{"c8gn.48xlarge": 2, "p5en.48xlarge": 16, "c7i.48xlarge": 1}

// testEFACapacity is an EFA capacity lookup with DescribeInstanceTypes fixed:
// the types in testEFACards have their real card counts, any other has other.
func testEFACapacity(other int) efaCapacityLookup {
	return func(instanceType, _ string) (spawnaws.EFACapacity, error) {
		n, ok := testEFACards[instanceType]
		if !ok {
			n = other
		}
		return cardsCapacity(instanceType, n), nil
	}
}

// stubEFACapacityByType replaces DescribeInstanceTypes with testEFACards and
// counts the calls per (region, type).
func stubEFACapacityByType(t *testing.T) map[string]int {
	t.Helper()
	calls := map[string]int{}
	prev := efaCapacityFor
	efaCapacityFor = func(_ context.Context, _ *spawnaws.Client, instanceType, region string) (spawnaws.EFACapacity, error) {
		calls[region+"|"+instanceType]++
		return testEFACapacity(1)(instanceType, region)
	}
	t.Cleanup(func() { efaCapacityFor = prev })
	return calls
}

// efaRows is a sweep whose rows name different instance types.
func efaRows(types ...string) *ParamFileFormat {
	pf := &ParamFileFormat{Defaults: map[string]interface{}{"region": "us-east-1", "ttl": "1h", "ami": "ami-1"}}
	for _, typ := range types {
		pf.Params = append(pf.Params, map[string]interface{}{"instance_type": typ})
	}
	return pf
}

// foregroundEFAMembers builds a foreground sweep's members and resolves their
// EFA count as launchParameterSweep does, DescribeInstanceTypes stubbed.
func foregroundEFAMembers(t *testing.T, base spawnaws.LaunchConfig, pf *ParamFileFormat) ([]*spawnaws.LaunchConfig, error) {
	t.Helper()
	cfgs, _, err := foregroundSweepMembers(base, pf, "sw-1", "sw")
	if err != nil {
		t.Fatalf("build the members: %v", err)
	}
	return cfgs, resolveSweepEFA(cfgs, nil, memoEFACapacity(context.Background(), nil))
}

// layoutCards is the network-card index of each interface Client.Launch sends
// for cfg (EFALaunchInterfaces, the RunInstances NetworkInterfaces), and their
// types.
func layoutCards(t *testing.T, cfg spawnaws.LaunchConfig) ([]int32, []string) {
	t.Helper()
	nis, err := spawnaws.EFALaunchInterfaces(cfg, cardsCapacity(cfg.InstanceType, testEFACards[cfg.InstanceType]))
	if err != nil {
		t.Fatalf("%s: the launch's layout: %v", cfg.InstanceType, err)
	}
	var cards []int32
	var types []string
	for _, ni := range nis {
		cards = append(cards, aws.ToInt32(ni.NetworkCardIndex))
		types = append(types, aws.ToString(ni.InterfaceType))
	}
	return cards, types
}

// #1407: --efa on a foreground sweep is one EFA per network card of each
// member's own type, as on a single launch (--efa-interfaces 0 = every card),
// and each row's type is read on its own.
func TestForegroundSweepEFAIsEveryCardOfEachMembersType(t *testing.T) {
	stubEFACapacityByType(t)
	base := spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFAEnabled: true}
	cfgs, err := foregroundEFAMembers(t, base, efaRows("c8gn.48xlarge", "p5en.48xlarge", "c7i.48xlarge"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i, want := range []int{2, 16, 1} {
		if cfgs[i].EFAInterfaces != want {
			t.Errorf("member %d (%s): EFAInterfaces = %d, want %d (every EFA card of its type)", i, cfgs[i].InstanceType, cfgs[i].EFAInterfaces, want)
		}
	}
	if cards, _ := layoutCards(t, *cfgs[0]); !slices.Equal(cards, []int32{0, 1}) {
		t.Errorf("c8gn.48xlarge member: RunInstances interfaces on cards %v, want [0 1]", cards)
	}
	if cards, _ := layoutCards(t, *cfgs[1]); len(cards) != 16 {
		t.Errorf("p5en.48xlarge member: %d RunInstances interfaces, want 16", len(cards))
	}
}

// An explicit --efa-interfaces keeps today's precedence: it is the count, and
// one above a member type's cards is refused before anything is created.
func TestForegroundSweepKeepsAnExplicitEFAInterfaces(t *testing.T) {
	stubEFACapacityByType(t)
	base := spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1}
	cfgs, err := foregroundEFAMembers(t, base, efaRows("c8gn.48xlarge", "p5en.48xlarge"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, c := range cfgs {
		if c.EFAInterfaces != 1 {
			t.Errorf("%s: --efa-interfaces 1 became %d", c.InstanceType, c.EFAInterfaces)
		}
		if cards, _ := layoutCards(t, *c); len(cards) != 1 {
			t.Errorf("%s: --efa-interfaces 1 lays out %d interfaces", c.InstanceType, len(cards))
		}
	}
	base.EFAInterfaces = 4
	if _, err := foregroundEFAMembers(t, base, efaRows("p5en.48xlarge", "c8gn.48xlarge")); err == nil ||
		!strings.Contains(err.Error(), "parameter set 1") || !strings.Contains(err.Error(), "c8gn.48xlarge") {
		t.Errorf("--efa-interfaces 4 on a 2-card row: err = %v, want a refusal naming the row and its type", err)
	}
}

// DescribeInstanceTypes is asked once per distinct (region, type), however
// many rows share it, on both sweep paths.
func TestSweepEFALookupIsOncePerRegionAndType(t *testing.T) {
	calls := stubEFACapacityByType(t)
	base := spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFAEnabled: true}
	pf := efaRows("c8gn.48xlarge", "c8gn.48xlarge", "p5en.48xlarge", "c8gn.48xlarge", "p5en.48xlarge")
	pf.Params = append(pf.Params, map[string]interface{}{"instance_type": "c8gn.48xlarge", "region": "us-west-2"})
	if _, err := foregroundEFAMembers(t, base, pf); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"us-east-1|c8gn.48xlarge": 1, "us-east-1|p5en.48xlarge": 1, "us-west-2|c8gn.48xlarge": 1}
	if len(calls) != len(want) {
		t.Errorf("foreground: DescribeInstanceTypes for %v, want %v", calls, want)
	}
	for k, n := range want {
		if calls[k] != n {
			t.Errorf("foreground: DescribeInstanceTypes for %s %d times, want %d", k, calls[k], n)
		}
	}

	// Detached: the production resolver (newDetachedBootstrap's), with one
	// EFA each so every member is seeded.
	clear(calls)
	base.EFAInterfaces = 1
	boot := testDetachedBootstrap(t)
	boot.efaOf = newDetachedBootstrap(context.Background(), nil, boot.plat).efaOf
	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", boot); err != nil {
		t.Fatalf("detached: %v", err)
	}
	for k, n := range want {
		if calls[k] != n {
			t.Errorf("detached: DescribeInstanceTypes for %s %d times, want %d (calls %v)", k, calls[k], n, calls)
		}
	}
}

// A detached sweep refuses the RESOLVED count (#1407 on #1353): plain --efa on
// a multi-card type is refused before anything is uploaded, saying what to do,
// never launched with one interface. On a single-card type it is carried.
func TestDetachedSweepRefusesEveryCardEFAOnAMultiCardType(t *testing.T) {
	stubEFACapacityByType(t)
	base := spawnaws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFAEnabled: true}
	boot := testDetachedBootstrap(t)
	boot.efaOf = newDetachedBootstrap(context.Background(), nil, boot.plat).efaOf

	pf := efaRows("c7i.48xlarge", "c8gn.48xlarge")
	err := seedDetachedBootstrap(pf, base, "sw-1", "sw", boot)
	if err == nil {
		t.Fatal("a detached --efa sweep of c8gn.48xlarge was seeded: its member would launch with 1 of 2 EFA interfaces")
	}
	for _, want := range []string{"param set 1", "--efa with 2 EFA interfaces", "c8gn.48xlarge", "cannot be used on a detached sweep", "--no-detach", "--efa-interfaces 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	for i, row := range pf.Params {
		if _, ok := row[detachedLaunchSettingsKey]; ok {
			t.Errorf("member %d: settings seeded for a refused sweep", i)
		}
	}

	pf = efaRows("c7i.48xlarge")
	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", boot); err != nil {
		t.Errorf("--efa on a single-card type, detached: %v", err)
	}
	// --efa-interfaces 1 is the single EFA the refusal offers.
	base.EFAInterfaces = 1
	if err := seedDetachedBootstrap(efaRows("c8gn.48xlarge"), base, "sw-1", "sw", boot); err != nil {
		t.Errorf("--efa --efa-interfaces 1 on c8gn.48xlarge, detached: %v", err)
	}
}

// An instance type pattern has no one type to read: "every card" is refused
// rather than launched as one interface; an explicit count needs no lookup.
func TestSweepEFAOnAnInstanceTypePattern(t *testing.T) {
	calls := stubEFACapacityByType(t)
	lookup := memoEFACapacity(context.Background(), nil)
	cfg := &spawnaws.LaunchConfig{InstanceType: "c8gn.48xlarge|p5en.48xlarge", EFAEnabled: true}
	if err := resolveSweepMemberEFA(cfg, lookup); err == nil || !strings.Contains(err.Error(), "--efa-interfaces 1") {
		t.Errorf("--efa on a pattern: err = %v, want a refusal offering --efa-interfaces 1", err)
	}
	cfg.EFAInterfaces = 1
	if err := resolveSweepMemberEFA(cfg, lookup); err != nil || cfg.EFAInterfaces != 1 {
		t.Errorf("--efa-interfaces 1 on a pattern: err = %v, EFAInterfaces = %d", err, cfg.EFAInterfaces)
	}
	if len(calls) != 0 {
		t.Errorf("a pattern was looked up: %v", calls)
	}
}

// #1404's stopgap is gone: --efa-secondary-type efa with plain --efa on a
// foreground sweep of a multi-card type is accepted, and card 1 is laid out as
// efa; where the member's resolved count is 1 it is still refused.
func TestForegroundSweepEFASecondaryTypeWithPlainEFA(t *testing.T) {
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	t.Setenv("HOME", dir)
	stubTeamName(t)
	stubEFACapacityByType(t)
	for k, v := range map[string]string{"instance-type": "c8gn.48xlarge", "region": "us-east-1", "ttl": "1h", "efa": "true", "efa-secondary-type": "efa"} {
		setLaunchFlag(t, k, v)
	}
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatalf("sweepBaseConfig: %v", err)
	}
	cfgs, err := foregroundEFAMembers(t, *base, &ParamFileFormat{Params: []map[string]interface{}{{"trial": "a"}}})
	if err != nil {
		t.Fatalf("--efa --efa-secondary-type efa on c8gn.48xlarge refused: %v", err)
	}
	if cfgs[0].EFAInterfaces != 2 {
		t.Errorf("EFAInterfaces = %d, want 2", cfgs[0].EFAInterfaces)
	}
	cards, types := layoutCards(t, *cfgs[0])
	if !slices.Equal(cards, []int32{0, 1}) || !slices.Equal(types, []string{"efa", "efa"}) {
		t.Errorf("layout: cards %v types %v, want cards [0 1] both efa", cards, types)
	}

	if _, err := foregroundEFAMembers(t, *base, efaRows("c7i.48xlarge")); err == nil || !strings.Contains(err.Error(), "--efa-secondary-type efa") {
		t.Errorf("--efa-secondary-type efa on a single-card row: err = %v, want refused", err)
	}
}

// Each multi-card member takes an Elastic IP: a foreground sweep checks each
// region has that many left, as a single launch does, before anything is
// created.
func TestPreflightSweepElasticIPs(t *testing.T) {
	calls := stubElasticIPQuota(t, spawnaws.ElasticIPQuota{InUse: 3, Limit: 5}, nil)
	two := func(region string) *spawnaws.LaunchConfig {
		return &spawnaws.LaunchConfig{Region: region, EFAEnabled: true, EFAInterfaces: 2}
	}
	one := &spawnaws.LaunchConfig{Region: "us-east-1", EFAEnabled: true, EFAInterfaces: 1}
	if err := preflightSweepElasticIPs(context.Background(), nil, []*spawnaws.LaunchConfig{two("us-east-1"), one, two("us-east-1"), two("us-west-2")}, &bytes.Buffer{}); err != nil {
		t.Errorf("2 needed of 2 free: %v", err)
	}
	if *calls != 2 {
		t.Errorf("quota read %d times, want once per region (2)", *calls)
	}
	err := preflightSweepElasticIPs(context.Background(), nil, []*spawnaws.LaunchConfig{two("us-east-1"), two("us-east-1"), two("us-east-1")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "needs 3 more") {
		t.Errorf("3 needed of 2 free: err = %v", err)
	}
}

// The production paths reach the resolution before anything is created: a
// foreground launch and a resume before the key pair and spored's role, a
// detached launch before the params are uploaded.
func TestSweepPathsResolveEFABeforeCreatingAnything(t *testing.T) {
	order := func(calls []string, first, then string) bool {
		i, j := slices.Index(calls, first), slices.Index(calls, then)
		return i >= 0 && j >= 0 && i < j
	}
	launch := funcCalls(t, "launch_sweep.go")
	resume := funcCalls(t, "resume.go")
	for _, c := range []struct {
		fn    string
		calls []string
	}{{"launchParameterSweep", launch["launchParameterSweep"]}, {"runResume", resume["runResume"]}} {
		for _, created := range []string{"setupSSHKey", "SetupSporedIAMRole"} {
			for _, check := range []string{"resolveSweepEFA", "preflightSweepElasticIPs"} {
				if !order(c.calls, check, created) {
					t.Errorf("%s does not call %s before %s", c.fn, check, created)
				}
			}
		}
	}
	if !order(launch["launchSweepDetached"], "seedDetachedBootstrap", "UploadParamsToS3") {
		t.Error("launchSweepDetached does not seed (and so refuse) the members before uploading the params")
	}
}
