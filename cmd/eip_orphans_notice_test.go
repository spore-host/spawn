package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

// fakeEIPAddr is one Elastic IP the fake EC2 serves.
type fakeEIPAddr struct {
	alloc, instanceTag, owner string
	assocInstance             string // non-empty: associated with this instance
	userOwned                 bool   // spawn:managed only, no instance tag
}

// fakeEIPEC2 is a loopback EC2 + STS: DescribeAddresses from addrs,
// DescribeInstances by instance-id filter from states (an absent ID is one EC2
// no longer knows), the listing DescribeInstances from listed, ReleaseAddress
// recorded. Everything else is refused, as an unpermitted call would be.
type fakeEIPEC2 struct {
	mu        sync.Mutex
	addrs     []fakeEIPAddr
	states    map[string]string
	listed    map[string]string // instance-id -> state, returned by a tag-filtered listing
	denyAddrs bool
	released  []string
	addrCalls []string // region of each DescribeAddresses
}

const fakeCallerARN = "arn:aws:iam::123456789012:user/test"

func (f *fakeEIPEC2) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	reply := func(xml string) { _, _ = io.WriteString(w, xml) } // nosemgrep
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	switch form.Get("Action") {
	case "GetCallerIdentity":
		reply(fmt.Sprintf(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><UserId>AIDATEST</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>t</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, fakeCallerARN))
	case "DescribeAddresses":
		f.addrCalls = append(f.addrCalls, signedRegion(req))
		if f.denyAddrs {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not authorized to perform: ec2:DescribeAddresses</Message></Error></Errors><RequestID>t</RequestID></Response>`)
			return
		}
		var b strings.Builder
		for _, a := range f.addrs {
			b.WriteString("<item><publicIp>198.51.100.7</publicIp><domain>vpc</domain><allocationId>" + a.alloc + "</allocationId>")
			if a.assocInstance != "" {
				b.WriteString("<associationId>eipassoc-" + a.alloc + "</associationId><instanceId>" + a.assocInstance + "</instanceId><networkInterfaceId>eni-1</networkInterfaceId>")
			}
			b.WriteString("<tagSet><item><key>spawn:managed</key><value>true</value></item>")
			if !a.userOwned {
				b.WriteString("<item><key>spawn:instance-id</key><value>" + a.instanceTag + "</value></item>")
				b.WriteString("<item><key>spawn:purpose</key><value>efa-public-ip</value></item>")
			}
			if a.owner != "" {
				b.WriteString("<item><key>spawn:iam-user</key><value>" + a.owner + "</value></item>")
			}
			b.WriteString("</tagSet></item>")
		}
		reply(fmt.Sprintf(`<DescribeAddressesResponse %s><requestId>t</requestId><addressesSet>%s</addressesSet></DescribeAddressesResponse>`, ns, b.String()))
	case "DescribeInstances":
		byID := map[string]string{}
		filtered := false
		for k, v := range form {
			if strings.HasPrefix(k, "Filter.") && strings.HasSuffix(k, ".Name") && v[0] == "instance-id" {
				filtered = true
				prefix := strings.TrimSuffix(k, "Name")
				for k2, v2 := range form {
					if strings.HasPrefix(k2, prefix+"Value.") {
						if st, ok := f.states[v2[0]]; ok {
							byID[v2[0]] = st
						}
					}
				}
			}
		}
		if !filtered {
			byID = f.listed
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var b strings.Builder
		for _, id := range ids {
			// nosemgrep
			fmt.Fprintf(&b, `<item><instanceId>%s</instanceId><instanceType>c8gn.48xlarge</instanceType><instanceState><code>80</code><name>%s</name></instanceState><placement><availabilityZone>us-west-2c</availabilityZone></placement><launchTime>2026-10-20T00:00:00.000Z</launchTime><tagSet><item><key>spawn:managed</key><value>true</value></item><item><key>Name</key><value>%s</value></item></tagSet></item>`, id, byID[id], id)
		}
		reply(fmt.Sprintf(`<DescribeInstancesResponse %s><requestId>t</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet>%s</instancesSet></item></reservationSet></DescribeInstancesResponse>`, ns, b.String()))
	case "ReleaseAddress":
		f.released = append(f.released, form.Get("AllocationId"))
		reply(fmt.Sprintf(`<ReleaseAddressResponse %s><requestId>t</requestId><return>true</return></ReleaseAddressResponse>`, ns))
	default:
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not in this fake</Message></Error></Errors><RequestID>t</RequestID></Response>`)
	}
}

// signedRegion reads the region out of the SigV4 credential scope.
func signedRegion(req *http.Request) string {
	m := regexp.MustCompile(`Credential=[^/]+/\d+/([^/]+)/`).FindStringSubmatch(req.Header.Get("Authorization"))
	if m == nil {
		return ""
	}
	return m[1]
}

