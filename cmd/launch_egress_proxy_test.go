package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	"github.com/spore-host/spawn/pkg/progress"
)

// --bootstrap-egress / --egress-proxy-idle (#1515 phase 3), CLI side.

func keepBootstrapEgressSource(t *testing.T) {
	t.Helper()
	prev := bootstrapEgressSource
	t.Cleanup(func() { bootstrapEgressSource = prev })
}

// flag > env > config file > default, and a bad value from any of them is an
// error naming where it came from.
func TestResolveBootstrapEgress(t *testing.T) {
	for _, tc := range []struct {
		changed               bool
		flag, env, file, want string
		source                string
	}{
		{false, "none", "", "", "none", "default"},
		{false, "none", "", "proxy", "proxy", "bootstrap_egress in ~/.spawn/config.yaml"},
		{false, "none", "proxy", "none", "proxy", bootstrapEgressEnv},
		{true, "none", "proxy", "proxy", "none", "--bootstrap-egress"},
		{true, "proxy", "", "", "proxy", "--bootstrap-egress"},
	} {
		mode, source, err := resolveBootstrapEgress(tc.changed, tc.flag, tc.env, tc.file)
		if err != nil || mode != tc.want || source != tc.source {
			t.Errorf("%+v: %q from %q, %v; want %q from %q", tc, mode, source, err, tc.want, tc.source)
		}
	}
	if _, _, err := resolveBootstrapEgress(false, "none", "nat", ""); err == nil || !strings.Contains(err.Error(), bootstrapEgressEnv) {
		t.Errorf("bad env value: %v, want an error naming %s", err, bootstrapEgressEnv)
	}
	if _, _, err := resolveBootstrapEgress(true, "", "", ""); err == nil {
		t.Error("an empty --bootstrap-egress was accepted")
	}
}

// `spawn defaults set bootstrap-egress proxy` is the set-once opt-in, and it
// reaches the launch.
func TestBootstrapEgressConfigDefault(t *testing.T) {
	keepLaunchGlobals(t)
	keepBootstrapEgressSource(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv(bootstrapEgressEnv, "")
	if err := spawnconfig.SetLaunchDefault("bootstrap-egress", "nat"); err == nil {
		t.Error("an invalid default was stored")
	}
	if err := spawnconfig.SetLaunchDefault("bootstrap-egress", "proxy"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".spawn", "config.yaml")); err != nil || !strings.Contains(string(b), "bootstrap_egress: proxy") {
		t.Fatalf("config file %q, %v; want the bootstrap_egress key", b, err)
	}
	if err := resolveBootstrapEgressFlag(false); err != nil {
		t.Fatal(err)
	}
	noPublicIP = true
	if bootstrapEgress != aws.BootstrapEgressProxy || launchBootstrapEgress() != aws.BootstrapEgressProxy {
		t.Errorf("bootstrap egress %q (launch %q), want proxy from the config file", bootstrapEgress, launchBootstrapEgress())
	}
	// A public launch does not use the default, and is not refused for it.
	noPublicIP = false
	if launchBootstrapEgress() != "" {
		t.Errorf("a public launch carries %q", launchBootstrapEgress())
	}
	if err := validateBootstrapEgressFlags(false, false); err != nil {
		t.Errorf("a default from the config file refused a public launch: %v", err)
	}
}

// Every refusal happens before a single AWS call: not a write, not a read.
func TestBootstrapEgressRefusalsMakeNoAWSCall(t *testing.T) {
	for name, tc := range map[string]struct {
		flags map[string]string
		env   string
		want  string
	}{
		"proxy without --no-public-ip": {map[string]string{"bootstrap-egress": "proxy"}, "", "applies only with --no-public-ip"},
		"an unknown mode":              {map[string]string{"bootstrap-egress": "nat"}, "", "invalid bootstrap egress"},
		"idle without the proxy": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"egress-proxy-idle": "1h"}, "", "applies only with --no-public-ip --bootstrap-egress proxy"},
		"idle above the proxy's life": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"bootstrap-egress": "proxy", "egress-proxy-idle": "145h"}, "", "longer than the proxy's life of 144h0m0s"},
		"a foreground sweep": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"bootstrap-egress": "proxy", "params": `[{"x":"1"}]`, "no-detach": "true"}, "", "bootstrap-egress"},
		"a detached sweep": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"bootstrap-egress": "proxy", "params": `[{"x":"1"}]`, "detach": "true"}, "", "bootstrap-egress"},
		"a detached sweep opted in by the environment": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"params": `[{"x":"1"}]`, "detach": "true"}, "proxy", "not supported here yet (#1515)"},
		"windows": {map[string]string{"no-public-ip": "true", "subnet-id": "subnet-1",
			"bootstrap-egress": "proxy", "os": "windows"}, "", "the egress proxy serves the Linux bootstrap only"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			keepBootstrapEgressSource(t)
			t.Setenv(bootstrapEgressEnv, tc.env)
			setEFALaunchFlags(t, map[string]string{"efa": "false"})
			for k, v := range tc.flags {
				setLaunchFlag(t, k, v)
			}
			err := runLaunch(launchCmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runLaunch = %v, want it to say %q", err, tc.want)
			}
			if len(f.actions) != 0 {
				t.Errorf("AWS calls before the refusal: %v", f.actions)
			}
		})
	}
}

