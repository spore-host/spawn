package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	smithy "github.com/aws/smithy-go"
)

// zoneEC2 is the wire fake for a whole Client.LaunchWithZoneFallback (#1458):
// RunInstances fails with runErr[zone] for a zone ("" = the request named
// none) and launches otherwise, placing the instance in the zone asked for;
// DescribeInstanceTypeOfferings answers offered; DescribeSubnets answers a
// default subnet in each of defaults. Everything else goes to fakeEC2, which
// records every request.
type zoneEC2 struct {
	*fakeEC2
	offered, defaults []string
	runErr            map[string]string
	// status is the HTTP status of a RunInstances error: 400 by default; 500,
	// as EC2 answers InsufficientInstanceCapacity, makes the SDK retry it.
	status int
}

func (f *zoneEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	var resp bytes.Buffer
	switch form.Get("Action") {
	case "RunInstances":
		zone := form.Get("Placement.AvailabilityZone")
		if code, ok := f.runErr[zone]; ok {
			f.record(form)
			status := f.status
			if status == 0 {
				status = http.StatusBadRequest
			}
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(status)
			// nosemgrep
			fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>no %s here</Message></Error></Errors><RequestID>r</RequestID></Response>`, code, code)
			return
		}
		placed := zone
		if placed == "" {
			placed = "us-west-2z" // EC2's own choice
		}
		// nosemgrep
		fmt.Fprintf(&resp, `<RunInstancesResponse %s><requestId>r</requestId><reservationId>r-1</reservationId><ownerId>111122223333</ownerId>`+
			`<instancesSet><item><instanceId>i-new</instanceId><placement><availabilityZone>%s</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`, ns, placed)
	case "DescribeInstanceTypeOfferings":
		fmt.Fprintf(&resp, `<DescribeInstanceTypeOfferingsResponse %s><requestId>r</requestId><instanceTypeOfferingSet>`, ns)
		for _, z := range f.offered {
			// nosemgrep
			fmt.Fprintf(&resp, `<item><instanceType>%s</instanceType><locationType>availability-zone</locationType><location>%s</location></item>`, form.Get("Filter.1.Value.1"), z)
		}
		resp.WriteString(`</instanceTypeOfferingSet></DescribeInstanceTypeOfferingsResponse>`)
	case "DescribeSubnets":
		fmt.Fprintf(&resp, `<DescribeSubnetsResponse %s><requestId>r</requestId><subnetSet>`, ns)
		for _, z := range f.defaults {
			fmt.Fprintf(&resp, `<item><subnetId>subnet-%s</subnetId><vpcId>vpc-default</vpcId><availabilityZone>%s</availabilityZone><defaultForAz>true</defaultForAz></item>`, z, z)
		}
		resp.WriteString(`</subnetSet></DescribeSubnetsResponse>`)
	default:
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fakeEC2.ServeHTTP(w, r)
		return
	}
	f.record(form)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write(resp.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
}

func (f *zoneEC2) record(form url.Values) {
	f.mu.Lock()
	f.requests = append(f.requests, form)
	f.mu.Unlock()
}

// sent returns every request with that Action, in order.
func (f *zoneEC2) sent(action string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, r := range f.requests {
		if r.Get("Action") == action {
			out = append(out, r)
		}
	}
	return out
}

// runZones are the zones the RunInstances requests named, in order.
func (f *zoneEC2) runZones() []string {
	var out []string
	for _, r := range f.sent("RunInstances") {
		out = append(out, r.Get("Placement.AvailabilityZone"))
	}
	return out
}

const ice = "InsufficientInstanceCapacity"

// zoneLaunchConfig is a launch naming no zone, with what the launch reuses on
// every attempt: a profile, a group, a key, user data.
func zoneLaunchConfig() LaunchConfig {
	return LaunchConfig{
		Name: "zones", InstanceType: "g7e.8xlarge", Region: "us-west-2", AMI: "ami-0123456789abcdef0",
		PricePerHour: 1, TTL: "1h", IamInstanceProfile: "spawn-instance-profile", SecurityGroupIDs: []string{"sg-1"},
		KeyName: "k", UserData: "IyEvYmluL2Jhc2gK",
	}
}

func launchAcross(t *testing.T, f *zoneEC2, cfg LaunchConfig) (*LaunchResult, string, error) {
	t.Helper()
	if f.fakeEC2 == nil {
		f.fakeEC2 = &fakeEC2{}
	}
	var out bytes.Buffer
	res, err := newFakeEC2ClientFor(t, f).LaunchWithZoneFallback(context.Background(), cfg, &out)
	return res, out.String(), err
}

// The gate for #1458's goal: a launch naming no zone that gets
// InsufficientInstanceCapacity tries the other zones that offer the type and
// launches in the first with capacity: exactly one instance, recorded in that
// zone, and everything before RunInstances done once and reused.
func TestLaunchWithZoneFallback_CapacityErrorLaunchesInTheNextOfferedZone(t *testing.T) {
	f := &zoneEC2{
		offered: []string{"us-west-2a", "us-west-2b", "us-west-2c"}, defaults: []string{"us-west-2a", "us-west-2b", "us-west-2c"},
		runErr: map[string]string{"": ice, "us-west-2a": ice},
	}
	res, out, err := launchAcross(t, f, zoneLaunchConfig())
	if err != nil {
		t.Fatalf("LaunchWithZoneFallback: %v", err)
	}
	if got, want := f.runZones(), []string{"", "us-west-2a", "us-west-2b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RunInstances named zones %q, want %q: the next offered zone after each capacity error, and nothing after the launch", got, want)
	}
	if res.AvailabilityZone != "us-west-2b" {
		t.Errorf("recorded zone %q, want us-west-2b", res.AvailabilityZone)
	}
	wantAttempts := []ZoneAttempt{{Zone: "", Code: ice, Message: "no " + ice + " here"}, {Zone: "us-west-2a", Code: ice, Message: "no " + ice + " here"}}
	if !reflect.DeepEqual(res.ZoneAttempts, wantAttempts) {
		t.Errorf("ZoneAttempts = %+v, want %+v", res.ZoneAttempts, wantAttempts)
	}
	for _, z := range []string{"us-west-2a", "us-west-2b"} {
		if !strings.Contains(out, z) {
			t.Errorf("progress output does not name %s:\n%s", z, out)
		}
	}

	// Reused, not redone: the work before RunInstances ran once ...
	for _, action := range []string{"GetCallerIdentity", "DescribeImages"} {
		if n := len(f.sent(action)); n != 1 {
			t.Errorf("%s sent %d times, want once for the whole launch", action, n)
		}
	}
	// ... and every attempt carried the same profile, group, key and user data.
	runs := f.sent("RunInstances")
	for _, key := range []string{"IamInstanceProfile.Name", "NetworkInterface.1.SecurityGroupId.1", "KeyName", "UserData", "TagSpecification.1.Tag.1.Value"} {
		for i, r := range runs {
			if r.Get(key) == "" || r.Get(key) != runs[0].Get(key) {
				t.Errorf("attempt %d sent %s=%q, want the first attempt's %q", i+1, key, r.Get(key), runs[0].Get(key))
			}
		}
	}
}

// Every attempt is its own request with its own ClientToken (one token on two
// placements is IdempotentParameterMismatch, or the first reservation back),
// and the SDK's own retry of one attempt resends that attempt's token, so it
// stays idempotent. EC2 answers InsufficientInstanceCapacity with a 500, which
// the SDK retries: the "exceeded maximum number of attempts, 3" in #1458.
func TestLaunchWithZoneFallback_EachAttemptHasItsOwnStableClientToken(t *testing.T) {
	f := &zoneEC2{
		fakeEC2: &fakeEC2{}, status: http.StatusInternalServerError,
		offered: []string{"us-west-2a", "us-west-2b"}, defaults: []string{"us-west-2a", "us-west-2b"},
		runErr: map[string]string{"": ice},
	}
	c := newRetryingFakeClient(t, f)
	if _, err := c.LaunchWithZoneFallback(context.Background(), zoneLaunchConfig(), io.Discard); err != nil {
		t.Fatalf("LaunchWithZoneFallback: %v", err)
	}
	tokens := map[string][]string{} // zone → tokens sent
	for _, r := range f.sent("RunInstances") {
		tokens[r.Get("Placement.AvailabilityZone")] = append(tokens[r.Get("Placement.AvailabilityZone")], r.Get("ClientToken"))
	}
	first, second := tokens[""], tokens["us-west-2a"]
	if len(first) != 3 || len(second) != 1 {
		t.Fatalf("RunInstances per zone: %v, want 3 sends (the SDK's retries) with no zone, then 1 in us-west-2a", tokens)
	}
	for _, tok := range first {
		if tok == "" || tok != first[0] {
			t.Errorf("the SDK's retries of one attempt sent tokens %q, want one token for all of them", first)
		}
	}
	if second[0] == "" || second[0] == first[0] {
		t.Errorf("the attempt in us-west-2a sent token %q, the first attempt %q: want a new one", second[0], first[0])
	}
}

// newRetryingFakeClient is a client of h with the SDK's standard retryer, as
// in production, minus its backoff.
func newRetryingFakeClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClientFromConfig(aws.Config{
		Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
				o.RateLimiter = ratelimit.None
			})
		},
	})
}

// All zones out of capacity: the error lists every zone tried with its code,
// and still unwraps to the LaunchError, so the code classifies it.
func TestLaunchWithZoneFallback_EveryZoneOutOfCapacityListsThemAll(t *testing.T) {
	f := &zoneEC2{
		offered: []string{"us-west-2a", "us-west-2b"}, defaults: []string{"us-west-2a", "us-west-2b"},
		runErr: map[string]string{"": ice, "us-west-2a": ice, "us-west-2b": "SpotMaxPriceTooLow"},
	}
	_, _, err := launchAcross(t, f, zoneLaunchConfig())
	var zf *ZoneFallbackError
	if !errors.As(err, &zf) {
		t.Fatalf("err = %v, want a *ZoneFallbackError", err)
	}
	if n := len(f.sent("RunInstances")); n != 3 {
		t.Errorf("RunInstances sent %d times, want 3 (no zone, then each offered zone once)", n)
	}
	msg := err.Error()
	for _, want := range []string{"EC2's choice of zone: " + ice, "us-west-2a: " + ice, "us-west-2b: SpotMaxPriceTooLow", "lagotto watch g7e.8xlarge --regions us-west-2"} {
		if !strings.Contains(strings.ReplaceAll(msg, " (none named)", ""), want) {
			t.Errorf("error does not say %q:\n%s", want, msg)
		}
	}
	var le *LaunchError
	if !errors.As(err, &le) || le.Code != "SpotMaxPriceTooLow" {
		t.Errorf("errors.As LaunchError = %+v, want the last attempt's code", le)
	}
}

// Only the zones that offer the type, and of those only the ones a launch with
// no subnet can place in (a default subnet), are tried: us-west-2b does not
// offer the type, us-west-2d offers it but has no default subnet.
func TestLaunchWithZoneFallback_TriesOnlyZonesThatOfferTheType(t *testing.T) {
	f := &zoneEC2{
		offered:  []string{"us-west-2c", "us-west-2a", "us-west-2d"},
		defaults: []string{"us-west-2a", "us-west-2b", "us-west-2c"},
		runErr:   map[string]string{"": ice, "us-west-2a": ice, "us-west-2b": ice, "us-west-2c": ice, "us-west-2d": ice},
	}
	_, _, err := launchAcross(t, f, zoneLaunchConfig())
	if err == nil {
		t.Fatal("launched; every zone was out of capacity")
	}
	if got, want := f.runZones(), []string{"", "us-west-2a", "us-west-2c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RunInstances named zones %q, want %q", got, want)
	}
	offerings := f.sent("DescribeInstanceTypeOfferings")
	if len(offerings) != 1 || offerings[0].Get("LocationType") != "availability-zone" || offerings[0].Get("Filter.1.Value.1") != "g7e.8xlarge" {
		t.Errorf("DescribeInstanceTypeOfferings requests %v, want one, by availability-zone, for g7e.8xlarge", offerings)
	}
}

// A launch with its zone pinned is not moved: one attempt, and the error names
// the other zones offering the type and lagotto watch.
func TestLaunchWithZoneFallback_PinnedZoneIsNotMovedAndHintsTheOthers(t *testing.T) {
	f := &zoneEC2{
		offered: []string{"us-west-2a", "us-west-2b", "us-west-2c"}, defaults: []string{"us-west-2a", "us-west-2b", "us-west-2c"},
		runErr: map[string]string{"us-west-2a": ice},
	}
	f.fakeEC2, f.status = &fakeEC2{}, http.StatusInternalServerError // as EC2 answers it, retried by the SDK
	cfg := zoneLaunchConfig()
	cfg.AvailabilityZone = "us-west-2a"
	_, err := newRetryingFakeClient(t, f).LaunchWithZoneFallback(context.Background(), cfg, io.Discard)
	if got := f.runZones(); !reflect.DeepEqual(got, []string{"us-west-2a", "us-west-2a", "us-west-2a"}) {
		t.Errorf("RunInstances named zones %q, want only the pinned us-west-2a (sent 3 times by the SDK's retryer)", got)
	}
	if err == nil {
		t.Fatal("launched; the pinned zone was out of capacity")
	}
	for _, want := range []string{"--az", "also offered in us-west-2b, us-west-2c", "lagotto watch", "re-sending the same request to the same zone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%v", want, err)
		}
	}
}

// A failure that is not about capacity fails the same way in every zone: no
// other zone is tried, no zone is even looked up, and the error is today's.
func TestLaunchWithZoneFallback_NonCapacityErrorIsNotRetriedElsewhere(t *testing.T) {
	for _, code := range []string{"UnauthorizedOperation", "InvalidParameterValue", "Unsupported"} {
		t.Run(code, func(t *testing.T) {
			f := &zoneEC2{offered: []string{"us-west-2a"}, defaults: []string{"us-west-2a"}, runErr: map[string]string{"": code}}
			_, _, err := launchAcross(t, f, zoneLaunchConfig())
			if n := len(f.sent("RunInstances")); n != 1 {
				t.Errorf("RunInstances sent %d times, want 1", n)
			}
			if n := len(f.sent("DescribeInstanceTypeOfferings")); n != 0 {
				t.Errorf("looked up zones %d times for a %s, want none", n, code)
			}
			var le *LaunchError
			var zf *ZoneFallbackError
			if !errors.As(err, &le) || le.Code != code || errors.As(err, &zf) {
				t.Errorf("err = %v, want today's *LaunchError with code %s", err, code)
			}
		})
	}
	// And one in a fallback zone stops the walk there.
	f := &zoneEC2{
		offered: []string{"us-west-2a", "us-west-2b"}, defaults: []string{"us-west-2a", "us-west-2b"},
		runErr: map[string]string{"": ice, "us-west-2a": "Unsupported"},
	}
	_, _, err := launchAcross(t, f, zoneLaunchConfig())
	if got := f.runZones(); !reflect.DeepEqual(got, []string{"", "us-west-2a"}) || err == nil {
		t.Errorf("RunInstances named zones %q (err %v), want to stop at us-west-2a's Unsupported", got, err)
	}
}

// MaxSpotInstanceCountExceeded is the region's Spot instance limit: every zone
// refuses it alike, so no zone is looked up or tried, pinned or not, and the
// error says it is regional, naming no other zone. Mid-walk it stops the walk.
func TestLaunchWithZoneFallback_RegionalSpotLimitIsNotRetriedElsewhere(t *testing.T) {
	const limit = "MaxSpotInstanceCountExceeded"
	for _, pinned := range []string{"", "us-west-2a"} {
		t.Run("az="+pinned, func(t *testing.T) {
			f := &zoneEC2{
				offered: []string{"us-west-2a", "us-west-2b"}, defaults: []string{"us-west-2a", "us-west-2b"},
				runErr: map[string]string{pinned: limit},
			}
			cfg := zoneLaunchConfig()
			cfg.Spot, cfg.AvailabilityZone = true, pinned
			_, _, err := launchAcross(t, f, cfg)
			if n := len(f.sent("RunInstances")); n != 1 {
				t.Errorf("RunInstances sent %d times, want 1: the limit is the region's", n)
			}
			if n := len(f.sent("DescribeInstanceTypeOfferings")); n != 0 {
				t.Errorf("looked up zones %d times, want none", n)
			}
			if err == nil {
				t.Fatal("launched; the region's Spot limit was reached")
			}
			msg := err.Error()
			if !strings.Contains(msg, "Spot instance limit") || !strings.Contains(msg, "us-west-2's") {
				t.Errorf("error does not say it is us-west-2's Spot instance limit:\n%s", msg)
			}
			for _, unwanted := range []string{"also offered", "us-west-2b", "lagotto watch", "  - "} {
				if strings.Contains(msg, unwanted) {
					t.Errorf("error says %q, but no other zone can help:\n%s", unwanted, msg)
				}
			}
			var le *LaunchError
			if !errors.As(err, &le) || le.Code != limit {
				t.Errorf("errors.As LaunchError = %+v, want code %s", le, limit)
			}
		})
	}
	f := &zoneEC2{
		offered: []string{"us-west-2a", "us-west-2b"}, defaults: []string{"us-west-2a", "us-west-2b"},
		runErr: map[string]string{"": ice, "us-west-2a": limit},
	}
	_, _, err := launchAcross(t, f, zoneLaunchConfig())
	if got := f.runZones(); !reflect.DeepEqual(got, []string{"", "us-west-2a"}) || err == nil || !strings.Contains(err.Error(), "Spot instance limit") {
		t.Errorf("RunInstances named zones %q (err %v), want to stop at us-west-2a's regional Spot limit and say so", got, err)
	}
}

// fakeZoneLaunchAPI is the zone-fallback seam at the Go level: every
// RunInstances fails on capacity, and the region offers three zones.
type fakeZoneLaunchAPI struct {
	mu   sync.Mutex
	runs []*ec2.RunInstancesInput
}

func (f *fakeZoneLaunchAPI) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	f.runs = append(f.runs, in)
	f.mu.Unlock()
	return nil, &smithy.GenericAPIError{Code: ice, Message: "none"}
}

func (f *fakeZoneLaunchAPI) DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	out := &ec2.DescribeInstanceTypeOfferingsOutput{}
	for _, z := range []string{"us-west-2a", "us-west-2b", "us-west-2c"} {
		out.InstanceTypeOfferings = append(out.InstanceTypeOfferings, types.InstanceTypeOffering{InstanceType: "g7e.8xlarge", Location: aws.String(z)})
	}
	return out, nil
}

func (f *fakeZoneLaunchAPI) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	out := &ec2.DescribeSubnetsOutput{}
	for _, z := range []string{"us-west-2a", "us-west-2b", "us-west-2c"} {
		out.Subnets = append(out.Subnets, types.Subnet{AvailabilityZone: aws.String(z), DefaultForAz: aws.Bool(true)})
	}
	return out, nil
}

// zoneBoundConfigs are launches bound to one zone or subnet (#1458's design
// constraint 3), each built directly: --no-public-ip and multi-card EFA need a
// subnet or AZ to get this far through Launch, and the guard must hold for a
// caller that skipped those checks too.
func zoneBoundConfigs() map[string]LaunchConfig {
	base := zoneLaunchConfig()
	with := func(f func(*LaunchConfig)) LaunchConfig { c := base; f(&c); return c }
	return map[string]LaunchConfig{
		"az":                   with(func(c *LaunchConfig) { c.AvailabilityZone = "us-west-2a" }),
		"subnet":               with(func(c *LaunchConfig) { c.SubnetID = "subnet-1" }),
		"capacity reservation": with(func(c *LaunchConfig) { c.ReservationID = "cr-1" }),
		"capacity block":       with(func(c *LaunchConfig) { c.ReservationID = "cr-2"; c.CapacityBlock = true }),
		"placement group":      with(func(c *LaunchConfig) { c.PlacementGroup = "pg" }),
		"existing FSx":         with(func(c *LaunchConfig) { c.FSxLustreID = "fs-1" }),
		"no public IP":         with(func(c *LaunchConfig) { c.NoPublicIP = true }),
		"multi-card EFA":       with(func(c *LaunchConfig) { c.EFAEnabled = true; c.EFAInterfaces = 4 }),
	}
}

// Each zone-bound launch makes one attempt and says why it did not move.
func TestLaunchAcrossZones_ZoneBoundLaunchesMakeOneAttempt(t *testing.T) {
	for name, cfg := range zoneBoundConfigs() {
		t.Run(name, func(t *testing.T) {
			api := &fakeZoneLaunchAPI{}
			p := &preparedLaunch{cfg: cfg, input: &ec2.RunInstancesInput{}}
			_, _, err := launchAcrossZones(context.Background(), api, p, io.Discard)
			if len(api.runs) != 1 {
				t.Errorf("RunInstances sent %d times, want 1: the launch is bound to its zone", len(api.runs))
			}
			var zf *ZoneFallbackError
			if !errors.As(err, &zf) || zf.Blocker == "" || !strings.Contains(err.Error(), "did not try another availability zone: "+zf.Blocker) {
				t.Errorf("err = %v, want it to say why no other zone was tried", err)
			}
		})
	}
	// And the unbound one, the control: it does move.
	api := &fakeZoneLaunchAPI{}
	_, _, _ = launchAcrossZones(context.Background(), api, &preparedLaunch{cfg: zoneLaunchConfig(), input: &ec2.RunInstancesInput{}}, io.Discard)
	if len(api.runs) != 4 {
		t.Errorf("an unbound launch sent RunInstances %d times, want 4 (no zone, then three)", len(api.runs))
	}
}

// Every LaunchConfig field that looks like placement or storage is classified
// as zone-binding (a case of zoneBoundConfigs) or not, with the reason, so a
// new one cannot arrive without someone deciding whether it pins the zone.
// The fields are discovered, not listed.
func TestEveryPlacementLikeLaunchConfigFieldIsClassifiedForZoneFallback(t *testing.T) {
	notBinding := map[string]string{
		"VPCID":                      "a VPC spans zones; --vpc without a subnet needs --az, which binds",
		"EFSID":                      "EFS is regional, with a mount target per zone",
		"EFSMountPoint":              "where EFS mounts, not where",
		"AttachVolumes":              "created at launch from regional snapshots, in the instance's zone",
		"FSxPending":                 "an ephemeral FSx is created after the launch, in the instance's zone",
		"FSxMountName":               "set with FSxLustreID, which binds",
		"FSxMountPoint":              "where FSx mounts, not where",
		"FSxImportPath":              "an S3 path",
		"FSxExportPath":              "an S3 path",
		"PlacementGroupPrefix":       "cohort-only: per-zone groups made as the cohort moves zone",
		"EFASecondaryType":           "how cards are typed; EFAInterfaces decides binding",
		"AllowMissingEndpoints":      "only with NoPublicIP, which binds",
		"Spot":                       "a Spot capacity error moves zone like an on-demand one",
		"SpotMaxPrice":               "a price, the same in every zone tried",
		"FSxLustreCreate":            "the CLI creates a durable FSx before the launch and sets FSxLustreID (binds); an ephemeral one after it",
		"FSxLustreRecall":            "the CLI recalls it before the launch and sets FSxLustreID, which binds",
		"FSxLifecycle":               "how long the FSx lives",
		"FSxTTL":                     "how long the FSx lives",
		"FSxStorageCapacity":         "how big the FSx is",
		"FSxS3Bucket":                "an S3 bucket",
		"RootVolumeSizeGiB":          "a size",
		"DNSEndpoint":                "a DNS API URL",
		"SpotInterruptionWebhookURL": "a URL",
	}
	binding := map[string]bool{"AvailabilityZone": true, "SubnetID": true, "ReservationID": true, "CapacityBlock": true,
		"PlacementGroup": true, "FSxLustreID": true, "NoPublicIP": true, "EFAEnabled": true, "EFAInterfaces": true}
	looksPlaced := regexp.MustCompile(`Zone|Subnet|VPC|Placement|Reservation|Capacity|FSx|EFS|EFA|Volume|PublicIP|Endpoint|Spot|Host|Tenancy`)
	found := 0
	rt := reflect.TypeOf(LaunchConfig{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !looksPlaced.MatchString(name) {
			continue
		}
		found++
		if _, ok := notBinding[name]; !ok && !binding[name] {
			t.Errorf("LaunchConfig.%s is not classified: add it to ZoneFallbackBlocker (and zoneBoundConfigs) if it ties a launch to one zone, else to notBinding with the reason", name)
		}
	}
	if found == 0 {
		t.Fatal("found no placement-like LaunchConfig fields; the discovery is broken")
	}
	for name := range binding {
		// nosemgrep
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("binding field %s no longer exists", name)
		}
	}
}
