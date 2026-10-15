package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type recordingTerminateAPI struct {
	cancelErr error
	calls     []string
}

func (r *recordingTerminateAPI) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	r.calls = append(r.calls, "cancel:"+strings.Join(in.SpotInstanceRequestIds, ","))
	if r.cancelErr != nil {
		return nil, r.cancelErr
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (r *recordingTerminateAPI) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	r.calls = append(r.calls, "terminate:"+strings.Join(in.InstanceIds, ","))
	return &ec2.TerminateInstancesOutput{}, nil
}

func spotInstance(reqID, reqType string) types.Instance {
	inst := types.Instance{InstanceId: aws.String("i-1"), SpotInstanceRequestId: aws.String(reqID)}
	if reqType != "" {
		inst.Tags = []types.Tag{{Key: aws.String("spawn:spot-request-type"), Value: aws.String(reqType)}}
	}
	return inst
}

// spawn#922: the dashboard's terminate must cancel a Spot request before
// terminating, or a persistent request launches a replacement.
func TestTerminateCancellingSpot_CancelsFirst(t *testing.T) {
	api := &recordingTerminateAPI{}
	if err := terminateCancellingSpot(context.Background(), api, spotInstance("sir-1", "persistent")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.calls, " "); got != "cancel:sir-1 terminate:i-1" {
		t.Errorf("calls = %q, want cancel then terminate", got)
	}
}

func TestTerminateCancellingSpot_RefusesWhenPersistentCancelFails(t *testing.T) {
	api := &recordingTerminateAPI{cancelErr: errors.New("UnauthorizedOperation")}
	err := terminateCancellingSpot(context.Background(), api, spotInstance("sir-1", "persistent"))
	if err == nil || !strings.Contains(err.Error(), "sir-1") {
		t.Fatalf("want a refusal naming the request, got %v", err)
	}
	for _, c := range api.calls {
		if strings.HasPrefix(c, "terminate") {
			t.Errorf("terminated despite an uncancelled persistent request: %v", api.calls)
		}
	}
}

func TestTerminateCancellingSpot_OneTimeCancelFailureStillTerminates(t *testing.T) {
	api := &recordingTerminateAPI{cancelErr: errors.New("UnauthorizedOperation")}
	if err := terminateCancellingSpot(context.Background(), api, spotInstance("sir-1", "")); err != nil {
		t.Fatalf("a one-time request closes with its instance; got %v", err)
	}
	if got := strings.Join(api.calls, " "); got != "cancel:sir-1 terminate:i-1" {
		t.Errorf("calls = %q", got)
	}
}

func TestTerminateCancellingSpot_OnDemandNoCancel(t *testing.T) {
	api := &recordingTerminateAPI{}
	if err := terminateCancellingSpot(context.Background(), api, types.Instance{InstanceId: aws.String("i-1")}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.calls, " "); got != "terminate:i-1" {
		t.Errorf("calls = %q", got)
	}
}
