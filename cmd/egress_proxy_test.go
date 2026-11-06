package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
)

// `spawn egress-proxy status | delete | upgrade` (#1515 phase 5), CLI side.
// The AWS work is pkg/egressproxy/deploy's and is tested there against a fake
// EC2; these drive the commands through their seams.

var epNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// egressProxyFakes are the seams' recordings.
type egressProxyFakes struct {
	inventory  []deploy.VPCInventory
	deletes    []deploy.DeletePlan
	deleteRes  deploy.DeleteResult
	deleteErr  error
	upgrades   []deploy.UpgradePlan
	requests   []deploy.Request
	keepOld    []bool
	upgradeRes deploy.UpgradeResult
	upgradeErr error
	prompts    int
	answer     bool
	profiles   int
}

func stubEgressProxyCommands(t *testing.T, inv []deploy.VPCInventory) *egressProxyFakes {
	t.Helper()
	withLogDir(t, false)
	f := &egressProxyFakes{inventory: inv, answer: true}
	prevEnv, prevInv, prevDel, prevUp, prevConfirm := newEgressProxyEnv, egressProxyInventory, egressProxyDelete, egressProxyUpgrade, egressProxyConfirm
	prevGlobals := []any{egressProxyStatusAllRegions, egressProxyStatusVPC, egressProxyDeleteVPC, egressProxyDeleteAll,
		egressProxyDeleteYes, egressProxyDeleteDryRun, egressProxyUpgradeVPC, egressProxyUpgradeYes, egressProxyUpgradeKeepOld, spawnOutputFormat}
	newEgressProxyEnv = func(context.Context) (*egressProxyEnv, error) {
		return &egressProxyEnv{
			regions: func(_ context.Context, all bool) ([]string, error) {
				if all {
					return []string{"us-east-1", "us-west-2"}, nil
				}
				return []string{"us-west-2"}, nil
			},
			client: func(string) deploy.Client {
				return deploy.Client{Profile: func(context.Context) (string, error) {
					f.profiles++
					return aws.SporedInstanceProfileName, nil
				}}
			},
			network: func(context.Context, string, string) (aws.PrivateNetwork, error) {
				return aws.PrivateNetwork{VPCID: "vpc-1", PublicSubnets: []aws.PublicSubnet{{SubnetID: "subnet-pub", AvailabilityZone: "us-west-2a"}}}, nil
			},
			audit: newAuditLogger("123456789012", "test"),
			now:   func() time.Time { return epNow },
		}, nil
	}
	egressProxyInventory = func(_ context.Context, _ deploy.EC2, vpc string, w deploy.Want) ([]deploy.VPCInventory, error) {
		if w.Version != version() || w.InstanceProfile != aws.SporedInstanceProfileName {
			t.Errorf("judged against %+v, want this CLI's version and the spored profile", w)
		}
		var out []deploy.VPCInventory
		for _, v := range f.inventory {
			if vpc == "" || v.VPCID == vpc {
				out = append(out, v)
			}
		}
		return out, nil
	}
	egressProxyDelete = func(_ context.Context, _ deploy.Deps, p deploy.DeletePlan, _ time.Duration) (deploy.DeleteResult, error) {
		f.deletes = append(f.deletes, p)
		return f.deleteRes, f.deleteErr
	}
	egressProxyUpgrade = func(_ context.Context, _ deploy.Deps, r deploy.Request, p deploy.UpgradePlan, keep bool) (deploy.UpgradeResult, error) {
		f.upgrades, f.requests, f.keepOld = append(f.upgrades, p), append(f.requests, r), append(f.keepOld, keep)
		return f.upgradeRes, f.upgradeErr
	}
	egressProxyConfirm = func(skip bool, _ string) bool {
		if skip {
			return true
		}
		f.prompts++
		return f.answer
	}
	t.Cleanup(func() {
		newEgressProxyEnv, egressProxyInventory, egressProxyDelete, egressProxyUpgrade, egressProxyConfirm = prevEnv, prevInv, prevDel, prevUp, prevConfirm
		egressProxyStatusAllRegions, egressProxyStatusVPC = prevGlobals[0].(bool), prevGlobals[1].(string)
		egressProxyDeleteVPC, egressProxyDeleteAll = prevGlobals[2].(string), prevGlobals[3].(bool)
		egressProxyDeleteYes, egressProxyDeleteDryRun = prevGlobals[4].(bool), prevGlobals[5].(bool)
		egressProxyUpgradeVPC, egressProxyUpgradeYes, egressProxyUpgradeKeepOld = prevGlobals[6].(string), prevGlobals[7].(bool), prevGlobals[8].(bool)
		spawnOutputFormat = prevGlobals[9].(string)
	})
	return f
}

