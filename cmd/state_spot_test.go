package cmd

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

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// recordingEC2 answers every EC2 query call with an empty success and records
// the actions, so a test can assert what `spawn stop` did — or that it did nothing.
type recordingEC2 struct {
	mu      sync.Mutex
	actions []string
}

func (f *recordingEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.actions = append(f.actions, form.Get("Action"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	a := form.Get("Action")
	_, _ = w.Write([]byte(`<` + a + `Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId></` + a + `Response>`)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- test fixture: a fake EC2 Query API answering the SDK in XML, not HTML served to a browser
}

func recordingClient(t *testing.T, f *recordingEC2) *aws.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return aws.NewClientFromConfig(awssdk.Config{
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: awssdk.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
}

func runningSpot(spotType string) *aws.InstanceInfo {
	tags := map[string]string{
		"spawn:managed":      "true",
		"spawn:ttl-deadline": time.Now().Add(22 * time.Minute).UTC().Format(time.RFC3339),
	}
	if spotType != "" {
		tags[spotrequest.TypeTag] = spotType
	}
	return &aws.InstanceInfo{
		InstanceID:   "i-028c23f816d2eb68e",
		Name:         "demo",
		State:        "running",
		Region:       "us-west-2",
		SpotInstance: true,
		Tags:         tags,
	}
}

// #922 option 3: `spawn stop` on a one-time Spot instance used to print "Saving
// remaining TTL: 22m" and then fail with EC2's UnsupportedOperation. It must
// explain up front — before printing anything about saving TTL, and without
// making a single AWS call.
func TestStop_OneTimeSpotExplainsUpFront(t *testing.T) {
	for _, hibernate := range []bool{false, true} {
		for _, spotType := range []string{"", spotrequest.TypeOneTime} {
			f := &recordingEC2{}
			client := recordingClient(t, f)
			var err error
			stderr := captureStderr(t, func() {
				err = stopOrHibernateInstance(context.Background(), client, runningSpot(spotType), hibernate, true)
			})
			if err == nil {
				t.Fatalf("hibernate=%v type=%q: stop of a one-time Spot instance succeeded", hibernate, spotType)
			}
			for _, want := range []string{"one-time Spot", "persistent", "spawn terminate i-028c23f816d2eb68e", "--idle-timeout"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("hibernate=%v type=%q: error should mention %q:\n%v", hibernate, spotType, want, err)
				}
			}
			if strings.Contains(stderr, "Saving remaining TTL") || strings.Contains(stderr, "Requesting") {
				t.Errorf("hibernate=%v type=%q: printed progress before refusing:\n%s", hibernate, spotType, stderr)
			}
			if len(f.actions) != 0 {
				t.Errorf("hibernate=%v type=%q: made AWS calls %v before refusing", hibernate, spotType, f.actions)
			}
		}
	}
}

// A persistent Spot instance — what --spot with a stop-based lifecycle now
// launches — is stopped like any other.
func TestStop_PersistentSpotIsStopped(t *testing.T) {
	f := &recordingEC2{}
	client := recordingClient(t, f)
	var err error
	stderr := captureStderr(t, func() {
		err = stopOrHibernateInstance(context.Background(), client, runningSpot(spotrequest.TypePersistent), false, true)
	})
	if err != nil {
		t.Fatalf("stop of a persistent Spot instance: %v", err)
	}
	if !strings.Contains(strings.Join(f.actions, ","), "StopInstances") {
		t.Errorf("no StopInstances call: %v", f.actions)
	}
	if !strings.Contains(stderr, "Saving remaining TTL") {
		t.Errorf("the TTL save should still happen for a stoppable instance:\n%s", stderr)
	}
}

// #922: `spawn status` promised "stops the instance when idle" for a one-time
// Spot instance EC2 cannot stop.
func TestIdleActionDescription(t *testing.T) {
	cases := []struct {
		name    string
		inst    *aws.InstanceInfo
		want    string
		mustNot string
	}{
		{"one-time spot (untagged, pre-#922)", runningSpot(""), "can't be stopped", "stops the instance"},
		{"one-time spot", runningSpot(spotrequest.TypeOneTime), "can't be stopped", "stops the instance"},
		{"persistent spot", runningSpot(spotrequest.TypePersistent), "stops the instance when idle", "can't"},
		{"on-demand", &aws.InstanceInfo{Tags: map[string]string{}}, "stops the instance when idle", "can't"},
		{"on-demand hibernate-on-idle, configured", &aws.InstanceInfo{HibernationConfigured: true, Tags: map[string]string{"spawn:hibernate-on-idle": "true"}}, "hibernates", "stops"},
		// #949: an instance not configured for hibernation is stopped, not
		// hibernated — what --on-idle hibernate launched before the fix.
		{"on-demand hibernate-on-idle, not configured", &aws.InstanceInfo{Tags: map[string]string{"spawn:hibernate-on-idle": "true"}}, "not configured", "hibernates the instance"},
	}
	for _, tc := range cases {
		got := idleActionDescription(tc.inst)
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.mustNot) {
			t.Errorf("%s: %q, want it to contain %q and not %q", tc.name, got, tc.want, tc.mustNot)
		}
	}

	// And through the block status actually prints.
	inst := runningSpot("")
	inst.IdleTimeout = "15m"
	block := lifecycleProtectionBlock(inst)
	if strings.Contains(block, "stops the instance when idle") || !strings.Contains(block, "Idle timeout:          15m (cannot act") {
		t.Errorf("status block misdescribes a one-time Spot idle timeout:\n%s", block)
	}
}

// #950: a stopped persistent-Spot instance whose request passed the ValidUntil
// spawn set at launch can't be started through it. EC2's error doesn't say
// so; spawn start does.
func TestStartFailedError_NamesAnExpiredSpotRequest(t *testing.T) {
	vu := time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	inst := runningSpot(spotrequest.TypePersistent)
	inst.Tags[spotrequest.ValidUntilTag] = vu.Format(time.RFC3339)
	cause := errors.New("IncorrectSpotRequestState")

	err := startFailedError(inst, cause, vu.Add(time.Hour))
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "Spot request expired at 2026-10-15 15:00 UTC") {
		t.Errorf("after expiry: %v", err)
	}
	if err := startFailedError(inst, cause, vu.Add(-time.Hour)); strings.Contains(err.Error(), "expired") {
		t.Errorf("before expiry the request is not the cause: %v", err)
	}
}
