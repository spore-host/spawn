package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #891: a launch against a custom DNS deployment must carry its zone and updater
// to the instance. Driven through runLaunchDryRun — the path a real launch takes
// through ensureAMIAndPreflight — and read back from the -o json plan.

const testEndpoint = "https://abc123.lambda-url.eu-west-1.on.aws/"

type dnsFlags struct{ domain, endpoint, function string }

const testFunction = "arn:aws:lambda:eu-west-1:111122223333:function:spawn-dns-updater"

func dryRunDNSPlan(t *testing.T, env *testutil.TestEnv, f dnsFlags, dnsRecord string) (launchPlan, error) {
	t.Helper()
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	stubDryRunPrice(t, 0.1)
	prevD, prevE, prevF, prevOS, prevOut := dnsDomain, dnsAPIEndpoint, dnsFunctionARN, osFlag, spawnOutputFormat
	t.Cleanup(func() {
		dnsDomain, dnsAPIEndpoint, dnsFunctionARN, osFlag, spawnOutputFormat = prevD, prevE, prevF, prevOS, prevOut
	})
	dnsDomain, dnsAPIEndpoint, dnsFunctionARN, osFlag, spawnOutputFormat = f.domain, f.endpoint, f.function, "linux", "json"

	cfg := &spawnaws.LaunchConfig{Name: "web", InstanceType: "m7i.large", Region: "us-east-1",
		AMI: "ami-0abc123", TTL: "1h", DNSName: dnsRecord}
	var out bytes.Buffer
	if err := runLaunchDryRun(context.Background(), &out, client, cfg); err != nil {
		return launchPlan{}, err
	}
	var p launchPlan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, out.String())
	}
	return p, nil
}

func isolateDNSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")
	t.Setenv("SPAWN_DNS_FUNCTION_ARN", "")
}

func TestLaunchCarriesTheCustomDNSTarget(t *testing.T) {
	isolateDNSEnv(t)
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)

	p, err := dryRunDNSPlan(t, env, dnsFlags{"spore.example.edu", "https://ABC123.lambda-url.eu-west-1.on.aws", testFunction}, "web")
	if err != nil {
		t.Fatal(err)
	}
	if p.DNSDomain != "spore.example.edu" || p.DNSEndpoint != testEndpoint || p.DNSFunctionARN != testFunction {
		t.Errorf("plan DNS target = (%q, %q, %q), want (spore.example.edu, %s, %s)", p.DNSDomain, p.DNSEndpoint, p.DNSFunctionARN, testEndpoint, testFunction)
	}

	// spore.host spelled out is the default: no tag for the common case.
	p, err = dryRunDNSPlan(t, env, dnsFlags{"spore.host", "", ""}, "web")
	if err != nil || p.DNSDomain != "" || p.DNSEndpoint != "" {
		t.Errorf("--dns-domain spore.host = (%q, %q, %v), want no custom target", p.DNSDomain, p.DNSEndpoint, err)
	}

	// No DNS name (e.g. --no-dns): nothing to point anywhere.
	p, err = dryRunDNSPlan(t, env, dnsFlags{"spore.example.edu", testEndpoint, ""}, "")
	if err != nil || p.DNSDomain != "" || p.DNSEndpoint != "" {
		t.Errorf("no DNS name = (%q, %q, %v), want no custom target", p.DNSDomain, p.DNSEndpoint, err)
	}
}

// An endpoint that could never authenticate is refused before launch, naming
// where it came from.
func TestLaunchRefusesAnInvalidDNSTarget(t *testing.T) {
	isolateDNSEnv(t)
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)

	_, err := dryRunDNSPlan(t, env, dnsFlags{"", "https://f4gm19tl70.execute-api.us-east-1.amazonaws.com/prod/update-dns", ""}, "web")
	if err == nil || !strings.Contains(err.Error(), "Function URL") || !strings.Contains(err.Error(), "flag") {
		t.Errorf("API Gateway endpoint: err = %v, want a refusal naming the flag", err)
	}
	t.Setenv("SPAWN_DNS_DOMAIN", "Not_A_Domain")
	_, err = dryRunDNSPlan(t, env, dnsFlags{}, "web")
	if err == nil || !strings.Contains(err.Error(), "SPAWN_DNS_DOMAIN") {
		t.Errorf("bad env domain: err = %v, want a refusal naming SPAWN_DNS_DOMAIN", err)
	}
}

