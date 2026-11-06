package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	"github.com/spore-host/spawn/pkg/progress"
)

// --bootstrap-egress proxy on a foreground parameter sweep (#1515, PR A).

// sweepProxyNet is one subnet of the sweep fixture: its VPC, and whether the
// preflight counts a need as reached through the proxy there (false: a NAT
// route, or an image that has an aws CLI).
type sweepProxyNet struct {
	vpc string
	via bool
}

// stubSweepPreflight makes the subnet check answer per member subnet, from
// nets, and records each member it was asked about.
func stubSweepPreflight(t *testing.T, nets map[string]sweepProxyNet) *[]string {
	t.Helper()
	var asked []string
	prev := preflightNoPublicIPFor
	preflightNoPublicIPFor = func(_ context.Context, _ *aws.Client, cfg *aws.LaunchConfig, _ []string, _ io.Writer) (aws.PrivateCheck, error) {
		asked = append(asked, cfg.Name)
		n, ok := nets[cfg.SubnetID]
		if !ok {
			return aws.PrivateCheck{}, fmt.Errorf("unexpected subnet %q", cfg.SubnetID)
		}
		c := aws.PrivateCheck{Network: aws.PrivateNetwork{Region: cfg.Region, SubnetID: cfg.SubnetID, VPCID: n.vpc,
			AvailabilityZone: cfg.Region + "a", PublicSubnets: []aws.PublicSubnet{{SubnetID: "subnet-pub-" + n.vpc, AvailabilityZone: cfg.Region + "a"}}}}
		if n.via && cfg.BootstrapEgress == aws.BootstrapEgressProxy {
			c.ViaEgressProxy = []string{"the Linux bootstrap's AWS CLI download (awscli.amazonaws.com)"}
		}
		return c, nil
	}
	t.Cleanup(func() { preflightNoPublicIPFor = prev })
	return &asked
}

// sweepProxyFake is find-or-create per VPC: each VPC's proxy has its own IP
// and groups; fail names the VPCs whose proxy fails, and reused the ones whose
// proxy is found rather than created.
type sweepProxyFake struct {
	fail, reused map[string]bool
	reqs         []deploy.Request
	regions      []string // the region of each egressProxyClient
	terminated   []string
}

func (f *sweepProxyFake) ip(vpc string) string {
	return map[string]string{"vpc-a": "10.0.1.5", "vpc-b": "10.1.1.5"}[vpc]
}

func stubSweepProxies(t *testing.T, f *sweepProxyFake) {
	t.Helper()
	prevFind, prevGroups, prevClient, prevCheck, prevTerm := egressProxyFindOrCreate, egressProxyMemberGroups,
		egressProxyClient, egressProxyCheckMemberEgress, egressProxyTerminate
	t.Cleanup(func() {
		egressProxyFindOrCreate, egressProxyMemberGroups, egressProxyClient = prevFind, prevGroups, prevClient
		egressProxyCheckMemberEgress, egressProxyTerminate = prevCheck, prevTerm
	})
	egressProxyClient = func(_ *aws.Client, region string) deploy.Client {
		f.regions = append(f.regions, region)
		return deploy.Client{Profile: func(context.Context) (string, error) { return "spored-instance-profile", nil }}
	}
	egressProxyFindOrCreate = func(_ context.Context, _ deploy.Deps, r deploy.Request) (deploy.Result, error) {
		f.reqs = append(f.reqs, r)
		if f.fail[r.VPCID] {
			return deploy.Result{}, fmt.Errorf("%w: proxy in %s never answered", errSweepProxyFailed, r.VPCID)
		}
		return deploy.Result{
			Groups: deploy.Groups{ClientsID: "sg-clients-" + r.VPCID, ProxyID: "sg-proxy-" + r.VPCID},
			Proxy: deploy.Proxy{InstanceID: "i-proxy-" + r.VPCID, PrivateIP: f.ip(r.VPCID),
				Deadline: time.Now().Add(egressproxy.MaxLife), Created: !f.reused[r.VPCID]},
		}, nil
	}
	egressProxyMemberGroups = func(_ context.Context, _ deploy.EC2, vpc string, own []string, g deploy.Groups) ([]string, error) {
		if len(own) == 0 {
			own = []string{"sg-default-" + vpc}
		}
		return append(append([]string(nil), own...), g.ClientsID), nil
	}
	egressProxyCheckMemberEgress = func(context.Context, deploy.EC2, []string, deploy.Groups, string) error { return nil }
	egressProxyTerminate = func(_ context.Context, _ deploy.EC2, id string) error {
		f.terminated = append(f.terminated, id)
		return nil
	}
}

