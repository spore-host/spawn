package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/progress"
)

// fakeSSMServer is the SSM JSON API's GetParameter, answering each parameter
// with a value or an error code, through the real SDK client — so the
// ParameterNotFound / AccessDenied distinction is tested on what the SDK
// actually deserialises, not on an error value a test made up.
type fakeSSMServer struct {
	mu     sync.Mutex
	values map[string]string // name -> value
	errs   map[string]string // name -> error __type
	def    string            // error __type for any other name; "" = ParameterNotFound
	asked  []string
}

func (f *fakeSSMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, in.Name)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if v, ok := f.values[in.Name]; ok {
		out, _ := json.Marshal(map[string]any{"Parameter": map[string]any{"Name": in.Name, "Value": v, "Type": "String"}})
		// nosemgrep
		_, _ = w.Write(out)
		return
	}
	code := f.errs[in.Name]
	if code == "" {
		code = f.def
	}
	if code == "" {
		code = "ParameterNotFound"
	}
	w.WriteHeader(http.StatusBadRequest)
	out, _ := json.Marshal(map[string]string{"__type": code, "message": code + " for " + in.Name})
	// nosemgrep
	_, _ = w.Write(out)
}

func newFakeSSMClient(t *testing.T, f *fakeSSMServer) *aws.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return aws.NewClientFromConfig(awssdk.Config{
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint:     awssdk.String(srv.URL),
		HTTPClient:       srv.Client(),
		RetryMaxAttempts: 1,
	})
}

// isolateLaunchDNS clears every source of a DNS target but SSM: the flags,
// SPAWN_DNS_*, and ~/.spawn/config.yaml (HOME is a fresh directory).
func isolateLaunchDNS(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "none"))
	for _, k := range []string{"SPAWN_DNS_DOMAIN", "SPAWN_DNS_API_ENDPOINT", "SPAWN_DNS_FUNCTION_ARN", "SPORE_DNS_URL", "SPORE_ENV"} {
		t.Setenv(k, "")
	}
	saved := []string{dnsDomain, dnsAPIEndpoint, dnsFunctionARN, instanceProfile}
	dnsDomain, dnsAPIEndpoint, dnsFunctionARN, instanceProfile = "", "", "", ""
	t.Cleanup(func() {
		dnsDomain, dnsAPIEndpoint, dnsFunctionARN, instanceProfile = saved[0], saved[1], saved[2], saved[3]
	})
}

// #1121, owner decision: an SSM read of /spawn/dns/* that fails with anything
// but ParameterNotFound refuses the launch, naming the parameter and the missing
// permission. The silent fallback to spore.host's updater was the bug.
func TestLaunchRefusedWhenTheAccountsDNSSettingIsUnreadable(t *testing.T) {
	isolateLaunchDNS(t)
	f := &fakeSSMServer{def: "AccessDeniedException"}
	c := newFakeSSMClient(t, f)
	cfg := &aws.LaunchConfig{DNSName: "web", Region: "us-east-1"}
	err := resolveLaunchDNSTarget(context.Background(), c, cfg)
	if err == nil {
		t.Fatalf("an AccessDenied on %s did not refuse the launch; it would register under spore.host (domain=%q endpoint=%q)",
			spawnconfig.SSMDNSDomainPath, cfg.DNSDomain, cfg.DNSEndpoint)
	}
	for _, want := range []string{spawnconfig.SSMDNSDomainPath, "ssm:GetParameter", "AccessDenied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%v", want, err)
		}
	}
	if cfg.DNSDomain != "" || cfg.DNSEndpoint != "" || cfg.DNSFunctionARN != "" {
		t.Errorf("a refused resolution still filled the config: %+v", cfg)
	}

	// The endpoint parameter, the domain having resolved.
	isolateLaunchDNS(t)
	f = &fakeSSMServer{values: map[string]string{spawnconfig.SSMDNSDomainPath: "spore.example.edu"},
		errs: map[string]string{spawnconfig.SSMDNSEndpointPath: "AccessDeniedException"}}
	err = resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), &aws.LaunchConfig{DNSName: "web", Region: "us-east-1"})
	if err == nil || !strings.Contains(err.Error(), spawnconfig.SSMDNSEndpointPath) {
		t.Errorf("AccessDenied on the endpoint parameter: err = %v, want a refusal naming %s", err, spawnconfig.SSMDNSEndpointPath)
	}
}

