package autoscaler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	healthRegistryTable = "spawn-registry-health-test"
	healthJobArrayID    = "job-health-001"
)

func createHealthRegistryTable(t *testing.T, client *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(healthRegistryTable),
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("job_array_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("instance_id"), KeyType: dynamodbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("job_array_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("instance_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
}

func seedHeartbeat(t *testing.T, client *dynamodb.Client, instanceID string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	ts := time.Now().Add(-age)
	_, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(healthRegistryTable),
		Item: map[string]dynamodbtypes.AttributeValue{
			"job_array_id":   &dynamodbtypes.AttributeValueMemberS{Value: healthJobArrayID},
			"instance_id":    &dynamodbtypes.AttributeValueMemberS{Value: instanceID},
			"last_heartbeat": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ts.Unix(), 10)},
		},
	})
	if err != nil {
		t.Fatalf("PutItem heartbeat for %s: %v", instanceID, err)
	}
}

func runHealthTestInstance(t *testing.T, ec2Client *ec2.Client) string {
	t.Helper()
	ctx := context.Background()
	out, err := ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	if len(out.Instances) == 0 {
		t.Fatal("RunInstances: no instances returned")
	}
	return aws.ToString(out.Instances[0].InstanceId)
}

// TestHealthChecker_AllHealthy verifies that a running instance with a recent
// heartbeat is reported healthy.
func TestHealthChecker_AllHealthy(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	createHealthRegistryTable(t, env.DynamoClient())

	instanceID := runHealthTestInstance(t, env.EC2Client())
	seedHeartbeat(t, env.DynamoClient(), instanceID, 30*time.Second)

	hc := NewHealthChecker(env.EC2Client(), env.DynamoClient(), healthRegistryTable)
	statuses, err := hc.CheckInstances(ctx, healthJobArrayID, []string{instanceID})
	if err != nil {
		t.Fatalf("CheckInstances: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}
	if !statuses[0].Healthy {
		t.Errorf("instance should be healthy, reason: %q", statuses[0].Reason)
	}
	if statuses[0].EC2State != "running" {
		t.Errorf("EC2State = %q, want running", statuses[0].EC2State)
	}
}

// TestHealthChecker_StaleHeartbeat verifies that a running instance whose
// heartbeat is more than 5 minutes old is reported unhealthy.
func TestHealthChecker_StaleHeartbeat(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	createHealthRegistryTable(t, env.DynamoClient())

	instanceID := runHealthTestInstance(t, env.EC2Client())
	seedHeartbeat(t, env.DynamoClient(), instanceID, 10*time.Minute)

	hc := NewHealthChecker(env.EC2Client(), env.DynamoClient(), healthRegistryTable)
	statuses, err := hc.CheckInstances(ctx, healthJobArrayID, []string{instanceID})
	if err != nil {
		t.Fatalf("CheckInstances: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}
	if statuses[0].Healthy {
		t.Errorf("instance with stale heartbeat should be unhealthy")
	}
	if statuses[0].Reason != "stale heartbeat" {
		t.Errorf("reason = %q, want %q", statuses[0].Reason, "stale heartbeat")
	}
}

// TestHealthChecker_InstanceTerminated verifies that a terminated instance is
// reported unhealthy.
func TestHealthChecker_InstanceTerminated(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	createHealthRegistryTable(t, env.DynamoClient())

	instanceID := runHealthTestInstance(t, env.EC2Client())

	_, err := env.EC2Client().TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		t.Fatalf("TerminateInstances: %v", err)
	}

	hc := NewHealthChecker(env.EC2Client(), env.DynamoClient(), healthRegistryTable)
	statuses, err := hc.CheckInstances(ctx, healthJobArrayID, []string{instanceID})
	if err != nil {
		t.Fatalf("CheckInstances: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses, want 1", len(statuses))
	}
	if statuses[0].Healthy {
		t.Errorf("terminated instance should be unhealthy, EC2State=%q", statuses[0].EC2State)
	}
}

// TestHealthChecker_Empty verifies that CheckInstances on an empty list returns
// an empty result without error.
func TestHealthChecker_Empty(t *testing.T) {
	env := testutil.SubstrateServer(t)
	hc := NewHealthChecker(env.EC2Client(), env.DynamoClient(), healthRegistryTable)
	statuses, err := hc.CheckInstances(context.Background(), healthJobArrayID, []string{})
	if err != nil {
		t.Fatalf("CheckInstances: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("got %d statuses, want 0", len(statuses))
	}
}

// TestCountPending verifies the CountPending helper across various status slices.
func TestCountPending(t *testing.T) {
	tests := []struct {
		name     string
		statuses []HealthStatus
		want     int
	}{
		{name: "empty", statuses: []HealthStatus{}, want: 0},
		{
			name: "one pending",
			statuses: []HealthStatus{
				{EC2State: "pending"},
				{EC2State: "running"},
			},
			want: 1,
		},
		{
			name: "all pending",
			statuses: []HealthStatus{
				{EC2State: "pending"},
				{EC2State: "pending"},
			},
			want: 2,
		},
		{
			name: "none pending",
			statuses: []HealthStatus{
				{EC2State: "running"},
				{EC2State: "terminated"},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountPending(tt.statuses)
			if got != tt.want {
				t.Errorf("CountPending = %d, want %d", got, tt.want)
			}
		})
	}
}
