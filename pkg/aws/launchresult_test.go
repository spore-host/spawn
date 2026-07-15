package aws

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestNewLaunchResult_NilPlacementAndState is a regression test: RunInstances
// responses may omit the optional Placement and State nested structs. spawn
// must not panic dereferencing them (it did — nil-pointer panic in Launch).
func TestNewLaunchResult_NilPlacementAndState(t *testing.T) {
	inst := ec2types.Instance{
		InstanceId:       aws.String("i-0abc123"),
		PrivateIpAddress: aws.String("10.0.0.5"),
		// Placement and State intentionally nil.
	}
	got := newLaunchResult(inst, "my-job", "us-east-1", "my-key")
	if got.InstanceID != "i-0abc123" {
		t.Errorf("InstanceID = %q, want i-0abc123", got.InstanceID)
	}
	if got.PrivateIP != "10.0.0.5" {
		t.Errorf("PrivateIP = %q, want 10.0.0.5", got.PrivateIP)
	}
	if got.AvailabilityZone != "" {
		t.Errorf("AvailabilityZone = %q, want empty (nil Placement)", got.AvailabilityZone)
	}
	if got.State != "" {
		t.Errorf("State = %q, want empty (nil State)", got.State)
	}
	if got.Name != "my-job" || got.KeyName != "my-key" {
		t.Errorf("Name/KeyName = %q/%q, want my-job/my-key", got.Name, got.KeyName)
	}
	if got.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", got.Region)
	}
	if !got.LaunchTime.IsZero() {
		t.Errorf("LaunchTime = %v, want zero (nil LaunchTime)", got.LaunchTime)
	}
}

// TestNewLaunchResult_Populated verifies normal mapping when the API returns
// the full response.
func TestNewLaunchResult_Populated(t *testing.T) {
	launched := time.Date(2026, 7, 15, 8, 30, 0, 0, time.UTC)
	inst := ec2types.Instance{
		InstanceId:       aws.String("i-0def456"),
		PrivateIpAddress: aws.String("10.0.0.6"),
		PublicIpAddress:  aws.String("54.1.2.3"),
		Placement:        &ec2types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:       aws.Time(launched),
	}
	got := newLaunchResult(inst, "job", "us-west-2", "key")
	if got.PublicIP != "54.1.2.3" {
		t.Errorf("PublicIP = %q", got.PublicIP)
	}
	if got.AvailabilityZone != "us-east-1a" {
		t.Errorf("AvailabilityZone = %q, want us-east-1a", got.AvailabilityZone)
	}
	if got.State != "running" {
		t.Errorf("State = %q, want running", got.State)
	}
	if got.Region != "us-west-2" {
		t.Errorf("Region = %q, want us-west-2", got.Region)
	}
	if !got.LaunchTime.Equal(launched) {
		t.Errorf("LaunchTime = %v, want %v", got.LaunchTime, launched)
	}
}
