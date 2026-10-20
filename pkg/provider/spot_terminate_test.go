package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// spotSelfEC2 is a wire-level EC2 fake for spored's own terminate: the instance
// is a Spot instance from request sir-self, and every action is recorded in order.
type spotSelfEC2 struct {
	mu           sync.Mutex
	actions      []string
	cancelled    string
	cancelDenied bool

	// #661: eips are the launch Elastic IPs DescribeAddresses answers with,
	// releaseErr an error code ReleaseAddress answers with per allocation, and
	// terminateFail fails TerminateInstances. released records release order.
	eips          []fakeEIP
	releaseErr    map[string]string
	terminateFail bool
	released      []string
	describeAddr  url.Values
}

type fakeEIP struct {
	alloc string
	tags  map[string]string
}

func (f *spotSelfEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.actions = append(f.actions, form.Get("Action"))
	f.mu.Unlock()

	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	w.Header().Set("Content-Type", "text/xml")
	switch form.Get("Action") {
	case "DescribeInstances":
		_, _ = w.Write([]byte(`<DescribeInstancesResponse ` + ns + `><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId>` +
			`<instancesSet><item><instanceId>i-self</instanceId><instanceLifecycle>spot</instanceLifecycle>` +
			`<spotInstanceRequestId>sir-self</spotInstanceRequestId><tagSet><item><key>spawn:spot-request-type</key><value>persistent</value></item></tagSet>` +
			`</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`))
	case "CancelSpotInstanceRequests":
		f.mu.Lock()
		f.cancelled = form.Get("SpotInstanceRequestId.1")
		f.mu.Unlock()
		if f.cancelDenied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`))
			return
		}
		_, _ = w.Write([]byte(`<CancelSpotInstanceRequestsResponse ` + ns + `><requestId>r</requestId><spotInstanceRequestSet/></CancelSpotInstanceRequestsResponse>`))
	case "TerminateInstances":
		if f.terminateFail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`))
			return
		}
		_, _ = w.Write([]byte(`<TerminateInstancesResponse ` + ns + `><requestId>r</requestId><instancesSet/></TerminateInstancesResponse>`))
	case "DescribeAddresses":
		f.mu.Lock()
		f.describeAddr = form
		f.mu.Unlock()
		var b strings.Builder
		b.WriteString(`<DescribeAddressesResponse ` + ns + `><requestId>r</requestId><addressesSet>`)
		for _, e := range f.eips {
			b.WriteString(`<item><allocationId>` + e.alloc + `</allocationId><publicIp>198.51.100.7</publicIp><tagSet>`)
			for k, v := range e.tags {
				b.WriteString(`<item><key>` + k + `</key><value>` + v + `</value></item>`)
			}
			b.WriteString(`</tagSet></item>`)
		}
		b.WriteString(`</addressesSet></DescribeAddressesResponse>`)
		_, _ = w.Write([]byte(b.String())) // nosemgrep
	case "ReleaseAddress":
		alloc := form.Get("AllocationId")
		f.mu.Lock()
		f.released = append(f.released, alloc)
		f.mu.Unlock()
		if code := f.releaseErr[alloc]; code != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>` + code + `</Code><Message>x</Message></Error></Errors><RequestID>r</RequestID></Response>`)) // nosemgrep
			return
		}
		_, _ = w.Write([]byte(`<ReleaseAddressResponse ` + ns + `><requestId>r</requestId><return>true</return></ReleaseAddressResponse>`))
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>x</Message></Error></Errors><RequestID>r</RequestID></Response>`))
	}
}

