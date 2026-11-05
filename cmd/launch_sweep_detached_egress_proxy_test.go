package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
)

// --bootstrap-egress proxy on a detached parameter sweep (#1515 PR B): the
// submit half.

// detachedProxyBoot is testDetachedBootstrap with a subnet check that counts
// a member on the proxy by its AZ: us-east-1a is vpc-a with no NAT,
// us-east-1c vpc-a with a NAT route, us-west-2a vpc-b with no NAT.
func detachedProxyBoot(t *testing.T) detachedBootstrap {
	t.Helper()
	boot := testDetachedBootstrap(t)
	boot.egress = &detachedEgressProxies{}
	boot.privateNetOf = func(cfg *aws.LaunchConfig) (aws.PrivateCheck, error) {
		if cfg.SubnetID == "" {
			cfg.SubnetID = "subnet-in-" + cfg.AvailabilityZone
		}
		vpc, via := map[string]string{"us-east-1a": "vpc-a", "us-east-1c": "vpc-a", "us-west-2a": "vpc-b"}[cfg.AvailabilityZone],
			cfg.AvailabilityZone != "us-east-1c"
		c := aws.PrivateCheck{Network: aws.PrivateNetwork{Region: cfg.Region, SubnetID: cfg.SubnetID, VPCID: vpc,
			AvailabilityZone: cfg.AvailabilityZone,
			PublicSubnets:    []aws.PublicSubnet{{SubnetID: "subnet-pub-" + vpc, AvailabilityZone: cfg.AvailabilityZone}}}}
		if via && cfg.BootstrapEgress == aws.BootstrapEgressProxy {
			c.ViaEgressProxy = []string{"the Linux bootstrap's AWS CLI download (awscli.amazonaws.com)"}
		}
		return c, nil
	}
	return boot
}

// detachedProxyParams: members 0 and 2 count on vpc-a's proxy, 1 on vpc-b's,
// 3 has a NAT route.
func detachedProxyParams() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "c7g.medium", "ami": "ami-1", "region": "us-east-1", "ttl": "1h", "az": "us-east-1a"},
		Params: []map[string]interface{}{
			{"trial": "a"},
			{"trial": "b", "region": "us-west-2", "az": "us-west-2a"},
			{"trial": "c"},
			{"trial": "d", "az": "us-east-1c"},
		},
	}
}

func detachedProxyBase() aws.LaunchConfig {
	return aws.LaunchConfig{NoPublicIP: true, BootstrapEgress: aws.BootstrapEgressProxy, SecurityGroupIDs: []string{"sg-own"}}
}

// stubDetachedProxyClient adds the AMI and base tags a target records to
// stubSweepProxies' client.
func stubDetachedProxyClient(t *testing.T, f *sweepProxyFake) {
	t.Helper()
	stubSweepProxies(t, f)
	prevClient, prevUD := egressProxyClient, egressProxyUserData
	t.Cleanup(func() { egressProxyClient, egressProxyUserData = prevClient, prevUD })
	egressProxyClient = func(c *aws.Client, region string) deploy.Client {
		dc := prevClient(c, region)
		dc.AMI = func(context.Context) (string, error) { return "ami-al2023-" + region, nil }
		dc.BaseTags = func(context.Context) ([]ec2types.Tag, error) {
			return []ec2types.Tag{tagOf("spawn:managed", "true"), tagOf("spawn:version", version()),
				tagOf("spawn:launch-time", "2026-11-05T00:00:00Z")}, nil
		}
		return dc
	}
	egressProxyUserData = func(idle time.Duration) (string, error) { return "proxy-ud-" + idle.String(), nil }
}

func tagOf(k, v string) ec2types.Tag { return ec2types.Tag{Key: &k, Value: &v} }

