package spotrequest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeReplacementEC2 serves one instance and its Spot request's tags, and
// records the order of cancel calls relative to shutdown.
type fakeReplacementEC2 struct {
	instance     *types.Instance // nil: DescribeInstances returns no match
	describeErr  error
	reqTags      map[string]string
	tagsErr      error
	cancelErr    error
	terminateErr error
	events       *[]string
}

func (f *fakeReplacementEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := &ec2.DescribeInstancesOutput{}
	if f.instance != nil {
		out.Reservations = []types.Reservation{{Instances: []types.Instance{*f.instance}}}
	}
	return out, nil
}

func (f *fakeReplacementEC2) DescribeTags(_ context.Context, in *ec2.DescribeTagsInput, _ ...func(*ec2.Options)) (*ec2.DescribeTagsOutput, error) {
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}
	reqID := in.Filters[0].Values[0]
	out := &ec2.DescribeTagsOutput{}
	for k, v := range f.reqTags {
		out.Tags = append(out.Tags, types.TagDescription{
			ResourceId: aws.String(reqID), ResourceType: types.ResourceTypeSpotInstancesRequest,
			Key: aws.String(k), Value: aws.String(v),
		})
	}
	return out, nil
}

func (f *fakeReplacementEC2) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	*f.events = append(*f.events, "cancel:"+in.SpotInstanceRequestIds[0])
	return &ec2.CancelSpotInstanceRequestsOutput{}, f.cancelErr
}

func (f *fakeReplacementEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	*f.events = append(*f.events, "terminate:"+strings.Join(in.InstanceIds, ","))
	return &ec2.TerminateInstancesOutput{}, f.terminateErr
}

const (
	selfID     = "i-0replacement0000"
	originalID = "i-0original000000"
	reqID      = "sir-leaked"
)

// spawnRequestTags is what a spawn persistent request carries: the original's
// launch tags plus spawn:original-instance-id written after RunInstances.
func spawnRequestTags(original string) map[string]string {
	t := map[string]string{
		"spawn:managed":    "true",
		TypeTag:            TypePersistent,
		"spawn:notify-url": "https://bot.example",
	}
	if original != "" {
		t[OriginalInstanceTag] = original
	}
	return t
}

func instance(id, sir string, tags map[string]string) *types.Instance {
	inst := &types.Instance{InstanceId: aws.String(id)}
	if sir != "" {
		inst.SpotInstanceRequestId = aws.String(sir)
	}
	for k, v := range tags {
		inst.Tags = append(inst.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return inst
}

func newFake(inst *types.Instance, reqTags map[string]string) (*fakeReplacementEC2, *[]string) {
	ev := &[]string{}
	return &fakeReplacementEC2{instance: inst, reqTags: reqTags, events: ev}, ev
}

func recordShutdown(ev *[]string) func(context.Context) error {
	return func(context.Context) error { *ev = append(*ev, "shutdown"); return nil }
}

// The original instance, in every state its request can be in, is never a
// replacement, and retiring it is refused before anything is called.
func TestDetectReplacement_OriginalIsNever(t *testing.T) {
	spawnTags := map[string]string{"spawn:managed": "true", TypeTag: TypePersistent}
	cases := map[string]struct {
		self    map[string]string
		reqTags map[string]string
	}{
		"tagged, request names it":           {spawnTags, spawnRequestTags(selfID)},
		"tagged, request not yet tagged":     {spawnTags, spawnRequestTags("")},
		"tagged, request names another (!)":  {spawnTags, spawnRequestTags(originalID)},
		"own tags unseen, request names it":  {nil, spawnRequestTags(selfID)},
		"own tags unseen, request untagged":  {nil, spawnRequestTags("")},
		"own tags unseen, empty original id": {nil, map[string]string{"spawn:managed": "true", TypeTag: TypePersistent, OriginalInstanceTag: ""}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, ev := newFake(instance(selfID, reqID, tc.self), tc.reqTags)
			d := DetectReplacement(context.Background(), f, selfID)
			if d.Verdict != VerdictNotReplacement {
				t.Fatalf("verdict = %s (%s), want not-replacement", d.Verdict, d.Reason)
			}
			if _, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev)); err == nil {
				t.Fatal("RetireReplacement accepted a not-replacement detection")
			}
			if len(*ev) != 0 {
				t.Fatalf("calls %v on the original, want none", *ev)
			}
		})
	}
}

