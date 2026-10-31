package aws

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// immediateSpotPriceRetries makes recordLaunchSpotPrice's NotFound retries
// immediate for one test, keeping their count.
func immediateSpotPriceRetries(t *testing.T) {
	t.Helper()
	saved := launchSpotPriceRetryDelays
	launchSpotPriceRetryDelays = make([]time.Duration, len(saved))
	t.Cleanup(func() { launchSpotPriceRetryDelays = saved })
}

// notFoundSpotEC2 is fakeSpotEC2 whose first notFound DescribeInstances calls
// fail as EC2 does just after RunInstances.
type notFoundSpotEC2 struct {
	fakeSpotEC2
	notFound int
}

func (f *notFoundSpotEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, o ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describes < f.notFound {
		f.describes++
		return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "The instance ID '" + in.InstanceIds[0] + "' does not exist"}
	}
	return f.fakeSpotEC2.DescribeInstances(ctx, in, o...)
}

// #1442: when the re-describe is needed and EC2 answers the first one with its
// post-launch InvalidInstanceID.NotFound, the describe is retried and the price
// is still recorded. Before the fix the first NotFound was the end of it.
func TestRecordLaunchSpotPrice_RetriesPostLaunchNotFound(t *testing.T) {
	immediateSpotPriceRetries(t)
	full := launchedSpot()
	f := &notFoundSpotEC2{notFound: 1, fakeSpotEC2: fakeSpotEC2{instance: &full,
		history: []types.SpotPrice{spotHistory("us-west-2b", "0.3600", time.Now())}}}

	recordLaunchSpotPrice(context.Background(), f, types.Instance{InstanceId: full.InstanceId})

	if f.describes != 2 {
		t.Errorf("describes = %d, want 2 (one NotFound, one retry)", f.describes)
	}
	if len(f.tagged) != 1 || aws.ToString(f.tagged[0].Value) != "0.360000" {
		t.Errorf("tagged %+v, want %s=0.360000 despite the first NotFound", f.tagged, SpotPriceTag)
	}
}

// The retry is bounded: an instance that stays NotFound is described once plus
// once per retry delay, then given up on without a tag. Any other error is not
// retried.
func TestRecordLaunchSpotPrice_RetryIsBoundedAndNotFoundOnly(t *testing.T) {
	immediateSpotPriceRetries(t)
	id := launchedSpot().InstanceId

	gone := &notFoundSpotEC2{notFound: 1000}
	recordLaunchSpotPrice(context.Background(), gone, types.Instance{InstanceId: id})
	if want := 1 + len(launchSpotPriceRetryDelays); gone.describes != want || len(gone.tagged) != 0 {
		t.Errorf("persistent NotFound: describes=%d tagged=%v, want %d describes and no tag", gone.describes, gone.tagged, want)
	}

	denied := &fakeSpotEC2{}
	_, err := describeForSpotPrice(context.Background(), &deniedDescribe{denied}, aws.ToString(id), launchSpotPriceRetryDelays)
	if err == nil || denied.describes != 1 {
		t.Errorf("UnauthorizedOperation: err=%v describes=%d, want an error after one describe", err, denied.describes)
	}
}

type deniedDescribe struct{ *fakeSpotEC2 }

func (d *deniedDescribe) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	d.describes++
	return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "denied"}
}

// launchedForSpotPrice gives a RunInstances-shaped instance (zone, type, but no
// PlatformDetails) everything the lookup needs, so it is priced without any
// describe; and a zone-less one gets the requested zone.
func TestLaunchedForSpotPrice_NeedsNoDescribe(t *testing.T) {
	run := launchedSpot()
	run.PlatformDetails = nil
	ami := amiLaunchInfo{platformDetails: "Linux/UNIX"}

	f := &notFoundSpotEC2{notFound: 1000, fakeSpotEC2: fakeSpotEC2{history: []types.SpotPrice{spotHistory("us-west-2b", "0.3600", time.Now())}}}
	recordLaunchSpotPrice(context.Background(), f, launchedForSpotPrice(run, ami, ""))
	if f.describes != 0 || len(f.tagged) != 1 {
		t.Errorf("describes=%d tagged=%v, want the price recorded with no describe", f.describes, f.tagged)
	}

	noZone := run
	noZone.Placement = nil
	got := launchedForSpotPrice(noZone, ami, "us-west-2c")
	if got.Placement == nil || aws.ToString(got.Placement.AvailabilityZone) != "us-west-2c" {
		t.Errorf("placement = %+v, want the requested us-west-2c", got.Placement)
	}
	win := launchedForSpotPrice(types.Instance{}, amiLaunchInfo{platform: types.PlatformValuesWindows}, "")
	if p, ok := spotProductDescription(win); !ok || p != "Windows" {
		t.Errorf("Windows AMI: product %q,%v, want Windows", p, ok)
	}
}