// ParameterNotFound is "not set", as before: spore.host's target, no tags.
func TestLaunchUsesSporeHostWhenTheDNSParametersDoNotExist(t *testing.T) {
	isolateLaunchDNS(t)
	f := &fakeSSMServer{}
	cfg := &aws.LaunchConfig{DNSName: "web", Region: "us-east-1"}
	if err := resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), cfg); err != nil {
		t.Fatalf("ParameterNotFound refused the launch: %v", err)
	}
	if cfg.DNSDomain != "" || cfg.DNSEndpoint != "" || cfg.DNSFunctionARN != "" {
		t.Errorf("no parameters set, but the config carries a custom target: %+v", cfg)
	}
	if len(f.asked) != 2 {
		t.Errorf("asked SSM for %v; want the domain and endpoint only", f.asked)
	}
}

// #1158: a custom endpoint needs its function's ARN, read through the same chain;
// without one the launch is refused rather than launched into a 403.
func TestLaunchResolvesTheCustomUpdatersFunction(t *testing.T) {
	const ep = "https://abc123.lambda-url.eu-west-1.on.aws/"
	const arn = "arn:aws:lambda:eu-west-1:111122223333:function:spawn-dns-updater"

	isolateLaunchDNS(t)
	f := &fakeSSMServer{values: map[string]string{spawnconfig.SSMDNSEndpointPath: ep, spawnconfig.SSMDNSFunctionARNPath: arn}}
	cfg := &aws.LaunchConfig{DNSName: "web", Region: "eu-west-1"}
	if err := resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DNSEndpoint != ep || cfg.DNSFunctionARN != arn {
		t.Errorf("config = %+v, want endpoint %s and function %s", cfg, ep, arn)
	}

	isolateLaunchDNS(t)
	f = &fakeSSMServer{values: map[string]string{spawnconfig.SSMDNSEndpointPath: ep}}
	err := resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), &aws.LaunchConfig{DNSName: "web", Region: "eu-west-1"})
	if err == nil || !strings.Contains(err.Error(), spawnconfig.SSMDNSFunctionARNPath) || !strings.Contains(err.Error(), "--dns-function-arn") {
		t.Errorf("custom endpoint with no function: err = %v, want a refusal saying how to name it", err)
	}

	// A function in another region than its URL cannot be the one behind it.
	isolateLaunchDNS(t)
	f = &fakeSSMServer{values: map[string]string{spawnconfig.SSMDNSEndpointPath: ep,
		spawnconfig.SSMDNSFunctionARNPath: "arn:aws:lambda:us-east-1:111122223333:function:spawn-dns-updater"}}
	if err := resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), &aws.LaunchConfig{DNSName: "web", Region: "eu-west-1"}); err == nil {
		t.Error("a function ARN in a different region from the endpoint was accepted")
	}

	// spore.host's endpoint, spelled out, needs no ARN: the baseline grants it.
	isolateLaunchDNS(t)
	f = &fakeSSMServer{values: map[string]string{spawnconfig.SSMDNSEndpointPath: spawnconfig.GetDNSEndpointURL()}}
	cfg = &aws.LaunchConfig{DNSName: "web", Region: "us-east-1"}
	if err := resolveLaunchDNSTarget(context.Background(), newFakeSSMClient(t, f), cfg); err != nil || cfg.DNSFunctionARN != "" {
		t.Errorf("spore.host's own endpoint: (%+v, %v), want no function ARN and no error", cfg, err)
	}
}