// The submit: the proxy of each (region, VPC) a member counts on is found or
// created once, each such member is bound to its VPC's proxy (address and
// clients group) and marked with its VPC for the orchestrator, and each VPC
// is recorded with what the orchestrator needs to replace its proxy.
func TestDetachedSweepEnsuresAndRecordsEachVPCsProxy(t *testing.T) {
	keepLaunchGlobals(t)
	f := &sweepProxyFake{reused: map[string]bool{"vpc-b": true}}
	stubDetachedProxyClient(t, f)
	pf := detachedProxyParams()
	boot := detachedProxyBoot(t)
	if err := seedDetachedBootstrap(pf, detachedProxyBase(), "sw-1", "sw", boot); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}
	// Before the prompt: nothing found or created, the members rendered with
	// the pending address.
	if len(f.reqs) != 0 {
		t.Fatalf("find-or-create ran before the prompt: %+v", f.reqs)
	}
	if !deploy.UserDataNames(memberUserData(pf, 0), deploy.PendingAddr) {
		t.Fatal("member 0 is not rendered with the pending address")
	}

	targets, err := ensureDetachedEgressProxies(context.Background(), nil, pf, boot.egress, 7*time.Hour, io.Discard)
	if err != nil {
		t.Fatalf("ensureDetachedEgressProxies: %v", err)
	}
	if len(f.reqs) != 2 || f.reqs[0].VPCID != "vpc-a" || f.reqs[1].VPCID != "vpc-b" ||
		f.reqs[0].Region != "us-east-1" || f.reqs[1].Region != "us-west-2" || f.reqs[0].Idle != 7*time.Hour {
		t.Fatalf("find-or-create %+v, want vpc-a in us-east-1 then vpc-b in us-west-2, idle 7h", f.reqs)
	}

	for i, want := range map[int]string{0: "vpc-a", 1: "vpc-b", 2: "vpc-a"} {
		if got := pf.Params[i][detachedEgressProxyVPCKey]; got != want {
			t.Errorf("member %d marked %v, want %s", i, got, want)
		}
		addr := f.ip(want) + ":3128"
		if ud := memberUserData(pf, i); !deploy.UserDataNames(ud, addr) || deploy.UserDataNames(ud, deploy.PendingAddr) {
			t.Errorf("member %d's user data does not name %s's proxy %s alone", i, want, addr)
		}
		if g := strings.Join(shippedSettings(t, pf, i).SecurityGroupIDs, ","); g != "sg-own,sg-clients-"+want {
			t.Errorf("member %d's groups %s, want sg-own and %s's clients group", i, g, want)
		}
	}
	if _, marked := pf.Params[3][detachedEgressProxyVPCKey]; marked {
		t.Error("the NAT member is marked as counting on a proxy")
	}
	if ud := memberUserData(pf, 3); deploy.UserDataNames(ud, f.ip("vpc-a")+":3128") || deploy.UserDataNames(ud, deploy.PendingAddr) {
		t.Error("the NAT member's user data names a proxy")
	}
	if g := strings.Join(shippedSettings(t, pf, 3).SecurityGroupIDs, ","); g != "sg-own" {
		t.Errorf("the NAT member's groups %s, want its own", g)
	}

	if len(targets) != 2 {
		t.Fatalf("%d targets, want 2", len(targets))
	}
	a := targets[0]
	if a.Region != "us-east-1" || a.VPCID != "vpc-a" || a.Idle != 7*time.Hour || a.Version != version() ||
		a.InstanceProfile != "spored-instance-profile" || a.AMI != "ami-al2023-us-east-1" || a.UserData != "proxy-ud-7h0m0s" ||
		len(a.PublicSubnets) != 1 || a.PublicSubnets[0].SubnetID != "subnet-pub-vpc-a" || a.AvailabilityZone != "us-east-1a" {
		t.Errorf("target %+v: the orchestrator could not launch the proxy the CLI would", a)
	}
	if a.BaseTags["spawn:version"] != version() || a.BaseTags["spawn:launch-time"] != "" {
		t.Errorf("base tags %v: want the version and no launch time", a.BaseTags)
	}
	if a.Phase != deploy.PhaseReady || a.ProxyID != "i-proxy-vpc-a" || a.ProxyAddr != "10.0.1.5:3128" ||
		a.Rendered() != a.Current() || a.RenderedClientsGroupID != "sg-clients-vpc-a" || a.ProxyGroupID != "sg-proxy-vpc-a" {
		t.Errorf("target %+v: not the ready proxy the members are bound to", a)
	}
	if len(a.Created) != 0 || len(a.CreatingProxies()) != 0 {
		t.Errorf("the CLI's proxy %v is offered to a sweep cancel", a.Created)
	}
}

