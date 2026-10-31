package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sweep"
)

// uploadedDetachedParams is the params `spawn launch --detach` uploads for
// base over rows, as the orchestrator (and a detached resume) reads them back:
// seeded by the detach's own bootstrap, then through JSON.
func uploadedDetachedParams(t *testing.T, base aws.LaunchConfig, rows []map[string]interface{}) *ParamFileFormat {
	t.Helper()
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "m7i.large", "ami": "ami-1", "region": "us-east-1", "ttl": "1h"},
		Params:   rows,
	}
	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}
	data, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	var back ParamFileFormat
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	return &back
}

// pinnedPrivateSubnets discovers, from the uploaded settings, the region and
// subnet of every member in pending that carries no_public_ip.
func pinnedPrivateSubnets(t *testing.T, pf *ParamFileFormat, pending []int) []string {
	t.Helper()
	var out []string
	for _, i := range pending {
		s, err := aws.DecodeLaunchSettings(pf.Params[i][detachedLaunchSettingsKey])
		if err != nil {
			t.Fatal(err)
		}
		if s.NoPublicIP {
			if k := paramSetRegion(pf, i) + "|" + s.SubnetID; !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// countedRecheck stubs the production check (preflightNoPublicIPFor) and the
// user's client, counting both. fail decides a subnet's answer.
type countedRecheck struct {
	checked []aws.LaunchConfig
	clients int
}

func (c *countedRecheck) install(t *testing.T, fail func(cfg *aws.LaunchConfig) error) {
	t.Helper()
	prev := preflightNoPublicIPFor
	preflightNoPublicIPFor = func(_ context.Context, _ *aws.Client, cfg *aws.LaunchConfig, _ []string, _ io.Writer) (aws.PrivateCheck, error) {
		c.checked = append(c.checked, *cfg)
		if fail != nil {
			return aws.PrivateCheck{}, fail(cfg)
		}
		return aws.PrivateCheck{}, nil
	}
	t.Cleanup(func() { preflightNoPublicIPFor = prev })
}

func (c *countedRecheck) newClient(context.Context) (*aws.Client, error) {
	c.clients++
	return nil, nil
}

func (c *countedRecheck) subnets() []string {
	var out []string
	for _, cfg := range c.checked {
		out = append(out, cfg.Region+"|"+cfg.SubnetID)
	}
	sort.Strings(out)
	return out
}

var privateSweepRows = func() []map[string]interface{} {
	return []map[string]interface{}{
		{"trial": "a"}, {"trial": "b", "az": "us-east-1c"}, {"trial": "c"},
		{"trial": "d", "az": "us-east-1d"}, {"trial": "e", "az": "us-east-1c"},
	}
}

// TestDetachedResumeRechecksEveryPinnedSubnet is #1468's gate: a detached
// resume of a --no-public-ip sweep re-runs the subnet check, through the
// production seam, for every distinct region and subnet the uploaded params
// pin for a member the orchestrator has still to launch, once each, as a
// private-only launch of that subnet, before the orchestrator is re-invoked.
func TestDetachedResumeRechecksEveryPinnedSubnet(t *testing.T) {
	base := aws.LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-east-1a"}
	pf := uploadedDetachedParams(t, base, privateSweepRows())

	for name, rec := range map[string]*sweep.SweepRecord{
		"from the start":  {TotalParams: 5},
		"part launched":   {TotalParams: 5, NextToLaunch: 2},
		"last one left":   {TotalParams: 5, NextToLaunch: 4},
		"multi-region":    {TotalParams: 5, NextToLaunch: 5, MultiRegion: true, RegionStatus: map[string]*sweep.RegionProgress{"us-east-1": {NextToLaunch: []int{3, 1}}}},
		"all launched":    {TotalParams: 5, NextToLaunch: 5},
		"multi, finished": {TotalParams: 5, MultiRegion: true, RegionStatus: map[string]*sweep.RegionProgress{"us-east-1": {}}},
	} {
		t.Run(name, func(t *testing.T) {
			want := pinnedPrivateSubnets(t, pf, detachedResumePending(rec))
			var c countedRecheck
			c.install(t, nil)
			if err := recheckDetachedNoPublicIP(context.Background(), rec, pf, false, c.newClient, io.Discard); err != nil {
				t.Fatalf("recheck: %v", err)
			}
			if got := c.subnets(); !slices.Equal(got, want) {
				t.Errorf("re-checked %v; want every pinned subnet of the pending members, once each: %v", got, want)
			}
			for _, cfg := range c.checked {
				if !cfg.NoPublicIP || cfg.AllowMissingEndpoints || cfg.TargetOS != "linux" {
					t.Errorf("checked %+v; want a private-only linux launch with no override", cfg)
				}
			}
			if wantClients := min(len(want), 1); c.clients != wantClients {
				t.Errorf("made %d clients, want %d", c.clients, wantClients)
			}
		})
	}
	if got := pinnedPrivateSubnets(t, pf, detachedResumePending(&sweep.SweepRecord{TotalParams: 5})); len(got) < 3 {
		t.Fatalf("the params pin only %v; the gate needs several subnets to tell every from some", got)
	}

	// Wired into the detached resume, with the user's credentials, before
	// anything is re-created or the orchestrator re-invoked.
	calls := funcCalls(t, "resume.go")["resumeSweepDetached"]
	at := func(name string) int { return slices.Index(calls, name) }
	if r := at("recheckDetachedNoPublicIP"); r < 0 || r > at("ensureDetachedResumeProfile") || r > at("InvokeSweepOrchestrator") {
		t.Errorf("resumeSweepDetached calls %v: want recheckDetachedNoPublicIP before ensureDetachedResumeProfile and InvokeSweepOrchestrator", calls)
	}
	if d := at("downloadDetachedParams"); d < 0 || d > at("recheckDetachedNoPublicIP") {
		t.Errorf("resumeSweepDetached calls %v: want the uploaded params read, then checked", calls)
	}
	src, err := os.ReadFile("resume.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`recheckDetachedNoPublicIP\(ctx, state, pf, resumeAllowMissingEndpoints, aws\.NewClient, `).Match(src) {
		t.Error("resumeSweepDetached does not check with the user's credentials (aws.NewClient) and resume's --allow-missing-endpoints")
	}
}

// TestDetachedResumeRefusesANowBadSubnet: a pinned subnet that fails the
// check now (an endpoint or NAT route removed since --detach) refuses the
// resume with PreflightNoPublicIP's own error, naming the member, and resume's
// --allow-missing-endpoints reaches the check as a launch's does.
func TestDetachedResumeRefusesANowBadSubnet(t *testing.T) {
	pf := uploadedDetachedParams(t, aws.LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-east-1a"}, privateSweepRows())
	rec := &sweep.SweepRecord{TotalParams: 5, NextToLaunch: 1}
	bad := func(cfg *aws.LaunchConfig) error {
		if cfg.SubnetID == "subnet-in-us-east-1c" && !cfg.AllowMissingEndpoints {
			return aws.ErrPrivateNetworkIncomplete
		}
		return nil
	}
	var c countedRecheck
	c.install(t, bad)
	err := recheckDetachedNoPublicIP(context.Background(), rec, pf, false, c.newClient, io.Discard)
	if !errors.Is(err, aws.ErrPrivateNetworkIncomplete) || !strings.Contains(err.Error(), "parameter set 1") {
		t.Fatalf("err = %v; want the subnet refusal for parameter set 1", err)
	}

	var o countedRecheck
	o.install(t, bad)
	if err := recheckDetachedNoPublicIP(context.Background(), rec, pf, true, o.newClient, io.Discard); err != nil {
		t.Fatalf("with --allow-missing-endpoints: %v", err)
	}
	if len(o.checked) == 0 {
		t.Fatal("with --allow-missing-endpoints nothing was checked; the override must still run the check, which reports what is missing")
	}
	for _, cfg := range o.checked {
		if !cfg.AllowMissingEndpoints {
			t.Errorf("subnet %s was checked without resume's --allow-missing-endpoints", cfg.SubnetID)
		}
	}

	// Settings that ask for no public IP but pin no subnet have nothing to check.
	unpinned := uploadedDetachedParams(t, aws.LaunchConfig{NoPublicIP: true, AvailabilityZone: "us-east-1a"}, privateSweepRows())
	settings := unpinned.Params[2][detachedLaunchSettingsKey].(map[string]interface{})
	delete(settings, "subnet_id")
	var u countedRecheck
	u.install(t, nil)
	if err := recheckDetachedNoPublicIP(context.Background(), rec, unpinned, false, u.newClient, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "parameter set 2") || !strings.Contains(err.Error(), "pin no subnet") {
		t.Errorf("err = %v; want parameter set 2's unpinned settings refused", err)
	}
}

// TestDetachedResumeWithoutNoPublicIPMakesNoCheck: a sweep launched without
// --no-public-ip makes no subnet check and creates no client on resume, and
// resume's --allow-missing-endpoints, with nothing to allow, is refused, still
// without a call.
func TestDetachedResumeWithoutNoPublicIPMakesNoCheck(t *testing.T) {
	pf := uploadedDetachedParams(t, aws.LaunchConfig{AvailabilityZone: "us-east-1a"}, privateSweepRows())
	rec := &sweep.SweepRecord{TotalParams: 5}
	for _, allow := range []bool{false, true} {
		var c countedRecheck
		c.install(t, nil)
		err := recheckDetachedNoPublicIP(context.Background(), rec, pf, allow, c.newClient, io.Discard)
		if allow != (err != nil) {
			t.Errorf("allow=%t: err = %v", allow, err)
		}
		if allow && !strings.Contains(fmtErr(err), "--allow-missing-endpoints applies only") {
			t.Errorf("allow=%t: err = %v; want the nothing-to-allow refusal", allow, err)
		}
		if len(c.checked) != 0 || c.clients != 0 {
			t.Errorf("allow=%t: %d subnet checks and %d clients for a sweep without --no-public-ip; want none", allow, len(c.checked), c.clients)
		}
	}
	// Legacy params, from a CLI that wrote no launch settings, are the same.
	legacy := &ParamFileFormat{Defaults: map[string]interface{}{"region": "us-east-1"}, Params: []map[string]interface{}{{"trial": "a"}}}
	var c countedRecheck
	c.install(t, nil)
	if err := recheckDetachedNoPublicIP(context.Background(), &sweep.SweepRecord{TotalParams: 1}, legacy, false, c.newClient, io.Discard); err != nil ||
		len(c.checked) != 0 || c.clients != 0 {
		t.Errorf("legacy params: err %v, %d checks, %d clients; want none", err, len(c.checked), c.clients)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestResumeAllowMissingEndpointsFlag: both resume spellings take
// --allow-missing-endpoints, and a foreground resume refuses it (its members
// are rebuilt with the launch's own).
func TestResumeAllowMissingEndpointsFlag(t *testing.T) {
	for _, c := range []*pflagCmd{{"resume", resumeCmd.Flags()}, {"sweep resume", sweepResumeCmd.Flags()}} {
		if c.fs.Lookup("allow-missing-endpoints") == nil {
			t.Errorf("spawn %s has no --allow-missing-endpoints", c.name)
		}
	}
	prevAllow, prevDetach := resumeAllowMissingEndpoints, resumeDetach
	t.Cleanup(func() { resumeAllowMissingEndpoints, resumeDetach = prevAllow, prevDetach })
	resumeAllowMissingEndpoints, resumeDetach = true, false
	var out bytes.Buffer
	resumeCmd.SetOut(&out)
	err := runResume(resumeCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "applies only with --detach") {
		t.Errorf("foreground resume with --allow-missing-endpoints: err = %v; want it refused", err)
	}
}
