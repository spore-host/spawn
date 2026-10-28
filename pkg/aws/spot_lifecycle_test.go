package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// fakeEC2 is a wire-level stand-in for the EC2 + STS query APIs: it records every
// request's form parameters and answers the handful of actions Launch and
// Terminate make. Wire-level on purpose — these tests assert what spawn SENDS to
// RunInstances / CancelSpotInstanceRequests, through the real Client methods.
type fakeEC2 struct {
	mu       sync.Mutex
	requests []url.Values

	// DescribeInstances answer for the Terminate tests.
	spotRequestID string
	spotType      string // spawn:spot-request-type tag value; "" = untagged
	cancelDenied  bool

	// memMiB, when set, answers DescribeInstanceTypes with that memory (#949);
	// otherwise the call is refused and the caller falls back.
	memMiB string

	// runSpotRequestID, when set, is returned as the launched instance's
	// spotInstanceRequestId (#950).
	runSpotRequestID string

	// eipAlloc / eipAssoc, when set, answer DescribeAddresses with a
	// spawn-allocated launch Elastic IP (#661) for Terminate to release.
	eipAlloc, eipAssoc string

	// efaCards, when set, answers DescribeInstanceTypes with an EFA type of that
	// many cards and DescribeInstances with a running instance and its primary
	// interface, for the multi-interface launch path (#661). allocDenied fails
	// AllocateAddress.
	efaCards    int
	allocDenied bool

	// subnets answers DescribeSubnets, filtered by the request's vpc-id and
	// availability-zone filters as EC2 does (#1389).
	subnets []fakeSubnet
}

type fakeSubnet struct {
	id, vpc, az string
	free        int
}

func (f *fakeEC2) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Get("Action"))
	}
	return out
}

func (f *fakeEC2) request(action string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Get("Action") == action {
			return r
		}
	}
	return nil
}