// An untagged instance whose spawn persistent request names a different
// instance is a replacement. Retiring it cancels the request, then terminates
// the instance itself (#1080), and does NOT shut the OS down: that would leave it
// stopped, untagged and billing.
func TestDetectReplacement_ReplacementIsRetired(t *testing.T) {
	f, ev := newFake(instance(selfID, reqID, nil), spawnRequestTags(originalID))
	d := DetectReplacement(context.Background(), f, selfID)
	if d.Verdict != VerdictReplacement {
		t.Fatalf("verdict = %s (%s), want replacement", d.Verdict, d.Reason)
	}
	if d.RequestID != reqID || d.OriginalInstanceID != originalID {
		t.Fatalf("detection = %+v", d)
	}
	if d.RequestTags["spawn:notify-url"] != "https://bot.example" {
		t.Fatalf("request tags not carried for the notification: %v", d.RequestTags)
	}
	outcome, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev))
	if err != nil || outcome != RetiredTerminated {
		t.Fatalf("RetireReplacement = (%v, %v), want RetiredTerminated", outcome, err)
	}
	if want := []string{"cancel:" + reqID, "terminate:" + selfID}; !equal(*ev, want) {
		t.Fatalf("calls %v, want %v (cancel first, or the terminate reopens the request; and only itself)", *ev, want)
	}
}

// A role from before #1080 refuses the self-terminate. The fallback is the old
// OS shutdown, and the outcome says plainly that this leaves the instance
// stopped, so spored can tell the operator rather than imply it is gone.
func TestRetireReplacement_RefusedTerminateFallsBackLoudly(t *testing.T) {
	f, ev := newFake(instance(selfID, reqID, nil), spawnRequestTags(originalID))
	f.terminateErr = errors.New("UnauthorizedOperation")
	d := DetectReplacement(context.Background(), f, selfID)
	outcome, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev))
	if outcome != RetiredShutdown {
		t.Fatalf("outcome = %v, want RetiredShutdown", outcome)
	}
	if err == nil || !strings.Contains(err.Error(), "STOPPED") {
		t.Errorf("err = %v; a shutdown that leaves the instance stopped must say so", err)
	}
	if want := []string{"cancel:" + reqID, "terminate:" + selfID, "shutdown"}; !equal(*ev, want) {
		t.Fatalf("calls %v, want %v", *ev, want)
	}
}

// If the cancel fails, neither the terminate nor the shutdown may happen: either
// would leave an open persistent request, which launches another replacement.
func TestRetireReplacement_NothingAfterAFailedCancel(t *testing.T) {
	f, ev := newFake(instance(selfID, reqID, nil), spawnRequestTags(originalID))
	f.cancelErr = errors.New("UnauthorizedOperation")
	d := DetectReplacement(context.Background(), f, selfID)
	if outcome, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev)); err == nil || outcome != 0 {
		t.Fatalf("RetireReplacement = (%v, %v) after a failed cancel, want (0, error)", outcome, err)
	}
	if want := []string{"cancel:" + reqID}; !equal(*ev, want) {
		t.Fatalf("calls %v, want only the cancel", *ev)
	}
}

