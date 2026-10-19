package aws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
)

// fakeVerifyAPI answers the EC2 Query and SSM JSON calls spored verification
// makes, and counts them. An action named in deny is refused the way AWS refuses
// it: UnauthorizedOperation for EC2, AccessDeniedException for SSM.
type fakeVerifyAPI struct {
	mu        sync.Mutex
	calls     map[string]int
	deny      map[string]bool
	heartbeat string // spawn:last-heartbeat value; "" means no tag
	noProfile bool
}

func (f *fakeVerifyAPI) count(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[action]
}

func (f *fakeVerifyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var action string
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		action = strings.TrimPrefix(target, "AmazonSSM.")
	} else {
		form, _ := url.ParseQuery(string(body))
		action = form.Get("Action")
	}
	f.mu.Lock()
	f.calls[action]++
	denied := f.deny[action]
	f.mu.Unlock()

	if target := r.Header.Get("X-Amz-Target"); target != "" {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		if denied {
			w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"not authorized"}`))
			return
		}
		switch action {
		case "DescribeInstanceInformation":
			_, _ = w.Write([]byte(`{"InstanceInformationList":[]}`))
		case "SendCommand":
			_, _ = w.Write([]byte(`{"Command":{"CommandId":"cmd-1"}}`))
		case "GetCommandInvocation":
			_, _ = w.Write([]byte(`{"Status":"InProgress"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"InvalidAction","message":"not in this fake"}`))
		}
		return
	}

	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	if denied {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not authorized</Message></Error></Errors><RequestID>r</RequestID></Response>`))
		return
	}
	if action != "DescribeInstances" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<Response><Errors><Error><Code>InvalidAction</Code><Message>not in this fake</Message></Error></Errors><RequestID>r</RequestID></Response>`))
		return
	}
	profile := `<iamInstanceProfile><arn>arn:aws:iam::123456789012:instance-profile/spored-instance-profile</arn><id>AIPA</id></iamInstanceProfile>`
	if f.noProfile {
		profile = ""
	}
	tags := ""
	if f.heartbeat != "" {
		tags = `<tagSet><item><key>` + HeartbeatTag + `</key><value>` + f.heartbeat + `</value></item></tagSet>`
	}
	// nosemgrep
	_, _ = w.Write([]byte(`<DescribeInstancesResponse ` + ns + `><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet><item>` +
		`<instanceId>i-0123456789abcdef0</instanceId><instanceState><code>16</code><name>running</name></instanceState>` + profile + tags +
		`</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`))
}

func newFakeVerifyClient(t *testing.T, f *fakeVerifyAPI) *Client {
	t.Helper()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewClientFromConfig(aws.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
}

const fakeInstance = "i-0123456789abcdef0"

func wantDenied(t *testing.T, err error, action string) {
	t.Helper()
	var ade *AccessDeniedError
	if !errors.As(err, &ade) {
		t.Fatalf("err = %v, want an *AccessDeniedError naming %s", err, action)
	}
	if ade.Action != action {
		t.Errorf("AccessDeniedError.Action = %q, want %q", ade.Action, action)
	}
	if !strings.Contains(err.Error(), action) {
		t.Errorf("message %q does not name %s", err.Error(), action)
	}
}

// #1126: an AccessDenied from DescribeInstanceInformation is the caller's
// missing permission, and must come back at once as that — not be polled until
// the deadline and then reported as an instance that never registered.
func TestWaitForSSMOnline_AccessDeniedIsTerminalAndNamed(t *testing.T) {
	f := &fakeVerifyAPI{deny: map[string]bool{"DescribeInstanceInformation": true}}
	c := newFakeVerifyClient(t, f)
	err := c.WaitForSSMOnline(context.Background(), "us-west-2", fakeInstance, 2*time.Second)
	wantDenied(t, err, "ssm:DescribeInstanceInformation")
	if n := f.count("DescribeInstanceInformation"); n != 1 {
		t.Errorf("DescribeInstanceInformation called %d times, want 1: a refusal is not retried", n)
	}
}

// SSMOnline is the one-shot form verification uses; it must classify the same way.
func TestSSMOnline_AccessDeniedIsNamed(t *testing.T) {
	f := &fakeVerifyAPI{deny: map[string]bool{"DescribeInstanceInformation": true}}
	_, err := newFakeVerifyClient(t, f).SSMOnline(context.Background(), "us-west-2", fakeInstance)
	wantDenied(t, err, "ssm:DescribeInstanceInformation")
}

// The probe after registration: SendCommand and GetCommandInvocation refusals
// are terminal and named too. GetCommandInvocation was polled until its own
// timeout, because every error there was treated as "not ready yet".
func TestRunCommand_AccessDeniedIsTerminalAndNamed(t *testing.T) {
	for _, tc := range []struct{ deny, action string }{
		{"SendCommand", "ssm:SendCommand"},
		{"GetCommandInvocation", "ssm:GetCommandInvocation"},
	} {
		for name, run := range map[string]func(*Client) error{
			"shell": func(c *Client) error {
				_, err := c.RunShellScript(context.Background(), "us-west-2", fakeInstance, "true", 5*time.Second)
				return err
			},
			"powershell": func(c *Client) error {
				_, err := c.RunPowerShell(context.Background(), "us-west-2", fakeInstance, "exit 0", 5*time.Second)
				return err
			},
		} {
			t.Run(tc.deny+"/"+name, func(t *testing.T) {
				f := &fakeVerifyAPI{deny: map[string]bool{tc.deny: true}}
				wantDenied(t, run(newFakeVerifyClient(t, f)), tc.action)
				if n := f.count(tc.deny); n != 1 {
					t.Errorf("%s called %d times, want 1: a refusal is not retried", tc.deny, n)
				}
			})
		}
	}
}

// DescribeSporedLiveness reads the heartbeat tag and profile in one call, and
// names ec2:DescribeInstances when refused.
func TestDescribeSporedLiveness(t *testing.T) {
	f := &fakeVerifyAPI{heartbeat: "2026-10-19T12:00:00Z"}
	l, err := newFakeVerifyClient(t, f).DescribeSporedLiveness(context.Background(), "us-west-2", fakeInstance)
	if err != nil {
		t.Fatal(err)
	}
	if l.Heartbeat != "2026-10-19T12:00:00Z" || !l.HasInstanceProfile || l.State != "running" {
		t.Errorf("liveness = %+v, want the heartbeat, a profile and running", l)
	}

	f = &fakeVerifyAPI{noProfile: true}
	l, err = newFakeVerifyClient(t, f).DescribeSporedLiveness(context.Background(), "us-west-2", fakeInstance)
	if err != nil {
		t.Fatal(err)
	}
	if l.Heartbeat != "" || l.HasInstanceProfile {
		t.Errorf("liveness = %+v, want no heartbeat and no profile", l)
	}

	f = &fakeVerifyAPI{deny: map[string]bool{"DescribeInstances": true}}
	_, err = newFakeVerifyClient(t, f).DescribeSporedLiveness(context.Background(), "us-west-2", fakeInstance)
	wantDenied(t, err, "ec2:DescribeInstances")
}

func TestIsAccessDenied(t *testing.T) {
	for code, want := range map[string]bool{
		"AccessDenied": true, "AccessDeniedException": true, "UnauthorizedOperation": true,
		"ThrottlingException": false, "InvalidInstanceId": false,
	} {
		err := &smithy.GenericAPIError{Code: code}
		if got := IsAccessDenied(err); got != want {
			t.Errorf("IsAccessDenied(%s) = %v, want %v", code, got, want)
		}
	}
	if IsAccessDenied(nil) || IsAccessDenied(errors.New("AccessDenied in some prose")) {
		t.Error("IsAccessDenied matched something that is not an AWS authorization error")
	}
}
