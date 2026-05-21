package autoscaler

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// testLaunchTemplate is a minimal LaunchTemplate for capacity tests.
var testLaunchTemplate = LaunchTemplate{
	InstanceType: "t3.micro",
	AMI:          "ami-12345678",
}

func runCapacityTestInstances(t *testing.T, ec2Client *ec2.Client, count int) []string {
	t.Helper()
	ctx := context.Background()
	out, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(int32(count)),
		MaxCount:     aws.Int32(int32(count)),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	ids := make([]string, len(out.Instances))
	for i, inst := range out.Instances {
		ids[i] = aws.ToString(inst.InstanceId)
	}
	return ids
}

// countSubstrateInstances returns the number of non-terminated instances in Substrate.
func countSubstrateInstances(t *testing.T, ec2Client *ec2.Client, ids []string) int {
	t.Helper()
	ctx := context.Background()
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: ids,
	})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	active := 0
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			state := string(inst.State.Name)
			if state != "terminated" && state != "shutting-down" {
				active++
			}
		}
	}
	return active
}

// --- PlanCapacity tests (pure logic, no AWS) ---

func TestPlanCapacity_ScaleUp(t *testing.T) {
	rc := &CapacityReconciler{}
	group := &AutoScaleGroup{
		DesiredCapacity: 3,
		LaunchTemplate:  testLaunchTemplate,
	}
	health := []HealthStatus{
		{InstanceID: "i-001", EC2State: "running", Healthy: true},
	}

	plan, err := rc.PlanCapacity(context.Background(), group, health)
	if err != nil {
		t.Fatalf("PlanCapacity: %v", err)
	}
	if plan.ToLaunch != 2 {
		t.Errorf("ToLaunch = %d, want 2", plan.ToLaunch)
	}
	if len(plan.ToTerminate) != 0 {
		t.Errorf("ToTerminate = %v, want empty", plan.ToTerminate)
	}
	if plan.CurrentCapacity != 1 {
		t.Errorf("CurrentCapacity = %d, want 1", plan.CurrentCapacity)
	}
}

func TestPlanCapacity_ScaleDown(t *testing.T) {
	rc := &CapacityReconciler{}
	group := &AutoScaleGroup{
		DesiredCapacity: 1,
		LaunchTemplate:  testLaunchTemplate,
	}
	health := []HealthStatus{
		{InstanceID: "i-001", EC2State: "running", Healthy: true},
		{InstanceID: "i-002", EC2State: "running", Healthy: true},
		{InstanceID: "i-003", EC2State: "running", Healthy: true},
	}

	plan, err := rc.PlanCapacity(context.Background(), group, health)
	if err != nil {
		t.Fatalf("PlanCapacity: %v", err)
	}
	if plan.ToLaunch != 0 {
		t.Errorf("ToLaunch = %d, want 0", plan.ToLaunch)
	}
	if len(plan.ToTerminate) != 2 {
		t.Errorf("len(ToTerminate) = %d, want 2", len(plan.ToTerminate))
	}
}

func TestPlanCapacity_NoOp(t *testing.T) {
	rc := &CapacityReconciler{}
	group := &AutoScaleGroup{
		DesiredCapacity: 2,
		LaunchTemplate:  testLaunchTemplate,
	}
	health := []HealthStatus{
		{InstanceID: "i-001", EC2State: "running", Healthy: true},
		{InstanceID: "i-002", EC2State: "running", Healthy: true},
	}

	plan, err := rc.PlanCapacity(context.Background(), group, health)
	if err != nil {
		t.Fatalf("PlanCapacity: %v", err)
	}
	if plan.ToLaunch != 0 {
		t.Errorf("ToLaunch = %d, want 0", plan.ToLaunch)
	}
	if len(plan.ToTerminate) != 0 {
		t.Errorf("ToTerminate = %v, want empty", plan.ToTerminate)
	}
}

func TestPlanCapacity_UnhealthyInstancesMarkedForTermination(t *testing.T) {
	rc := &CapacityReconciler{}
	group := &AutoScaleGroup{
		DesiredCapacity: 2,
		LaunchTemplate:  testLaunchTemplate,
	}
	health := []HealthStatus{
		{InstanceID: "i-001", EC2State: "running", Healthy: true},
		{InstanceID: "i-002", EC2State: "terminated", Healthy: false},
		{InstanceID: "i-003", EC2State: "running", Healthy: true},
	}

	plan, err := rc.PlanCapacity(context.Background(), group, health)
	if err != nil {
		t.Fatalf("PlanCapacity: %v", err)
	}
	// i-002 is unhealthy → terminate; current healthy = 2 = desired, so no launches
	if len(plan.ToTerminate) != 1 || plan.ToTerminate[0] != "i-002" {
		t.Errorf("ToTerminate = %v, want [i-002]", plan.ToTerminate)
	}
	if plan.ToLaunch != 0 {
		t.Errorf("ToLaunch = %d, want 0", plan.ToLaunch)
	}
}

// --- ExecutePlan tests (Substrate) ---

func TestExecutePlan_Launch(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	rc := NewCapacityReconciler(env.EC2Client())
	group := &AutoScaleGroup{
		AutoScaleGroupID: "asg-exec-001",
		GroupName:        "test-group",
		JobArrayID:       "job-exec-001",
		LaunchTemplate:   testLaunchTemplate,
	}
	plan := &CapacityPlan{
		ToLaunch:    2,
		ToTerminate: []string{},
	}

	if err := rc.ExecutePlan(ctx, group, plan); err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	// Verify Substrate has 2 running instances.
	out, err := env.EC2Client().DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	count := 0
	for _, r := range out.Reservations {
		count += len(r.Instances)
	}
	if count != 2 {
		t.Errorf("got %d instances after launch, want 2", count)
	}
}

func TestExecutePlan_Terminate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	// Pre-launch 2 instances via Substrate.
	ids := runCapacityTestInstances(t, env.EC2Client(), 2)

	rc := NewCapacityReconciler(env.EC2Client())
	group := &AutoScaleGroup{
		AutoScaleGroupID: "asg-exec-002",
		GroupName:        "test-group",
		JobArrayID:       "job-exec-002",
		LaunchTemplate:   testLaunchTemplate,
	}
	plan := &CapacityPlan{
		ToLaunch:    0,
		ToTerminate: ids,
	}

	if err := rc.ExecutePlan(ctx, group, plan); err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	// All instances should now be terminated.
	active := countSubstrateInstances(t, env.EC2Client(), ids)
	if active != 0 {
		t.Errorf("got %d active instances after termination, want 0", active)
	}
}
