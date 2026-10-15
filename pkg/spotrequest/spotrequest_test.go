package spotrequest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

type fakeCancel struct {
	err   error
	calls int
}

func (f *fakeCancel) CancelSpotInstanceRequests(context.Context, *ec2.CancelSpotInstanceRequestsInput, ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	f.calls++
	return &ec2.CancelSpotInstanceRequestsOutput{}, f.err
}

func TestCancel_EmptyIDIsNoCall(t *testing.T) {
	f := &fakeCancel{}
	if err := Cancel(context.Background(), f, ""); err != nil || f.calls != 0 {
		t.Fatalf("err=%v calls=%d, want nil and no call for an on-demand instance", err, f.calls)
	}
}

// A request EC2 no longer knows is already gone: success, so a retried
// terminate does not fail on its own earlier cancel.
func TestCancel_NotFoundIsSuccess(t *testing.T) {
	f := &fakeCancel{err: errors.New("api error InvalidSpotInstanceRequestID.NotFound: gone")}
	if err := Cancel(context.Background(), f, "sir-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestCancel_OtherErrorsNameTheRequest(t *testing.T) {
	f := &fakeCancel{err: errors.New("UnauthorizedOperation")}
	err := Cancel(context.Background(), f, "sir-1")
	if err == nil || !strings.Contains(err.Error(), "sir-1") {
		t.Fatalf("want an error naming sir-1, got %v", err)
	}
}

func TestIsOneTime(t *testing.T) {
	if !IsOneTime(nil) || !IsOneTime(map[string]string{TypeTag: TypeOneTime}) {
		t.Error("untagged (pre-#922) and one-time-tagged instances are one-time")
	}
	if IsOneTime(map[string]string{TypeTag: TypePersistent}) {
		t.Error("persistent-tagged instance reported one-time")
	}
}