// spotLaunchEC2 is the wire fake for a whole spot Client.Launch as EC2 really
// answers one (#1442): DescribeImages carries the AMI's platformDetails, the
// RunInstances response has the placement but NO platformDetails, and
// DescribeInstances answers InvalidInstanceID.NotFound, as it does for a moment
// after RunInstances. Everything else goes to fakeEC2.
type spotLaunchEC2 struct {
	*fakeEC2
}

func (f spotLaunchEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	var resp string
	switch form.Get("Action") {
	case "DescribeImages":
		resp = `<DescribeImagesResponse ` + ns + `><requestId>r</requestId><imagesSet><item><imageId>ami-0123456789abcdef0` +
			`</imageId><rootDeviceName>/dev/xvda</rootDeviceName><platformDetails>Linux/UNIX</platformDetails></item></imagesSet></DescribeImagesResponse>`
	case "RunInstances":
		resp = `<RunInstancesResponse ` + ns + `><requestId>r</requestId><reservationId>r-1</reservationId><ownerId>111122223333</ownerId>` +
			`<instancesSet><item><instanceId>i-new</instanceId><instanceType>m8a.4xlarge</instanceType>` +
			`<placement><availabilityZone>us-west-2b</availabilityZone></placement>` +
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`
	case "DescribeInstances":
		f.record(form)
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>The instance ID 'i-new' does not exist</Message></Error></Errors><RequestID>r</RequestID></Response>`))
		return
	case "DescribeSpotPriceHistory":
		resp = `<DescribeSpotPriceHistoryResponse ` + ns + `><requestId>r</requestId><spotPriceHistorySet><item>` +
			`<instanceType>m8a.4xlarge</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>0.360000</spotPrice>` +
			`<timestamp>` + time.Now().UTC().Format(time.RFC3339) + `</timestamp><availabilityZone>us-west-2b</availabilityZone></item></spotPriceHistorySet></DescribeSpotPriceHistoryResponse>`
	default:
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fakeEC2.ServeHTTP(w, r)
		return
	}
	f.record(form)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(resp)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
}

func (f spotLaunchEC2) record(form url.Values) {
	f.mu.Lock()
	f.requests = append(f.requests, form)
	f.mu.Unlock()
}

// The gate for #1442 through a real Client.Launch: a spot launch records its
// spot price without describing the instance it just launched. Before the fix
// the RunInstances response's missing platformDetails sent it to
// DescribeInstances, which EC2 answers NotFound for a moment after launch, and
// the price was never recorded (99 of 99 launches in the owner's spawn.log).
func TestLaunch_SpotPriceRecordedWithoutDescribingTheNewInstance(t *testing.T) {
	immediateSpotPriceRetries(t)
	f := spotLaunchEC2{&fakeEC2{}}
	c := newFakeEC2ClientFor(t, f)
	if _, err := c.Launch(context.Background(), LaunchConfig{
		Name: "spotprice", InstanceType: "m8a.4xlarge", Region: "us-west-2", AMI: "ami-0123456789abcdef0",
		PricePerHour: 1, Spot: true, TTL: "1h",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	recorded := false
	for _, r := range f.requests {
		switch r.Get("Action") {
		case "DescribeInstances":
			if !recorded {
				t.Errorf("Launch described instance %s before recording its spot price; EC2 may not admit it exists yet (#1442)", r.Get("InstanceId.1"))
			}
		case "CreateTags":
			for i := 1; r.Get("Tag."+itoa(i)+".Key") != ""; i++ {
				if r.Get("Tag."+itoa(i)+".Key") == SpotPriceTag && r.Get("Tag."+itoa(i)+".Value") == "0.360000" {
					recorded = true
				}
			}
		}
	}
	if !recorded {
		t.Errorf("no %s=0.360000 CreateTags was sent (actions: %v)", SpotPriceTag, actionsOf(f.requests))
	}
}

// newFakeEC2ClientFor is newFakeEC2Client for any handler.
func newFakeEC2ClientFor(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClientFromConfig(aws.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
}

func actionsOf(reqs []url.Values) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Get("Action"))
	}
	return out
}