// A VPC whose proxy cannot be made ready fails the submit before anything is
// uploaded, and the proxies the CLI created for the other VPCs are
// terminated; a reused one is not.
func TestDetachedSweepProxyFailureTerminatesWhatItCreated(t *testing.T) {
	keepLaunchGlobals(t)
	f := &sweepProxyFake{fail: map[string]bool{"vpc-b": true}}
	stubDetachedProxyClient(t, f)
	pf := detachedProxyParams()
	boot := detachedProxyBoot(t)
	if err := seedDetachedBootstrap(pf, detachedProxyBase(), "sw-1", "sw", boot); err != nil {
		t.Fatal(err)
	}
	_, err := ensureDetachedEgressProxies(context.Background(), nil, pf, boot.egress, time.Hour, io.Discard)
	if !errors.Is(err, errSweepProxyFailed) || !strings.Contains(err.Error(), "nothing was uploaded") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(f.terminated, ",") != "i-proxy-vpc-a" {
		t.Errorf("terminated %v, want vpc-a's new proxy", f.terminated)
	}
}

// A member that counts on the proxy on a path with no proxy support (a
// schedule's params) is refused, not launched unable to reach its downloads.
func TestDetachedProxyMemberWithoutSupportIsRefused(t *testing.T) {
	pf := detachedProxyParams()
	boot := detachedProxyBoot(t)
	boot.egress = nil
	if err := seedDetachedBootstrap(pf, detachedProxyBase(), "sw-1", "sw", boot); !errors.Is(err, errEgressProxyUnsupported) {
		t.Errorf("err = %v, want the refusal", err)
	}
}

// A member's user data that was never bound fails closed.
func TestNormalizeDetachedUserDataRefusesAPendingAddress(t *testing.T) {
	pf := detachedProxyParams()
	boot := detachedProxyBoot(t)
	if err := seedDetachedBootstrap(pf, detachedProxyBase(), "sw-1", "sw", boot); err != nil {
		t.Fatal(err)
	}
	if err := normalizeDetachedUserData(pf); err == nil || !strings.Contains(err.Error(), "never bound") {
		t.Errorf("err = %v, want unbound user data refused", err)
	}
}

// The detached path finds or creates the proxies after the confirmation
// prompt and the instance profile, and before the upload, and records them.
func TestDetachedSweepEnsuresTheProxyAfterThePromptBeforeTheUpload(t *testing.T) {
	src, err := os.ReadFile("launch_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	at := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("launch_sweep.go has no %q", sub)
		}
		return i
	}
	prompt, profile := at(`"Launch sweep? [Y/n]: "`), at("ensureDetachedInstanceProfile(ctx, regionClient, paramFormat)")
	ensure := at("ensureDetachedEgressProxies(ctx, regionClient, paramFormat, boot.egress, egressProxyIdle, os.Stderr)")
	upload, record := at("sweep.UploadParamsToS3("), at("EgressProxies:          egressTargets,")
	if !(prompt < profile && profile < ensure && ensure < upload && upload < record) {
		t.Errorf("order prompt %d, profile %d, ensure %d, upload %d, record %d", prompt, profile, ensure, upload, record)
	}
	if strings.Contains(s, "refuseDetachedSweepEgressProxy") {
		t.Error("launch_sweep.go still refuses the proxy on a detached sweep")
	}
}