var errSweepProxyFailed = errors.New("proxy not ready")

// sweepProxyMembers is the fixture: two members in vpc-a (us-east-1), one in
// vpc-b (us-west-2), one in a NAT subnet of vpc-a, one not opted in.
func sweepProxyMembers() []*aws.LaunchConfig {
	m := func(name, region, subnet string, optIn bool) *aws.LaunchConfig {
		c := &aws.LaunchConfig{Name: name, Region: region, SubnetID: subnet, NoPublicIP: true, AMI: "ami-1", TargetOS: "linux"}
		if optIn {
			c.BootstrapEgress = aws.BootstrapEgressProxy
		}
		return c
	}
	return []*aws.LaunchConfig{
		m("a0", "us-east-1", "subnet-a1", true),
		m("b0", "us-west-2", "subnet-b1", true),
		m("a1", "us-east-1", "subnet-a2", true),
		m("nat", "us-east-1", "subnet-nat", true),
		m("off", "us-east-1", "subnet-a1", false),
	}
}

func sweepProxyNets() map[string]sweepProxyNet {
	return map[string]sweepProxyNet{
		"subnet-a1": {"vpc-a", true}, "subnet-a2": {"vpc-a", true},
		"subnet-b1": {"vpc-b", true}, "subnet-nat": {"vpc-a", false},
	}
}

