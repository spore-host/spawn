package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeMemberEC2 is one region of a member account. DescribeInstances returns
// EVERY instance whatever the filters say, so the ownership check in the code,
// not EC2's filter, is what these tests prove keeps other instances alive.
type fakeMemberEC2 struct {
	mu        sync.Mutex
	instances []ec2types.Instance
	calls     []string // "terminate:<id>" and "cancel-spot:<req>", in order
}

func (f *fakeMemberEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: append([]ec2types.Instance(nil), f.instances...)}}}, nil
}

func (f *fakeMemberEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.TerminateInstancesOutput{}
	for _, id := range in.InstanceIds {
		f.calls = append(f.calls, "terminate:"+id)
		for i := range f.instances {
			if awssdk.ToString(f.instances[i].InstanceId) != id {
				continue
			}
			prev := f.instances[i].State.Name
			f.instances[i].State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}
			out.TerminatingInstances = append(out.TerminatingInstances, ec2types.InstanceStateChange{
				InstanceId:    awssdk.String(id),
				PreviousState: &ec2types.InstanceState{Name: prev},
				CurrentState:  &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown},
			})
		}
	}
	return out, nil
}

func (f *fakeMemberEC2) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.SpotInstanceRequestIds {
		f.calls = append(f.calls, "cancel-spot:"+id)
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (f *fakeMemberEC2) terminated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, c := range f.calls {
		if id, ok := strings.CutPrefix(c, "terminate:"); ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func fakeInstance(id string, state ec2types.InstanceStateName, spotReq string, tags map[string]string) ec2types.Instance {
	inst := ec2types.Instance{InstanceId: awssdk.String(id), State: &ec2types.InstanceState{Name: state}}
	if spotReq != "" {
		inst.SpotInstanceRequestId = awssdk.String(spotReq)
	}
	for k, v := range tags {
		inst.Tags = append(inst.Tags, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return inst
}

func member(sweepID string) map[string]string {
	return map[string]string{"spawn:managed": "true", "spawn:sweep-id": sweepID}
}

type cancelRecord struct {
	sweepID    string
	terminated int
}

// cancelHarness wires the handler's seams to fakes: the record loadState
// returns, one fake EC2 per region memberClientsFor is asked for, and the
// outcomes recordCancelOutcome is asked to write (which also become the
// record's state, as DynamoDB would hold it).
type cancelHarness struct {
	record  *SweepRecord
	regions map[string]*fakeMemberEC2
	written []cancelRecord
}

func newCancelHarness(t *testing.T, record *SweepRecord, regions map[string]*fakeMemberEC2) *cancelHarness {
	t.Helper()
	h := &cancelHarness{record: record, regions: regions}
	prevLoad, prevClients, prevRecord := loadState, memberClientsFor, recordCancelOutcome
	t.Cleanup(func() { loadState, memberClientsFor, recordCancelOutcome = prevLoad, prevClients, prevRecord })
	loadState = func(_ context.Context, id string) (*SweepRecord, error) {
		if id != h.record.SweepID {
			return nil, errors.New("sweep not found")
		}
		cp := *h.record
		return &cp, nil
	}
	memberClientsFor = func(_ context.Context, state *SweepRecord) (map[string]memberEC2API, error) {
		out := map[string]memberEC2API{}
		for _, r := range sweepRegions(state) {
			if f, ok := h.regions[r]; ok {
				out[r] = f
			}
		}
		return out, nil
	}
	recordCancelOutcome = func(_ context.Context, id string, n int) error {
		h.written = append(h.written, cancelRecord{id, n})
		h.record.Status = "CANCELLED"
		h.record.CancelRequested = true
		h.record.CancelTerminated += n
		return nil
	}
	return h
}

// invoke runs the handler as the dashboard's invoke starts it: from the JSON
// payload, decoded into the handler's own event type.
func invoke(t *testing.T, sweepID string) {
	t.Helper()
	var ev SweepEvent
	if err := json.Unmarshal([]byte(`{"sweep_id":"`+sweepID+`"}`), &ev); err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), ev); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

// TestCancelledSweepTerminatesItsMembersOnly is #1349: an invocation that finds
// its sweep cancelled terminates every live member, in every region the sweep
// has, cancelling a Spot member's request before its instance, and leaves alone
// another sweep's member, an untagged instance, an instance carrying the sweep
// ID without spawn:managed, and a member already gone. It records CANCELLED
// with the count.
func TestCancelledSweepTerminatesItsMembersOnly(t *testing.T) {
	east := &fakeMemberEC2{instances: []ec2types.Instance{
		fakeInstance("i-run", ec2types.InstanceStateNameRunning, "", member("sw-a")),
		fakeInstance("i-spot", ec2types.InstanceStateNameStopped, "sir-1", member("sw-a")),
		fakeInstance("i-pending", ec2types.InstanceStateNamePending, "", member("sw-a")),
		fakeInstance("i-gone", ec2types.InstanceStateNameTerminated, "", member("sw-a")),
		fakeInstance("i-other-sweep", ec2types.InstanceStateNameRunning, "", member("sw-b")),
		fakeInstance("i-untagged", ec2types.InstanceStateNameRunning, "", nil),
		fakeInstance("i-unmanaged", ec2types.InstanceStateNameRunning, "", map[string]string{"spawn:sweep-id": "sw-a"}),
	}}
	west := &fakeMemberEC2{instances: []ec2types.Instance{
		fakeInstance("i-west", ec2types.InstanceStateNameRunning, "", member("sw-a")),
		fakeInstance("i-west-other", ec2types.InstanceStateNameRunning, "", member("sw-b")),
	}}
	h := newCancelHarness(t, &SweepRecord{
		SweepID: "sw-a", Status: "CANCELLED", CancelRequested: true, AWSAccountID: "435415984226",
		Region: "us-east-1", MultiRegion: true,
		RegionStatus: map[string]*RegionProgress{"us-east-1": {}, "us-west-2": {}},
		// The record lists only one member: a member launched after the last
		// save is found by its tags, not the record.
		Instances: []SweepInstance{{InstanceID: "i-run", Region: "us-east-1", State: "running"}},
	}, map[string]*fakeMemberEC2{"us-east-1": east, "us-west-2": west})

	invoke(t, "sw-a")

	if got, want := strings.Join(east.terminated(), ","), "i-pending,i-run,i-spot"; got != want {
		t.Errorf("us-east-1 terminated %q, want %q", got, want)
	}
	if got, want := strings.Join(west.terminated(), ","), "i-west"; got != want {
		t.Errorf("us-west-2 terminated %q, want %q", got, want)
	}
	cancelAt, termAt := -1, -1
	for i, c := range east.calls {
		switch c {
		case "cancel-spot:sir-1":
			cancelAt = i
		case "terminate:i-spot":
			termAt = i
		}
	}
	if cancelAt < 0 || cancelAt > termAt {
		t.Errorf("Spot request sir-1 not cancelled before its instance was terminated (calls %v): a persistent request would relaunch it (#922)", east.calls)
	}
	if len(h.written) != 1 || h.written[0] != (cancelRecord{"sw-a", 4}) {
		t.Errorf("recorded %+v, want one CANCELLED outcome of 4 terminated", h.written)
	}
}

// A second run, and any later invocation of a cancelled sweep, does nothing:
// no terminate, no Spot cancel, no write.
func TestCancelReRunIsANoOp(t *testing.T) {
	east := &fakeMemberEC2{instances: []ec2types.Instance{
		fakeInstance("i-run", ec2types.InstanceStateNameRunning, "sir-2", member("sw-a")),
	}}
	h := newCancelHarness(t, &SweepRecord{SweepID: "sw-a", Status: "CANCELLED", CancelRequested: true, Region: "us-east-1"},
		map[string]*fakeMemberEC2{"us-east-1": east})

	invoke(t, "sw-a")
	first, writes := len(east.calls), len(h.written)
	if first == 0 || writes != 1 {
		t.Fatalf("first run made %d EC2 calls and %d writes; the setup is wrong", first, writes)
	}
	invoke(t, "sw-a")
	if extra := east.calls[first:]; len(extra) != 0 {
		t.Errorf("re-run made EC2 calls %v, want none", extra)
	}
	if len(h.written) != writes {
		t.Errorf("re-run wrote %+v, want no write", h.written[writes:])
	}
	if h.record.CancelTerminated != 1 || h.record.Status != "CANCELLED" {
		t.Errorf("record after re-run: status %q, cancel_terminated %d; want CANCELLED, 1", h.record.Status, h.record.CancelTerminated)
	}
}

// A cancel flagged by cancel_requested alone (status not yet CANCELLED) is
// still a cancel, and its outcome is written so the status says so.
func TestCancelRequestedAloneIsACancel(t *testing.T) {
	east := &fakeMemberEC2{}
	h := newCancelHarness(t, &SweepRecord{SweepID: "sw-a", Status: "RUNNING", CancelRequested: true, Region: "us-east-1"},
		map[string]*fakeMemberEC2{"us-east-1": east})
	invoke(t, "sw-a")
	if len(h.written) != 1 || h.record.Status != "CANCELLED" {
		t.Errorf("status %q after a cancel_requested-only run, writes %+v; want CANCELLED recorded", h.record.Status, h.written)
	}
}

// recordingTransport answers every DynamoDB request with a conditional-check
// failure and keeps the request bodies.
type recordingTransport struct {
	bodies []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	r.bodies = append(r.bodies, string(b))
	return &http.Response{
		StatusCode: 400,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"__type":"com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException","message":"The conditional request failed"}`)),
		Request: req,
	}, nil
}

// The polling loops save their own copy of the record. That save must not
// overwrite a cancel written since the copy was read: it would reset status
// and cancel_requested, and the loop would launch on (#1349).
func TestLoopSaveDoesNotClobberACancel(t *testing.T) {
	rt := &recordingTransport{}
	prev := dynamodbClient
	t.Cleanup(func() { dynamodbClient = prev })
	dynamodbClient = dynamodb.New(dynamodb.Options{
		Region:      "us-east-1",
		Credentials: awssdk.AnonymousCredentials{},
		HTTPClient:  &http.Client{Transport: rt},
	})

	err := saveSweepState(context.Background(), &SweepRecord{SweepID: "sw-a", Status: "RUNNING"})
	if !errors.Is(err, errSweepCancelled) {
		t.Fatalf("save over a cancelled record returned %v, want errSweepCancelled", err)
	}
	if len(rt.bodies) != 1 {
		t.Errorf("a refused save was retried %d times; a cancel will not go away", len(rt.bodies)-1)
	}
	if !strings.Contains(rt.bodies[0], `"ConditionExpression":"NOT (#status = :cancelled) AND NOT (cancel_requested = :t)"`) {
		t.Errorf("PutItem carries no condition protecting a cancel: %s", rt.bodies[0])
	}
}
