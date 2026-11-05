package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	"github.com/spore-host/spawn/pkg/egressproxy/sweepproxy"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sweep"
)

const (
	proxyVPC      = "vpc-1"
	renderedAddr  = "10.0.1.5:3128"
	renderedGroup = "sg-clients-1"
)

// recordingEC2 is an EC2 transport that records each RunInstances request's
// form and fails every request, so nothing reaches AWS.
type recordingEC2 struct {
	mu   sync.Mutex
	runs []url.Values
}

func (f *recordingEC2) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if v, err := url.ParseQuery(string(body)); err == nil && v.Get("Action") == "RunInstances" {
		f.mu.Lock()
		f.runs = append(f.runs, v)
		f.mu.Unlock()
	}
	return nil, io.ErrUnexpectedEOF
}

func (f *recordingEC2) launched() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.runs...)
}

// proxyHarness wires the launch paths to fakes: the EC2 transport above,
// DynamoDB that fails at once, saves counted, and the egress proxy's ensure
// step scripted by ensure.
type proxyHarness struct {
	ec2     *recordingEC2
	client  *ec2.Client
	saves   int
	ensures map[string]int // by VPC
	checks  []string       // CheckMemberEgress proxy IPs
}

func newProxyHarness(t *testing.T, ensure func(t *deploy.SweepTarget) deploy.SweepStep) *proxyHarness {
	t.Helper()
	h := &proxyHarness{ec2: &recordingEC2{}, ensures: map[string]int{}}
	dynamo := make(dynamoSignal, 256)
	prevDynamo, prevSave := dynamodbClient, saveProgress
	prevEnsure, prevDeps, prevCheck := ensureEgressProxy, egressProxyDeps, checkMemberProxyEgress
	var mu sync.Mutex
	t.Cleanup(func() {
		for range h.ec2.launched() {
			select {
			case <-dynamo:
			case <-time.After(30 * time.Second):
				t.Error("a launch's availability record never ran")
			}
		}
		dynamodbClient, saveProgress = prevDynamo, prevSave
		ensureEgressProxy, egressProxyDeps, checkMemberProxyEgress = prevEnsure, prevDeps, prevCheck
	})
	dynamodbClient = dynamodb.New(dynamodb.Options{
		Region: "us-east-1", Credentials: awssdk.AnonymousCredentials{},
		HTTPClient: &http.Client{Transport: dynamo}, RetryMaxAttempts: 1,
	})
	saveProgress = func(context.Context, *SweepRecord) error {
		mu.Lock()
		h.saves++
		mu.Unlock()
		return nil
	}
	egressProxyDeps = func(context.Context, *SweepRecord, string) (deploy.Deps, error) { return deploy.Deps{}, nil }
	ensureEgressProxy = func(_ context.Context, _ deploy.Deps, tgt *deploy.SweepTarget) deploy.SweepStep {
		mu.Lock()
		h.ensures[tgt.VPCID]++
		mu.Unlock()
		return ensure(tgt)
	}
	checkMemberProxyEgress = func(_ context.Context, _ deploy.EC2, _ []string, _ deploy.Groups, ip string) error {
		mu.Lock()
		h.checks = append(h.checks, ip)
		mu.Unlock()
		return nil
	}
	h.client = ec2.New(ec2.Options{
		Region: "us-east-1", Credentials: awssdk.AnonymousCredentials{},
		HTTPClient: &http.Client{Transport: h.ec2}, RetryMaxAttempts: 1,
	})
	return h
}