// serveFakeEIPEC2 points every AWS client the command builds at f.
func serveFakeEIPEC2(t *testing.T, f *fakeEIPEC2) {
	t.Helper()
	dir := isolateAWSEnv(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// runSpawnForTest runs the real root command and returns its stderr (where the
// orphan warning goes), discarding stdout, then resets every flag it may set.
func runSpawnForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		spawnRegion, spawnProfile, spawnAccount = "", "", ""
		spawnconfig.SetSharedFlags("", "", "")
		listRegion, listRegions, listState = "", nil, ""
		cleanupRegion, cleanupAll, cleanupYes, cleanupDryRun, cleanupElasticIPs, cleanupAllRegions = "", false, false, false, false, false
		for _, fs := range []*pflag.FlagSet{rootCmd.PersistentFlags(), listCmd.Flags(), cleanupCmd.Flags(), statusCmd.Flags()} {
			fs.VisitAll(func(fl *pflag.Flag) { fl.Changed = false })
		}
		rootCmd.SetArgs(nil)
	})
	var stderr bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(&stderr)
	var err error
	_ = captureStdout(t, func() { err = rootCmd.ExecuteContext(context.Background()) })
	return stderr.String(), err
}

func orphanFixture() *fakeEIPEC2 {
	return &fakeEIPEC2{
		addrs: []fakeEIPAddr{
			{alloc: "eipalloc-gone", instanceTag: "i-gone", owner: fakeCallerARN},
			{alloc: "eipalloc-terminated", instanceTag: "i-term", owner: fakeCallerARN},
			{alloc: "eipalloc-associated", instanceTag: "i-live", assocInstance: "i-live", owner: fakeCallerARN},
			{alloc: "eipalloc-user", userOwned: true},
			{alloc: "eipalloc-running", instanceTag: "i-run", owner: fakeCallerARN},
			{alloc: "eipalloc-stopped", instanceTag: "i-stop", owner: fakeCallerARN},
		},
		states: map[string]string{"i-term": "terminated", "i-live": "running", "i-run": "running", "i-stop": "stopped"},
		listed: map[string]string{"i-stop": "stopped"},
	}
}

var cleanupCommandRE = regexp.MustCompile(`(?m)Release them: (spawn cleanup [^\n]*)$`)

// assertOrphanWarning checks the warning names exactly the two orphans, the
// region, the cost, and a cleanup command, and returns that command's args.
func assertOrphanWarning(t *testing.T, stderr string) []string {
	t.Helper()
	if !strings.Contains(stderr, "2 orphaned spawn Elastic IP(s) in us-west-2: eipalloc-gone, eipalloc-terminated") {
		t.Fatalf("no warning naming exactly the two orphans in us-west-2; stderr:\n%s", stderr)
	}
	for _, notOrphan := range []string{"eipalloc-associated", "eipalloc-user", "eipalloc-running", "eipalloc-stopped"} {
		if strings.Contains(stderr, notOrphan) {
			t.Errorf("warning names %s, which is not an orphan", notOrphan)
		}
	}
	if !strings.Contains(stderr, "bills") || !strings.Contains(stderr, "Elastic IP limit") {
		t.Errorf("warning does not say the addresses bill and count toward the limit:\n%s", stderr)
	}
	m := cleanupCommandRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("warning prints no cleanup command:\n%s", stderr)
	}
	return strings.Fields(m[1])[1:] // drop "spawn"
}

// #1232: `spawn list` warns about spawn-tagged Elastic IPs left behind by an
// instance that is gone, and the command it prints releases exactly those. A
// stopped spawn instance is present, which a full `spawn cleanup` would
// terminate; the printed command must touch only the two addresses. Run it
// against the same account and check.
func TestListWarnsAboutOrphanedElasticIPsAndItsCommandReleasesThem(t *testing.T) {
	f := orphanFixture()
	serveFakeEIPEC2(t, f)

	stderr, err := runSpawnForTest(t, "list", "--region", "us-west-2")
	if err != nil {
		t.Fatalf("spawn list: %v\n%s", err, stderr)
	}
	args := assertOrphanWarning(t, stderr)
	if want := []string{"cleanup", "--elastic-ips", "--region", "us-west-2"}; strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("printed command %v, want %v", args, want)
	}
	if len(f.addrCalls) != 1 || f.addrCalls[0] != "us-west-2" {
		t.Errorf("DescribeAddresses calls by region = %v, want exactly one in us-west-2", f.addrCalls)
	}

	// The printed command, confirmed, releases the two orphans and nothing else.
	out, err := runSpawnForTest(t, append(args, "--yes")...)
	if err != nil {
		t.Fatalf("spawn %s --yes: %v\n%s", strings.Join(args, " "), err, out)
	}
	sort.Strings(f.released)
	if got := strings.Join(f.released, ","); got != "eipalloc-gone,eipalloc-terminated" {
		t.Errorf("released %q, want exactly eipalloc-gone,eipalloc-terminated", got)
	}
}

