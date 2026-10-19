package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func describeClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClientFromConfig(aws.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
}

// TestLiveInstancesWithTagFiltersByTagAndLiveState: only instances carrying the
// tag come back, and the describe asks for exactly the states in which an
// instance may still use its instance profile.
func TestLiveInstancesWithTagFiltersByTagAndLiveState(t *testing.T) {
	var sent url.Values
	c := describeClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent, _ = url.ParseQuery(string(body))
		item := func(id, run string) string {
			return `<item><instanceId>` + id + `</instanceId><instanceState><code>16</code><name>running</name></instanceState><tagSet>` +
				`<item><key>spawn:managed</key><value>true</value></item><item><key>spawn:pool-run-id</key><value>` + run + `</value></item></tagSet></item>`
		}
		// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter, go.lang.security.injection.raw-html-format.raw-html-format -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
		_, _ = w.Write([]byte(`<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet>` +
			item("i-a", "run-a") + item("i-b", "run-b") + `</instancesSet></item></reservationSet></DescribeInstancesResponse>`))
	})
	got, err := c.LiveInstancesWithTag(context.Background(), "us-east-1", "spawn:pool-run-id", "run-a")
	if err != nil {
		t.Fatalf("LiveInstancesWithTag: %v", err)
	}
	if len(got) != 1 || got[0].InstanceID != "i-a" {
		t.Fatalf("got %+v, want only i-a", got)
	}
	states := map[string]bool{}
	for k, v := range sent {
		if len(k) > 7 && k[:7] == "Filter." && v[0] == "instance-state-name" {
			prefix := k[:len(k)-len("Name")]
			for i := 1; ; i++ {
				s := sent.Get(prefix + "Value." + itoa(i))
				if s == "" {
					break
				}
				states[s] = true
			}
		}
	}
	for _, s := range []string{"pending", "running", "stopping", "stopped"} {
		if !states[s] {
			t.Errorf("describe does not ask for %q instances (sent states %v)", s, states)
		}
	}
}

// TestLiveInstancesWithTagReturnsTheDescribeError: callers delete a role when
// this comes back empty, so a failed describe must be an error, not "none".
func TestLiveInstancesWithTagReturnsTheDescribeError(t *testing.T) {
	c := describeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>r</RequestID></Response>`))
	})
	got, err := c.LiveInstancesWithTag(context.Background(), "us-east-1", "spawn:pool-run-id", "run-a")
	if err == nil {
		t.Fatalf("a denied describe returned %v and no error", got)
	}
}
