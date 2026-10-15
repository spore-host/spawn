package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// spotEC2 serves one expired spot instance and records the order of the calls the
// reaper makes against it.
type spotEC2 struct {
	requestID string
	cancelErr error
	calls     []string
	cancelled []string
}

func (s *spotEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId:            aws.String("i-spot"),
		InstanceLifecycle:     ec2types.InstanceLifecycleTypeSpot,
		SpotInstanceRequestId: aws.String(s.requestID),
		State:                 &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:ttl-deadline"), Value: aws.String(past)},
		},
	}}}}}, nil
}

func (s *spotEC2) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	s.calls = append(s.calls, "cancel")
	s.cancelled = append(s.cancelled, in.SpotInstanceRequestIds...)
	if s.cancelErr != nil {
		return nil, s.cancelErr
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (s *spotEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	s.calls = append(s.calls, "terminate")
	return &ec2.TerminateInstancesOutput{}, nil
}

func spotReaper(c *spotEC2) *reaper {
	return &reaper{
		accounts: []account{{label: "acct", accountID: "111111111111", ec2For: func(string) ec2API { return c }}},
		regions:  []string{"us-east-1"},
		maxAge:   defaultMaxAge,
		dryRun:   false,
	}
}

// #922: a persistent Spot request reopens when its instance is terminated and EC2
// launches a replacement, so the reaper must cancel the request, and must do it
// BEFORE the terminate.
func TestReap_CancelsSpotRequestBeforeTerminate(t *testing.T) {
	c := &spotEC2{requestID: "sir-persist"}
	sum, err := spotReaper(c).run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Reaped != 1 {
		t.Fatalf("reaped %d, want 1 (calls %v)", sum.Reaped, c.calls)
	}
	if strings.Join(c.calls, ",") != "cancel,terminate" {
		t.Errorf("calls = %v, want cancel then terminate", c.calls)
	}
	if len(c.cancelled) != 1 || c.cancelled[0] != "sir-persist" {
		t.Errorf("cancelled %v, want [sir-persist]", c.cancelled)
	}
}

// The hard-deadline guarantee (#72) outranks the cancel: a role without
// ec2:CancelSpotInstanceRequests still reaps, and says what it could not do.
func TestReap_CancelFailureStillTerminatesAndSaysSo(t *testing.T) {
	c := &spotEC2{requestID: "sir-denied", cancelErr: errors.New("UnauthorizedOperation: denied")}
	var sum Summary
	out := captureLogs(func() { sum, _ = spotReaper(c).run(context.Background()) })
	if sum.Reaped != 1 {
		t.Fatalf("reaped %d, want 1 — a refused cancel must not spare an expired instance", sum.Reaped)
	}
	if !strings.Contains(out, "SPOT_CANCEL_FAILED") || !strings.Contains(out, "sir-denied") {
		t.Errorf("a failed cancel must be logged with the request id; got:\n%s", out)
	}
}

// An on-demand instance has no request to cancel, and the reaper makes no call.
func TestReap_OnDemandMakesNoCancelCall(t *testing.T) {
	c := &spotEC2{requestID: ""}
	if _, err := spotReaper(c).run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(c.calls, ",") != "terminate" {
		t.Errorf("calls = %v, want only terminate", c.calls)
	}
}
