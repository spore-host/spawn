package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// fakeIAM is a stateful IAM + STS Query API that records every write. It holds
// roles with their inline policies and instance profiles, enough for
// CreateOrGetInstanceProfile to run end to end.
type fakeIAM struct {
	mu       sync.Mutex
	roles    map[string]map[string]string // role -> policy name -> document
	profiles map[string]bool
	puts     []url.Values // every PutRolePolicy form, in order
}

const iamNS = `xmlns="https://iam.amazonaws.com/doc/2010-05-08/"`

func (f *fakeIAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roles == nil {
		f.roles, f.profiles = map[string]map[string]string{}, map[string]bool{}
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<ErrorResponse ` + iamNS + `><Error><Type>Sender</Type><Code>NoSuchEntity</Code><Message>not found</Message></Error><RequestId>r</RequestId></ErrorResponse>`))
	}
	ok := func(action, result string) {
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake IAM Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<` + action + `Response ` + iamNS + `>` + result + `<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></` + action + `Response>`))
	}
	role := func(name string) string {
		return `<Role><Path>/</Path><RoleName>` + name + `</RoleName><RoleId>AROAX</RoleId><Arn>arn:aws:iam::123456789012:role/` + name +
			`</Arn><CreateDate>2026-01-01T00:00:00Z</CreateDate></Role>`
	}
	profile := func(name string) string {
		return `<InstanceProfile><Path>/</Path><InstanceProfileName>` + name + `</InstanceProfileName><InstanceProfileId>AIPAX</InstanceProfileId><Arn>arn:aws:iam::123456789012:instance-profile/` + name +
			`</Arn><CreateDate>2026-01-01T00:00:00Z</CreateDate><Roles></Roles></InstanceProfile>`
	}
	switch a := form.Get("Action"); a {
	case "GetCallerIdentity":
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake STS Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/t</Arn><UserId>AIDAX</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	case "GetRole":
		n := form.Get("RoleName")
		if _, exists := f.roles[n]; !exists {
			notFound()
			return
		}
		// nosemgrep
		ok(a, `<GetRoleResult>`+role(n)+`</GetRoleResult>`)
	case "CreateRole":
		n := form.Get("RoleName")
		if _, exists := f.roles[n]; !exists {
			f.roles[n] = map[string]string{}
		}
		// nosemgrep
		ok(a, `<CreateRoleResult>`+role(n)+`</CreateRoleResult>`)
	case "PutRolePolicy":
		f.puts = append(f.puts, form)
		n := form.Get("RoleName")
		if f.roles[n] == nil {
			notFound()
			return
		}
		f.roles[n][form.Get("PolicyName")] = form.Get("PolicyDocument")
		ok(a, "")
	case "AttachRolePolicy", "AddRoleToInstanceProfile":
		ok(a, "")
	case "GetInstanceProfile":
		n := form.Get("InstanceProfileName")
		if !f.profiles[n] {
			notFound()
			return
		}
		// nosemgrep
		ok(a, `<GetInstanceProfileResult>`+profile(n)+`</GetInstanceProfileResult>`)
	case "CreateInstanceProfile":
		n := form.Get("InstanceProfileName")
		f.profiles[n] = true
		// nosemgrep
		ok(a, `<CreateInstanceProfileResult>`+profile(n)+`</CreateInstanceProfileResult>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<ErrorResponse ` + iamNS + `><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error><RequestId>r</RequestId></ErrorResponse>`))
	}
}

func newFakeIAMClient(t *testing.T, f *fakeIAM) *aws.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return aws.NewClientFromConfig(awssdk.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: awssdk.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
}