// proxyUserData is a member's user data as the CLI uploads it, bound to the
// proxy at addr.
func proxyUserData(t *testing.T, addr string) string {
	t.Helper()
	env, err := spawnaws.EgressProxyEnv(addr, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return launcher.EncodeLinuxUserData("#!/bin/bash\n" + env + " spawn_need_aws || true\n")
}

// proxyRows are members as the CLI uploads them: proxied[i] says member i
// counts on vpc-1's proxy (and was bound to it), else it has a NAT route.
func proxyRows(t *testing.T, proxied ...bool) *ParamFileFormat {
	t.Helper()
	pf := &ParamFileFormat{Defaults: map[string]interface{}{}}
	for _, p := range proxied {
		row := map[string]interface{}{
			userDataParam: "x", tagsParam: cliTags(nil), "instance_type": "t3.micro",
			"iam_role":          spawnaws.SporedInstanceProfileName,
			launchSettingsParam: cliSettings(t, &spawnaws.LaunchConfig{SubnetID: "subnet-1", NoPublicIP: true, SecurityGroupIDs: []string{"sg-own"}}),
		}
		if p {
			row[userDataParam] = proxyUserData(t, renderedAddr)
			row[launchSettingsParam] = cliSettings(t, &spawnaws.LaunchConfig{SubnetID: "subnet-1", NoPublicIP: true,
				SecurityGroupIDs: []string{"sg-own", renderedGroup}})
			row[egressProxyVPCParam] = proxyVPC
		}
		pf.Params = append(pf.Params, row)
	}
	return pf
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func proxyState(n int) *SweepRecord {
	return &SweepRecord{
		SweepID: "sw-1515", SweepName: "sw", TotalParams: n, Region: "us-east-1", AWSAccountID: "435415984226",
		EgressProxies: []deploy.SweepTarget{{
			Region: "us-east-1", VPCID: proxyVPC, RenderedAddr: renderedAddr, RenderedClientsGroupID: renderedGroup,
			Phase: deploy.PhaseReady, ProxyID: "i-proxy", ProxyAddr: renderedAddr, ClientsGroupID: renderedGroup,
			ProxyGroupID: "sg-proxy-1",
		}},
	}
}

// A member whose VPC's proxy is being made ready is held, with every member
// after it, and the batch ends: the next poll looks again. The member before
// it, which needs no proxy, launches.
func TestEgressProxyHoldsTheQueue(t *testing.T) {
	h := newProxyHarness(t, func(tgt *deploy.SweepTarget) deploy.SweepStep {
		tgt.Phase, tgt.ProxyID = deploy.PhaseCreating, "i-new"
		return deploy.SweepStep{Outcome: deploy.SweepHold, Changed: true, Reason: "launched egress proxy i-new"}
	})
	state := proxyState(3)
	launchBatch(context.Background(), h.client, state, proxyRows(t, false, true, false), 3, 0)

	if n := len(h.ec2.launched()); n != 1 {
		t.Fatalf("%d RunInstances, want 1: only the member before the held one", n)
	}
	if state.NextToLaunch != 1 {
		t.Errorf("NextToLaunch = %d, want 1: the held member must be tried again", state.NextToLaunch)
	}
	if state.EgressProxies[0].Phase != deploy.PhaseCreating || h.saves == 0 {
		t.Errorf("phase %s after %d saves: the creating proxy must be recorded", state.EgressProxies[0].Phase, h.saves)
	}

	// The next poll: still creating. The ensure step runs once per pass.
	h.ensures = map[string]int{}
	launchBatch(context.Background(), h.client, state, proxyRows(t, false, true, false), 2, 0)
	if h.ensures[proxyVPC] != 1 || state.NextToLaunch != 1 || len(h.ec2.launched()) != 1 {
		t.Errorf("%d ensures, NextToLaunch %d, %d launches", h.ensures[proxyVPC], state.NextToLaunch, len(h.ec2.launched()))
	}
}

// When no proxy can be made ready, the VPC's members are failed with the
// reason as the queue reaches them, and the others still launch.
func TestEgressProxyFailureFailsTheVPCsMembers(t *testing.T) {
	const why = "the egress proxy the sweep orchestrator launched for vpc-1 in us-east-1 did not become ready"
	h := newProxyHarness(t, func(tgt *deploy.SweepTarget) deploy.SweepStep {
		tgt.Phase, tgt.FailReason = deploy.PhaseFailed, why
		return deploy.SweepStep{Outcome: deploy.SweepFail, Changed: true, Reason: why}
	})
	state := proxyState(4)
	launchBatch(context.Background(), h.client, state, proxyRows(t, false, true, false, true), 4, 0)

	if n := len(h.ec2.launched()); n != 2 {
		t.Errorf("%d RunInstances, want 2: the members with no proxy", n)
	}
	if h.ensures[proxyVPC] != 1 {
		t.Errorf("%d ensures in one pass, want 1", h.ensures[proxyVPC])
	}
	if state.NextToLaunch != 4 {
		t.Errorf("NextToLaunch = %d, want 4", state.NextToLaunch)
	}
	failed := map[int]string{}
	for _, m := range state.Instances {
		if m.State == "failed" {
			failed[m.Index] = m.ErrorMessage
		}
	}
	for _, i := range []int{1, 3} {
		if !strings.Contains(failed[i], why) || !strings.Contains(failed[i], "was not launched") {
			t.Errorf("member %d failed with %q, want the proxy's reason", i, failed[i])
		}
	}
}

// A member launched after the proxy was replaced is rebound to the new one:
// its user data names the new address and its groups the current clients
// group, and its own groups are checked to reach the new proxy.
func TestEgressProxyRebindsToTheCurrentProxy(t *testing.T) {
	h := newProxyHarness(t, func(tgt *deploy.SweepTarget) deploy.SweepStep {
		tgt.ProxyID, tgt.ProxyAddr, tgt.ClientsGroupID = "i-new", "10.0.7.9:3128", "sg-clients-2"
		return deploy.SweepStep{Outcome: deploy.SweepLaunch, Changed: true}
	})
	state := proxyState(2)
	launchBatch(context.Background(), h.client, state, proxyRows(t, true, true), 2, 0)

	runs := h.ec2.launched()
	if len(runs) != 2 {
		t.Fatalf("%d RunInstances, want 2", len(runs))
	}
	for i, v := range runs {
		raw, err := base64.StdEncoding.DecodeString(v.Get("UserData"))
		if err != nil {
			t.Fatal(err)
		}
		script := gunzip(t, raw)
		if !strings.Contains(script, "http://10.0.7.9:3128") || strings.Contains(script, "http://10.0.1.5:3128") {
			t.Errorf("member %d's user data names the old proxy:\n%s", i, script)
		}
		var groups []string
		for k, vals := range v {
			if strings.HasPrefix(k, "NetworkInterface.1.SecurityGroupId.") {
				groups = append(groups, vals...)
			}
		}
		if g := strings.Join(groups, ","); !strings.Contains(g, "sg-clients-2") || strings.Contains(g, renderedGroup) || !strings.Contains(g, "sg-own") {
			t.Errorf("member %d's groups %s, want sg-own and the current clients group only", i, g)
		}
	}
	if len(h.checks) != 1 || h.checks[0] != "10.0.7.9" {
		t.Errorf("member egress checked for %v, want once, for the new proxy", h.checks)
	}
}

// A member whose proxy is the one it was rendered for launches as uploaded.
func TestEgressProxyReusedLaunchesAsUploaded(t *testing.T) {
	h := newProxyHarness(t, func(*deploy.SweepTarget) deploy.SweepStep {
		return deploy.SweepStep{Outcome: deploy.SweepLaunch}
	})
	pf := proxyRows(t, true)
	config := mergeParams(pf.Defaults, pf.Params[0])
	gate := newEgressGate(proxyState(1))
	if v, why := gate.admit(context.Background(), "us-east-1", config, 0); v != admitLaunch {
		t.Fatalf("verdict %v: %s", v, why)
	}
	if config[userDataParam] != pf.Params[0][userDataParam] || len(h.checks) != 0 {
		t.Errorf("an unchanged proxy rewrote the member (%d egress checks)", len(h.checks))
	}
}

// A member that names a VPC the record carries no proxy for is refused.
func TestEgressProxyMemberWithoutATargetIsRefused(t *testing.T) {
	newProxyHarness(t, func(*deploy.SweepTarget) deploy.SweepStep {
		t.Error("ensured a target that does not exist")
		return deploy.SweepStep{}
	})
	pf := proxyRows(t, true)
	state := proxyState(1)
	state.EgressProxies = nil
	v, why := newEgressGate(state).admit(context.Background(), "us-east-1", mergeParams(pf.Defaults, pf.Params[0]), 0)
	if v != admitFail || !strings.Contains(why, "carries no egress proxy") {
		t.Errorf("verdict %v, %q", v, why)
	}
}

// On a multi-region sweep a held region does not hold the others.
func TestEgressProxyHoldIsPerRegion(t *testing.T) {
	h := newProxyHarness(t, func(tgt *deploy.SweepTarget) deploy.SweepStep {
		return deploy.SweepStep{Outcome: deploy.SweepHold, Reason: "creating"}
	})
	state := proxyState(4)
	state.MultiRegion = true
	state.RegionStatus = map[string]*RegionProgress{
		"us-east-1": {NextToLaunch: []int{0, 1}},
		"us-west-2": {NextToLaunch: []int{2, 3}},
	}
	pf := proxyRows(t, true, false, false, false)
	orch := &RegionalOrchestrator{ec2Clients: map[string]*ec2.Client{"us-east-1": h.client, "us-west-2": h.client}}
	if err := launchAcrossRegions(context.Background(), orch, state, pf, map[string]int{}, 4, 0); err != nil {
		t.Fatal(err)
	}
	if got := state.RegionStatus["us-east-1"].NextToLaunch; len(got) != 2 {
		t.Errorf("us-east-1 queue %v, want both held", got)
	}
	if got := state.RegionStatus["us-west-2"].NextToLaunch; len(got) != 0 {
		t.Errorf("us-west-2 queue %v, want it launched", got)
	}
	if n := len(h.ec2.launched()); n != 2 {
		t.Errorf("%d RunInstances, want us-west-2's 2", n)
	}
}

// A cancel terminates a proxy this orchestrator launched that is still
// creating, and never a ready one or one it did not launch.
func TestCancelTerminatesOnlyItsCreatingProxy(t *testing.T) {
	east := &fakeMemberEC2{instances: []ec2types.Instance{
		fakeInstance("i-creating", ec2types.InstanceStateNamePending, "", nil),
		fakeInstance("i-ready", ec2types.InstanceStateNameRunning, "", nil),
		fakeInstance("i-cli", ec2types.InstanceStateNamePending, "", nil),
	}}
	h := newCancelHarness(t, &SweepRecord{
		SweepID: "sw-a", Status: "CANCELLED", CancelRequested: true, Region: "us-east-1",
		EgressProxies: []deploy.SweepTarget{
			{Region: "us-east-1", VPCID: "vpc-a", Phase: deploy.PhaseCreating, ProxyID: "i-creating", Created: []string{"i-creating"}},
			{Region: "us-east-1", VPCID: "vpc-b", Phase: deploy.PhaseReady, ProxyID: "i-ready", Created: []string{"i-ready"}},
			// Creating, but not launched by this orchestrator: not offered.
			{Region: "us-east-1", VPCID: "vpc-c", Phase: deploy.PhaseCreating, ProxyID: "i-cli"},
		},
	}, map[string]*fakeMemberEC2{"us-east-1": east})

	invoke(t, "sw-a")
	if got := strings.Join(east.terminated(), ","); got != "i-creating" {
		t.Errorf("terminated %q, want only i-creating", got)
	}
	calls := len(east.calls)
	invoke(t, "sw-a")
	if extra := east.calls[calls:]; len(extra) != 0 {
		t.Errorf("re-run made EC2 calls %v, want none", extra)
	}
	_ = h
}

// The record the CLI writes (pkg/sweep) carries the targets to this
// function's record through DynamoDB, field for field, and back.
func TestEgressProxiesRoundTripTheRecord(t *testing.T) {
	want := deploy.SweepTarget{
		Region: "us-east-1", VPCID: proxyVPC, AvailabilityZone: "us-east-1a",
		PublicSubnets: []sweepproxy.Subnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-east-1a"}},
		Idle:          72 * time.Hour, Version: "0.160.0", InstanceProfile: spawnaws.SporedInstanceProfileName,
		AMI: "ami-1", UserData: "ud", BaseTags: map[string]string{"spawn:managed": "true"},
		RenderedAddr: renderedAddr, RenderedClientsGroupID: renderedGroup,
		Phase: deploy.PhaseCreating, ProxyID: "i-new", ClientsGroupID: renderedGroup, ProxyGroupID: "sg-proxy-1",
		CreatingSince: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), Created: []string{"i-new"},
	}
	item, err := attributevalue.MarshalMap(&sweep.SweepRecord{SweepID: "sw", EgressProxies: []deploy.SweepTarget{want}})
	if err != nil {
		t.Fatal(err)
	}
	var state SweepRecord
	if err := attributevalue.UnmarshalMap(item, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.EgressProxies) != 1 {
		t.Fatalf("targets %+v", state.EgressProxies)
	}
	got := state.EgressProxies[0]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
	// And this function's save keeps them.
	again, err := attributevalue.MarshalMap(&state)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again["egress_proxies"]; !ok {
		t.Error("the orchestrator's save drops egress_proxies")
	}
}
