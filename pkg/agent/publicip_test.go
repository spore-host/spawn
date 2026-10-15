package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
	"github.com/spore-host/spawn/pkg/testutil"
)

// Every user of the public IP must see the one re-read after a resume, not the
// boot-time identity snapshot (#1019). The tests below hibernate an instance
// whose IP was 1.2.3.4 (newTestAgent's) and resume it at 5.6.7.8.

const resumedIP = "5.6.7.8"

// resumeAt arms the agent's resume detection, then simulates a hibernation
// after which IMDS reports ip.
func resumeAt(t *testing.T, a *Agent, ip string) func() {
	t.Helper()
	susp := useFakeSuspend(t)
	a.checkResume(context.Background()) // baseline
	return func() {
		a.lookupPublicIP = func(context.Context) (string, error) { return ip, nil }
		susp.hibernate(2 * time.Hour)
	}
}

// hermeticTicks keeps checkAndAct off the real-AWS tag writes.
func hermeticTicks(a *Agent) {
	a.lastSessionTagWrite = time.Now().Add(time.Hour)
	a.lastComputeTagWrite = time.Now().Add(time.Hour)
	a.lastHeartbeatTagWrite = time.Now().Add(time.Hour)
}

func readyURLHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("spawn:ready-url %q: %v", raw, err)
	}
	return u.Hostname()
}

// 1a. A DCV ready-url built from the public IP (no DNS name) follows the IP
// across a resume, on the monitor tick, with the same token, which the
// verifier still accepts.
func TestResumeRewritesDCVReadyURLWithTheNewIP(t *testing.T) {
	ctx := context.Background()
	tagger := &fakeTagPutter{}
	a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "Session: 'console' (owner ec2-user)"}, tagger)
	hermeticTicks(a)
	resume := resumeAt(t, a, resumedIP)

	a.maybeSetupDCVAuth(ctx)
	before := tagger.tags["spawn:ready-url"]
	if h := readyURLHost(t, before); h != "1.2.3.4" {
		t.Fatalf("setup: ready-url host %q, want the boot IP 1.2.3.4", h)
	}
	token := tagger.tags["spawn:ready-token"]

	resume()
	a.checkAndAct(ctx)

	after := tagger.tags["spawn:ready-url"]
	if h := readyURLHost(t, after); h != resumedIP {
		t.Fatalf("ready-url after the resume = %q, want host %s", after, resumedIP)
	}
	if want := strings.Replace(before, "1.2.3.4", resumedIP, 1); after != want {
		t.Errorf("ready-url after the resume = %q, want %q (same token and session)", after, want)
	}
	if tagger.tags["spawn:ready-token"] != token {
		t.Errorf("spawn:ready-token changed across the resume")
	}
	if _, ok := a.dcvTokens[token]; !ok {
		t.Error("the verifier no longer accepts the token the rewritten URL carries")
	}

	// Nothing changes on the next tick: no rewrite loop.
	n := tagger.calls
	a.checkAndAct(ctx)
	if tagger.calls != n {
		t.Errorf("a tick with no IP change wrote tags again (%d -> %d calls)", n, tagger.calls)
	}
}

// 1b. The same for a web app's ready-url: the proxy's access token is kept.
func TestResumeRewritesWebReadyURLWithTheNewIP(t *testing.T) {
	ctx := context.Background()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})) // the web app spored probes; it answers 200 with no body
	t.Cleanup(app.Close)
	port, err := strconv.Atoi(app.URL[strings.LastIndex(app.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, &provider.Config{AppMode: "web", ReadyPort: port})
	a.identity.Provider = "ec2"
	a.webProxyStarted = true // skip the real :443 bind
	tagger := &fakeTagPutter{}
	a.tagger = tagger
	hermeticTicks(a)
	resume := resumeAt(t, a, resumedIP)

	a.maybeSetupWebReady(ctx)
	if !a.webReadyDone {
		t.Fatal("setup: the web handshake did not complete")
	}
	before := tagger.tags["spawn:ready-url"]
	if h := readyURLHost(t, before); h != "1.2.3.4" {
		t.Fatalf("setup: ready-url host %q, want 1.2.3.4", h)
	}

	resume()
	a.checkAndAct(ctx)

	if want := buildWebReadyURL(resumedIP, a.webToken); tagger.tags["spawn:ready-url"] != want {
		t.Errorf("ready-url after the resume = %q, want %q", tagger.tags["spawn:ready-url"], want)
	}
}

// 1c. With a DNS name the URL names the FQDN, which DNS keeps current: no
// rewrite. A failed rewrite is retried on the next tick.
func TestReadyURLRewriteOnlyForAnIPHostAndRetried(t *testing.T) {
	ctx := context.Background()

	t.Run("FQDN host is left alone", func(t *testing.T) {
		tagger := &fakeTagPutter{}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "Session: 'console'"}, tagger)
		a.config.DNSName, a.dnsDomain = "box", "spore.host"
		a.maybeSetupDCVAuth(ctx)
		url0, n := tagger.tags["spawn:ready-url"], tagger.calls
		a.currentIP, a.currentIPSet = resumedIP, true
		a.refreshReadyURL(ctx)
		if tagger.calls != n || tagger.tags["spawn:ready-url"] != url0 {
			t.Errorf("rewrote an FQDN ready-url on an IP change: %q -> %q", url0, tagger.tags["spawn:ready-url"])
		}
	})

	t.Run("failed write retried", func(t *testing.T) {
		tagger := &fakeTagPutter{}
		a := dcvAuthAgent(t, &fakeDCVRunner{isInstalled: true, listOut: "Session: 'console'"}, tagger)
		a.maybeSetupDCVAuth(ctx)
		a.currentIP, a.currentIPSet = resumedIP, true
		tagger.err = errors.New("throttled")
		a.refreshReadyURL(ctx)
		tagger.err = nil
		a.refreshReadyURL(ctx)
		if h := readyURLHost(t, tagger.tags["spawn:ready-url"]); h != resumedIP {
			t.Errorf("ready-url host %q after a failed then a good write, want %s", h, resumedIP)
		}
	})
}

