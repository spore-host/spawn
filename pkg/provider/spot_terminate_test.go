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
		_, _ = w.Write([]byte(`<TerminateInstancesResponse ` + ns + `><requestId>r</requestId><instancesSet/></TerminateInstancesResponse>`))
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
	if got := strings.Join(f.actions, ","); got != "DescribeInstances,CancelSpotInstanceRequests,TerminateInstances" {
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
	if a := f.actions; len(a) == 0 || a[len(a)-1] != "TerminateInstances" {
		t.Errorf("actions = %v, want a terminate despite the refused cancel", a)
	}
}