// stubPreflightViaProxy makes the subnet check count the EFA need as met
// through the proxy, in vpc-1 with a public subnet.
func stubPreflightViaProxy(t *testing.T, via bool) {
	t.Helper()
	prev := preflightNoPublicIPFor
	preflightNoPublicIPFor = func(_ context.Context, _ *aws.Client, cfg *aws.LaunchConfig, _ []string, _ io.Writer) (aws.PrivateCheck, error) {
		c := aws.PrivateCheck{Network: aws.PrivateNetwork{Region: cfg.Region, SubnetID: cfg.SubnetID, VPCID: "vpc-1",
			AvailabilityZone: "us-east-1a", PublicSubnets: []aws.PublicSubnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-east-1a"}}}}
		if via && cfg.BootstrapEgress == aws.BootstrapEgressProxy {
			c.ViaEgressProxy = []string{"the EFA installer (efa-installer.amazonaws.com)"}
		}
		return c, nil
	}
	t.Cleanup(func() { preflightNoPublicIPFor = prev })
}

// stubEgressProxy replaces find-or-create and the client calls around it,
// recording the request; it returns a proxy, or err.
func stubEgressProxy(t *testing.T, err error) *[]deploy.Request {
	t.Helper()
	var reqs []deploy.Request
	prevFind, prevGroups, prevClient := egressProxyFindOrCreate, egressProxyMemberGroups, egressProxyClient
	egressProxyClient = func(*aws.Client, string) deploy.Client {
		return deploy.Client{Profile: func(context.Context) (string, error) { return "spored-instance-profile", nil }}
	}
	egressProxyFindOrCreate = func(_ context.Context, _ deploy.Deps, r deploy.Request) (deploy.Result, error) {
		reqs = append(reqs, r)
		if err != nil {
			return deploy.Result{}, err
		}
		return deploy.Result{
			Groups: deploy.Groups{ClientsID: "sg-clients", ProxyID: "sg-proxy"},
			Proxy:  deploy.Proxy{InstanceID: "i-proxy", PrivateIP: "10.0.1.5", Deadline: time.Now().Add(egressproxy.MaxLife), Created: true},
		}, nil
	}
	egressProxyMemberGroups = func(_ context.Context, _ deploy.EC2, _ string, own []string, g deploy.Groups) ([]string, error) {
		if len(own) == 0 {
			own = []string{"sg-default"}
		}
		return append(append([]string(nil), own...), g.ClientsID), nil
	}
	t.Cleanup(func() {
		egressProxyFindOrCreate, egressProxyMemberGroups, egressProxyClient = prevFind, prevGroups, prevClient
	})
	return &reqs
}

// The proxy is found or created, and the members get its client group and its
// address, only when the subnet check counts on it.
func TestEnsureEgressProxy(t *testing.T) {
	keepLaunchGlobals(t)
	prog := progress.NewQuietProgress()
	cfg := &aws.LaunchConfig{Region: "us-east-1", SubnetID: "subnet-1", NoPublicIP: true, BootstrapEgress: aws.BootstrapEgressProxy}

	stubPreflightViaProxy(t, true)
	reqs := stubEgressProxy(t, nil)
	egressProxyIdle = 48 * time.Hour
	if err := ensureEgressProxy(context.Background(), nil, cfg, prog, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("%d find-or-create calls, want 1", len(*reqs))
	}
	r := (*reqs)[0]
	if r.VPCID != "vpc-1" || r.AvailabilityZone != "us-east-1a" || len(r.PublicSubnets) != 1 || r.Idle != 48*time.Hour ||
		r.InstanceProfile != "spored-instance-profile" || r.Version != version() {
		t.Errorf("request %+v", r)
	}
	if strings.Join(cfg.SecurityGroupIDs, ",") != "sg-default,sg-clients" {
		t.Errorf("member groups %v, want the VPC default and the clients group", cfg.SecurityGroupIDs)
	}
	if cfg.EgressProxyAddr != "10.0.1.5:3128" {
		t.Errorf("proxy address %q", cfg.EgressProxyAddr)
	}

	// Not opted in, or nothing goes through the proxy: no proxy.
	for name, c := range map[string]*aws.LaunchConfig{
		"not opted in":  {Region: "us-east-1", SubnetID: "subnet-1", NoPublicIP: true},
		"public launch": {Region: "us-east-1", BootstrapEgress: aws.BootstrapEgressProxy},
	} {
		*reqs = nil
		if err := ensureEgressProxy(context.Background(), nil, c, prog, io.Discard); err != nil || len(*reqs) != 0 || len(c.SecurityGroupIDs) != 0 {
			t.Errorf("%s: err %v, %d find-or-create calls, groups %v", name, err, len(*reqs), c.SecurityGroupIDs)
		}
	}
	stubPreflightViaProxy(t, false)
	*reqs = nil
	c := &aws.LaunchConfig{Region: "us-east-1", SubnetID: "subnet-1", NoPublicIP: true, BootstrapEgress: aws.BootstrapEgressProxy}
	if err := ensureEgressProxy(context.Background(), nil, c, prog, io.Discard); err != nil || len(*reqs) != 0 || c.EgressProxyAddr != "" {
		t.Errorf("nothing via the proxy: err %v, %d calls, addr %q", err, len(*reqs), c.EgressProxyAddr)
	}
}

// Wired where it lives: `spawn launch` reaches find-or-create before it
// launches anything, and its failure fails the launch with no instance.
func TestLaunchFindsOrCreatesTheProxyBeforeAnyInstance(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"single launch": {},
		"job array":     {"count": "2", "job-array-name": "arr"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &efaSubnetAWS{}
			serveEFASubnetAWS(t, f)
			// The launch writes audit records (the security group): send
			// ~/.spawn/audit.log to a temp dir and close it at the end, or
			// Windows cannot remove the open file from HOME's temp dir.
			withLogDir(t, false)
			keepBootstrapEgressSource(t)
			t.Setenv(bootstrapEgressEnv, "")
			stubPreflightViaProxy(t, true)
			sentinel := errors.New("find-or-create reached")
			reqs := stubEgressProxy(t, sentinel)
			setEFALaunchFlags(t, map[string]string{"efa": "false", "instance-type": "c7g.medium",
				"no-public-ip": "true", "subnet-id": "subnet-1", "bootstrap-egress": "proxy",
				"key-name": "k", "instance-profile": "my-profile"})
			for k, v := range extra {
				setLaunchFlag(t, k, v)
			}
			err := runLaunch(launchCmd, nil)
			if !errors.Is(err, sentinel) {
				t.Fatalf("runLaunch = %v, want find-or-create's error", err)
			}
			if len(*reqs) != 1 || (*reqs)[0].InstanceProfile != "spored-instance-profile" {
				t.Errorf("requests %+v: the proxy runs as the spored profile, not the members' --instance-profile", *reqs)
			}
			if n := f.count("RunInstances"); n != 0 {
				t.Errorf("%d RunInstances before the proxy was ready", n)
			}
		})
	}
}