// 2. {{ instance.ip }} in a plugin step run after the resume renders the new
// IP: the runtime newAgent builds reads the agent's current value.
func TestNewAgentPluginRuntimeRendersTheCurrentIP(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-test123", Region: "us-east-1", PublicIP: "1.2.3.4", Provider: "stub"}
	a, err := newAgent(context.Background(), &stubProvider{identity: identity, config: &provider.Config{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.pluginRuntime.InstanceIP(); got != "1.2.3.4" {
		t.Fatalf("setup: instance.ip %q at boot, want 1.2.3.4", got)
	}
	a.lookupPublicIP = func(context.Context) (string, error) { return resumedIP, nil }
	a.refreshPublicIP(context.Background())
	if got := a.pluginRuntime.InstanceIP(); got != resumedIP {
		t.Errorf("instance.ip %q after the IP was re-read, want %s", got, resumedIP)
	}
}

// ... and end to end: a step installed after the resume writes the new IP.
func TestPluginStepAfterResumeGetsTheNewIP(t *testing.T) {
	a := newTestAgent(t, &provider.Config{})
	hermeticTicks(a)
	a.attachPluginRuntime(pluginruntime.NewRuntimeWithStore(plugin.NewDiskStateStore(t.TempDir()), a.identity, ""))
	resume := resumeAt(t, a, resumedIP)
	resume()
	a.checkAndAct(context.Background())

	out := filepath.Join(t.TempDir(), "ip")
	spec := &plugin.PluginSpec{Name: "echo-ip", Version: "1.0.0", Remote: plugin.RemoteBlock{
		Start: []plugin.Step{{Type: "run", Run: fmt.Sprintf("printf %%s '{{ instance.ip }}' > %s", out)}},
	}}
	if err := a.pluginRuntime.Install(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != resumedIP {
		t.Errorf("{{ instance.ip }} rendered %q after the resume, want %s", b, resumedIP)
	}
}

// 3. An instance whose first boot had no public IP gets a DNS client, and a
// registration, when one appears: found by the renewal, or by a resume.
func TestDNSRegisteredWhenThePublicIPAppearsAfterBoot(t *testing.T) {
	for _, viaResume := range []bool{false, true} {
		t.Run(map[bool]string{false: "found by the renewal", true: "found by a resume"}[viaResume], func(t *testing.T) {
			f := newResumeFixture(t, &provider.Config{})
			a := f.a
			a.identity.PublicIP = "" // the first boot had none
			a.identity.Provider = "ec2"
			a.dnsDomain = "spore.host"
			a.dnsClient = nil
			a.nextDNSRefresh = time.Time{}
			t.Setenv("SPORE_DNS_URL", f.dnsURL)
			ip := ""
			a.lookupPublicIP = func(context.Context) (string, error) {
				if ip == "" {
					return "", errors.New("IMDS public-ipv4: 404")
				}
				return ip, nil
			}

			a.checkResume(context.Background()) // baseline
			a.refreshDNSLease(context.Background())
			if a.dnsClient != nil || len(f.dns()) != 0 {
				t.Fatalf("setup: with no public IP, client=%v requests=%v", a.dnsClient != nil, f.dns())
			}

			ip = resumedIP
			if viaResume {
				f.susp.hibernate(time.Hour)
			} else {
				a.nextDNSRefresh = time.Time{} // the next look is due
			}
			a.checkAndAct(context.Background())

			reqs := f.dns()
			if len(reqs) != 1 || reqs[0].Action != "UPSERT" || reqs[0].IPAddress != resumedIP {
				t.Errorf("DNS requests after the IP appeared = %+v, want one UPSERT at %s", reqs, resumedIP)
			}
		})
	}
}

// 4. The job-array registry entry re-registered on the resume carries the new
// IP, not the identity's.
func TestResumeReRegistersTheJobArrayEntryAtTheNewIP(t *testing.T) {
	env := testutil.SubstrateServer(t)
	createHybridRegistryTable(t, env)

	a := newTestAgent(t, &provider.Config{JobArrayID: "arr-1019"})
	hermeticTicks(a)
	reg := registry.NewPeerRegistryFromConfig(a.identity, env.AWSConfig)
	a.attachRegistry(reg)
	if err := reg.Register(context.Background(), "arr-1019", 0); err != nil {
		t.Fatal(err)
	}
	resume := resumeAt(t, a, resumedIP)
	resume()
	a.checkAndAct(context.Background())

	peers, err := reg.DiscoverPeers(context.Background(), "arr-1019")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].IP != resumedIP {
		t.Errorf("registry after the resume = %+v, want one entry at %s", peers, resumedIP)
	}
}

// createHybridRegistryTable creates registry.TableName in the emulator, with
// the key schema pkg/registry's own tests use.
func createHybridRegistryTable(t *testing.T, env *testutil.TestEnv) {
	t.Helper()
	_, err := env.DynamoClient().CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName: aws.String(registry.TableName),
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("job_array_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("instance_id"), KeyType: dynamodbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("job_array_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("instance_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("create %s: %v", registry.TableName, err)
	}
}