// grantedQueues returns the SQS resources a policy document grants.
func grantedQueues(t *testing.T, doc string) []string {
	t.Helper()
	var pol struct {
		Statement []struct {
			Action   interface{} `json:"Action"`
			Resource interface{} `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, doc)
	}
	// IAM allows a string or an array for both fields.
	list := func(v interface{}) []string {
		switch x := v.(type) {
		case string:
			return []string{x}
		case []interface{}:
			var out []string
			for _, e := range x {
				out = append(out, fmt.Sprint(e))
			}
			return out
		}
		return nil
	}
	var out []string
	for _, st := range pol.Statement {
		for _, a := range list(st.Action) {
			if strings.HasPrefix(a, "sqs:") {
				out = append(out, list(st.Resource)...)
				break
			}
		}
	}
	return out
}

// TestConcurrentPoolCreatesKeepTheirOwnGrants is #1147. Two `pool create`s run
// at once, through the real CreateOrGetInstanceProfile against a recording
// IAM. Afterwards each pool's instance profile must still grant ITS queue: with
// one shared role the later PutRolePolicy replaced the earlier, and the first
// pool's workers lost their queue.
func TestConcurrentPoolCreatesKeepTheirOwnGrants(t *testing.T) {
	f := &fakeIAM{}
	c := newFakeIAMClient(t, f)
	const account, region = "123456789012", "us-east-1"
	bucket := poolSpecBucket(account, region)
	runs := []string{"run-a", "run-b"}

	profiles := make([]string, len(runs))
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Add(1)
		go func(i int, run string) {
			defer wg.Done()
			profiles[i], errs[i] = c.CreateOrGetInstanceProfile(context.Background(),
				poolWorkerRoleConfig(account, region, bucket, run, nil, nil, nil))
		}(i, run)
	}
	wg.Wait()

	f.mu.Lock()
	defer f.mu.Unlock()
	for i, run := range runs {
		if errs[i] != nil {
			t.Fatalf("pool create %s: %v", run, errs[i])
		}
		want := fmt.Sprintf("arn:aws:sqs:%s:%s:spawn-pool-%s", region, account, run)
		// The profile a pool's workers launch with carries the role of the same name.
		pols := f.roles[profiles[i]]
		var got []string
		for _, doc := range pols {
			got = append(got, grantedQueues(t, doc)...)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("pool %s's profile %s grants SQS on %v, want only %s (another pool's create overwrote it)",
				run, profiles[i], got, want)
		}
	}
	if profiles[0] == profiles[1] {
		t.Errorf("both pools got instance profile %q", profiles[0])
	}
}

// TestPoolWorkerRoleNameFitsIAM: role names are capped at 64 characters, and a
// run id is not; the name must stay valid and distinct for any run id.
func TestPoolWorkerRoleNameFitsIAM(t *testing.T) {
	long := strings.Repeat("x", 200)
	for _, run := range []string{"a", long, long + "y"} {
		if n := poolWorkerRoleName(run); len(n) > 64 || !instancerole.IsCandidate(n) {
			t.Errorf("poolWorkerRoleName(%d chars) = %q (%d chars)", len(run), n, len(n))
		}
	}
	if poolWorkerRoleName(long) == poolWorkerRoleName(long+"y") {
		t.Error("two run ids share a role name")
	}
}

type fakePoolIdentity struct {
	live    []int // live workers on each successive check; the last repeats
	listErr error
	checks  int
	deleted []string
}

func (f *fakePoolIdentity) LiveInstancesWithTag(_ context.Context, _, key, value string) ([]aws.InstanceInfo, error) {
	f.checks++
	if f.listErr != nil {
		return nil, f.listErr
	}
	n := 0
	if len(f.live) > 0 {
		n = f.live[min(f.checks, len(f.live))-1]
	}
	out := make([]aws.InstanceInfo, n)
	for i := range out {
		out[i].Tags = map[string]string{key: value}
	}
	return out, nil
}

func (f *fakePoolIdentity) DeleteInstanceProfileAndRole(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

// TestDrainRemovesThePoolRoleOnlyOnceNoWorkerNeedsIt: a drained pool leaves no
// IAM behind (#653), but deleting the role under a live worker strips its
// credentials and spored can no longer terminate it, so the role goes only
// when the check finds no live worker, and never when the check itself failed.
func TestDrainRemovesThePoolRoleOnlyOnceNoWorkerNeedsIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		api        *fakePoolIdentity
		wantDelete bool
	}{
		{"no live workers", &fakePoolIdentity{}, true},
		{"workers still live", &fakePoolIdentity{live: []int{2}}, false},
		{"check failed", &fakePoolIdentity{listErr: fmt.Errorf("throttled")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := removePoolWorkerIdentity(context.Background(), tc.api, "us-east-1", "run-a", 0, noSleep)
			if got := slices.Contains(tc.api.deleted, poolWorkerRoleName("run-a")); got != tc.wantDelete {
				t.Errorf("deleted %v, want delete=%v (message %q)", tc.api.deleted, tc.wantDelete, msg)
			}
			if !tc.wantDelete && !strings.Contains(msg, "re-run 'spawn pool drain") {
				t.Errorf("kept role without saying how to remove it: %q", msg)
			}
		})
	}
}

func noSleep(context.Context, time.Duration) bool { return true }

// TestDrainWaitsForWorkersBeforeRemovingTheRole: workers terminate shortly
// after their queue is deleted, and nf-spawn drains only once, so drain
// re-checks within --wait and removes the role once the workers are gone,
// instead of leaving one role behind per run. Counted through the sleep seam.
func TestDrainWaitsForWorkersBeforeRemovingTheRole(t *testing.T) {
	api := &fakePoolIdentity{live: []int{3, 1, 0}}
	sleeps := 0
	// Capped, so a drain that ignores --wait fails the test instead of hanging it.
	sleep := func(context.Context, time.Duration) bool { sleeps++; return sleeps < 100 }
	removePoolWorkerIdentity(context.Background(), api, "us-east-1", "run-a", time.Minute, sleep)
	if !slices.Contains(api.deleted, poolWorkerRoleName("run-a")) || api.checks != 3 || sleeps != 2 {
		t.Errorf("deleted %v after %d checks and %d sleeps, want the role deleted on the 3rd check after 2 sleeps", api.deleted, api.checks, sleeps)
	}

	// Bounded: workers that never go away exhaust --wait and the role is kept.
	api = &fakePoolIdentity{live: []int{1}}
	sleeps = 0
	removePoolWorkerIdentity(context.Background(), api, "us-east-1", "run-a", time.Minute, sleep)
	if len(api.deleted) != 0 {
		t.Errorf("deleted the role of a pool whose workers never terminated")
	}
	if want := int(time.Minute / poolDrainWorkerPoll); sleeps != want {
		t.Errorf("waited %d polls for a 1m --wait at %s per poll, want %d", sleeps, poolDrainWorkerPoll, want)
	}
}

// TestAbandonedPoolRoleHasADeleter (#1319): `spawn pool drain` is not the only
// deleter of a pool's role. The role pool create asks for, tagged as
// CreateOrGetInstanceProfile tags it, is one pkg/instancerole's default rule
// (the TTL reaper's) deletes once no worker holds it and it has been unused
// for the grace, and keeps while a worker holds it.
func TestAbandonedPoolRoleHasADeleter(t *testing.T) {
	cfg := poolWorkerRoleConfig("123456789012", "us-east-1", "spawn-pool-specs", "nf-run-1", nil, nil, nil)
	tags := map[string]string{instancerole.ManagedTagKey: instancerole.ManagedTagValue}
	for k, v := range cfg.Tags {
		tags[k] = v
	}
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	old := now.Add(-2 * instancerole.Grace)
	f := instancerole.Facts{
		Name: cfg.RoleName, RoleTags: tags,
		ProfileExists: true, ProfileTags: tags, ProfileCreated: old,
		Created: old, LastUsed: old,
	}
	if ok, why := instancerole.Decide(f, now); !ok {
		t.Errorf("an undrained pool's role %s is kept forever: %s", cfg.RoleName, why)
	}
	f.InUse = true
	if ok, _ := instancerole.Decide(f, now); ok {
		t.Errorf("the role of a pool with a live worker would be deleted")
	}
}

// TestDrainAlsoRemovesThePrePoolRenameRole: a pool created before #1319 has its
// role under spawn-pool-worker-<hash>; draining it with this version removes
// that one too, and a failure to (a principal never granted the old name) is
// a note, not a failed removal of the current role.
func TestDrainAlsoRemovesThePrePoolRenameRole(t *testing.T) {
	api := &fakePoolIdentity{}
	msg := removePoolWorkerIdentity(context.Background(), api, "us-east-1", "run-a", 0, noSleep)
	for _, want := range []string{poolWorkerRoleName("run-a"), instancerole.LegacyPoolWorkerRoleName("run-a")} {
		if !slices.Contains(api.deleted, want) {
			t.Errorf("drain did not delete %s (deleted %v)", want, api.deleted)
		}
	}
	if strings.Contains(msg, "⚠️") {
		t.Errorf("clean drain warned: %q", msg)
	}
}
