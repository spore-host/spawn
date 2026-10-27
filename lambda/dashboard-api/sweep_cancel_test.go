package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// awsCall is one request the dashboard made to AWS, as the wire saw it.
type awsCall struct {
	service string // host prefix: dynamodb, lambda, sts, ec2, ...
	target  string // X-Amz-Target (JSON protocols) or the Action= (query protocols)
	path    string
	headers http.Header
	body    string
}

// wireAWS serves the dashboard's AWS calls in process and records every one, so
// a test sees which services were called, with what, whatever client made the
// call.
type wireAWS struct {
	mu    sync.Mutex
	calls []awsCall
	item  string // the DynamoDB item GetItem returns, in DynamoDB JSON
}

func (w *wireAWS) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	service, _, _ := strings.Cut(req.URL.Host, ".")
	c := awsCall{service: service, target: req.Header.Get("X-Amz-Target"), path: req.URL.Path, headers: req.Header.Clone(), body: string(b)}
	if c.target == "" {
		if i := strings.Index(c.body, "Action="); i >= 0 {
			c.target, _, _ = strings.Cut(c.body[i+len("Action="):], "&")
		}
	}
	w.mu.Lock()
	w.calls = append(w.calls, c)
	w.mu.Unlock()

	status, body, ctype := 200, "{}", "application/x-amz-json-1.0"
	switch {
	case service == "dynamodb" && strings.HasSuffix(c.target, ".GetItem"):
		body = `{"Item":` + w.item + `}`
	case service == "dynamodb":
	case service == "lambda":
		status, body, ctype = 202, "", "application/json"
	default:
		status, ctype = 403, "text/xml"
		body = `<ErrorResponse><Error><Code>AccessDenied</Code><Message>not served by this test</Message></Error></ErrorResponse>`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{ctype}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    req,
	}, nil
}

func (w *wireAWS) of(service string) []awsCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []awsCall
	for _, c := range w.calls {
		if c.service == service {
			out = append(out, c)
		}
	}
	return out
}

const cancelOwner = "arn:aws:iam::435415984226:user/alice"

func runningSweepItem() string {
	return `{"sweep_id":{"S":"sw-a"},"user_id":{"S":"` + cancelOwner + `"},"status":{"S":"RUNNING"},` +
		`"aws_account_id":{"S":"435415984226"},"region":{"S":"us-east-1"},` +
		`"instances":{"L":[{"M":{"index":{"N":"0"},"instance_id":{"S":"i-1"},"state":{"S":"running"}}},` +
		`{"M":{"index":{"N":"1"},"instance_id":{"S":"i-2"},"state":{"S":"terminated"}}}]}}`
}

func cancelOnTheWire(t *testing.T, item string) (*wireAWS, int, CancelSweepResponse) {
	t.Helper()
	w := &wireAWS{item: item}
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "not-a-secret", ""),
		HTTPClient:  &http.Client{Transport: w},
	}
	resp, err := handleCancelSweep(context.Background(), cfg, "sw-a", cancelOwner)
	if err != nil {
		t.Fatalf("handleCancelSweep: %v", err)
	}
	var body CancelSweepResponse
	_ = json.Unmarshal([]byte(resp.Body), &body)
	return w, resp.StatusCode, body
}

// TestDashboardSweepCancelAssumesNoRole is #1349: the dashboard's cancel used
// to assume SpawnSweepCrossAccountRole, whose trust names only the sweep
// orchestrator, so it was always denied and the members ran to their TTL. The
// cancel now makes no STS call and touches no EC2: it marks the record and
// leaves the terminate to the orchestrator.
func TestDashboardSweepCancelAssumesNoRole(t *testing.T) {
	w, status, body := cancelOnTheWire(t, runningSweepItem())
	if status != 200 || !body.Success {
		t.Fatalf("cancel returned %d %+v", status, body)
	}
	if sts := w.of("sts"); len(sts) != 0 {
		t.Errorf("the cancel called STS %d time(s) (%s): the dashboard role cannot assume into the member account", len(sts), sts[0].target)
	}
	if ec2 := w.of("ec2"); len(ec2) != 0 {
		t.Errorf("the cancel called EC2 (%s): only the sweep orchestrator terminates members", ec2[0].target)
	}
	var marked bool
	for _, c := range w.of("dynamodb") {
		if strings.HasSuffix(c.target, ".UpdateItem") && strings.Contains(c.body, `"CANCELLED"`) && strings.Contains(c.body, "cancel_requested") {
			marked = true
		}
	}
	if !marked {
		t.Error("the cancel did not mark the record CANCELLED with cancel_requested")
	}
	if body.InstancesTerminated != 0 || body.InstancesToTerminate != 1 {
		t.Errorf("response says %d terminated, %d to terminate; want 0 and 1 (the dashboard terminates nothing)",
			body.InstancesTerminated, body.InstancesToTerminate)
	}
	if !strings.Contains(body.Message, "orchestrator") {
		t.Errorf("message %q does not say the orchestrator terminates the instances", body.Message)
	}
}

// The cancel starts the orchestrator, asynchronously, with the sweep's ID in the
// payload the orchestrator decodes (sweep_id, pkg/sweep.OrchestratorEvent), and
// only after the record says CANCELLED: an orchestrator started first would
// find nothing to do.
func TestDashboardSweepCancelInvokesTheOrchestrator(t *testing.T) {
	w, _, _ := cancelOnTheWire(t, runningSweepItem())
	inv := w.of("lambda")
	if len(inv) != 1 {
		t.Fatalf("the cancel invoked Lambda %d times, want once", len(inv))
	}
	c := inv[0]
	if want := "/2015-03-31/functions/" + defaultSweepOrchestratorFunction + "/invocations"; c.path != want {
		t.Errorf("invoked %s, want %s", c.path, want)
	}
	if got := c.headers.Get("X-Amz-Invocation-Type"); got != "Event" {
		t.Errorf("invocation type %q, want Event: the orchestrator polls for minutes", got)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(c.body), &ev); err != nil || ev["sweep_id"] != "sw-a" {
		t.Errorf("payload %s does not carry sweep_id sw-a", c.body)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	updated, invoked := -1, -1
	for i, call := range w.calls {
		if call.service == "dynamodb" && strings.HasSuffix(call.target, ".UpdateItem") {
			updated = i
		}
		if call.service == "lambda" {
			invoked = i
		}
	}
	if updated < 0 || invoked < updated {
		t.Errorf("orchestrator invoked (call %d) before the record was marked cancelled (call %d)", invoked, updated)
	}
}

// Someone else's sweep is refused before anything is written or invoked.
func TestDashboardSweepCancelChecksOwnershipFirst(t *testing.T) {
	item := strings.Replace(runningSweepItem(), cancelOwner, "arn:aws:iam::435415984226:user/mallory", 1)
	w, status, _ := cancelOnTheWire(t, item)
	if status != 403 {
		t.Errorf("cancelling another user's sweep returned %d, want 403", status)
	}
	if n := len(w.of("lambda")); n != 0 {
		t.Errorf("another user's sweep: the orchestrator was invoked %d time(s)", n)
	}
	for _, c := range w.of("dynamodb") {
		if !strings.HasSuffix(c.target, ".GetItem") {
			t.Errorf("another user's sweep: DynamoDB %s was called", c.target)
		}
	}
}