// invokeGrants returns, per inline policy, the Resources of every statement that
// allows lambda:InvokeFunctionUrl — discovered from the documents, not looked up
// by policy name, so a grant that moves to another policy is still seen.
func invokeGrants(t *testing.T, policies map[string]string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for name, doc := range policies {
		var pol struct {
			Statement []struct {
				Effect   string
				Action   interface{}
				Resource interface{}
			}
		}
		if err := json.Unmarshal([]byte(doc), &pol); err != nil {
			t.Fatalf("policy %s is not JSON: %v", name, err)
		}
		list := func(v interface{}) []string {
			switch x := v.(type) {
			case string:
				return []string{x}
			case []interface{}:
				var s []string
				for _, e := range x {
					s = append(s, fmt.Sprint(e))
				}
				return s
			}
			return nil
		}
		for _, st := range pol.Statement {
			for _, a := range list(st.Action) {
				if st.Effect == "Allow" && (a == "lambda:InvokeFunctionUrl" || a == "lambda:*" || a == "*") {
					out[name] = append(out[name], list(st.Resource)...)
					break
				}
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// #1158: the instance role's lambda:InvokeFunctionUrl grant follows the resolved
// updater. Run through ensureIAMProfile, the launch's own IAM step, on both role
// paths: the shared spored-instance-role (the static sporedDCVRolePolicy) and a
// spawn-managed role from --iam-policy (the dynamic baseline).
//
// The intent, from the issue: the grant is "pinned to spore.host's function",
// so a self-hosted updater answers 403; "derive the grant from the resolved
// dns_endpoint". So: with spore.host's updater, every grant on the role is
// exactly spore.host's function; with a custom one, the role ALSO grants exactly
// that function, and spore.host's baseline grant is untouched — these roles are
// shared, and other launches' instances on them still use spore.host's.
func TestLaunchGrantsTheInstanceRoleInvokeOnTheResolvedUpdater(t *testing.T) {
	const custom = "arn:aws:lambda:eu-west-1:111122223333:function:spawn-dns-updater"
	def := spawnconfig.DefaultDNSFunctionARN

	type path struct {
		name     string
		policies []string // --iam-policy
		role     func(profile string) string
	}
	paths := []path{
		{"shared spored-instance-role", nil, func(string) string { return aws.SporedRoleName }},
		{"spawn-managed --iam-policy role", []string{"s3:ReadOnly"}, func(p string) string { return p }},
	}
	for _, p := range paths {
		for _, fn := range []string{"", custom} {
			t.Run(fmt.Sprintf("%s/function=%q", p.name, fn), func(t *testing.T) {
				isolateLaunchDNS(t)
				savedPol := iamPolicy
				iamPolicy = p.policies
				t.Cleanup(func() { iamPolicy = savedPol })

				f := &fakeIAM{}
				c := newFakeIAMClient(t, f)
				cfg := &aws.LaunchConfig{DNSName: "web"}
				if fn != "" {
					cfg.DNSEndpoint = "https://abc123.lambda-url.eu-west-1.on.aws/"
					cfg.DNSFunctionARN = fn
				}
				if err := ensureIAMProfile(context.Background(), c, cfg, progress.NewQuietProgress(), audit.NewLogger(io.Discard, "", "")); err != nil {
					t.Fatalf("ensureIAMProfile: %v", err)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				role := p.role(cfg.IamInstanceProfile)
				grants := invokeGrants(t, f.roles[role])
				if len(grants) == 0 {
					t.Fatalf("role %s grants lambda:InvokeFunctionUrl on nothing; policies: %v", role, policyNames(f.roles[role]))
				}
				for name, res := range grants {
					want := []string{def}
					if fn != "" && name == aws.DNSInvokePolicyName(fn) {
						want = []string{fn}
					}
					if strings.Join(res, ",") != strings.Join(want, ",") {
						t.Errorf("policy %s on %s grants invoke on %v, want exactly %v", name, role, res, want)
					}
				}
				if fn != "" {
					if _, ok := grants[aws.DNSInvokePolicyName(fn)]; !ok {
						t.Errorf("role %s has no grant on the custom updater %s (policies: %v); spored's requests would be refused with 403", role, fn, policyNames(f.roles[role]))
					}
				}
			})
		}
	}
}

// spawn does not edit a role it did not create: with --instance-profile, nothing
// is written to IAM, custom updater or not.
func TestLaunchDoesNotEditAnInstanceProfileItWasGiven(t *testing.T) {
	isolateLaunchDNS(t)
	instanceProfile = "callers-own-profile"
	f := &fakeIAM{}
	c := newFakeIAMClient(t, f)
	cfg := &aws.LaunchConfig{DNSName: "web", DNSEndpoint: "https://abc123.lambda-url.eu-west-1.on.aws/",
		DNSFunctionARN: "arn:aws:lambda:eu-west-1:111122223333:function:spawn-dns-updater"}
	if err := ensureIAMProfile(context.Background(), c, cfg, progress.NewQuietProgress(), audit.NewLogger(io.Discard, "", "")); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 0 {
		t.Errorf("--instance-profile, but spawn wrote %d policies", len(f.puts))
	}
}

func policyNames(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
