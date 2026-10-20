package tasklaunch

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeAWS is an offline AWS: it answers every account-side call and records
// what it was asked, so Launch runs end to end with no network.
type fakeAWS struct {
	profiles []aws.IAMRoleConfig
	deleted  []string
	memCalls int
}

func (f *fakeAWS) GetAccountID(context.Context) (string, error) { return "123456789012", nil }
func (f *fakeAWS) CreateS3BucketIfNotExists(context.Context, string, string) error {
	return nil
}
func (f *fakeAWS) DeleteS3Object(_ context.Context, _, _, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}
func (f *fakeAWS) CreateOrGetInstanceProfile(_ context.Context, cfg aws.IAMRoleConfig) (string, error) {
	f.profiles = append(f.profiles, cfg)
	return "spawn-task-profile", nil
}
func (f *fakeAWS) GetFSxFilesystem(context.Context, string, string) (*aws.FSxInfo, error) {
	return nil, errors.New("fakeAWS: no FSx")
}
func (f *fakeAWS) InstanceMemoryMiB(context.Context, string, string) (int64, bool) {
	f.memCalls++
	return 0, false
}

// capacityProvisioner fails every Spot attempt with a capacity LaunchError and
// launches every On-Demand one, recording each call.
type capacityProvisioner struct {
	configs []aws.LaunchConfig
	opts    []launcher.Options
}

func (p *capacityProvisioner) Provision(_ context.Context, cfg aws.LaunchConfig, opts launcher.Options) (*aws.LaunchResult, error) {
	p.configs = append(p.configs, cfg)
	p.opts = append(p.opts, opts)
	if cfg.Spot {
		return nil, &aws.LaunchError{Code: "InsufficientInstanceCapacity"}
	}
	return &aws.LaunchResult{InstanceID: "i-ondemand", Region: cfg.Region, AvailabilityZone: "us-west-2a"}, nil
}

// spotFallbackSpec is the spore-host-mcp#61 repro: a Spot task with an
// On-Demand fallback, plus an EFS placement so the launch carries a storage
// script as well as the terminal-flush hook.
func spotFallbackSpec() *taskproto.TaskSpec {
	return &taskproto.TaskSpec{
		TaskID:    "mcp-61",
		Command:   []string{"true"},
		Resources: taskproto.ResourceRequest{InstanceType: "r8g.2xlarge", Purchase: taskproto.PurchaseSpot, Fallback: taskproto.PurchaseOnDemand},
		Placement: taskproto.Placement{EFSID: "fs-0123456789abcdef0"},
		Lifecycle: taskproto.Lifecycle{TTL: "45m", OnComplete: "terminate"},
	}
}

// The spore-host-mcp#61 gate, through the exported entry point MCP calls: a
// Spot launch that fails for capacity is retried On-Demand, and both attempts
// get the same, non-empty launcher.Options. spore-host-mcp called
// launcher.Provision with an empty Options{} and no retry, so its launches had
// no storage mounts or flush hook (#1144) and no fallback at all.
func TestLaunch_CapacityFallbackKeepsNonEmptyOptions(t *testing.T) {
	prov := &capacityProvisioner{}
	fa := &fakeAWS{}
	var out bytes.Buffer
	deps := Deps{
		Finder:      fakeTaskFinder{cands: []taskproto.Candidate{{InstanceType: "r8g.2xlarge", Family: "r8g", VCPUs: 8, MemoryGiB: 64, OnDemandPrice: 0.4713}}},
		AWS:         fa,
		Provisioner: prov,
		Out:         &out,
	}
	res, err := Launch(context.Background(), deps, spotFallbackSpec(), "us-west-2")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(prov.configs) != 2 {
		t.Fatalf("Provision called %d times, want 2 (Spot, then On-Demand)", len(prov.configs))
	}
	if !prov.configs[0].Spot || prov.configs[1].Spot {
		t.Errorf("attempts Spot = %v,%v; want true,false", prov.configs[0].Spot, prov.configs[1].Spot)
	}
	if prov.opts[0].TaskFlushScript == "" || prov.opts[0].StorageScript == "" {
		t.Errorf("the Spot attempt's Options are missing the flush hook or storage script: %+v", prov.opts[0])
	}
	if !reflect.DeepEqual(prov.opts[0], prov.opts[1]) {
		t.Errorf("the On-Demand retry got different Options than the Spot attempt:\n spot:      %+v\n on-demand: %+v", prov.opts[0], prov.opts[1])
	}
	if res.InstanceID != "i-ondemand" || res.Spot {
		t.Errorf("result = %+v, want the On-Demand instance with Spot=false", res)
	}
	if !strings.Contains(out.String(), "retrying on-demand") {
		t.Errorf("no fallback notice on Deps.Out: %q", out.String())
	}
	// The rest of the launch ran too: a scoped profile and the stale-record clear.
	if len(fa.profiles) != 1 || fa.profiles[0].InlinePolicyJSON == "" {
		t.Errorf("instance profile calls = %+v", fa.profiles)
	}
	if len(fa.deleted) != 2 {
		t.Errorf("stale-record clear deleted %v, want completion.json and .exitcode", fa.deleted)
	}
}

// Plan sizes with SizingRequest, so a container task reaches the finder with
// at least the container memory floor (spore-host-mcp#60, spawn#1180).
func TestPlan_ContainerSizedAtTheFloor(t *testing.T) {
	f := &recordingSizeFinder{}
	_, _ = Plan(context.Background(), Deps{Finder: f}, containerCPUOnlySpec(), "us-east-1")
	if len(f.got) != 1 || taskproto.EffectiveMemoryGiB(f.got[0]) < taskproto.ContainerMemoryFloorGiB {
		t.Fatalf("finder asked %+v, want one request of at least %.0f GiB", f.got, taskproto.ContainerMemoryFloorGiB)
	}
}

// spawn#1253, through Plan: a pinned type is looked up once, through the
// finder, and the pinned-type checks reuse that lookup rather than asking AWS.
func TestPlan_PinLookedUpOnceAndReused(t *testing.T) {
	spec := containerCPUOnlySpec()
	spec.Resources.InstanceType = "t4g.nano"
	shm := 0.25
	spec.Resources.ShmGiB = &shm
	f := &recordingSizeFinder{cands: []taskproto.Candidate{{InstanceType: "t4g.nano", Family: "t4g", VCPUs: 2, MemoryGiB: 0.5, OnDemandPrice: 0.0042}}}
	fa := &fakeAWS{}
	p, err := Plan(context.Background(), Deps{Finder: f, AWS: fa}, spec, "us-east-1")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(f.got) != 1 {
		t.Errorf("finder called %d times, want 1", len(f.got))
	}
	if fa.memCalls != 0 {
		t.Errorf("AWS asked for the pin's memory %d times; the sizing lookup already had it", fa.memCalls)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "t4g.nano has 0.5 GiB") {
		t.Errorf("warnings = %q, want the container-floor warning from the sized memory", p.Warnings)
	}
	if !p.MaxCostKnown || p.MaxCost <= 0 {
		t.Errorf("max cost = %v (known %v), want a cost from the pin's rate", p.MaxCost, p.MaxCostKnown)
	}
}

// recordingSizeFinder records each request and returns a fixed list.
type recordingSizeFinder struct {
	cands []taskproto.Candidate
	got   []taskproto.ResourceRequest
}

func (f *recordingSizeFinder) FindCandidates(_ context.Context, req taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	f.got = append(f.got, req)
	if f.cands == nil {
		return nil, errors.New("recordingSizeFinder: stop")
	}
	return f.cands, nil
}
