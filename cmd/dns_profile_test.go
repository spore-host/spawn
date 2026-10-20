package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The two identities in these tests: --profile names one, the environment holds
// the other. A request signed with the second ignored --profile (#967).
const (
	profileKey = "TESTPROFILEOTHERKEY1"
	ambientKey = "TESTAMBIENTCHAINKEY1"
)

var sigV4Credential = regexp.MustCompile(`Credential=([A-Z0-9]+)/`)

// signedCall is one request as a recorder saw it: who signed it, and what it was.
type signedCall struct {
	key, target, action string
	tagKeys             []string // DeleteTags only
}

type signingRecorder struct {
	mu    sync.Mutex
	calls []signedCall
	// denySSM answers every SSM GetParameter with AccessDeniedException instead
	// of passing it to Substrate, which has no way to deny one.
	denySSM bool
}

func (r *signingRecorder) record(req *http.Request, body []byte) {
	c := signedCall{}
	if m := sigV4Credential.FindStringSubmatch(req.Header.Get("Authorization")); m != nil {
		c.key = m[1]
	}
	c.target = req.Header.Get("X-Amz-Target")
	if form, err := url.ParseQuery(string(body)); err == nil {
		c.action = form.Get("Action")
		if c.action == "DeleteTags" {
			for k, v := range form {
				if strings.HasPrefix(k, "Tag.") && strings.HasSuffix(k, ".Key") {
					c.tagKeys = append(c.tagKeys, v...)
				}
			}
			sort.Strings(c.tagKeys)
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *signingRecorder) snapshot() []signedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]signedCall(nil), r.calls...)
}