// An orphan launched by another principal is still worth the warning (it bills
// this account and uses its limit), and the command must widen to --all, since
// cleanup's default scope would skip it.
func TestOrphanWarningAddsAllForAnotherPrincipalsAddress(t *testing.T) {
	f := orphanFixture()
	f.addrs[0].owner = "arn:aws:iam::123456789012:user/someone-else"
	serveFakeEIPEC2(t, f)

	stderr, err := runSpawnForTest(t, "list", "--region", "us-west-2")
	if err != nil {
		t.Fatalf("spawn list: %v", err)
	}
	args := assertOrphanWarning(t, stderr)
	if args[len(args)-1] != "--all" {
		t.Fatalf("printed command %v lacks --all for another principal's address", args)
	}
	if _, err := runSpawnForTest(t, append(args, "--yes")...); err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.released)
	if got := strings.Join(f.released, ","); got != "eipalloc-gone,eipalloc-terminated" {
		t.Errorf("released %q, want both orphans", got)
	}
}

// `spawn status` warns the same way, for the instance's region.
func TestStatusWarnsAboutOrphanedElasticIPs(t *testing.T) {
	f := orphanFixture()
	serveFakeEIPEC2(t, f)

	stderr, err := runSpawnForTest(t, "status", "i-stop", "--region", "us-west-2")
	if err != nil {
		t.Fatalf("spawn status: %v\n%s", err, stderr)
	}
	args := assertOrphanWarning(t, stderr)
	if strings.Join(args, " ") != "cleanup --elastic-ips --region us-west-2" {
		t.Errorf("printed command %v", args)
	}
}

// Nothing orphaned (only associated, user-owned and live-instance addresses):
// no warning at all.
func TestNoOrphanNoWarning(t *testing.T) {
	f := orphanFixture()
	f.addrs = f.addrs[2:]
	serveFakeEIPEC2(t, f)
	for _, args := range [][]string{{"list", "--region", "us-west-2"}, {"status", "i-stop", "--region", "us-west-2"}} {
		stderr, err := runSpawnForTest(t, args...)
		if err != nil {
			t.Fatalf("spawn %v: %v", args, err)
		}
		if strings.Contains(stderr, "orphaned spawn Elastic IP") {
			t.Errorf("spawn %v warned with no orphan present:\n%s", args, stderr)
		}
	}
}

// A refused DescribeAddresses (the IAM baseline may lack it) must not fail
// list or status: the warning is best-effort and says nothing.
func TestDescribeAddressesErrorDoesNotFailListOrStatus(t *testing.T) {
	f := orphanFixture()
	f.denyAddrs = true
	serveFakeEIPEC2(t, f)
	for _, args := range [][]string{{"list", "--region", "us-west-2"}, {"status", "i-stop", "--region", "us-west-2"}} {
		stderr, err := runSpawnForTest(t, args...)
		if err != nil {
			t.Errorf("spawn %v failed on a refused DescribeAddresses: %v", args, err)
		}
		if strings.Contains(stderr, "orphaned spawn Elastic IP") || strings.Contains(stderr, "DescribeAddresses") {
			t.Errorf("spawn %v printed something for a refused lookup:\n%s", args, stderr)
		}
		if len(f.addrCalls) == 0 {
			t.Errorf("spawn %v never asked for addresses, so this proves nothing", args)
		}
		f.addrCalls = nil
	}
}

// One region's failure must not hide another region's orphans.
func TestCollectOrphanedAddressesSkipsAFailedRegion(t *testing.T) {
	find := func(_ context.Context, region string) ([]aws.ManagedResource, error) {
		if region == "us-east-1" {
			return nil, errors.New("UnauthorizedOperation")
		}
		return []aws.ManagedResource{{ID: "eipalloc-b", Region: region}}, nil
	}
	got := collectOrphanedAddresses(context.Background(), []string{"us-east-1", "us-west-2"}, find)
	if len(got) != 1 || got[0].region != "us-west-2" {
		t.Fatalf("got %+v, want us-west-2's orphan despite us-east-1 failing", got)
	}
}

// The list of IDs is capped; the count is not.
func TestOrphanedAddressNoticeCapsTheIDs(t *testing.T) {
	var addrs []aws.ManagedResource
	for i := range 7 {
		addrs = append(addrs, aws.ManagedResource{ID: fmt.Sprintf("eipalloc-%d", i)})
	}
	got := orphanedAddressNotice([]regionOrphans{{region: "us-west-2", addrs: addrs}}, fakeCallerARN)
	if !strings.Contains(got, "7 orphaned") || !strings.Contains(got, "eipalloc-4 (+2 more)") || strings.Contains(got, "eipalloc-5") {
		t.Errorf("notice = %q", got)
	}
}