func runEP(t *testing.T, run func(*cobra.Command, []string) error) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetContext(context.Background())
	err := run(c, nil)
	return out.String(), err
}

func groupsState(vpc string, clientUsers ...string) deploy.GroupsState {
	return deploy.GroupsState{
		Proxy:   deploy.GroupState{Name: deploy.ProxyGroupName(vpc), ID: "sg-proxy", Users: []string{"i-new"}, ENIs: 1},
		Clients: deploy.GroupState{Name: deploy.ClientsGroupName(vpc), ID: "sg-clients", Users: clientUsers, ENIs: 2 * len(clientUsers)},
	}
}

// proxyListed is a proxy as deploy.Inventory reports it.
func proxyListed(id, ver string, launched time.Time, notReused, notSpawns string) deploy.Listed {
	return deploy.Listed{
		Proxy: deploy.Proxy{InstanceID: id, PrivateIP: "10.0.1.5", SubnetID: "subnet-pub", AvailabilityZone: "us-west-2a",
			Version: ver, State: "running", LaunchTime: launched, Deadline: launched.Add(egressproxy.MaxLife)},
		PublicIP: "54.0.0.1", NotReused: notReused, NotSpawns: notSpawns,
	}
}

func statusInventory() []deploy.VPCInventory {
	cur := proxyListed("i-new", version(), epNow.Add(-time.Hour), "", "")
	cur.LastActivity, cur.Idle = epNow.Add(-3*time.Hour), 72*time.Hour
	old := proxyListed("i-old", "0.157.0", epNow.Add(-5*24*time.Hour), "it was launched by spawn 0.157.0, not "+version(), "")
	return []deploy.VPCInventory{{VPCID: "vpc-1", Groups: groupsState("vpc-1", "i-m1"), Proxies: []deploy.Listed{cur, old}}}
}