// Anything unreadable or unexpected is unknown or not-replacement, never
// replacement, and nothing is cancelled or shut down.
func TestDetectReplacement_UnknownDoesNothing(t *testing.T) {
	cases := map[string]struct {
		f    *fakeReplacementEC2
		want Verdict
	}{
		"describe instances denied": {&fakeReplacementEC2{describeErr: errors.New("UnauthorizedOperation")}, VerdictUnknown},
		"own instance not returned": {&fakeReplacementEC2{}, VerdictUnknown},
		"request tags denied":       {&fakeReplacementEC2{instance: instance(selfID, reqID, nil), tagsErr: errors.New("UnauthorizedOperation")}, VerdictUnknown},
		"request tags empty":        {&fakeReplacementEC2{instance: instance(selfID, reqID, nil), reqTags: map[string]string{}}, VerdictUnknown},
		"malformed original id":     {&fakeReplacementEC2{instance: instance(selfID, reqID, nil), reqTags: spawnRequestTags("garbage")}, VerdictUnknown},
		"not spot":                  {&fakeReplacementEC2{instance: instance(selfID, "", nil)}, VerdictNotReplacement},
		"request not spawn-managed": {&fakeReplacementEC2{instance: instance(selfID, reqID, nil), reqTags: map[string]string{OriginalInstanceTag: originalID, TypeTag: TypePersistent}}, VerdictNotReplacement},
		"request not persistent":    {&fakeReplacementEC2{instance: instance(selfID, reqID, nil), reqTags: map[string]string{"spawn:managed": "true", TypeTag: TypeOneTime, OriginalInstanceTag: originalID}}, VerdictNotReplacement},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := &[]string{}
			tc.f.events = ev
			d := DetectReplacement(context.Background(), tc.f, selfID)
			if d.Verdict != tc.want {
				t.Fatalf("verdict = %s (%s), want %s", d.Verdict, d.Reason, tc.want)
			}
			if _, err := RetireReplacement(context.Background(), tc.f, d, selfID, recordShutdown(ev)); err == nil {
				t.Fatal("RetireReplacement accepted a non-replacement detection")
			}
			if len(*ev) != 0 {
				t.Fatalf("calls %v, want none", *ev)
			}
		})
	}
	if d := DetectReplacement(context.Background(), &fakeReplacementEC2{}, ""); d.Verdict != VerdictUnknown {
		t.Fatalf("empty own id: verdict %s, want unknown", d.Verdict)
	}
}

// RetireReplacement checks the verdict itself, not only through
// DetectReplacement: a hand-built detection must be refused too.
func TestRetireReplacement_RefusesNonReplacementDirectly(t *testing.T) {
	for _, v := range []Verdict{VerdictUnknown, VerdictNotReplacement} {
		ev := &[]string{}
		f := &fakeReplacementEC2{events: ev}
		d := Detection{Verdict: v, RequestID: reqID, OriginalInstanceID: originalID}
		if _, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev)); err == nil || len(*ev) != 0 {
			t.Fatalf("verdict %s: err=%v calls=%v, want refusal and no calls", v, err, *ev)
		}
	}
}

// A request that is already cancelled (EC2 accepts the repeat cancel) or that
// EC2 no longer knows still ends the replacement: nothing is left to relaunch.
// A cancel that fails for any other reason ends nothing.
func TestRetireReplacement_RequestAlreadyCancelled(t *testing.T) {
	cases := map[string]struct {
		cancelErr error
		wantEnded bool
	}{
		"already cancelled":    {nil, true},
		"request gone":         {errors.New("api error InvalidSpotInstanceRequestID.NotFound: gone"), true},
		"cancel denied":        {errors.New("UnauthorizedOperation: not authorized"), false},
		"cancel throttled etc": {errors.New("RequestLimitExceeded"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, ev := newFake(instance(selfID, reqID, nil), spawnRequestTags(originalID))
			f.cancelErr = tc.cancelErr
			d := DetectReplacement(context.Background(), f, selfID)
			if d.Verdict != VerdictReplacement {
				t.Fatalf("verdict = %s (%s)", d.Verdict, d.Reason)
			}
			outcome, err := RetireReplacement(context.Background(), f, d, selfID, recordShutdown(ev))
			ended := equal(*ev, []string{"cancel:" + reqID, "terminate:" + selfID})
			if ended != tc.wantEnded {
				t.Fatalf("calls %v (err %v); ended=%v, want %v", *ev, err, ended, tc.wantEnded)
			}
			if (err == nil && outcome == RetiredTerminated) != tc.wantEnded {
				t.Fatalf("(%v, %v), want RetiredTerminated iff ended", outcome, err)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// With no instance id to terminate, nothing may happen at all, not even the
// cancel: cancelling and then being unable to terminate leaves the replacement
// to the OS shutdown, which only stops it (#1080).
func TestRetireReplacement_RefusesWithoutAnInstanceID(t *testing.T) {
	f, ev := newFake(instance(selfID, reqID, nil), spawnRequestTags(originalID))
	d := DetectReplacement(context.Background(), f, selfID)
	if _, err := RetireReplacement(context.Background(), f, d, "", recordShutdown(ev)); err == nil {
		t.Fatal("RetireReplacement accepted an empty instance id")
	}
	if len(*ev) != 0 {
		t.Fatalf("calls %v, want none", *ev)
	}
}