// profileOverSubstrate sets up the two identities and routes every AWS call to
// Substrate through a recorder: --profile other has its own keys in a temp
// AWS config file, and the environment has different ones.
func profileOverSubstrate(t *testing.T) (*testutil.TestEnv, *signingRecorder) {
	t.Helper()
	dir := isolateAWSEnv(t)
	env := testutil.SubstrateServer(t)
	target, _ := url.Parse(env.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	rec := &signingRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec.record(req, body)
		rec.mu.Lock()
		deny := rec.denySSM
		rec.mu.Unlock()
		if deny && req.Header.Get("X-Amz-Target") == "AmazonSSM.GetParameter" {
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"not authorized to perform: ssm:GetParameter"}`)
			return
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	if err := os.WriteFile(filepath.Join(dir, "aws-config"), []byte(
		"[profile other]\naws_access_key_id = "+profileKey+"\naws_secret_access_key = secret\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", ambientKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	for _, k := range []string{"SPAWN_DNS_DOMAIN", "SPAWN_DNS_API_ENDPOINT", "SPORE_ENV"} {
		t.Setenv(k, "")
	}
	return env, rec
}

// fakeUpdater serves the DNS updater locally, recording who signed each request.
// The command's own DNS client is used; only its transport is redirected here,
// because a custom spawn:dns-endpoint must be a real Lambda Function URL.
func fakeUpdater(t *testing.T) *signingRecorder {
	t.Helper()
	rec := &signingRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec.record(req, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dns.DNSUpdateResponse{Success: true, ChangeID: "C-TEST"})
	}))
	t.Cleanup(srv.Close)
	to, _ := url.Parse(srv.URL)
	real := newDNSCommandClient
	newDNSCommandClient = func(cfg sdkaws.Config, domain, endpoint string) *dns.Client {
		c := real(cfg, domain, endpoint)
		c.SetHTTPClient(&http.Client{Transport: rewriteHost{to: to}})
		return c
	}
	t.Cleanup(func() { newDNSCommandClient = real })
	return rec
}

type rewriteHost struct{ to *url.URL }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

// #967 and #1159, through the real root command: `spawn dns delete --profile
// other` must sign the updater request as `other`, not as the ambient chain,
// and must remove every spawn:dns-* tag — the name, the zone and updater the
// launch wrote, and spored's report — not just blank the name.
func TestDNSDeleteSignsWithTheProfileAndRemovesTheDNSTags(t *testing.T) {
	env, aws := profileOverSubstrate(t)
	updater := fakeUpdater(t)
	saved := dnsDeleteYes
	t.Cleanup(func() { dnsDeleteYes = saved; dnsDomain, dnsAPIEndpoint = "", "" })

	dnsTags := map[string]string{
		"spawn:dns-name":     "web",
		"spawn:dns-domain":   "spore.example.edu",
		"spawn:dns-endpoint": "https://abc123.lambda-url.us-east-1.on.aws/",
		"spawn:dns-status":   "registered",
	}
	id := seedInstance(t, ec2.NewFromConfig(env.AWSConfig), mergeTags(dnsTags, map[string]string{"Name": "web", "Project": "keep-me"}))

	if err := runRootForTest(t, "dns", "delete", id, "--yes", "--profile", "other"); err != nil {
		t.Fatalf("spawn dns delete: %v", err)
	}

	calls := updater.snapshot()
	if len(calls) != 1 {
		t.Fatalf("updater saw %d requests, want 1 DELETE: %+v", len(calls), calls)
	}
	if calls[0].key != profileKey {
		t.Errorf("the DNS request was signed by %q, want --profile other's %s (ambient is %s)", calls[0].key, profileKey, ambientKey)
	}

	var deleted []string
	for _, c := range aws.snapshot() {
		if c.key != "" && c.key != profileKey {
			t.Errorf("an AWS call (%s%s) was signed by %q, not --profile other", c.action, c.target, c.key)
		}
		if c.action == "DeleteTags" {
			deleted = append(deleted, c.tagKeys...)
		}
	}
	sort.Strings(deleted)
	want := dnsTagKeys(dnsTags)
	if strings.Join(deleted, ",") != strings.Join(want, ",") {
		t.Errorf("dns delete removed tags %v, want exactly %v", deleted, want)
	}

	// And on the instance: no spawn:dns-* tag is left, blank or not, and the
	// others are untouched.
	out, err := ec2.NewFromConfig(env.AWSConfig).DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		t.Fatalf("describe %s: %v", id, err)
	}
	left := map[string]bool{}
	for _, tag := range out.Reservations[0].Instances[0].Tags {
		k := sdkaws.ToString(tag.Key)
		left[k] = true
		if strings.HasPrefix(k, "spawn:dns-") {
			t.Errorf("tag %s=%q is still on the instance after dns delete", k, sdkaws.ToString(tag.Value))
		}
	}
	if !left["Project"] || !left["spawn:managed"] {
		t.Errorf("dns delete removed a non-DNS tag; left: %v", left)
	}
}

// #1121 on the dns commands, combined with #967: with nothing naming the zone
// but the account's SSM setting, an AccessDenied reading it (as --profile other)
// refuses `spawn dns delete`, naming the parameter and this command's own flag.
// Nothing is sent to an updater and no tag is touched.
func TestDNSDeleteRefusesWhenTheAccountsDNSSettingIsUnreadable(t *testing.T) {
	env, aws := profileOverSubstrate(t)
	updater := fakeUpdater(t)
	t.Cleanup(func() { dnsDomain, dnsAPIEndpoint = "", "" })
	id := seedInstance(t, ec2.NewFromConfig(env.AWSConfig), map[string]string{"spawn:dns-name": "web", "spawn:dns-status": "registered"})
	aws.mu.Lock()
	aws.denySSM = true
	aws.mu.Unlock()

	err := runRootForTest(t, "dns", "delete", id, "--yes", "--profile", "other")
	if err == nil {
		t.Fatal("an AccessDenied on /spawn/dns/domain did not refuse dns delete; it would act on spore.host")
	}
	for _, want := range []string{"/spawn/dns/domain", "ssm:GetParameter", "--domain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if n := len(updater.snapshot()); n != 0 {
		t.Errorf("the updater got %d requests after the refusal", n)
	}
	var sawSSM bool
	for _, c := range aws.snapshot() {
		if c.action == "DeleteTags" || c.action == "CreateTags" {
			t.Errorf("a refused dns delete still sent %s", c.action)
		}
		if c.target == "AmazonSSM.GetParameter" {
			sawSSM = true
			if c.key != profileKey {
				t.Errorf("the SSM read was signed by %q, want --profile other's %s", c.key, profileKey)
			}
		}
	}
	if !sawSSM {
		t.Error("dns delete never read the account's SSM setting")
	}
}

// #967: `spawn cost <sweep> --profile other` reads the sweep table AND prices its
// instances as `other`. The pricer used to load the ambient chain itself, so
// with only --profile's credentials usable compute came back unpriced.
func TestCostPricesWithTheProfile(t *testing.T) {
	env, aws := profileOverSubstrate(t)
	ctx := context.Background()
	db := dynamodb.NewFromConfig(env.AWSConfig)
	if _, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            sdkaws.String("spawn-sweep-orchestration"),
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: sdkaws.String("sweep_id"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: sdkaws.String("sweep_id"), KeyType: ddbtypes.KeyTypeHash}},
		BillingMode:          ddbtypes.BillingModePayPerRequest,
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	inst := &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
		"index":       &ddbtypes.AttributeValueMemberN{Value: "0"},
		"region":      &ddbtypes.AttributeValueMemberS{Value: "us-east-1"},
		"instance_id": &ddbtypes.AttributeValueMemberS{Value: "i-0123456789abcdef0"},
		"actual_type": &ddbtypes.AttributeValueMemberS{Value: "m7i.large"},
		"state":       &ddbtypes.AttributeValueMemberS{Value: "terminated"},
		"launched_at": &ddbtypes.AttributeValueMemberS{Value: "2026-10-01T00:00:00Z"},
	}}
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: sdkaws.String("spawn-sweep-orchestration"),
		Item: map[string]ddbtypes.AttributeValue{
			"sweep_id":  &ddbtypes.AttributeValueMemberS{Value: "sw-967"},
			"instances": &ddbtypes.AttributeValueMemberL{Value: []ddbtypes.AttributeValue{inst}},
		},
	}); err != nil {
		t.Fatalf("put sweep: %v", err)
	}
	before := len(aws.snapshot())

	_ = runRootForTest(t, "cost", "sw-967", "--profile", "other")

	var sawGet, sawPricing bool
	for _, c := range aws.snapshot()[before:] {
		if c.key != profileKey {
			t.Errorf("%s%s was signed by %q, want --profile other's %s", c.action, c.target, c.key, profileKey)
		}
		sawGet = sawGet || strings.HasSuffix(c.target, ".GetItem")
		sawPricing = sawPricing || strings.HasPrefix(c.target, "AWSPriceListService.")
	}
	if !sawGet {
		t.Error("spawn cost never read the sweep table through the recorder")
	}
	if !sawPricing {
		t.Error("spawn cost never priced the sweep's instance with the --profile config (the pricer loaded its own)")
	}
}

// #1159: `spawn dns --help` listed list/register/delete with blank descriptions.
// Discovered from the cobra tree: every command spawn registers must describe
// itself in `--help`, after i18n has run as it does at startup.
func TestEveryCommandHasAShortDescription(t *testing.T) {
	ensureI18nInitialized()
	seen := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			seen++
			if strings.TrimSpace(sub.Short) == "" {
				t.Errorf("%q has no Short, so `%s --help` lists it with a blank description", sub.CommandPath(), c.CommandPath())
			}
			walk(sub)
		}
	}
	walk(rootCmd)
	if seen == 0 {
		t.Fatal("discovered no commands under rootCmd: this gate is checking nothing")
	}
	if cmd, _, err := rootCmd.Find([]string{"dns"}); err != nil || !strings.Contains(cmd.Long, "--name") {
		t.Errorf("`spawn dns --help` does not say names are registered from --name by default (#1159):\n%s", cmd.Long)
	}
}

func mergeTags(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
