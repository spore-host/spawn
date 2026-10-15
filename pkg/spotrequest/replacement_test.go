package spotrequest

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeReplacementEC2 serves one instance and its Spot request's tags, and
// records the order of cancel calls relative to shutdown.
type fakeReplacementEC2 struct {
	instance    *types.Instance // nil: DescribeInstances returns no match
	describeErr error
	reqTags     map[string]string
	tagsErr     error
	cancelErr   error
	events      *[]string
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
			if err := RetireReplacement(context.Background(), f, d, recordShutdown(ev)); err == nil {
				t.Fatal("RetireReplacement accepted a not-replacement detection")
			}
			if len(*ev) != 0 {
				t.Fatalf("calls %v on the original, want none", *ev)
			}
		})
	}
}

// An untagged instance whose spawn persistent request names a different
// instance is a replacement. Retiring it cancels the request, then shuts down.
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
	if err := RetireReplacement(context.Background(), f, d, recordShutdown(ev)); err != nil {
		t.Fatalf("RetireReplacement: %v", err)
	}
	if want := []string{"cancel:" + reqID, "shutdown"}; !equal(*ev, want) {
		t.Fatalf("calls %v, want %v (cancel first, or a terminate-on-shutdown relaunches)", *ev, want)
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
			if err := RetireReplacement(context.Background(), tc.f, d, recordShutdown(ev)); err == nil {
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
		if err := RetireReplacement(context.Background(), f, d, recordShutdown(ev)); err == nil || len(*ev) != 0 {
			t.Fatalf("verdict %s: err=%v calls=%v, want refusal and no calls", v, err, *ev)
		}
	}
}

// A request that is already cancelled (EC2 accepts the repeat cancel) or that
// EC2 no longer knows still shuts the replacement down: nothing is left to
// relaunch. A cancel that fails for any other reason does not shut down.
func TestRetireReplacement_RequestAlreadyCancelled(t *testing.T) {
	cases := map[string]struct {
		cancelErr    error
		wantShutdown bool
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
			err := RetireReplacement(context.Background(), f, d, recordShutdown(ev))
			shut := len(*ev) == 2 && (*ev)[1] == "shutdown"
			if shut != tc.wantShutdown {
				t.Fatalf("calls %v (err %v); shutdown=%v, want %v", *ev, err, shut, tc.wantShutdown)
			}
			if (err == nil) != tc.wantShutdown {
				t.Fatalf("err = %v, want error iff no shutdown", err)
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