// Two VPCs: find-or-create runs once for each, in its own region, and every
// member that counts on the proxy gets its own VPC's address and client
// group. A member that does not (a NAT subnet, or not opted in) gets none.
func TestSweepFindsOrCreatesOneProxyPerVPC(t *testing.T) {
	keepLaunchGlobals(t)
	stubSweepPreflight(t, sweepProxyNets())
	f := &sweepProxyFake{}
	stubSweepProxies(t, f)
	cfgs := sweepProxyMembers()
	if err := ensureSweepEgressProxies(context.Background(), nil, cfgs, nil, 48*time.Hour, progress.NewQuietProgress(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 || f.reqs[0].VPCID != "vpc-a" || f.reqs[1].VPCID != "vpc-b" {
		t.Fatalf("find-or-create requests %+v, want one for vpc-a then one for vpc-b", f.reqs)
	}
	if f.reqs[0].Region != "us-east-1" || f.reqs[1].Region != "us-west-2" || strings.Join(f.regions, ",") != "us-east-1,us-west-2" {
		t.Errorf("regions: requests %s/%s, clients %v", f.reqs[0].Region, f.reqs[1].Region, f.regions)
	}
	for _, r := range f.reqs {
		if r.Idle != 48*time.Hour || r.InstanceProfile != "spored-instance-profile" || len(r.PublicSubnets) != 1 {
			t.Errorf("request %+v: want the sweep's idle period, the spored profile and the VPC's public subnet", r)
		}
	}
	want := map[string]struct{ addr, groups string }{
		"a0":  {"10.0.1.5:3128", "sg-default-vpc-a,sg-clients-vpc-a"},
		"a1":  {"10.0.1.5:3128", "sg-default-vpc-a,sg-clients-vpc-a"},
		"b0":  {"10.1.1.5:3128", "sg-default-vpc-b,sg-clients-vpc-b"},
		"nat": {"", ""},
		"off": {"", ""},
	}
	for _, c := range cfgs {
		w := want[c.Name]
		if c.EgressProxyAddr != w.addr || strings.Join(c.SecurityGroupIDs, ",") != w.groups {
			t.Errorf("member %s: addr %q groups %v, want %q %q", c.Name, c.EgressProxyAddr, c.SecurityGroupIDs, w.addr, w.groups)
		}
	}
	if len(f.terminated) != 0 {
		t.Errorf("terminated %v on success", f.terminated)
	}
}

// No member counts on the proxy: no find-or-create at all.
func TestSweepWithNoProxyNeedMakesNoProxy(t *testing.T) {
	keepLaunchGlobals(t)
	stubSweepPreflight(t, map[string]sweepProxyNet{"subnet-a1": {"vpc-a", false}, "subnet-b1": {"vpc-b", false},
		"subnet-a2": {"vpc-a", false}, "subnet-nat": {"vpc-a", false}})
	f := &sweepProxyFake{}
	stubSweepProxies(t, f)
	cfgs := sweepProxyMembers()
	if err := ensureSweepEgressProxies(context.Background(), nil, cfgs, nil, egressproxy.DefaultIdle, progress.NewQuietProgress(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 0 || len(f.regions) != 0 {
		t.Errorf("find-or-create %+v, clients %v; want none", f.reqs, f.regions)
	}
	for _, c := range cfgs {
		if c.EgressProxyAddr != "" || len(c.SecurityGroupIDs) != 0 {
			t.Errorf("member %s given a proxy: %q %v", c.Name, c.EgressProxyAddr, c.SecurityGroupIDs)
		}
	}
}

// One VPC's proxy never becomes ready: the sweep fails naming it, no member
// gets a proxy (so none is launched), and the proxy this sweep created in the
// other VPC is terminated. A reused one is left alone: it is not this
// sweep's.
func TestSweepProxyFailureInOneVPCLaunchesNoMember(t *testing.T) {
	for name, tc := range map[string]struct {
		reused        bool
		wantTerminate string
	}{
		"created in the other VPC": {false, "i-proxy-vpc-a"},
		"reused in the other VPC":  {true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			keepLaunchGlobals(t)
			stubSweepPreflight(t, sweepProxyNets())
			f := &sweepProxyFake{fail: map[string]bool{"vpc-b": true}, reused: map[string]bool{"vpc-a": tc.reused}}
			stubSweepProxies(t, f)
			cfgs := sweepProxyMembers()
			var out strings.Builder
			err := ensureSweepEgressProxies(context.Background(), nil, cfgs, nil, egressproxy.DefaultIdle, progress.NewQuietProgress(), &out)
			if !errors.Is(err, errSweepProxyFailed) || !strings.Contains(err.Error(), "vpc-b in us-west-2") ||
				!strings.Contains(err.Error(), "no member was launched") {
				t.Fatalf("err = %v, want vpc-b's failure and that no member was launched", err)
			}
			if strings.Join(f.terminated, ",") != tc.wantTerminate {
				t.Errorf("terminated %v, want %q", f.terminated, tc.wantTerminate)
			}
			if tc.wantTerminate != "" && !strings.Contains(out.String(), "terminated egress proxy "+tc.wantTerminate) {
				t.Errorf("output %q does not say it terminated %s", out.String(), tc.wantTerminate)
			}
			for _, c := range cfgs {
				if c.EgressProxyAddr != "" {
					t.Errorf("member %s given proxy %q by a failed sweep", c.Name, c.EgressProxyAddr)
				}
			}
		})
	}
}

// A member whose own groups cannot reach its VPC's proxy fails the sweep
// before any member launches, naming its parameter set, and the proxies this
// sweep created are terminated.
func TestSweepMemberThatCannotReachItsProxyFailsTheSweep(t *testing.T) {
	keepLaunchGlobals(t)
	stubSweepPreflight(t, sweepProxyNets())
	f := &sweepProxyFake{}
	stubSweepProxies(t, f)
	egressProxyCheckMemberEgress = func(_ context.Context, _ deploy.EC2, groups []string, _ deploy.Groups, ip string) error {
		if ip == "10.1.1.5" {
			return fmt.Errorf("%w: none of %v allows outbound tcp 3128", deploy.ErrMemberEgress, groups)
		}
		return nil
	}
	cfgs := sweepProxyMembers()
	err := ensureSweepEgressProxies(context.Background(), nil, cfgs, []int{10, 11, 12, 13, 14}, egressproxy.DefaultIdle,
		progress.NewQuietProgress(), io.Discard)
	if !errors.Is(err, deploy.ErrMemberEgress) || !strings.Contains(err.Error(), "parameter set 11") {
		t.Fatalf("err = %v, want the member-egress refusal of parameter set 11", err)
	}
	if strings.Join(f.terminated, ",") != "i-proxy-vpc-a,i-proxy-vpc-b" {
		t.Errorf("terminated %v, want both proxies this sweep created", f.terminated)
	}
}

// The preflight's error names the member's parameter set.
func TestSweepProxyPreflightErrorNamesTheParameterSet(t *testing.T) {
	keepLaunchGlobals(t)
	stubSweepPreflight(t, map[string]sweepProxyNet{"subnet-a1": {"vpc-a", true}})
	stubSweepProxies(t, &sweepProxyFake{})
	cfgs := sweepProxyMembers()[:2] // b0's subnet is unknown to the fixture
	err := ensureSweepEgressProxies(context.Background(), nil, cfgs, []int{4, 7}, egressproxy.DefaultIdle, progress.NewQuietProgress(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "parameter set 7") || !strings.Contains(err.Error(), "subnet-b1") {
		t.Errorf("err = %v, want parameter set 7's subnet check failure", err)
	}
}

// The proxy's report names each VPC's proxy and client group.
func TestSweepProxyReportNamesEachVPC(t *testing.T) {
	keepLaunchGlobals(t)
	stubSweepPreflight(t, sweepProxyNets())
	stubSweepProxies(t, &sweepProxyFake{reused: map[string]bool{"vpc-b": true}})
	var out strings.Builder
	if err := ensureSweepEgressProxies(context.Background(), nil, sweepProxyMembers(), nil, egressproxy.DefaultIdle,
		progress.NewQuietProgress(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"✓ egress proxy: launched i-proxy-vpc-a at 10.0.1.5:3128 in vpc-a",
		"✓ egress proxy: reusing i-proxy-vpc-b at 10.1.1.5:3128 in vpc-b",
		"members get " + deploy.ClientsGroupName("vpc-a"),
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not say %q", out.String(), want)
		}
	}
}

// writeSweepParamFile writes a two-row param file and returns its path.
func writeSweepParamFile(t *testing.T) string {
	t.Helper()
	pf := filepath.Join(t.TempDir(), "sweep.yaml")
	if err := os.WriteFile(pf, []byte("defaults:\n  ttl: 1h\nparams:\n  - alpha: 1\n  - alpha: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pf
}

// Wired where it lives: `spawn launch --param-file … --no-detach` with the
// proxy reaches find-or-create, once for the members' VPC, before a single
// RunInstances, and its failure fails the sweep with no member launched.
func TestForegroundSweepFindsOrCreatesTheProxyBeforeAnyMember(t *testing.T) {
	f := &efaSubnetAWS{}
	serveEFASubnetAWS(t, f)
	// The sweep writes audit records: keep ~/.spawn/audit.log in a temp dir
	// and close it, or Windows cannot remove the open file.
	withLogDir(t, false)
	keepBootstrapEgressSource(t)
	t.Setenv(bootstrapEgressEnv, "")
	stubSweepPreflight(t, map[string]sweepProxyNet{"subnet-a1": {"vpc-a", true}})
	fake := &sweepProxyFake{fail: map[string]bool{"vpc-a": true}}
	stubSweepProxies(t, fake)
	setEFALaunchFlags(t, map[string]string{"efa": "false", "instance-type": "c7g.medium",
		"no-public-ip": "true", "subnet-id": "subnet-a1", "bootstrap-egress": "proxy", "egress-proxy-idle": "5h",
		"key-name": "k", "instance-profile": "my-profile", "param-file": writeSweepParamFile(t), "no-detach": "true"})
	err := runLaunch(launchCmd, nil)
	if !errors.Is(err, errSweepProxyFailed) {
		t.Fatalf("runLaunch = %v, want find-or-create's error", err)
	}
	if len(fake.reqs) != 1 || fake.reqs[0].VPCID != "vpc-a" || fake.reqs[0].Idle != 5*time.Hour {
		t.Errorf("find-or-create %+v, want one for vpc-a with --egress-proxy-idle", fake.reqs)
	}
	if n := f.count("RunInstances"); n != 0 {
		t.Errorf("%d RunInstances after the proxy failed", n)
	}
}

// A detached sweep is no longer refused (#1515 PR B), whether the opt-in
// comes from the flag or the environment, and whether --detach is passed or
// is the sweep's default: runLaunch goes on to the detached path, which finds
// or creates the proxy at submit time. (Here it stops at the first AWS call
// the fake does not answer.)
func TestDetachedSweepAcceptsTheProxy(t *testing.T) {
	for name, tc := range map[string]struct {
		flags map[string]string
		env   string
	}{
		"--detach, flag":           {map[string]string{"bootstrap-egress": "proxy", "detach": "true"}, ""},
		"--detach, environment":    {map[string]string{"detach": "true"}, "proxy"},
		"default detach, flag":     {map[string]string{"bootstrap-egress": "proxy"}, ""},
		"default detach, env":      {map[string]string{}, "proxy"},
		"--detach with idle, flag": {map[string]string{"bootstrap-egress": "proxy", "detach": "true", "egress-proxy-idle": "2h"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			withLogDir(t, false)
			keepBootstrapEgressSource(t)
			t.Setenv(bootstrapEgressEnv, tc.env)
			setEFALaunchFlags(t, map[string]string{"efa": "false", "no-public-ip": "true", "subnet-id": "subnet-1",
				"param-file": writeSweepParamFile(t), "max-concurrent": "2"})
			for k, v := range tc.flags {
				setLaunchFlag(t, k, v)
			}
			// The detached path's infra step: the spore-host-infra profile,
			// which the hermetic environment has none of. (Its compute step
			// used to stop it first, on the spore-host-dev profile; compute is
			// the caller's own credentials since #1538.)
			err := runLaunch(launchCmd, nil)
			if errors.Is(err, errEgressProxyUnsupported) || err == nil || !strings.Contains(err.Error(), "load infra AWS config") {
				t.Fatalf("runLaunch = %v, want it to reach the detached path (load infra AWS config)", err)
			}
			if bootstrapEgress != aws.BootstrapEgressProxy {
				t.Errorf("bootstrap egress %q, want proxy", bootstrapEgress)
			}
		})
	}
}

// The detached member guard accepts the opt-in, as the foreground one does,
// and still refuses a proxy address the config names itself, which spawn did
// not find or create and the orchestrator would never ensure.
func TestDetachedGuardAcceptsTheProxyOptIn(t *testing.T) {
	cfg := aws.LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", BootstrapEgress: aws.BootstrapEgressProxy}
	if err := sweepMemberGuard(cfg); err != nil {
		t.Errorf("foreground guard refused the proxy: %v", err)
	}
	if err := detachedSettingsGuard(cfg); err != nil {
		t.Errorf("detached guard refused the opt-in: %v", err)
	}
	cfg.EgressProxyAddr = "10.0.0.1:3128"
	if err := detachedSettingsGuard(cfg); !errors.Is(err, errEgressProxyUnsupported) {
		t.Errorf("detached guard: %v, want a named address refused", err)
	}
}

// A resume of a foreground sweep finds or creates the proxy with the idle
// period the sweep was launched with, recorded in its checkpoint.
func TestSweepRecordCarriesTheProxyIdle(t *testing.T) {
	keepLaunchGlobals(t)
	egressProxyIdle = 7 * time.Hour
	rec, err := newSweepMemberRecord(&aws.LaunchConfig{NoPublicIP: true, BootstrapEgress: aws.BootstrapEgressProxy}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.egressProxyIdle() != 7*time.Hour {
		t.Errorf("recorded idle %v, want 7h", rec.egressProxyIdle())
	}
	rec, err = newSweepMemberRecord(&aws.LaunchConfig{NoPublicIP: true}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.EgressProxyIdle != 0 || rec.egressProxyIdle() != egressproxy.DefaultIdle {
		t.Errorf("a sweep without the proxy recorded %v", rec.EgressProxyIdle)
	}
	if (*SweepMemberRecord)(nil).egressProxyIdle() != egressproxy.DefaultIdle {
		t.Error("an old checkpoint's idle is not the default")
	}
}

// Both foreground sweep paths find or create the proxy after the subnet check
// and before rendering the members' user data, which carries its address.
func TestSweepPathsEnsureTheProxyBeforeRenderingUserData(t *testing.T) {
	for file, call := range map[string]string{
		"launch_sweep.go": "ensureSweepEgressProxies(ctx, awsClient, launchConfigs, nil, egressProxyIdle, ",
		"resume.go":       "ensureSweepEgressProxies(ctx, awsClient, launchConfigs, pending, member.egressProxyIdle(),",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		pre := strings.Index(s, "preflightSweepNoPublicIP(ctx, awsClient, launchConfigs, ")
		at := strings.Index(s, call)
		render := strings.Index(s, "renderForegroundSweepUserData(ctx, plat, launchConfigs, storage)")
		if pre < 0 || at < 0 || render < 0 || !(pre < at && at < render) {
			t.Errorf("%s: want the proxy ensured after the subnet check and before the user data (offsets %d, %d, %d)", file, pre, at, render)
		}
	}
}

// The foreground sweep checks each member's subnet after its AMI and target
// OS are known: they decide its internet needs (an image with no aws CLI
// downloads one), so a check before them counts every unnamed image as one.
func TestForegroundSweepChecksSubnetsAfterTheImage(t *testing.T) {
	src, err := os.ReadFile("launch_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	ami := strings.Index(s, `prog.Complete("Detecting AMI")`)
	osGuard := strings.Index(s, "windowsLifecycleGuard(cfg)")
	pre := strings.Index(s, "preflightSweepNoPublicIP(ctx, awsClient, launchConfigs, nil, os.Stderr)")
	if ami < 0 || osGuard < 0 || pre < 0 || !(ami < pre && osGuard < pre) {
		t.Errorf("want the subnet check after the AMI and OS are resolved (offsets %d, %d, %d)", ami, osGuard, pre)
	}
}