// An account can publish its deployment once, in SSM, for every launch.
func TestLaunchUsesTheAccountsSSMDNSTarget(t *testing.T) {
	isolateDNSEnv(t)
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	ssmc := ssm.NewFromConfig(env.AWSConfig)
	for k, v := range map[string]string{"/spawn/dns/domain": "spore.example.edu", "/spawn/dns/api_endpoint": testEndpoint, "/spawn/dns/function_arn": testFunction} {
		if _, err := ssmc.PutParameter(context.Background(), &ssm.PutParameterInput{
			Name: aws.String(k), Value: aws.String(v), Type: ssmtypes.ParameterTypeString}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := dryRunDNSPlan(t, env, dnsFlags{}, "web")
	if err != nil {
		t.Fatal(err)
	}
	if p.DNSDomain != "spore.example.edu" || p.DNSEndpoint != testEndpoint || p.DNSFunctionARN != testFunction {
		t.Errorf("plan DNS target from SSM = (%q, %q, %q)", p.DNSDomain, p.DNSEndpoint, p.DNSFunctionARN)
	}
	// The flag still wins.
	p, err = dryRunDNSPlan(t, env, dnsFlags{"other.example.org", "", ""}, "web")
	if err != nil || p.DNSDomain != "other.example.org" || p.DNSEndpoint != testEndpoint {
		t.Errorf("flag over SSM = (%q, %q, %v)", p.DNSDomain, p.DNSEndpoint, err)
	}
}

// `spawn dns register` / `delete` must act where spored registered the record:
// the instance's own tags, unless a flag says otherwise (#891). Before, delete
// always targeted spore.host, so a custom-zone record could not be removed.
func TestDNSCommandTargetPrecedence(t *testing.T) {
	isolateDNSEnv(t)
	env := testutil.SubstrateServer(t)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()
	prevD, prevE := dnsDomain, dnsAPIEndpoint
	t.Cleanup(func() { dnsDomain, dnsAPIEndpoint = prevD, prevE })

	tagged := map[string]string{"spawn:dns-domain": "spore.example.edu", "spawn:dns-endpoint": testEndpoint}

	dnsDomain, dnsAPIEndpoint = "", ""
	d, e, err := dnsCommandTarget(ctx, client, "us-east-1", nil)
	if err != nil || d != "spore.host" || e != "" {
		t.Errorf("untagged, no flags = (%q, %q, %v), want spore.host and the compiled-in updater", d, e, err)
	}
	d, e, err = dnsCommandTarget(ctx, client, "us-east-1", tagged)
	if err != nil || d != "spore.example.edu" || e != testEndpoint {
		t.Errorf("tagged instance = (%q, %q, %v), want its own target", d, e, err)
	}
	dnsDomain = "other.example.org"
	d, e, err = dnsCommandTarget(ctx, client, "us-east-1", tagged)
	if err != nil || d != "other.example.org" || e != testEndpoint {
		t.Errorf("--domain over the tag = (%q, %q, %v)", d, e, err)
	}
	dnsDomain = ""
	_, _, err = dnsCommandTarget(ctx, client, "us-east-1", map[string]string{"spawn:dns-endpoint": "https://collector.example.net/"})
	if err == nil || !strings.Contains(err.Error(), "spawn:dns-endpoint tag") {
		t.Errorf("a tampered endpoint tag: err = %v, want a refusal naming the tag", err)
	}
}