func (f *fakeEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("Action") == "" {
		// Not a query-protocol call (e.g. the account API): refuse, non-retryably.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"not in this fake"}`))
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, form)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/xml")
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	switch form.Get("Action") {
	case "GetCallerIdentity":
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::123456789012:user/t</Arn><UserId>U</UserId><Account>123456789012</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	case "RunInstances":
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<RunInstancesResponse ` + ns + `><requestId>r</requestId><reservationId>r-1</reservationId><ownerId>111122223333</ownerId>` +
			`<instancesSet><item><instanceId>i-new</instanceId>` + spotReqXML(f.runSpotRequestID) +
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`))
	case "CreateTags":
		_, _ = w.Write([]byte(`<CreateTagsResponse ` + ns + `><requestId>r</requestId><return>true</return></CreateTagsResponse>`))
	case "DescribeInstances":
		var b bytes.Buffer
		b.WriteString(`<DescribeInstancesResponse ` + ns + `><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet><item><instanceId>i-1</instanceId>`)
		if f.spotRequestID != "" {
			b.WriteString(`<instanceLifecycle>spot</instanceLifecycle><spotInstanceRequestId>` + f.spotRequestID + `</spotInstanceRequestId>`)
		}
		if f.efaCards > 0 {
			b.WriteString(`<instanceState><code>16</code><name>running</name></instanceState><networkInterfaceSet>` +
				`<item><networkInterfaceId>eni-card1</networkInterfaceId><attachment><deviceIndex>0</deviceIndex><networkCardIndex>1</networkCardIndex></attachment></item>` +
				`<item><networkInterfaceId>eni-primary</networkInterfaceId><attachment><deviceIndex>0</deviceIndex><networkCardIndex>0</networkCardIndex></attachment></item>` +
				`</networkInterfaceSet>`)
		}
		b.WriteString(`<tagSet><item><key>spawn:managed</key><value>true</value></item>`)
		if f.spotType != "" {
			b.WriteString(`<item><key>` + spotrequest.TypeTag + `</key><value>` + f.spotType + `</value></item>`)
		}
		b.WriteString(`</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
	case "CancelSpotInstanceRequests":
		if f.cancelDenied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`))
			return
		}
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<CancelSpotInstanceRequestsResponse ` + ns + `><requestId>r</requestId><spotInstanceRequestSet><item>` +
			`<spotInstanceRequestId>` + form.Get("SpotInstanceRequestId.1") + `</spotInstanceRequestId><state>cancelled</state></item></spotInstanceRequestSet></CancelSpotInstanceRequestsResponse>`))
	case "DescribeInstanceTypes":
		if f.efaCards > 0 {
			var b bytes.Buffer
			// nosemgrep
			b.WriteString(`<DescribeInstanceTypesResponse ` + ns + `><requestId>r</requestId><instanceTypeSet><item><instanceType>` + form.Get("InstanceType.1") +
				`</instanceType><networkInfo><efaSupported>true</efaSupported><efaInfo><maximumEfaInterfaces>` + fmt.Sprint(f.efaCards) + `</maximumEfaInterfaces></efaInfo><networkCards>`)
			for i := 0; i < f.efaCards; i++ {
				b.WriteString(`<item><networkCardIndex>` + fmt.Sprint(i) + `</networkCardIndex><interfaceTypeSet><item>interface</item><item>efa</item><item>efa-only</item></interfaceTypeSet></item>`)
			}
			b.WriteString(`</networkCards></networkInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`)
			_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
			return
		}
		if f.memMiB == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`))
			return
		}
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<DescribeInstanceTypesResponse ` + ns + `><requestId>r</requestId><instanceTypeSet><item><instanceType>` +
			form.Get("InstanceType.1") + `</instanceType><memoryInfo><sizeInMiB>` + f.memMiB + `</sizeInMiB></memoryInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`))
	case "DescribeSubnets":
		want := map[string]string{}
		for i := 1; form.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
			want[form.Get(fmt.Sprintf("Filter.%d.Name", i))] = form.Get(fmt.Sprintf("Filter.%d.Value.1", i))
		}
		var b bytes.Buffer
		b.WriteString(`<DescribeSubnetsResponse ` + ns + `><requestId>r</requestId><subnetSet>`)
		for _, sn := range f.subnets {
			if v, ok := want["vpc-id"]; ok && v != sn.vpc {
				continue
			}
			if v, ok := want["availability-zone"]; ok && v != sn.az {
				continue
			}
			fmt.Fprintf(&b, `<item><subnetId>%s</subnetId><vpcId>%s</vpcId><availabilityZone>%s</availabilityZone><availableIpAddressCount>%d</availableIpAddressCount></item>`,
				sn.id, sn.vpc, sn.az, sn.free)
		}
		b.WriteString(`</subnetSet></DescribeSubnetsResponse>`)
		_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
	case "TerminateInstances":
		_, _ = w.Write([]byte(`<TerminateInstancesResponse ` + ns + `><requestId>r</requestId><instancesSet/></TerminateInstancesResponse>`))
	case "DescribeAddresses":
		var b bytes.Buffer
		b.WriteString(`<DescribeAddressesResponse ` + ns + `><requestId>r</requestId><addressesSet>`)
		if f.eipAlloc != "" {
			b.WriteString(`<item><allocationId>` + f.eipAlloc + `</allocationId><publicIp>198.51.100.7</publicIp>`)
			if f.eipAssoc != "" {
				b.WriteString(`<associationId>` + f.eipAssoc + `</associationId>`)
			}
			b.WriteString(`</item>`)
		}
		b.WriteString(`</addressesSet></DescribeAddressesResponse>`)
		_, _ = w.Write(b.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
	case "AllocateAddress":
		if f.allocDenied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`))
			return
		}
		_, _ = w.Write([]byte(`<AllocateAddressResponse ` + ns + `><requestId>r</requestId><allocationId>eipalloc-new</allocationId><publicIp>198.51.100.9</publicIp><domain>vpc</domain></AllocateAddressResponse>`))
	case "AssociateAddress":
		_, _ = w.Write([]byte(`<AssociateAddressResponse ` + ns + `><requestId>r</requestId><associationId>eipassoc-new</associationId></AssociateAddressResponse>`))
	case "DisassociateAddress":
		_, _ = w.Write([]byte(`<DisassociateAddressResponse ` + ns + `><requestId>r</requestId><return>true</return></DisassociateAddressResponse>`))
	case "ReleaseAddress":
		_, _ = w.Write([]byte(`<ReleaseAddressResponse ` + ns + `><requestId>r</requestId><return>true</return></ReleaseAddressResponse>`))
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`))
	}
}

func newFakeEC2Client(t *testing.T, f *fakeEC2) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	}
	return NewClientFromConfig(cfg)
}

// launchSpot runs a real Client.Launch against the fake and returns the
// RunInstances form it sent.
func launchSpot(t *testing.T, lc LaunchConfig) url.Values {
	t.Helper()
	return launchWith(t, &fakeEC2{}, lc)
}

// launchWith is launchSpot against a caller-configured fake.
func launchWith(t *testing.T, f *fakeEC2, lc LaunchConfig) url.Values {
	t.Helper()
	c := newFakeEC2Client(t, f)
	lc.InstanceType = "m8a.4xlarge"
	lc.Region = "us-west-2"
	lc.AMI = "ami-0123456789abcdef0"
	lc.PricePerHour = 1 // skip the pricing lookup
	if _, err := c.Launch(context.Background(), lc); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run := f.request("RunInstances")
	if run == nil {
		t.Fatalf("Launch sent no RunInstances (actions: %v)", f.actions())
	}
	return run
}

// tagSpecTypes returns the ResourceType of every TagSpecification in a
// RunInstances form.
func tagSpecTypes(form url.Values) []string {
	var out []string
	for i := 1; ; i++ {
		v := form.Get("TagSpecification." + itoa(i) + ".ResourceType")
		if v == "" {
			return out
		}
		out = append(out, v)
	}
}

func hasTag(form url.Values, specType, key, value string) bool {
	for i := 1; ; i++ {
		rt := form.Get("TagSpecification." + itoa(i) + ".ResourceType")
		if rt == "" {
			return false
		}
		if rt != specType {
			continue
		}
		for j := 1; ; j++ {
			k := form.Get("TagSpecification." + itoa(i) + ".Tag." + itoa(j) + ".Key")
			if k == "" {
				break
			}
			if k == key && form.Get("TagSpecification."+itoa(i)+".Tag."+itoa(j)+".Value") == value {
				return true
			}
		}
	}
}

// TestLaunch_SpotRequestShapeForEachLifecycle is #922. EC2 can only stop a Spot
// instance from a persistent request, so a --spot launch whose lifecycle stops or
// hibernates must send SpotInstanceType=persistent with an interruption behaviour
// of stop (hibernate when configured for hibernation), and tag the request so it
// can be found and cancelled. A terminate-only lifecycle stays one-time and sends
// no interruption behaviour and no request tags, exactly as before.
func TestLaunch_SpotRequestShapeForEachLifecycle(t *testing.T) {
	cases := []struct {
		name          string
		lc            LaunchConfig
		wantType      string // "" = no SpotOptions at all
		wantInterrupt string // "" = not sent
	}{
		{"ttl only", LaunchConfig{Spot: true, TTL: "1h"}, "one-time", ""},
		{"on-complete terminate", LaunchConfig{Spot: true, TTL: "1h", OnComplete: "terminate"}, "one-time", ""},
		// An idle timeout of "0" is refused by Launch since #990 (ValidateDurations),
		// so that it reads as "off" is checked in pkg/spotrequest instead.
		{"idle timeout (default action stop)", LaunchConfig{Spot: true, TTL: "1h", IdleTimeout: "15m"}, "persistent", "stop"},
		// #949: --on-idle / --on-complete hibernate configure hibernation, so the
		// persistent request is interrupted by hibernating, as the idle action is.
		{"on-idle hibernate implies hibernation", LaunchConfig{Spot: true, IdleTimeout: "15m", HibernateOnIdle: true}, "persistent", "hibernate"},
		{"on-complete hibernate implies hibernation", LaunchConfig{Spot: true, TTL: "1h", OnComplete: "hibernate"}, "persistent", "hibernate"},
		{"on-idle hibernate with --hibernate", LaunchConfig{Spot: true, IdleTimeout: "15m", HibernateOnIdle: true, Hibernate: true}, "persistent", "hibernate"},
		{"on-complete stop", LaunchConfig{Spot: true, TTL: "1h", OnComplete: "stop"}, "persistent", "stop"},
		{"on-complete hibernate", LaunchConfig{Spot: true, TTL: "1h", OnComplete: "hibernate", Hibernate: true}, "persistent", "hibernate"},
		{"--hibernate alone (for spawn hibernate)", LaunchConfig{Spot: true, TTL: "1h", Hibernate: true}, "persistent", "hibernate"},
		{"on-demand with idle timeout", LaunchConfig{TTL: "1h", IdleTimeout: "15m"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := launchSpot(t, tc.lc)

			if tc.wantType == "" {
				if mt := run.Get("InstanceMarketOptions.MarketType"); mt != "" {
					t.Fatalf("on-demand launch sent MarketType=%q", mt)
				}
				return
			}
			if mt := run.Get("InstanceMarketOptions.MarketType"); mt != "spot" {
				t.Fatalf("MarketType = %q, want spot", mt)
			}
			if got := run.Get("InstanceMarketOptions.SpotOptions.SpotInstanceType"); got != tc.wantType {
				t.Errorf("SpotInstanceType = %q, want %q", got, tc.wantType)
			}
			if got := run.Get("InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior"); got != tc.wantInterrupt {
				t.Errorf("InstanceInterruptionBehavior = %q, want %q", got, tc.wantInterrupt)
			}

			specs := strings.Join(tagSpecTypes(run), ",")
			persistent := tc.wantType == "persistent"
			if got := strings.Contains(specs, "spot-instances-request"); got != persistent {
				t.Errorf("spot-instances-request tag spec present = %v, want %v (specs %s)", got, persistent, specs)
			}
			if persistent && !hasTag(run, "spot-instances-request", "spawn:managed", "true") {
				t.Error("the persistent request must carry spawn:managed=true — the tag-conditioned cancel grants match on it")
			}
			if !hasTag(run, "instance", spotrequest.TypeTag, tc.wantType) {
				t.Errorf("instance must be tagged %s=%s", spotrequest.TypeTag, tc.wantType)
			}
		})
	}
}

// The max price rides along on both request kinds.
func TestLaunch_SpotMaxPriceKept(t *testing.T) {
	for _, idle := range []string{"", "15m"} {
		run := launchSpot(t, LaunchConfig{Spot: true, TTL: "1h", IdleTimeout: idle, SpotMaxPrice: "0.42"})
		if got := run.Get("InstanceMarketOptions.SpotOptions.MaxPrice"); got != "0.42" {
			t.Errorf("idle=%q: MaxPrice = %q, want 0.42", idle, got)
		}
	}
}

// TestNeedsStoppable_Direct exercises the decision with inputs no CLI path
// produces (mixed case, whitespace, garbage durations).
func TestNeedsStoppable_Direct(t *testing.T) {
	cases := []struct {
		idle, onComplete     string
		hibernate, hibOnIdle bool
		want                 bool
	}{
		{"", "", false, false, false},
		{"garbage", "", false, false, false},
		{"-5m", "", false, false, false},
		{" 1h ", "", false, false, true},
		{"", " Stop ", false, false, true},
		{"", "HIBERNATE", false, false, true},
		{"", "terminate", false, false, false},
		{"", "", true, false, true},
		// hibernate-on-idle without an idle timeout never fires.
		{"", "", false, true, false},
	}
	for _, tc := range cases {
		if got := spotrequest.NeedsStoppable(tc.idle, tc.onComplete, tc.hibernate, tc.hibOnIdle); got != tc.want {
			t.Errorf("NeedsStoppable(%q,%q,%v,%v) = %v, want %v", tc.idle, tc.onComplete, tc.hibernate, tc.hibOnIdle, got, tc.want)
		}
	}
}

// TestTerminate_CancelsSpotRequestFirst: every CLI terminate path (terminate,
// cleanup, cohorts, launch rollback) goes through Client.Terminate, and it must
// cancel the instance's Spot request BEFORE terminating — after, a persistent
// request has already reopened and launched a replacement (#922).
func TestTerminate_CancelsSpotRequestFirst(t *testing.T) {
	f := &fakeEC2{spotRequestID: "sir-abc", spotType: spotrequest.TypePersistent}
	c := newFakeEC2Client(t, f)
	if err := c.Terminate(context.Background(), "us-west-2", "i-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	// DescribeAddresses last: the launch Elastic IP lookup runs after the
	// terminate (#661).
	if got := strings.Join(f.actions(), ","); got != "DescribeInstances,CancelSpotInstanceRequests,TerminateInstances,DescribeAddresses" {
		t.Errorf("actions = %s, want describe, cancel, terminate, then the Elastic IP lookup", got)
	}
	if got := f.request("CancelSpotInstanceRequests").Get("SpotInstanceRequestId.1"); got != "sir-abc" {
		t.Errorf("cancelled %q, want sir-abc", got)
	}
}

// A persistent request the caller cannot cancel means terminating would launch a
// replacement, so the CLI refuses and says how to cancel by hand.
func TestTerminate_RefusesWhenPersistentRequestCannotBeCancelled(t *testing.T) {
	f := &fakeEC2{spotRequestID: "sir-abc", spotType: spotrequest.TypePersistent, cancelDenied: true}
	c := newFakeEC2Client(t, f)
	err := c.Terminate(context.Background(), "us-west-2", "i-1")
	if err == nil {
		t.Fatal("Terminate succeeded although the persistent request could not be cancelled")
	}
	for _, a := range f.actions() {
		if a == "TerminateInstances" {
			t.Fatal("sent TerminateInstances with the persistent request still open")
		}
	}
	for _, want := range []string{"sir-abc", "cancel-spot-instance-requests"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
}

// A one-time request closes with its instance: a refused cancel must not block
// terminating a pure --spot launch (or one from before #922, which is untagged).
func TestTerminate_OneTimeCancelRefusalStillTerminates(t *testing.T) {
	for _, typ := range []string{"", spotrequest.TypeOneTime} {
		f := &fakeEC2{spotRequestID: "sir-abc", spotType: typ, cancelDenied: true}
		c := newFakeEC2Client(t, f)
		if err := c.Terminate(context.Background(), "us-west-2", "i-1"); err != nil {
			t.Errorf("type %q: Terminate: %v", typ, err)
		}
		if a := f.actions(); len(a) < 2 || a[len(a)-2] != "TerminateInstances" {
			t.Errorf("type %q: actions %v, want a terminate", typ, a)
		}
	}
}

// On-demand: no cancel call at all.
func TestTerminate_OnDemandMakesNoCancel(t *testing.T) {
	f := &fakeEC2{}
	c := newFakeEC2Client(t, f)
	if err := c.Terminate(context.Background(), "us-west-2", "i-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := strings.Join(f.actions(), ","); got != "DescribeInstances,TerminateInstances,DescribeAddresses" {
		t.Errorf("actions = %s", got)
	}
}

// A multi-interface EFA launch's Elastic IP (#661) is disassociated and
// released by the terminate, after TerminateInstances — never before it, where a
// terminate that then failed would leave a running instance without its address.
func TestTerminate_ReleasesLaunchElasticIP(t *testing.T) {
	f := &fakeEC2{eipAlloc: "eipalloc-1", eipAssoc: "eipassoc-1"}
	c := newFakeEC2Client(t, f)
	if err := c.Terminate(context.Background(), "us-west-2", "i-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := strings.Join(f.actions(), ","); got != "DescribeInstances,TerminateInstances,DescribeAddresses,DisassociateAddress,ReleaseAddress" {
		t.Errorf("actions = %s", got)
	}
	d := f.request("DescribeAddresses")
	filters := map[string]string{}
	for i := 1; d.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
		filters[d.Get(fmt.Sprintf("Filter.%d.Name", i))] = d.Get(fmt.Sprintf("Filter.%d.Value.1", i))
	}
	if filters["tag:spawn:managed"] != "true" || filters["tag:"+ElasticIPInstanceTagKey] != "i-1" {
		t.Errorf("DescribeAddresses filters = %v: must select only spawn's address for this instance, never a user's", filters)
	}
	if got := f.request("ReleaseAddress").Get("AllocationId"); got != "eipalloc-1" {
		t.Errorf("released %q", got)
	}
}

// spored cancels its own persistent Spot request before it self-terminates
// (#922), so every spored role policy must grant ec2:CancelSpotInstanceRequests
// — on the spawn:managed rail, like the other instance-affecting actions. The
// baseline is re-applied on every launch, which is what heals existing roles.
func TestSporedPoliciesGrantSpotCancelOnManagedRail(t *testing.T) {
	docs := map[string]string{
		"baseline (every instance role)": (&Client{}).sporedBaselinePolicyDoc(),
		"shared spored role (DCV)":       sporedDCVRolePolicy,
	}
	for name, doc := range docs {
		var parsed struct {
			Statement []struct {
				Effect    string
				Action    interface{}
				Condition map[string]map[string]interface{}
			}
		}
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, st := range parsed.Statement {
			if st.Effect != "Allow" || !actionListed(st.Action, "ec2:CancelSpotInstanceRequests") {
				continue
			}
			found = true
			if st.Condition["StringEquals"]["ec2:ResourceTag/spawn:managed"] != "true" {
				t.Errorf("%s: ec2:CancelSpotInstanceRequests must be conditioned on spawn:managed=true, got %v", name, st.Condition)
			}
		}
		if !found {
			t.Errorf("%s does not grant ec2:CancelSpotInstanceRequests", name)
		}
	}
}

func actionListed(action interface{}, want string) bool {
	switch a := action.(type) {
	case string:
		return a == want
	case []interface{}:
		for _, v := range a {
			if v == want {
				return true
			}
		}
	}
	return false
}

func spotReqXML(id string) string {
	if id == "" {
		return ""
	}
	return `<spotInstanceRequestId>` + id + `</spotInstanceRequestId>`
}