// Every job array or MPI member is cloned from the config ensureEgressProxy
// set, so each carries the clients group and the proxy address.
func TestJobArrayMembersCarryTheProxy(t *testing.T) {
	base := &aws.LaunchConfig{Name: "arr", SecurityGroupIDs: []string{"sg-default", "sg-clients"}, EgressProxyAddr: "10.0.1.5:3128",
		NoPublicIP: true, BootstrapEgress: aws.BootstrapEgressProxy, SubnetID: "subnet-1"}
	m, err := buildJobArrayMemberConfig(base, memberParams{}, "arr-1", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.SecurityGroupIDs, ",") != "sg-default,sg-clients" || m.EgressProxyAddr != base.EgressProxyAddr {
		t.Errorf("member groups %v addr %q", m.SecurityGroupIDs, m.EgressProxyAddr)
	}
}

// The sweep guard refuses the proxy on both sweep paths, for a caller that did
// not come through runLaunch.
func TestSweepMemberGuardRefusesTheProxy(t *testing.T) {
	cfg := aws.LaunchConfig{NoPublicIP: true, SubnetID: "subnet-1", BootstrapEgress: aws.BootstrapEgressProxy}
	if err := sweepMemberGuard(cfg); err == nil || !strings.Contains(err.Error(), "--bootstrap-egress proxy") {
		t.Errorf("foreground: %v", err)
	}
	if err := detachedSettingsGuard(cfg); err == nil || !strings.Contains(err.Error(), "--bootstrap-egress proxy") {
		t.Errorf("detached: %v", err)
	}
	cfg.BootstrapEgress = aws.BootstrapEgressNone
	if err := sweepMemberGuard(cfg); err != nil {
		t.Errorf("none refused: %v", err)
	}
}