// Text: each proxy's address, version, deadline, activity and reuse verdict,
// and each group with how many interfaces use it; a proxy that predates the
// activity tags says so instead of failing.
func TestEgressProxyStatusText(t *testing.T) {
	stubEgressProxyCommands(t, statusInventory())
	spawnOutputFormat = outputFormatTable
	out, err := runEP(t, runEgressProxyStatus)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Egress proxies in us-west-2",
		"i-new  running  10.0.1.5:3128  public 54.0.0.1",
		"version   " + version() + " (matches this spawn)",
		"deadline  " + epNow.Add(-time.Hour).Add(egressproxy.MaxLife).Format(time.RFC3339) + " (5d23h left)",
		"activity  last at or after 2026-11-03T09:00:00Z (tag 3h old; spored updates it at most every 10m in steady use)",
		"expires   no earlier than 2026-11-06T09:00:00Z, in 2d21h (idle period 3d)",
		"reuse     yes",
		"version   0.157.0 (not this spawn's " + version() + ")",
		"activity  unknown (proxy predates spawn v" + egressproxy.ActivityTagsSince + ")",
		"reuse     no: it was launched by spawn 0.157.0",
		"group spawn-egress-proxy-vpc-1 (sg-proxy): on 1 network interface(s) of i-new",
		"group spawn-egress-clients-vpc-1 (sg-clients): on 2 network interface(s) of i-m1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

// -o json: stable snake_case keys. Adding a key is fine; renaming or removing
// one breaks a script, so the set is pinned here.
func TestEgressProxyStatusJSON(t *testing.T) {
	stubEgressProxyCommands(t, statusInventory())
	spawnOutputFormat = outputFormatJSON
	out, err := runEP(t, runEgressProxyStatus)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	keys := func(m any) string {
		var ks []string
		for k := range m.(map[string]any) {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	vpc := doc["vpcs"].([]any)[0]
	proxies := vpc.(map[string]any)["proxies"].([]any)
	for name, tc := range map[string]struct {
		m    any
		want string
	}{
		"top":      {doc, "cli_version,errors,generated_at,regions,vpcs"},
		"vpc":      {vpc, "clients_group,proxies,proxy_group,region,vpc_id"},
		"group":    {vpc.(map[string]any)["clients_group"], "eni_count,exists,id,instances,name"},
		"proxy":    {proxies[0], "activity,deadline,instance_id,launch_time,port,private_ip,public_ip,reusable,spawns_proxy,state,time_left_seconds,version,version_matches"},
		"activity": {proxies[0].(map[string]any)["activity"], "expires_at,expires_at_is_lower_bound,idle_expires_at,idle_period_seconds,idle_seconds,known,last_activity,publish_interval_seconds"},
		"unknown":  {proxies[1].(map[string]any)["activity"], "expires_at,expires_at_is_lower_bound,idle_expires_at,idle_period_seconds,idle_seconds,known,last_activity,note,publish_interval_seconds"},
	} {
		if got := keys(tc.m); got != tc.want {
			t.Errorf("%s keys %s, want %s", name, got, tc.want)
		}
	}
	p := proxies[0].(map[string]any)
	if p["reusable"] != true || p["version_matches"] != true || p["port"].(float64) != 3128 {
		t.Errorf("proxy %v", p)
	}
	if a := p["activity"].(map[string]any); a["idle_seconds"].(float64) != 3*3600 || a["idle_period_seconds"].(float64) != 72*3600 ||
		a["expires_at_is_lower_bound"] != true || a["publish_interval_seconds"].(float64) != 600 {
		t.Errorf("activity %v", a)
	}
	if old := proxies[1].(map[string]any); old["reusable"] != false || !strings.Contains(old["not_reused_reason"].(string), "0.157.0") {
		t.Errorf("old proxy %v", old)
	}
}

// #1528: the activity tag can trail the proxy's real activity by up to the
// publish throttle, so each line states the bound it is. The issue's live case:
// at 23:42, the tag still said the launch time 23:36:25 though traffic ran at
// 23:39:58, and status said "expires in 8m"; spored ended the proxy at 23:55.
func TestEgressProxyActivityLinesStateTheirBounds(t *testing.T) {
	at := func(hms string) *time.Time {
		v, err := time.Parse(time.DateTime, "2026-11-04 "+hms)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	sec := func(d time.Duration) *int64 { return secondsPtr(d) }
	now := *at("23:42:25")
	idle := egressProxyActivity{Known: true, LastActivity: at("23:36:25"), IdleSeconds: sec(6 * time.Minute),
		IdlePeriodSeconds: sec(15 * time.Minute), IdleExpiresAt: at("23:51:25"), ExpiresAt: at("23:51:25"),
		ExpiresAtIsLowerBound: true, PublishIntervalSeconds: sec(egressproxy.ActivityPublishEvery)}
	deadline := idle
	deadline.ExpiresAt, deadline.ExpiresAtIsLowerBound = at("23:45:25"), false
	past := idle
	past.ExpiresAt = at("23:40:25")
	pastDeadline := deadline
	pastDeadline.ExpiresAt = at("23:40:25")
	for name, tc := range map[string]struct {
		a    egressProxyActivity
		want []string
	}{
		"idle expiry": {idle, []string{
			"activity  last at or after 2026-11-04T23:36:25Z (tag 6m old; spored updates it at most every 10m in steady use)",
			"expires   no earlier than 2026-11-04T23:51:25Z, in 9m (idle period 15m)",
		}},
		"deadline first": {deadline, []string{
			"activity  last at or after 2026-11-04T23:36:25Z (tag 6m old; spored updates it at most every 10m in steady use)",
			"expires   at its deadline, in 3m",
		}},
		"idle expiry passed by the tag": {past, []string{
			"activity  last at or after 2026-11-04T23:36:25Z (tag 6m old; spored updates it at most every 10m in steady use)",
			"expires   no earlier than 2026-11-04T23:40:25Z (idle period 15m; by the tag it passed 2m ago, but spored ends it on its own clock, from its real last activity)",
		}},
		"deadline passed": {pastDeadline, []string{
			"activity  last at or after 2026-11-04T23:36:25Z (tag 6m old; spored updates it at most every 10m in steady use)",
			"expires   at its deadline, passed 2m ago",
		}},
	} {
		if got := egressProxyActivityLines(tc.a, now); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// Nothing there is not an error, and says so.
func TestEgressProxyStatusEmpty(t *testing.T) {
	stubEgressProxyCommands(t, nil)
	spawnOutputFormat = outputFormatTable
	egressProxyStatusAllRegions = true
	out, err := runEP(t, runEgressProxyStatus)
	if err != nil || !strings.Contains(out, "No spawn egress proxy or proxy security group in 2 regions.") {
		t.Errorf("out %q, err %v", out, err)
	}
}

func deleteInventory() []deploy.VPCInventory {
	good := proxyListed("i-p", "0.150.0", epNow.Add(-time.Hour), "launched by spawn 0.150.0", "")
	bad := proxyListed("i-forged", version(), epNow.Add(-time.Hour), "x",
		"its security groups are sg-other, sg-proxy, not exactly the proxy group sg-proxy, so spawn cannot tell it launched it")
	return []deploy.VPCInventory{{VPCID: "vpc-1", Groups: groupsState("vpc-1", "i-m1"), Proxies: []deploy.Listed{good, bad}}}
}

// --dry-run prints the plan, prompts for nothing and changes nothing; a
// refusal in the plan still exits non-zero.
func TestEgressProxyDeleteDryRunWritesNothing(t *testing.T) {
	f := stubEgressProxyCommands(t, deleteInventory())
	egressProxyDeleteVPC, egressProxyDeleteDryRun = "vpc-1", true
	out, err := runEP(t, runEgressProxyDelete)
	if len(f.deletes) != 0 || f.prompts != 0 {
		t.Fatalf("dry run: %d deletes, %d prompts", len(f.deletes), f.prompts)
	}
	if !errors.Is(err, errEgressProxyRefused) {
		t.Errorf("err = %v, want the refusal", err)
	}
	for _, want := range []string{"terminate  i-p", "REFUSE     i-forged: its security groups", "delete     spawn-egress-proxy-vpc-1 (sg-proxy)",
		"delete     spawn-egress-clients-vpc-1 (sg-clients) (kept while used by i-m1)", "Dry run: nothing was terminated or deleted."} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}

// A tagged instance that fails the structural checks is never in what is
// terminated, and the command exits non-zero naming it.
func TestEgressProxyDeleteRefusesAStructuralFailure(t *testing.T) {
	f := stubEgressProxyCommands(t, deleteInventory())
	egressProxyDeleteVPC, egressProxyDeleteYes = "vpc-1", true
	f.deleteRes = deploy.DeleteResult{Terminated: []string{"i-p"}, DeletedGroups: []string{"sg-proxy"}}
	_, err := runEP(t, runEgressProxyDelete)
	if !errors.Is(err, errEgressProxyRefused) || exitForError(err) == 0 {
		t.Fatalf("err = %v (exit %d), want a non-zero refusal", err, exitForError(err))
	}
	if len(f.deletes) != 1 {
		t.Fatalf("%d deletes", len(f.deletes))
	}
	p := f.deletes[0]
	if len(p.Terminate) != 1 || p.Terminate[0].InstanceID != "i-p" || len(p.Refused) != 1 || p.Refused[0].InstanceID != "i-forged" {
		t.Errorf("plan %+v: want i-p terminated and i-forged refused", p)
	}
}

// The clients group members still carry is kept, its users named, and the
// reaper named as what collects it.
func TestEgressProxyDeleteKeepsAClientsGroupInUse(t *testing.T) {
	inv := deleteInventory()
	inv[0].Proxies = inv[0].Proxies[:1]
	f := stubEgressProxyCommands(t, inv)
	egressProxyDeleteVPC, egressProxyDeleteYes = "vpc-1", true
	f.deleteRes = deploy.DeleteResult{Terminated: []string{"i-p"}, DeletedGroups: []string{"sg-proxy"},
		Kept: &deploy.GroupInUseError{Name: "spawn-egress-clients-vpc-1", ID: "sg-clients", Users: []string{"i-m1", "i-m2"}}}
	out, err := runEP(t, runEgressProxyDelete)
	if err != nil {
		t.Fatalf("err = %v: keeping an in-use clients group is not a failure", err)
	}
	for _, want := range []string{"terminated i-p", "deleted security group sg-proxy",
		"kept spawn-egress-clients-vpc-1 (sg-clients): still used by i-m1, i-m2", "TTL reaper's managed-network sweep"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// Without --yes it asks; a "no" changes nothing and exits with the abort code.
func TestEgressProxyDeleteDeclined(t *testing.T) {
	inv := deleteInventory()
	inv[0].Proxies = inv[0].Proxies[:1]
	f := stubEgressProxyCommands(t, inv)
	f.answer = false
	egressProxyDeleteVPC = "vpc-1"
	_, err := runEP(t, runEgressProxyDelete)
	if f.prompts != 1 || len(f.deletes) != 0 || exitForError(err) != ExitCodeAborted {
		t.Errorf("%d prompts, %d deletes, err %v", f.prompts, len(f.deletes), err)
	}
}

func TestEgressProxyDeleteFlags(t *testing.T) {
	stubEgressProxyCommands(t, nil)
	for name, set := range map[string]func(){
		"neither":   func() { egressProxyDeleteVPC, egressProxyDeleteAll = "", false },
		"both":      func() { egressProxyDeleteVPC, egressProxyDeleteAll = "vpc-1", true },
		"not a vpc": func() { egressProxyDeleteVPC, egressProxyDeleteAll = "subnet-1", false },
	} {
		set()
		if _, err := runEP(t, runEgressProxyDelete); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	egressProxyDeleteVPC, egressProxyDeleteAll = "vpc-9", false
	if out, err := runEP(t, runEgressProxyDelete); err != nil || !strings.Contains(out, "nothing to delete") {
		t.Errorf("empty VPC: %q, %v", out, err)
	}
}

// A proxy at this version with life left: "already current", and nothing is
// asked, launched or ended.
func TestEgressProxyUpgradeAlreadyCurrent(t *testing.T) {
	f := stubEgressProxyCommands(t, statusInventory())
	egressProxyUpgradeVPC = "vpc-1"
	out, err := runEP(t, runEgressProxyUpgrade)
	if err != nil || !strings.Contains(out, "i-new is already current") {
		t.Fatalf("out %q, err %v", out, err)
	}
	if len(f.upgrades) != 0 || f.prompts != 0 || f.profiles != 0 {
		t.Errorf("%d upgrades, %d prompts, %d profile setups; want none", len(f.upgrades), f.prompts, f.profiles)
	}
}

func upgradeInventory() []deploy.VPCInventory {
	old := proxyListed("i-old", "0.157.0", epNow.Add(-time.Hour), "it was launched by spawn 0.157.0, not "+version(), "")
	old.Idle = 24 * time.Hour
	return []deploy.VPCInventory{{VPCID: "vpc-1", Groups: groupsState("vpc-1"), Proxies: []deploy.Listed{old}}}
}

// An older proxy is replaced: find-or-create at this version, in the old
// proxy's AZ, keeping its idle period, with the mid-bootstrap warning.
func TestEgressProxyUpgradeReplaces(t *testing.T) {
	f := stubEgressProxyCommands(t, upgradeInventory())
	egressProxyUpgradeVPC = "vpc-1"
	f.upgradeRes = deploy.UpgradeResult{New: deploy.Proxy{InstanceID: "i-new", PrivateIP: "10.0.1.9", Version: version(), Created: true,
		Deadline: epNow.Add(egressproxy.MaxLife)}, Terminated: []string{"i-old"}}
	out, err := runEP(t, runEgressProxyUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if f.prompts != 1 || len(f.upgrades) != 1 || f.keepOld[0] {
		t.Fatalf("%d prompts, %d upgrades, keepOld %v", f.prompts, len(f.upgrades), f.keepOld)
	}
	r := f.requests[0]
	if r.VPCID != "vpc-1" || r.Version != version() || r.Idle != 24*time.Hour || r.AvailabilityZone != "us-west-2a" ||
		len(r.PublicSubnets) != 1 || r.InstanceProfile != aws.SporedInstanceProfileName {
		t.Errorf("request %+v", r)
	}
	for _, want := range []string{"terminate i-old (spawn 0.157.0)", "mid-bootstrap through the old proxy",
		"launched egress proxy i-new at 10.0.1.9:3128", "terminated the old proxy i-old"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// --keep-old reaches the upgrade, and the old proxy is reported kept.
func TestEgressProxyUpgradeKeepOld(t *testing.T) {
	f := stubEgressProxyCommands(t, upgradeInventory())
	egressProxyUpgradeVPC, egressProxyUpgradeYes, egressProxyUpgradeKeepOld = "vpc-1", true, true
	f.upgradeRes = deploy.UpgradeResult{New: deploy.Proxy{InstanceID: "i-new", PrivateIP: "10.0.1.9", Created: true}, Kept: []string{"i-old"}}
	out, err := runEP(t, runEgressProxyUpgrade)
	if err != nil || len(f.keepOld) != 1 || !f.keepOld[0] || f.prompts != 0 {
		t.Fatalf("err %v, keepOld %v, prompts %d", err, f.keepOld, f.prompts)
	}
	if !strings.Contains(out, "kept the old proxy i-old") || strings.Contains(out, "mid-bootstrap") {
		t.Errorf("output:\n%s", out)
	}
}

// No proxy: nothing is created. The next opted-in launch makes one.
func TestEgressProxyUpgradeWithNoProxy(t *testing.T) {
	f := stubEgressProxyCommands(t, nil)
	egressProxyUpgradeVPC = "vpc-1"
	out, err := runEP(t, runEgressProxyUpgrade)
	if err != nil || len(f.upgrades) != 0 || !strings.Contains(out, "nothing to upgrade") {
		t.Errorf("out %q, err %v, %d upgrades", out, err, len(f.upgrades))
	}
	if _, err := func() (string, error) { egressProxyUpgradeVPC = ""; return runEP(t, runEgressProxyUpgrade) }(); err == nil {
		t.Error("upgrade without --vpc was accepted")
	}
}

// A failed replacement is a failure, and nothing old is reported ended.
func TestEgressProxyUpgradeFailure(t *testing.T) {
	f := stubEgressProxyCommands(t, upgradeInventory())
	egressProxyUpgradeVPC, egressProxyUpgradeYes = "vpc-1", true
	f.upgradeErr = deploy.ErrNotReady
	out, err := runEP(t, runEgressProxyUpgrade)
	if !errors.Is(err, deploy.ErrNotReady) || strings.Contains(out, "terminated the old proxy") {
		t.Errorf("err %v, out %q", err, out)
	}
}