func spotSelfProvider(t *testing.T, f *spotSelfEC2) *EC2Provider {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := ec2.NewFromConfig(aws.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
	return &EC2Provider{
		ec2Client: client,
		identity:  &Identity{InstanceID: "i-self", Region: "us-west-2", Provider: "ec2"},
		config:    &Config{},
	}
}

// #922: spored self-terminates at the TTL, on --on-complete terminate and at the
// cost limit, all through EC2Provider.Terminate. A persistent Spot request left
// open launches a replacement, so spored cancels its own request first.
func TestEC2Provider_Terminate_CancelsOwnSpotRequestFirst(t *testing.T) {
	f := &spotSelfEC2{}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := strings.Join(f.actions, ","); got != "DescribeInstances,CancelSpotInstanceRequests,TerminateInstances,DescribeAddresses" {
		t.Errorf("actions = %s, want describe, cancel, then terminate", got)
	}
	if f.cancelled != "sir-self" {
		t.Errorf("cancelled %q, want sir-self", f.cancelled)
	}
}

// An instance past its deadline must die even if its role cannot cancel the
// request (a role not yet healed by a newer spawn launch): spored is best-effort.
func TestEC2Provider_Terminate_CancelRefusedStillTerminates(t *testing.T) {
	f := &spotSelfEC2{cancelDenied: true}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if a := f.actions; len(a) < 2 || a[len(a)-2] != "TerminateInstances" {
		t.Errorf("actions = %v, want a terminate despite the refused cancel", a)
	}
}

func launchEIP(alloc, instanceID string) fakeEIP {
	return fakeEIP{alloc: alloc, tags: map[string]string{
		"spawn:managed": "true", "spawn:purpose": "efa-public-ip", "spawn:instance-id": instanceID,
	}}
}

// #661: every way spored ends the instance (TTL, idle, cost limit, on-complete)
// goes through Terminate, and only `spawn terminate` used to release the launch
// Elastic IP, so each of them left it allocated and billing. Terminate releases
// it, AFTER TerminateInstances (the address is the instance's egress for that
// call), with ReleaseAddress alone: a DisassociateAddress first would cut the
// connection the release then needs.
func TestEC2Provider_Terminate_ReleasesItsLaunchElasticIPAfterTerminating(t *testing.T) {
	f := &spotSelfEC2{eips: []fakeEIP{launchEIP("eipalloc-1", "i-self")}}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := strings.Join(f.actions, ","); got != "DescribeInstances,CancelSpotInstanceRequests,TerminateInstances,DescribeAddresses,ReleaseAddress" {
		t.Errorf("actions = %s, want the release after the terminate and no DisassociateAddress", got)
	}
	if strings.Join(f.released, ",") != "eipalloc-1" {
		t.Errorf("released %v, want eipalloc-1", f.released)
	}
	if f.describeAddr.Get("Filter.2.Value.1") != "i-self" && f.describeAddr.Get("Filter.1.Value.1") != "i-self" {
		t.Errorf("DescribeAddresses not scoped to this instance: %v", f.describeAddr)
	}
}

// An address tagged for ANOTHER instance, or one without spawn's launch tags,
// is never released even if the describe returns it.
func TestEC2Provider_Terminate_ReleasesOnlyItsOwnLaunchElasticIP(t *testing.T) {
	user := fakeEIP{alloc: "eipalloc-user", tags: map[string]string{"Name": "prod"}}
	f := &spotSelfEC2{eips: []fakeEIP{launchEIP("eipalloc-other", "i-other"), user, launchEIP("eipalloc-mine", "i-self")}}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if strings.Join(f.released, ",") != "eipalloc-mine" {
		t.Errorf("released %v, want only eipalloc-mine", f.released)
	}
}

// A nondefault VPC answers InUse (the address stays associated until EC2
// deletes the primary interface): logged and left to the reaper, and the
// terminate still succeeds.
func TestEC2Provider_Terminate_InUseElasticIPDoesNotFailTheTerminate(t *testing.T) {
	f := &spotSelfEC2{eips: []fakeEIP{launchEIP("eipalloc-1", "i-self")}, releaseErr: map[string]string{"eipalloc-1": "InvalidIPAddress.InUse"}}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

// If the terminate fails the instance is still running and needs its address.
func TestEC2Provider_Terminate_FailedTerminateKeepsTheElasticIP(t *testing.T) {
	f := &spotSelfEC2{eips: []fakeEIP{launchEIP("eipalloc-1", "i-self")}, terminateFail: true}
	if err := spotSelfProvider(t, f).Terminate(context.Background(), "TTL expired"); err == nil {
		t.Fatal("Terminate succeeded against a refused TerminateInstances")
	}
	for _, a := range f.actions {
		if a == "ReleaseAddress" || a == "DescribeAddresses" {
			t.Errorf("actions = %v: released the address of an instance that is still running", f.actions)
		}
	}
}
