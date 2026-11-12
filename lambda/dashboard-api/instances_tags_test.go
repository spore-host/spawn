package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// An instance still running a spored from before session tokens moved off
// tags carries its token in spawn:ready-token and inside spawn:ready-url. The
// API must not hand either to whoever lists instances.
func TestConvertReservationsWithholdsSessionTokenTags(t *testing.T) {
	inst := types.Instance{
		InstanceId:   aws.String("i-1"),
		InstanceType: types.InstanceTypeT3Micro,
		State:        &types.InstanceState{Name: types.InstanceStateNameRunning},
		Placement:    &types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		Tags: []types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:ready-token"), Value: aws.String("s3cr3t")},
			{Key: aws.String("spawn:ready-url"), Value: aws.String("https://h:8443/?authToken=s3cr3t#console")},
			{Key: aws.String("spawn:ready-status"), Value: aws.String("ready")},
		},
	}
	out := convertReservationsToInstances([]types.Reservation{{Instances: []types.Instance{inst}}}, "us-east-1", "abc")
	if len(out) != 1 {
		t.Fatalf("got %d instances", len(out))
	}
	for k, v := range out[0].Tags {
		if v == "s3cr3t" || k == "spawn:ready-token" || k == "spawn:ready-url" {
			t.Errorf("tag %s=%q reached the API response", k, v)
		}
	}
	if out[0].Tags["spawn:ready-status"] != "ready" || out[0].Tags["spawn:managed"] != "true" {
		t.Errorf("other tags dropped: %v", out[0].Tags)
	}
}
